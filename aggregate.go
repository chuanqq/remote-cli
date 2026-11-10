package main

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ---------------------------------------------------------------------------
// remote_aggregate 的纯 Go 聚合核心：对文件/目录树做流式扫描，按行取
// 1-based 字段，一遍算出 count / min / max / top-N。文件发现、沙箱、
// glob 过滤、二进制/超大文件跳过、逐行编码与超时都复用 scanTextFiles；
// 行 pattern 与字段过滤由聚合层自己处理，每行经 consume 回调进入。
//
// 数值精度设计：
//   - 所有数值比较（min/max、top-N、filters 的 gt/ge/lt/le）都走 big.Rat
//     精确有理数，不经 float64：|v| > 2^53 的整数在 float64 下会相撞
//     （9007199254740993 与 9007199254740992 都舍入到 2^53），这里仍严格
//     区分；0.1 与 0.10000000000000001 这类小数同样精确分序。
//   - 接受格式：[+-]?(D+ .D*? | .D+)([eE][+-]?D+)?，即普通十进制与科学
//     计数法；拒绝 NaN、Inf、十六进制（0x10）、分数（1/2）、下划线
//     （1_000）和空串（big.Rat.SetString 会接受其中一些，先手写校验挡掉）。
//   - 防内存失控：有效数字位 <= maxAggDigits、十进制指数绝对值
//     <= maxAggExponent、字面量长度 <= maxAggValueLen。1e10000 展开约
//     4.3KB，top_n(<=1000) 条记录常驻约 6MB 上限；超限字面量按
//     invalid_number 跳过。
//   - 覆盖范围：约 ±10^14096（4096 位尾数 + 10000 指数），对日志数值
//     （金额、耗时、计数器、订单号）绰绰有余。
// ---------------------------------------------------------------------------

const (
	defaultAggTopN = 10   // top_n 缺省值
	maxAggTopN     = 1000 // top_n 上限
	// defaultAggPreview 是记录里行内容摘录的缺省字节上限（仅用于定位行）。
	defaultAggPreview = 200
	maxAggPreview     = 2000
	// maxAggDigits 限制数值的有效数字位（整数位+小数位）。
	maxAggDigits = 4096
	// maxAggExponent 限制十进制指数的绝对值：1e10000 展开约 4.3KB，再大
	// 的指数会让 big.Rat 的分子/分母线性膨胀。
	maxAggExponent = 10000
	// maxAggValueLen 限制数值字面量总长（含符号、小数点与指数部分）。
	// 合法数值必短于此，记录里保留的原值也不会超（截断仅作防御）。
	maxAggValueLen       = 8192
	maxAggField          = 1024
	maxAggFilters        = 32
	maxAggDelimiterLen   = 256
	maxAggFilterValueLen = 8192
)

// 聚合行的跳过原因（result.lines_skipped 的键）。cut 行整行跳过（无法
// 保证字段完整，绝不能拿截断字段做极值）；缺列与坏数值只淘汰该行并计数。
const (
	aggSkipMissingField  = "missing_field"  // 目标/过滤字段越界（行内没有这一列）
	aggSkipInvalidNumber = "invalid_number" // 字段不是有效数值（含超限、NaN/Inf）
	aggSkipLineCut       = "line_cut"       // 原行超过单行扫描上限（1MB）被截断
)

// AggregateFilter 对单个字段做行内过滤；多条 filter 之间是 AND。
type AggregateFilter struct {
	Field int    `json:"field"` // 1-based 字段号
	Op    string `json:"op"`    // eq|ne|contains|regex|gt|ge|lt|le（后四个按精确数值比较）
	Value string `json:"value"`
}

// AggregateRequest 描述一次字段聚合：扫描 path 下的文件/目录，逐行按
// delimiter 切分取 1-based Field，可选行 pattern 与 filters 过滤，统计
// count / min / max / top-N。不做 group-by/sum/avg。
type AggregateRequest struct {
	Path           string
	Field          int    // 统计目标字段（1-based，>=1）
	Delimiter      string // 空 = 按空白合并切分；非空 = 字面分隔符（保留空列，非 CSV）
	Pattern        string // 可选的行级 RE2 预过滤（空 = 匹配所有行）
	IgnoreCase     bool   // 作用于行 pattern、filters 的 eq/ne/contains/regex
	FixedStrings   bool   // 行 pattern 是字面文本（grep -F）
	Filters        []AggregateFilter
	IncludeGlob    []string
	ExcludeGlob    []string
	MaxFileSize    int64
	IncludeHidden  bool
	Encoding       string
	TopN           int    // 默认 10，上限 1000；count/min/max 不受影响
	SortOrder      string // "desc"（默认，top 取最大 N 条）| "asc"（取最小 N 条）
	MaxLinePreview int    // 记录里行内容摘录的字节上限：0 = 默认 200，负数 = 不返回
	Deadline       time.Time
	Context        context.Context
}

// AggregateRecord 是一条 min/max/top 记录：保留字段原值字符串与定位信息，
// 行内容摘录仅用于回文件定位，长度受 MaxLinePreview 限制。
type AggregateRecord struct {
	Value   string `json:"value"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Content string `json:"content,omitempty"`
}

// AggregateResult 把统计放前面，摘要字段在前、记录列表在后。
// Count 定义：通过行 pattern 与全部 filters 且目标字段为有效数值的行数。
// LinesMatched：通过行 pattern 与全部 filters 的行数（含目标字段缺列或
// 坏数值的行）。无有效数值时 Count=0 且省略 min/max。
type AggregateResult struct {
	Path          string            `json:"path"`
	Pattern       string            `json:"pattern,omitempty"`
	Field         int               `json:"field"`
	Delimiter     string            `json:"delimiter,omitempty"` // 空 = 按空白合并切分
	SortOrder     string            `json:"sort_order"`          // desc | asc
	Count         int               `json:"count"`
	Min           *AggregateRecord  `json:"min,omitempty"`
	Max           *AggregateRecord  `json:"max,omitempty"`
	LinesScanned  int               `json:"lines_scanned"`           // consume 见到的行数（含 cut 行）
	LinesMatched  int               `json:"lines_matched"`           // 通过 pattern+filters 的行数
	LinesSkipped  map[string]int    `json:"lines_skipped,omitempty"` // missing_field / invalid_number / line_cut
	FilesSearched int               `json:"files_searched"`
	FilesSkipped  int               `json:"files_skipped"`
	SkipReasons   map[string]int    `json:"skip_reasons,omitempty"` // 来自扫描层：binary, too_large, unreadable, ...
	SymlinkDirs   []string          `json:"symlink_dirs,omitempty"`
	LongLines     int               `json:"long_lines,omitempty"`    // 超过单行扫描上限的行数（与 line_cut 一致）
	Truncated     bool              `json:"truncated"`               // 扫描提前结束（超时/取消）：统计只覆盖已扫描部分
	ScanComplete  bool              `json:"scan_complete"`           // 扫描无超时、跳过文件或超长行
	TopTruncated  bool              `json:"top_truncated,omitempty"` // top 明细受响应窗口限制，总数与极值仍保留
	Hint          string            `json:"hint,omitempty"`
	Top           []AggregateRecord `json:"top"`
}

// aggOpts 是校验后的 AggregateRequest。
type aggOpts struct {
	field      int
	maxField   int
	delim      string
	pattern    string // 原始 pattern（提示用）
	re         *regexp.Regexp
	filters    []*aggFilter
	ignoreCase bool
	topN       int
	asc        bool
	preview    int
}

// aggFilter 是编译后的 AggregateFilter。
type aggFilter struct {
	field int
	op    string
	value string
	re    *regexp.Regexp // regex
	num   *big.Rat       // gt/ge/lt/le 的常量侧（构造期解析，失败即请求错误）
}

func newAggOpts(req AggregateRequest) (*aggOpts, error) {
	if req.Field < 1 || req.Field > maxAggField {
		return nil, fmt.Errorf("field must be between 1 and %d (got %d)", maxAggField, req.Field)
	}
	if len(req.Filters) > maxAggFilters || len(req.Delimiter) > maxAggDelimiterLen {
		return nil, fmt.Errorf("at most %d filters and a delimiter of at most %d bytes are allowed", maxAggFilters, maxAggDelimiterLen)
	}
	o := &aggOpts{field: req.Field, maxField: req.Field, delim: req.Delimiter, ignoreCase: req.IgnoreCase}
	if p := req.Pattern; p != "" {
		o.pattern = p
		expr := p
		if req.FixedStrings {
			expr = regexp.QuoteMeta(expr)
		}
		if req.IgnoreCase {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern: %w (RE2 syntax; set fixed_strings=true to match literal text)", err)
		}
		o.re = re
	}
	for i, f := range req.Filters {
		af := &aggFilter{field: f.Field, op: strings.ToLower(strings.TrimSpace(f.Op)), value: f.Value}
		if af.field < 1 || af.field > maxAggField {
			return nil, fmt.Errorf("filters[%d].field must be between 1 and %d", i, maxAggField)
		}
		if len(af.value) > maxAggFilterValueLen {
			return nil, fmt.Errorf("filters[%d].value exceeds %d bytes", i, maxAggFilterValueLen)
		}
		o.maxField = max(o.maxField, af.field)
		switch af.op {
		case "eq", "ne", "contains":
		case "regex":
			expr := af.value
			if req.IgnoreCase {
				expr = "(?i)" + expr
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, fmt.Errorf("filters[%d].value %q: %w (RE2 syntax)", i, af.value, err)
			}
			af.re = re
		case "gt", "ge", "lt", "le":
			n, ok := parseAggNumber(af.value)
			if !ok {
				return nil, fmt.Errorf("filters[%d].value %q is not a valid number (op %s wants a decimal number; mantissa <= %d digits, |exponent| <= %d)",
					i, af.value, af.op, maxAggDigits, maxAggExponent)
			}
			af.num = n
		default:
			return nil, fmt.Errorf("filters[%d].op %q is invalid (want eq, ne, contains, regex, gt, ge, lt or le)", i, af.op)
		}
		o.filters = append(o.filters, af)
	}
	switch strings.ToLower(strings.TrimSpace(req.SortOrder)) {
	case "", "desc":
	case "asc":
		o.asc = true
	default:
		return nil, fmt.Errorf("invalid sort_order %q (want desc or asc)", req.SortOrder)
	}
	o.topN = req.TopN
	if o.topN <= 0 {
		o.topN = defaultAggTopN
	}
	o.topN = min(o.topN, maxAggTopN)
	switch {
	case req.MaxLinePreview > 0:
		o.preview = min(req.MaxLinePreview, maxAggPreview)
	case req.MaxLinePreview == 0:
		o.preview = defaultAggPreview
	} // 负数 = 不返回行内容摘录
	return o, nil
}

// AggregateContent 扫描 path（文件或目录树，沙箱限制在 roots 内），对每行
// 取 1-based Field 字段做 count / min / max / top-N，单遍流式完成：min/max/
// count 始终覆盖全部有效行，top 堆只占 O(top_n) 内存。空结果（Count=0）是
// 成功；扫描超时/取消不报错，结果带 Truncated=true。
func AggregateContent(req AggregateRequest, roots []string) (*AggregateResult, error) {
	o, err := newAggOpts(req)
	if err != nil {
		return nil, err
	}
	order := "desc"
	if o.asc {
		order = "asc"
	}
	r := &aggRun{opts: o, res: &AggregateResult{Pattern: o.pattern, Field: o.field, Delimiter: o.delim,
		SortOrder: order, Top: []AggregateRecord{}}, top: &aggTopHeap{cap: o.topN, asc: o.asc}}
	// 行 pattern 由聚合层自己匹配，扫描层只负责发现文件与逐行解码，
	// 其 Pattern 字段不可为空，用 .* 表示逐行全交。
	scan, err := scanTextFiles(SearchContentRequest{
		Path:          req.Path,
		Pattern:       ".*",
		IncludeGlob:   req.IncludeGlob,
		ExcludeGlob:   req.ExcludeGlob,
		MaxFileSize:   req.MaxFileSize,
		IncludeHidden: req.IncludeHidden,
		Encoding:      req.Encoding,
		Deadline:      req.Deadline,
		Context:       req.Context,
	}, roots, r.consume)
	if err != nil {
		return nil, err
	}
	r.finish(scan)
	return r.res, nil
}

// aggRun 是一次聚合的状态。
type aggRun struct {
	opts *aggOpts
	res  *AggregateResult
	minR *aggRec // 当前最小值记录（nil = 尚无有效行）
	maxR *aggRec // 当前最大值记录
	top  *aggTopHeap
}

// aggRec 是一条候选/极值记录：数值与展示用原值各存一份。
type aggRec struct {
	num   *big.Rat
	rec   AggregateRecord
	order int
}

func (r *aggRun) skip(reason string) {
	if r.res.LinesSkipped == nil {
		r.res.LinesSkipped = map[string]int{}
	}
	r.res.LinesSkipped[reason]++
}

// consume 处理扫描层交来的一行（已解码为 UTF-8 文本）。cut=true 表示原行
// 超过单行扫描上限被截断，字段不可信：整行跳过并计数，绝不参与统计。
func (r *aggRun) consume(path string, line int, text string, cut bool) {
	o, res := r.opts, r.res
	res.LinesScanned++
	if cut {
		r.skip(aggSkipLineCut)
		return
	}
	if o.re != nil && !o.re.MatchString(text) {
		return
	}
	fields := splitAggFields(text, o.delim, o.maxField)
	for _, f := range o.filters {
		if pass, reason := f.eval(fields, o.ignoreCase); !pass {
			if reason != "" {
				r.skip(reason) // 缺列/坏数值：淘汰并计数；正常不匹配只淘汰
			}
			return
		}
	}
	res.LinesMatched++
	if o.field > len(fields) {
		r.skip(aggSkipMissingField)
		return
	}
	raw := strings.TrimSpace(fields[o.field-1])
	num, ok := parseAggNumber(raw)
	if !ok {
		r.skip(aggSkipInvalidNumber)
		return
	}
	res.Count++
	if r.minR != nil && num.Cmp(r.minR.num) >= 0 && num.Cmp(r.maxR.num) <= 0 && !r.top.accepts(num) {
		return
	}
	rec := r.record(raw, path, line, text)
	// 严格更小/更大才替换：并列保留先出现者；count 与 min/max 不受 top_n 限制。
	if r.minR == nil || num.Cmp(r.minR.num) < 0 {
		r.minR = &aggRec{num: num, rec: rec}
	}
	if r.maxR == nil || num.Cmp(r.maxR.num) > 0 {
		r.maxR = &aggRec{num: num, rec: rec}
	}
	r.top.offer(num, rec)
}

// record 组装一条 min/max/top 记录：保留字段原值字符串，行内容摘录仅用于
// 定位（受 MaxLinePreview 限制）。
func (r *aggRun) record(raw, path string, line int, text string) AggregateRecord {
	rec := AggregateRecord{Value: strings.Clone(raw), File: path, Line: line}
	if len(rec.Value) > maxAggValueLen { // 防御：合法数值必短于此
		rec.Value = excerpt(rec.Value, nil, maxAggValueLen)
	}
	if r.opts.preview > 0 {
		rec.Content = strings.Clone(excerpt(text, nil, r.opts.preview))
	}
	return rec
}

// finish 从扫描结果复制文件级统计，并解释截断与跳过。
func (r *aggRun) finish(scan *SearchContentResult) {
	res, o := r.res, r.opts
	res.Path = scan.Path
	res.FilesSearched = scan.FilesSearched
	res.FilesSkipped = scan.FilesSkipped
	res.SkipReasons = scan.SkipReasons
	res.SymlinkDirs = scan.SymlinkDirs
	res.LongLines = scan.LongLines
	res.Truncated = scan.Truncated
	res.ScanComplete = !scan.Truncated && scan.FilesSkipped == 0 && scan.LongLines == 0
	if r.minR != nil {
		res.Min = &r.minR.rec
	}
	if r.maxR != nil {
		res.Max = &r.maxR.rec
	}
	res.Top = r.top.sorted()

	var hints []string
	if res.Truncated {
		hints = append(hints, fmt.Sprintf("扫描提前结束（超时或取消）：统计仅覆盖已扫描的 %d 个文件；收窄 path/include_glob 或加大超时后重扫", scan.FilesSearched))
	}
	if res.Count == 0 && o.pattern != "" {
		if h := patternHint(o.pattern); h != "" {
			hints = append(hints, h)
		}
	}
	if n := res.LinesSkipped[aggSkipLineCut]; n > 0 {
		hints = append(hints, fmt.Sprintf("%d 行超过单行扫描上限（%d 字节）被跳过，其字段可能不完整", n, searchLineBuf))
	}
	if n := res.LinesSkipped[aggSkipMissingField]; n > 0 {
		hints = append(hints, fmt.Sprintf("%d 行缺少字段（目标或过滤字段越界）被跳过", n))
	}
	if n := res.LinesSkipped[aggSkipInvalidNumber]; n > 0 {
		hints = append(hints, fmt.Sprintf("%d 行的字段不是有效数值被跳过（格式 [+-]D[.D][eE[+-]D]，尾数 <= %d 位，|指数| <= %d，拒绝 NaN/Inf）", n, maxAggDigits, maxAggExponent))
	}
	if n := res.SkipReasons["too_large"]; n > 0 {
		hints = append(hints, fmt.Sprintf("%d 个超过 max_file_size 的文件被跳过", n))
	}
	if len(res.SymlinkDirs) > 0 {
		hints = append(hints, "符号链接目录不会被进入：可把其路径作为 path 单独聚合")
	}
	if res.Count > len(res.Top) && len(res.Top) == o.topN {
		hints = append(hints, fmt.Sprintf("top 展示 %d 条（%s），count/min/max 覆盖全部 %d 条有效记录", len(res.Top), res.SortOrder, res.Count))
	}
	if res.FilesSkipped > 0 {
		hints = append(hints, fmt.Sprintf("%d 个文件或目录被跳过，完整范围请检查 skip_reasons", res.FilesSkipped))
	}
	res.Hint = strings.Join(hints, "; ")
	r.limitTop()
}

// limitTop 按实际 JSON 字节数控制返回窗口；裁剪展示记录不影响 count/min/max。
func (r *aggRun) limitTop() {
	res := r.res
	view := *res
	view.Top = []AggregateRecord{}
	base, _ := json.Marshal(view)
	used, kept := len(base)+256, 0 // 为提示、标记和包装元数据预留空间
	for _, rec := range res.Top {
		encoded, _ := json.Marshal(rec)
		if used+len(encoded)+1 > maxSearchOutputBytes {
			break
		}
		used += len(encoded) + 1
		kept++
	}
	if kept < len(res.Top) {
		keptRecords := make([]AggregateRecord, kept)
		copy(keptRecords, res.Top[:kept])
		res.Top = keptRecords
		res.TopTruncated = true
		if res.Hint != "" {
			res.Hint += "; "
		}
		res.Hint += "top 明细达到 64KB 响应窗口，count/min/max 不受影响；可减小 top_n 或关闭 include_content"
	}
}

// splitAggFields 切分一行：delimiter 为空时按空白合并切分（awk 风格：连续
// 空白算一个分隔符，首尾空白忽略，不产生空字段）；非空时按字面分隔符
// 切分并保留空列（"a,,b" 按逗号是 3 列，第 2 列为空串），不做引号/转义
// 处理（非 CSV）。delimiter 可以是任意字面字符串（如 "|"、"::"）。
func splitAggFields(text, delim string, limit int) []string {
	fields := make([]string, 0, min(limit, 16))
	if delim == "" {
		start := -1
		for i, c := range text {
			if unicode.IsSpace(c) {
				if start >= 0 {
					fields = append(fields, text[start:i])
					if len(fields) == limit {
						return fields
					}
					start = -1
				}
			} else if start < 0 {
				start = i
			}
		}
		if start >= 0 {
			fields = append(fields, text[start:])
		}
		return fields
	}
	for len(fields) < limit {
		i := strings.Index(text, delim)
		if i < 0 {
			fields = append(fields, text)
			break
		}
		fields = append(fields, text[:i])
		text = text[i+len(delim):]
	}
	return fields
}

// eval 对切分后的字段求值一条过滤。第二返回值非空表示淘汰原因是数据
// 问题（缺列/坏数值），需要计数；为空表示正常不匹配，只淘汰不计数。
func (f *aggFilter) eval(fields []string, ignoreCase bool) (bool, string) {
	if f.field < 1 || f.field > len(fields) {
		return false, aggSkipMissingField
	}
	v := fields[f.field-1]
	switch f.op {
	case "eq":
		if ignoreCase {
			return strings.EqualFold(v, f.value), ""
		}
		return v == f.value, ""
	case "ne":
		if ignoreCase {
			return !strings.EqualFold(v, f.value), ""
		}
		return v != f.value, ""
	case "contains":
		if ignoreCase {
			return strings.Contains(strings.ToLower(v), strings.ToLower(f.value)), ""
		}
		return strings.Contains(v, f.value), ""
	case "regex":
		return f.re.MatchString(v), ""
	case "gt", "ge", "lt", "le":
		n, ok := parseAggNumber(v)
		if !ok {
			return false, aggSkipInvalidNumber
		}
		c := n.Cmp(f.num)
		switch f.op {
		case "gt":
			return c > 0, ""
		case "ge":
			return c >= 0, ""
		case "lt":
			return c < 0, ""
		default:
			return c <= 0, ""
		}
	}
	return false, ""
}

// parseAggNumber 把严格十进制字面量解析为精确有理数（big.Rat）。先手写
// 校验格式与长度再交给 big.Rat.SetString：SetString 会接受 0x10、1/2、
// 1_000 这类形式，且 1e100000000 会把分子展开成约 40MB 的整数。
// 超限字面量返回 false，调用方按 invalid_number 跳过。
func parseAggNumber(s string) (*big.Rat, bool) {
	if s == "" || len(s) > maxAggValueLen {
		return nil, false
	}
	i, digits := 0, 0
	if s[i] == '+' || s[i] == '-' {
		i++
	}
	for i < len(s) && isDigit(s[i]) { // 整数部分
		i, digits = i+1, digits+1
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && isDigit(s[i]) { // 小数部分
			i, digits = i+1, digits+1
		}
	}
	if digits == 0 || digits > maxAggDigits { // 整数和小数部分至少一位数字
		return nil, false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		if i == start { // 指数无数字
			return nil, false
		}
		if e, err := strconv.Atoi(s[start:i]); err != nil || e > maxAggExponent || e < -maxAggExponent {
			return nil, false // err 只在 Atoi 溢出时出现（长度已被 maxAggValueLen 限制）
		}
	}
	if i != len(s) { // 尾部有多余字符
		return nil, false
	}
	q, ok := new(big.Rat).SetString(s) // 格式已校验，不会失败，防御性检查
	if !ok {
		return nil, false
	}
	return q, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// aggTopHeap 是容量受限的 top-N 堆：desc（默认）保留最大的 N 个值，是
// 小顶堆（堆顶为入围值中最小者）；asc 保留最小的 N 个，是大顶堆。新值
// 严格胜过堆顶才替换（并列保留先出现者），内存 O(top_n)。
type aggTopHeap struct {
	cap   int
	asc   bool
	items []*aggRec
	next  int
}

func (h *aggTopHeap) Len() int { return len(h.items) }
func (h *aggTopHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
}
func (h *aggTopHeap) Push(x any) { h.items = append(h.items, x.(*aggRec)) }
func (h *aggTopHeap) Pop() any {
	old := h.items
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	h.items = old[:n-1]
	return it
}

// Less 决定谁更靠近堆顶：堆顶必须是最差候选（desc 时最小、asc 时最大）。
func (h *aggTopHeap) Less(i, j int) bool {
	c := h.items[i].num.Cmp(h.items[j].num)
	if c == 0 {
		return h.items[i].order > h.items[j].order
	}
	if h.asc {
		return c > 0
	}
	return c < 0
}

// accepts 先判定是否入围，避免为不会保留的行复制字段和正文。
func (h *aggTopHeap) accepts(num *big.Rat) bool {
	if h.cap <= 0 {
		return false
	}
	if len(h.items) < h.cap {
		return true
	}
	c := num.Cmp(h.items[0].num)
	return h.asc && c < 0 || !h.asc && c > 0
}

// offer 把一条记录纳入 top-N 候选；数值并列时保留先遇到的行。
func (h *aggTopHeap) offer(num *big.Rat, rec AggregateRecord) {
	if !h.accepts(num) {
		return
	}
	item := &aggRec{num: num, rec: rec, order: h.next}
	h.next++
	if len(h.items) < h.cap {
		heap.Push(h, item)
		return
	}
	h.items[0] = item
	heap.Fix(h, 0)
}

// sorted 返回 top-N：desc 按值从大到小、asc 从小到大（并列按首见顺序）。
func (h *aggTopHeap) sorted() []AggregateRecord {
	items := make([]*aggRec, len(h.items))
	copy(items, h.items)
	sort.SliceStable(items, func(i, j int) bool {
		c := items[i].num.Cmp(items[j].num)
		if c == 0 {
			return items[i].order < items[j].order
		}
		if h.asc {
			return c < 0
		}
		return c > 0
	})
	out := make([]AggregateRecord, len(items))
	for i, it := range items {
		out[i] = it.rec
	}
	return out
}
