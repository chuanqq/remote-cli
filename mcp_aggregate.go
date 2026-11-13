package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// 参数使用整数类型解码，避免将 1.5 这样的列号静默截断到第一列。
type aggregateToolArgs struct {
	Path           string            `json:"path"`
	Field          int               `json:"field"`
	Delimiter      string            `json:"delimiter"`
	Pattern        string            `json:"pattern"`
	IgnoreCase     bool              `json:"ignore_case"`
	FixedStrings   bool              `json:"fixed_strings"`
	IncludeGlob    []string          `json:"include_glob"`
	ExcludeGlob    []string          `json:"exclude_glob"`
	MaxFileSize    int64             `json:"max_file_size"`
	IncludeHidden  bool              `json:"include_hidden"`
	Encoding       string            `json:"encoding"`
	TopN           int               `json:"top_n"`
	SortOrder      string            `json:"sort_order"`
	IncludeContent bool              `json:"include_content"`
	Filters        []AggregateFilter `json:"filters"`
}

func registerAggregateTool(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc, scans *ScanManager) {
	if !cfg.toolEnabled("remote_aggregate") {
		return
	}
	options := []mcp.ToolOption{
		readOnlyHints(),
		mcp.WithDescription("在远端流式读取日志，按分隔符取第 N 列、过滤并计算数值 count/min/max/top-N；无需下载完整日志，也不执行 shell。列号从 1 开始。delimiter 省略时按连续空白分列，否则按字面分隔符保留空列（不是 CSV 解析器）。pattern 过滤整行，filters 是字段条件的 AND。count 仅统计条件通过且目标字段是有效数值的行；无有效值时 count=0。大整数与十进制数精确比较；min/max/top 带原值、文件和行号。默认 top_n=10，最大 1000；sort_order=asc 取最小 N 项，默认 desc 取最大 N 项。文件扫描规则和沙箱与 remote_search_content 一致；缺列、坏数值、超长行和跳过文件均报告原因。超出本次等待返回 scan_id，用 remote_scan_result 续取同一只读扫描。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件或递归扫描的目录；接受与 search_content 相同的 FSRoot 沙箱规则。")),
		mcp.WithNumber("field", mcp.Required(), mcp.Description("要聚合的数值列号，必须是从 1 开始的整数，最大 1024。")),
		mcp.WithString("delimiter", mcp.Description("字面分隔符，例如 |、逗号或制表符；省略或空字符串表示按连续空白分列。保留空列，不解析 CSV 引号。")),
		mcp.WithString("pattern", mcp.Description("可选整行 RE2 过滤；交替用裸 |。fixed_strings=true 时按字面匹配。")),
		mcp.WithBoolean("ignore_case", mcp.Description("行 pattern 及 eq/ne/contains/regex 字段过滤忽略大小写；默认 false。")),
		mcp.WithBoolean("fixed_strings", mcp.Description("行 pattern 按字面文本匹配；默认 false。")),
		mcp.WithArray("filters", mcp.Description("全部条件必须通过；value 使用字符串以保留大整数精度。eq/ne/contains/regex 是文本条件，gt/ge/lt/le 是精确数值比较。"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"field": map[string]any{"type": "integer", "minimum": 1, "description": "1-based 字段列号。"},
					"op":    map[string]any{"type": "string", "enum": []string{"eq", "ne", "contains", "regex", "gt", "ge", "lt", "le"}},
					"value": map[string]any{"type": "string", "description": "用于比较的文本或十进制数；regex 时是 RE2 模式。"},
				},
				"required": []string{"field", "op", "value"},
			})),
		mcp.WithNumber("top_n", mcp.Description("保留的 top 记录数，默认 10，最大 1000；不会限制扫描总量或 count/min/max。")),
		mcp.WithString("sort_order", mcp.Description("desc（默认）取数值最大的 N 条，asc 取最小的 N 条。"), mcp.Enum("desc", "asc")),
		mcp.WithBoolean("include_content", mcp.Description("在极值和 top 记录中附带有长度上限的原行片段；默认仅返回原数值、文件和行号。")),
		mcp.WithArray("include_glob", mcp.Description("只扫描匹配的文件；支持 *.log、dir/*.log、**/*.log、*.{log,txt}。"), mcp.WithStringItems()),
		mcp.WithArray("exclude_glob", mcp.Description("跳过匹配的文件和目录，目录不进入。"), mcp.WithStringItems()),
		mcp.WithNumber("max_file_size", mcp.Description("跳过超过该字节数的文件，默认 256MB；文件始终流式读取。")),
		mcp.WithBoolean("include_hidden", mcp.Description("包含点文件和点目录，默认 false。")),
		mcp.WithString("encoding", mcp.Description("强制源编码；省略时逐文件和逐行识别。"), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
		formatOption(),
	}
	options = append(options, scanToolOptions()...)
	s.AddTool(mcp.NewTool("remote_aggregate", options...), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args aggregateToolArgs
		data, err := json.Marshal(req.GetArguments())
		if err == nil {
			err = json.Unmarshal(data, &args)
		}
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid aggregate arguments: %v", err)), nil
		}
		if args.Path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}
		aggregateReq := AggregateRequest{
			Path: args.Path, Field: args.Field, Delimiter: args.Delimiter,
			Pattern: args.Pattern, IgnoreCase: args.IgnoreCase, FixedStrings: args.FixedStrings,
			IncludeGlob: args.IncludeGlob, ExcludeGlob: args.ExcludeGlob,
			MaxFileSize: args.MaxFileSize, IncludeHidden: args.IncludeHidden, Encoding: args.Encoding,
			TopN: args.TopN, SortOrder: args.SortOrder, MaxLinePreview: -1, Filters: args.Filters,
		}
		if args.IncludeContent {
			aggregateReq.MaxLinePreview = defaultAggPreview
		}
		return runReadScan(ctx, req, cfg, scans, auditFileOp, "remote_aggregate", args.Path,
			func(scanCtx context.Context) (any, error) {
				aggregateReq.Context = scanCtx
				return AggregateContent(aggregateReq, roots)
			}), nil
	})
}
