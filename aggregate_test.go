package main

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// aggSetupTree 建一棵常见的聚合测试树：CSV 风格（逗号分隔）、空白分隔
// 日志、隐藏目录与二进制文件各一。
func aggSetupTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub"), 0755)
	os.MkdirAll(filepath.Join(root, ".git"), 0755)
	writeTree(t, root, map[string]string{
		"a.csv":      "user,amt\nalice,10\nbob,20\nalice,-5\nbob,not_a_number\nnobody\n",
		"sub/b.csv":  "user,amt\ncarol,300\n",
		".git/c.csv": "user,amt\ndave,9999\n",
	})
	os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0, 1, 'x'}, 0644)
	return root
}

func TestAggregateBasic(t *testing.T) {
	root := aggSetupTree(t)
	// 逗号字面分隔：a.csv + sub/b.csv 各有一行表头（user 非数值 → invalid_number）、
	// a.csv 另有一行缺列、一行坏数值；.git 隐藏跳过。
	res, err := AggregateContent(AggregateRequest{Path: root, Field: 2, Delimiter: ","}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 4 { // 10, 20, -5, 300
		t.Errorf("count=%d, want 4: %+v", res.Count, res)
	}
	if res.Min.Value != "-5" || res.Max.Value != "300" {
		t.Errorf("min=%q max=%q", res.Min.Value, res.Max.Value)
	}
	if got := res.LinesSkipped[aggSkipInvalidNumber]; got != 3 { // 两个表头 + not_a_number
		t.Errorf("invalid_number=%d, want 3", got)
	}
	if got := res.LinesSkipped[aggSkipMissingField]; got != 1 { // nobody
		t.Errorf("missing_field=%d, want 1", got)
	}
	// 表头行（user,amt）通过了 pattern+filters（没有过滤），计入 lines_matched
	if res.LinesMatched != 8 { // 6 + 2 行（两文件的表头也通过过滤）
		t.Errorf("lines_matched=%d, want 8", res.LinesMatched)
	}
	if len(res.Top) != 4 || res.Top[0].Value != "300" || res.Top[3].Value != "-5" {
		t.Errorf("top=%+v", res.Top)
	}
	if res.Top[0].File != filepath.Join(root, "sub", "b.csv") || res.Top[0].Line != 2 {
		t.Errorf("top[0] 定位错误: %+v", res.Top[0])
	}
	if res.FilesSearched != 2 || res.SkipReasons["binary"] != 1 {
		t.Errorf("files: searched=%d skips=%+v", res.FilesSearched, res.SkipReasons)
	}
	// count/min/max/top 都覆盖全部有效行，不受 top_n 限制（这里本来就少）。
	if res.Truncated {
		t.Errorf("不应截断: %+v", res)
	}
}

// 空白合并切分 vs 字面分隔符：delimiter 省略时连续空白算一个、不产生空
// 列；字面分隔符保留空列。
func TestAggregateDelimiters(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"ws.log":  "a  10\tok\n  b   20  \n",
		"lit.log": "a,,20\n,10\n",
	})
	res, _ := AggregateContent(AggregateRequest{Path: filepath.Join(root, "ws.log"), Field: 2}, []string{root})
	if res.Count != 2 || res.Max.Value != "20" || res.Min.Value != "10" {
		t.Errorf("whitespace 合并切分: %+v", res)
	}
	// 字面逗号：a,,20 第 2 列是空串（invalid_number）；,10 第 1 列空。
	res, _ = AggregateContent(AggregateRequest{Path: filepath.Join(root, "lit.log"), Field: 2, Delimiter: ","}, []string{root})
	if res.Count != 1 || res.Max.Value != "10" {
		t.Errorf("literal 保留空列: %+v", res)
	}
	if res.LinesSkipped[aggSkipInvalidNumber] != 1 {
		t.Errorf("空列应按 invalid_number 计数: %+v", res.LinesSkipped)
	}
}

// 行 pattern 与 filters（AND 语义）。
func TestAggregatePatternAndFilters(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"s.log": "GET /a 10\nPOST /b 20\nGET /c 30\nGET /d abc\nPOST /e 40\n",
	})
	req := AggregateRequest{
		Path:    filepath.Join(root, "s.log"),
		Field:   3,
		Pattern: "^GET",
		Filters: []AggregateFilter{
			{Field: 2, Op: "ne", Value: "/a"},
			{Field: 3, Op: "ge", Value: "20"},
		},
	}
	res, err := AggregateContent(req, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	// GET /c 30 唯一通过：/a 被 ne 挡，/d 数值无效。
	if res.Count != 1 || res.Max.Value != "30" {
		t.Errorf("pattern+filters: %+v", res)
	}
	if res.LinesScanned != 5 {
		t.Errorf("lines_scanned=%d, want 5", res.LinesScanned)
	}

	// ignore_case 作用于 pattern 与 eq/contains。
	writeTree(t, root, map[string]string{"i.log": "GET 1\nget 2\n"})
	res, _ = AggregateContent(AggregateRequest{Path: filepath.Join(root, "i.log"), Field: 2,
		Pattern: "get", IgnoreCase: true}, []string{root})
	if res.Count != 2 {
		t.Errorf("icase pattern: %+v", res)
	}

	// fixed_strings：点号不当正则。
	writeTree(t, root, map[string]string{"f.log": "a.b 1\naxb 2\n"})
	res, _ = AggregateContent(AggregateRequest{Path: filepath.Join(root, "f.log"), Field: 2,
		Pattern: "a.b", FixedStrings: true}, []string{root})
	if res.Count != 1 {
		t.Errorf("fixed_strings: %+v", res)
	}
}

// filters 的全部 op；数值过滤对非数值字段计 invalid_number。
func TestAggregateFilterOps(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"d.log": "v1 v2\n1 10\n2 20\n3 30\nx 40\n",
	})
	base := AggregateRequest{Path: filepath.Join(root, "d.log"), Field: 2}
	cases := []struct {
		op    string
		value string
		count int
	}{
		// 期望值按“过滤通过 且 目标字段(field 2)有效数值”计：表头行 v1 v2
		// 的第 2 列是 "v2"，字符串 op 会通过它但计数时按 invalid_number 淘汰；
		// 数值 op 直接在过滤处淘汰并计 invalid_number。
		{"eq", "20", 1},
		{"ne", "20", 3},      // 10/30/40（v2 行目标字段无效）
		{"contains", "2", 1}, // 通过: v2、20；有效: 20
		{"regex", "^2", 1},   // 20
		{"gt", "10", 3},      // 20/30/40（v2 非数值，过滤处淘汰）
		{"ge", "20", 3},      // 20/30/40
		{"lt", "30", 2},      // 10/20
		{"le", "20", 2},      // 10/20
	}
	for _, c := range cases {
		req := base
		req.Filters = []AggregateFilter{{Field: 2, Op: c.op, Value: c.value}}
		res, err := AggregateContent(req, []string{root})
		if err != nil {
			t.Fatalf("%s: %v", c.op, err)
		}
		if res.Count != c.count {
			t.Errorf("op=%s value=%s: count=%d, want %d", c.op, c.value, res.Count, c.count)
		}
	}
	// 数值过滤遇到非数值字段：淘汰 + invalid_number 计数（只有表头 v2）。
	req := base
	req.Filters = []AggregateFilter{{Field: 2, Op: "gt", Value: "0"}}
	res, _ := AggregateContent(req, []string{root})
	if got := res.LinesSkipped[aggSkipInvalidNumber]; got != 1 {
		t.Errorf("gt 非数值计数=%d, want 1", got)
	}
	if res.Count != 4 { // 10/20/30/40 全 > 0
		t.Errorf("gt 0: count=%d, want 4", res.Count)
	}
	// ignore_case 作用于 eq/contains。
	writeTree(t, root, map[string]string{"c.log": "Ab 1\nab 2\ncd 3\n"})
	req = AggregateRequest{Path: filepath.Join(root, "c.log"), Field: 2, IgnoreCase: true,
		Filters: []AggregateFilter{{Field: 1, Op: "eq", Value: "AB"}}}
	res, _ = AggregateContent(req, []string{root})
	if res.Count != 2 {
		t.Errorf("icase eq: %+v", res)
	}
}

// contains 与数值无关时不受目标字段影响：20、21 都含 "2"。
func TestAggregateFilterContains(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"d.log": "1 10\n2 20\n3 30\n4 21\n"})
	res, _ := AggregateContent(AggregateRequest{Path: filepath.Join(root, "d.log"), Field: 2,
		Filters: []AggregateFilter{{Field: 2, Op: "contains", Value: "2"}}}, []string{root})
	if res.Count != 2 { // 20、21
		t.Errorf("contains 2: count=%d, want 2", res.Count)
	}
}

// top-N：默认 10 条、desc/asc、并列保留首见、count/min/max 不受 top_n 限制。
func TestAggregateTopN(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 25; i++ {
		sb.WriteString(strings.Repeat("x", i%3+1) + " " + strconv.Itoa(i) + "\n")
	}
	sb.WriteString("dup 20\n") // 与行 20 的 20 并列，应保留先见者
	writeTree(t, root, map[string]string{"n.log": sb.String()})

	// top_n=6，默认 desc：25/24/23/22/21/20（并列的 20 保留先出现的行 20）。
	res, _ := AggregateContent(AggregateRequest{Path: filepath.Join(root, "n.log"), Field: 2, TopN: 6}, []string{root})
	if res.Count != 26 || len(res.Top) != 6 {
		t.Fatalf("top: count=%d len=%d", res.Count, len(res.Top))
	}
	if res.Top[0].Value != "25" || res.Top[5].Value != "20" || res.Top[5].Line != 20 {
		t.Errorf("desc top6: %+v", res.Top)
	}
	if !strings.Contains(res.Hint, "count/min/max 覆盖全部") {
		t.Errorf("top 截断提示缺失: %q", res.Hint)
	}

	// asc：最小 3 个 1、2、3。
	res, _ = AggregateContent(AggregateRequest{Path: filepath.Join(root, "n.log"), Field: 2, TopN: 3, SortOrder: "asc"}, []string{root})
	if res.Top[0].Value != "1" || res.Top[2].Value != "3" || res.SortOrder != "asc" {
		t.Errorf("asc top3: %+v", res.Top)
	}

	// top_n 缺省 10：min/max/count 仍覆盖全部 26 条（dup 20 也占一个槽位）。
	res, _ = AggregateContent(AggregateRequest{Path: filepath.Join(root, "n.log"), Field: 2}, []string{root})
	if len(res.Top) != 10 || res.Top[9].Value != "17" {
		t.Errorf("default top: len=%d last=%+v", len(res.Top), res.Top[9])
	}
	if res.Min.Value != "1" || res.Max.Value != "25" || res.Count != 26 {
		t.Errorf("全量统计: min=%q max=%q count=%d", res.Min.Value, res.Max.Value, res.Count)
	}
}

// 精度保障：超过 2^53 的整数严格分序，不走 float64。
func TestAggregateBigNumberPrecision(t *testing.T) {
	root := t.TempDir()
	// 2^53+1 与 2^53 在 float64 下相等；9007199254740993 = 2^53+1。
	writeTree(t, root, map[string]string{
		"ids.log": "k 9007199254740992\nk 9007199254740993\nk 9007199254740992\n",
	})
	res, _ := AggregateContent(AggregateRequest{Path: filepath.Join(root, "ids.log"), Field: 2}, []string{root})
	if res.Max.Value != "9007199254740993" || res.Min.Value != "9007199254740992" {
		t.Errorf("2^53 精度: min=%q max=%q", res.Min.Value, res.Max.Value)
	}
	// big.Rat 直接比较二者确实不等。
	if new(big.Rat).SetInt64(9007199254740993).Cmp(new(big.Rat).SetInt64(9007199254740992)) == 0 {
		t.Error("前提不成立")
	}
	// 大数过滤同样精确。
	res, _ = AggregateContent(AggregateRequest{Path: filepath.Join(root, "ids.log"), Field: 2,
		Filters: []AggregateFilter{{Field: 2, Op: "ge", Value: "9007199254740993"}}}, []string{root})
	if res.Count != 1 || res.Max.Value != "9007199254740993" {
		t.Errorf("大数 ge 过滤: %+v", res)
	}
	// 小数精度：0.1 与 0.10000000000000001 精确分序。
	writeTree(t, root, map[string]string{"f.log": "a 0.1\nb 0.10000000000000001\n"})
	res, _ = AggregateContent(AggregateRequest{Path: filepath.Join(root, "f.log"), Field: 2}, []string{root})
	if res.Min.Value != "0.1" || res.Max.Value != "0.10000000000000001" {
		t.Errorf("小数精度: min=%q max=%q", res.Min.Value, res.Max.Value)
	}
}

// parseAggNumber 的接受/拒绝范围与内存防线。
func TestAggregateParseNumber(t *testing.T) {
	ok := []string{"0", "10", "-10", "+10", "3.14", "-3.14", ".5", "+.5", "5.", "1e3", "1E+5", "-2.5e-3",
		"007", "9007199254740993", "1e10000", "1e-10000", "0.000001", strings.Repeat("9", maxAggDigits)}
	for _, s := range ok {
		if _, good := parseAggNumber(s); !good {
			t.Errorf("应接受 %q", s)
		}
	}
	bad := []string{"", "NaN", "nan", "Inf", "+Inf", "-Inf", "inf", "infinity", "0x10", "1/2", "1_000", "1e",
		"1e+", "e5", "abc", "10 ", " 10", "1.2.3", "--1", "1e10001", "1e-10001", strings.Repeat("9", maxAggDigits+1),
		strings.Repeat("9", maxAggValueLen+1), "1,000", "1e99999999999999999999"}
	for _, s := range bad {
		if _, good := parseAggNumber(s); good {
			t.Errorf("应拒绝 %q", s)
		}
	}
	// 1e10000 被接受且可精确比较（内存上限内的有意设计）。
	a, _ := parseAggNumber("1e10000")
	b, _ := parseAggNumber("9e9999")
	if a.Cmp(b) <= 0 {
		t.Errorf("1e10000 应大于 9e9999")
	}
	// 拒绝在请求构造期发生：filters 的数值非法直接报错。
	if _, err := newAggOpts(AggregateRequest{Field: 1, Filters: []AggregateFilter{{Field: 1, Op: "gt", Value: "NaN"}}}); err == nil {
		t.Error("gt NaN 应在构造期报错")
	}
}

// 参数校验错误。
func TestAggregateValidation(t *testing.T) {
	root := t.TempDir()
	if _, err := AggregateContent(AggregateRequest{Path: root, Field: 0}, []string{root}); err == nil {
		t.Error("field=0 应报错")
	}
	if _, err := AggregateContent(AggregateRequest{Path: root, Field: -1}, []string{root}); err == nil {
		t.Error("field<0 应报错")
	}
	if _, err := AggregateContent(AggregateRequest{Path: root, Field: 1, Pattern: "["}, []string{root}); err == nil {
		t.Error("坏 pattern 应报错")
	}
	if _, err := AggregateContent(AggregateRequest{Path: root, Field: 1,
		Filters: []AggregateFilter{{Field: 1, Op: "between", Value: "1"}}}, []string{root}); err == nil {
		t.Error("坏 op 应报错")
	}
	if _, err := AggregateContent(AggregateRequest{Path: root, Field: 1,
		Filters: []AggregateFilter{{Field: 0, Op: "eq", Value: "1"}}}, []string{root}); err == nil {
		t.Error("filter field=0 应报错")
	}
	if _, err := AggregateContent(AggregateRequest{Path: root, Field: 1,
		Filters: []AggregateFilter{{Field: 1, Op: "regex", Value: "("}}}, []string{root}); err == nil {
		t.Error("坏 filter regex 应报错")
	}
	if _, err := AggregateContent(AggregateRequest{Path: root, Field: 1, SortOrder: "up"}, []string{root}); err == nil {
		t.Error("坏 sort_order 应报错")
	}
	if _, err := AggregateContent(AggregateRequest{Path: filepath.Join(root, "nope"), Field: 1}, []string{root}); err == nil {
		t.Error("不存在的 path 应报错")
	}
	// 沙箱：roots 之外的兄弟目录不可达。
	outside := filepath.Join(filepath.Dir(root), "other-"+filepath.Base(root))
	if _, err := AggregateContent(AggregateRequest{Path: outside, Field: 1}, []string{root}); err == nil {
		t.Error("沙箱外 path 应报错")
	}
}

// 空结果是明确成功：count=0、min/max 省略、top 为空数组。
func TestAggregateEmptyResult(t *testing.T) {
	root := aggSetupTree(t)
	res, err := AggregateContent(AggregateRequest{Path: root, Field: 2, Delimiter: ",",
		Pattern: "never-appears"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 0 || res.Min != nil || res.Max != nil || len(res.Top) != 0 {
		t.Errorf("空结果应 count=0 且省略 min/max: %+v", res)
	}
	if res.LinesScanned == 0 || res.FilesSearched == 0 {
		t.Errorf("扫描过就应有行/文件计数: %+v", res)
	}
}

// 截断行（cut）整行跳过；行内容摘录有长度上限；记录保留 file/line。
func TestAggregateCutLinesAndPreview(t *testing.T) {
	root := t.TempDir()
	// 一行超长（>1MB）整行跳过；一行 5KB（未超 1MB）正常参与但摘录要截断。
	long := strings.Repeat("y", searchLineBuf+100)
	mid := strings.Repeat("p", 5000)
	writeTree(t, root, map[string]string{
		"c.log": "a 1\nb 2\nz " + long + "\nw 3 " + mid + "\n",
	})
	res, _ := AggregateContent(AggregateRequest{Path: filepath.Join(root, "c.log"), Field: 2}, []string{root})
	if res.Count != 3 || res.LinesSkipped[aggSkipLineCut] != 1 {
		t.Errorf("cut 行应跳过并计数: %+v", res)
	}
	if !strings.Contains(res.Hint, "单行扫描上限") {
		t.Errorf("cut 提示缺失: %q", res.Hint)
	}
	// top[0] 是 3（w 行，5KB）：摘录默认 200 字节上限，超出带省略标记。
	if res.Top[0].Value != "3" || res.Top[0].Line != 4 {
		t.Fatalf("top[0]: %+v", res.Top[0])
	}
	if c := res.Top[0].Content; len(c) > defaultAggPreview+64 || !strings.Contains(c, "more bytes") {
		t.Errorf("preview 超限或无截断标记: %d %q", len(c), c[:min(60, len(c))])
	}
	res, _ = AggregateContent(AggregateRequest{Path: filepath.Join(root, "c.log"), Field: 2, MaxLinePreview: 50}, []string{root})
	if c := res.Top[0].Content; len(c) > 50+64 {
		t.Errorf("preview 50 超限: %d", len(c))
	}
	res, _ = AggregateContent(AggregateRequest{Path: filepath.Join(root, "c.log"), Field: 2, MaxLinePreview: -1}, []string{root})
	if res.Top[0].Content != "" {
		t.Errorf("负数应关闭 preview: %q", res.Top[0].Content)
	}
	if res.Top[0].File == "" || res.Top[0].Line == 0 {
		t.Errorf("记录缺定位: %+v", res.Top[0])
	}
}

// Deadline 到期：不报错，返回 Truncated=true 与提示。
func TestAggregateDeadline(t *testing.T) {
	root := aggSetupTree(t)
	res, err := AggregateContent(AggregateRequest{Path: root, Field: 2, Delimiter: ",",
		Deadline: time.Now().Add(-time.Second)}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || !strings.Contains(res.Hint, "提前结束") {
		t.Errorf("过期 deadline 应 Truncated: %+v", res)
	}
}

// Context 取消同样触发截断。
func TestAggregateContextCancel(t *testing.T) {
	root := aggSetupTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := AggregateContent(AggregateRequest{Path: root, Field: 2, Delimiter: ",", Context: ctx}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Errorf("已取消 ctx 应 Truncated: %+v", res)
	}
}

// 文件级：glob、include_hidden、max_file_size、二进制跳过统计透传。
func TestAggregateFileLevel(t *testing.T) {
	root := aggSetupTree(t)
	res, _ := AggregateContent(AggregateRequest{Path: root, Field: 2, Delimiter: ",", IncludeHidden: true}, []string{root})
	if res.FilesSearched != 3 || res.SkipReasons["binary"] != 1 { // a.csv + sub/b.csv + .git/c.csv
		t.Errorf("include_hidden: searched=%d skips=%+v", res.FilesSearched, res.SkipReasons)
	}
	if res.Max.Value != "9999" {
		t.Errorf("隐藏目录应参与: max=%q", res.Max.Value)
	}
	res, _ = AggregateContent(AggregateRequest{Path: root, Field: 2, Delimiter: ",", IncludeGlob: []string{"*.csv"}}, []string{root})
	if res.FilesSearched != 2 {
		t.Errorf("include_glob: searched=%d, want 2", res.FilesSearched)
	}
	writeTree(t, root, map[string]string{"big.log": strings.Repeat("1", 2000)})
	res, _ = AggregateContent(AggregateRequest{Path: root, Field: 2, Delimiter: ",", MaxFileSize: 1000}, []string{root})
	if res.SkipReasons["too_large"] != 1 || !strings.Contains(res.Hint, "max_file_size") {
		t.Errorf("too_large: %+v", res.SkipReasons)
	}
}

// GBK 编码文件逐行解码后聚合（复用扫描层）。
func TestAggregateEncoding(t *testing.T) {
	root := t.TempDir()
	gbk, err := encodeFromUTF8("甲 1\n乙 2\n", "gbk")
	if err != nil {
		t.Skip("gbk 编码不可用:", err)
	}
	if err := os.WriteFile(filepath.Join(root, "gbk.log"), gbk, 0644); err != nil {
		t.Fatal(err)
	}
	res, err := AggregateContent(AggregateRequest{Path: filepath.Join(root, "gbk.log"), Field: 2,
		Pattern: "乙"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 1 || res.Max.Value != "2" {
		t.Errorf("gbk: %+v", res)
	}
	if len(res.Top[0].Content) == 0 || !strings.HasPrefix(res.Top[0].Content, "乙") {
		t.Errorf("gbk 行内容: %q", res.Top[0].Content)
	}
}

// 大数值原值保留：科学计数法原样返回，不归一化。
func TestAggregateValuePreserved(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"e.log": "a 1.5e3\nb 1500\nc +7\n"})
	res, _ := AggregateContent(AggregateRequest{Path: filepath.Join(root, "e.log"), Field: 2}, []string{root})
	if res.Count != 3 {
		t.Fatalf("count=%d", res.Count)
	}
	if res.Max.Value != "1.5e3" && res.Max.Value != "1500" {
		t.Errorf("1.5e3 与 1500 数值相等，max 保留先见者: %q", res.Max.Value)
	}
	if res.Min.Value != "+7" {
		t.Errorf("原值应保留符号: %q", res.Min.Value)
	}
	// 相等时 min/max 保留首见：a 1.5e3 在 b 1500 前。
	if res.Max.Line != 1 {
		t.Errorf("相等保留首见: line=%d", res.Max.Line)
	}
}
