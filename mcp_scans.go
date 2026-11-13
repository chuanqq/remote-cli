package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// scanInstructions 根据实际启用的工具描述扫描能力，禁用的入口不出现在说明中。
func scanInstructions(cfg *Config) string {
	var b strings.Builder
	if cfg.toolEnabled("remote_aggregate") {
		b.WriteString("Numeric log analysis: remote_aggregate splits each line by delimiter and reads a 1-based field; AND filters support text and numeric conditions; returns count/min/max/top-N without downloading the log. ")
	}
	if cfg.toolEnabled("remote_search_content") {
		b.WriteString("remote_search_content output_mode=count returns total_matches (matching lines), files_matched and files[].count; counts_complete reports completeness, independently of the file-details page. ")
	}
	if cfg.toolEnabled("remote_scan_result") && (cfg.toolEnabled("remote_search_content") || cfg.toolEnabled("remote_aggregate")) {
		fmt.Fprintf(&b, "Read-only scans: remote_search_content / remote_aggregate run for timeout_ms (default %d, max %d), independently of the call budget. A running scan returns state=running with scan_id; use remote_scan_result to fetch the same scan instead of re-running it. wait_ms=0 starts without waiting. Without scan_id, remote_scan_result lists retained scans after a lost reply; cancel=true stops only that read task. Up to %d scans run at once; the newest %d finished results are retained for %d minutes and are lost on restart. ",
			defaultScanTimeout.Milliseconds(), maxScanTimeout.Milliseconds(), maxActiveScans, maxRetainedScans, int(scanRetention/time.Minute))
	}
	return b.String()
}

// 扫描时限约束整个任务，等待预算只约束本次请求，二者互不替代。
func scanToolOptions() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithNumber("timeout_ms", mcp.Description(fmt.Sprintf("整个只读扫描的超时毫秒数，默认 %d，最大 %d；独立于单次调用预算。超时返回部分结果及 state=timed_out。", defaultScanTimeout.Milliseconds(), maxScanTimeout.Milliseconds()))),
		mcp.WithNumber("wait_ms", mcp.Description("本次调用等待扫描的毫秒数，默认接近调用预算且预留响应时间；超过预算会被限制。0 表示立即返回 scan_id，用 remote_scan_result 续取。")),
	}
}

func readScanTiming(req mcp.CallToolRequest, cfg *Config) (time.Duration, time.Duration, error) {
	timeoutMs := req.GetInt("timeout_ms", int(defaultScanTimeout.Milliseconds()))
	if timeoutMs <= 0 || int64(timeoutMs) > maxScanTimeout.Milliseconds() {
		return 0, 0, fmt.Errorf("timeout_ms must be between 1 and %d", maxScanTimeout.Milliseconds())
	}
	wait, err := readScanWait(req, cfg)
	return time.Duration(timeoutMs) * time.Millisecond, wait, err
}

func readScanWait(req mcp.CallToolRequest, cfg *Config) (time.Duration, error) {
	budgetMs := scanBudget(cfg.callBudget()).Milliseconds()
	waitMs := req.GetInt("wait_ms", int(budgetMs))
	if waitMs < 0 {
		return 0, fmt.Errorf("wait_ms must be non-negative")
	}
	return time.Duration(min(int64(waitMs), budgetMs)) * time.Millisecond, nil
}

// runReadScan 的 runner 只能读文件；请求取消不取消后台任务。
func runReadScan(ctx context.Context, req mcp.CallToolRequest, cfg *Config, scans *ScanManager,
	auditFileOp fileAuditFunc, tool, path string, run func(context.Context) (any, error)) *mcp.CallToolResult {
	timeout, wait, err := readScanTiming(req, cfg)
	if err != nil {
		return mcp.NewToolResultError(err.Error())
	}
	if !cfg.toolEnabled("remote_scan_result") {
		// 结果工具被禁用时保留同步行为，不能留下无法取回的后台扫描。
		scanCtx, cancel := context.WithTimeout(ctx, min(timeout, scanBudget(cfg.callBudget())))
		defer cancel()
		result, err := run(scanCtx)
		if err != nil {
			return mcp.NewToolResultError(err.Error())
		}
		auditScanResult(ctx, auditFileOp, tool, path, ScanSnapshot{Result: result})
		return renderScanResult(ScanSnapshot{Result: result}, textFormat(req))
	}
	scan, err := scans.Start(tool, path, timeout, run)
	if err != nil {
		return mcp.NewToolResultError(err.Error())
	}
	scan.Await(ctx, wait)
	snap := scan.Snapshot()
	auditScanResult(ctx, auditFileOp, tool, path, snap)
	return renderScanResult(snap, textFormat(req))
}

func auditScanResult(ctx context.Context, auditFileOp fileAuditFunc, tool, path string, snap ScanSnapshot) {
	bytes, truncated := 0, false
	switch r := snap.Result.(type) {
	case *SearchContentResult:
		if r != nil {
			bytes, truncated = r.TotalMatches, r.Truncated
		}
	case *AggregateResult:
		if r != nil {
			truncated = r.Truncated
		}
	}
	auditFileOp(ctx, strings.TrimPrefix(tool, "remote_"), path, snap.Error == "", bytes, truncated)
	noteAudit(ctx, func(n *auditNote) {
		if snap.ScanID != "" {
			n.Command += " scan_id=" + snap.ScanID
		}
		n.State, n.Error, n.TimedOut = snap.State, snap.Error, snap.State == ScanTimedOut
	})
}

// 扫描完成后仍保持原工具的顶层结果字段，新增的 scan_* 元数据便于续取与审计。
func renderScanResult(snap ScanSnapshot, text bool) *mcp.CallToolResult {
	if text {
		var b strings.Builder
		if snap.ScanID != "" {
			b.WriteString(header("scan_id="+snap.ScanID, "state="+snap.State,
				fmt.Sprintf("elapsed_ms=%d", snap.ElapsedMs), fmt.Sprintf("timeout_ms=%d", snap.TimeoutMs)) + "\n")
		}
		if snap.Error != "" {
			b.WriteString("error: " + snap.Error + "\n")
		}
		switch r := snap.Result.(type) {
		case *SearchContentResult:
			if r != nil {
				b.WriteString(searchText(r))
			}
		case *AggregateResult:
			if r != nil {
				b.WriteString(aggregateText(r))
			}
		}
		if snap.State == ScanRunning {
			b.WriteString(scanPendingHint(snap.ScanID) + "\n")
		}
		out := textResult(b.String())
		out.IsError = snap.State == ScanFailed
		return out
	}
	if snap.ScanID == "" && snap.Result != nil {
		return jsonResult(snap.Result)
	}
	var out *mcp.CallToolResult
	switch r := snap.Result.(type) {
	case *SearchContentResult:
		out = jsonResult(struct {
			ScanInfo
			*SearchContentResult
			Error string `json:"error,omitempty"`
		}{snap.ScanInfo, r, snap.Error})
	case *AggregateResult:
		out = jsonResult(struct {
			ScanInfo
			*AggregateResult
			Error string `json:"error,omitempty"`
		}{snap.ScanInfo, r, snap.Error})
	default:
		hint := ""
		if snap.State == ScanRunning {
			hint = scanPendingHint(snap.ScanID)
		}
		out = jsonResult(struct {
			ScanInfo
			Error string `json:"error,omitempty"`
			Hint  string `json:"hint,omitempty"`
		}{snap.ScanInfo, snap.Error, hint})
	}
	out.IsError = snap.State == ScanFailed
	return out
}

func scanPendingHint(id string) string {
	return fmt.Sprintf("只读扫描仍在运行，未因本次等待结束而停止。请用 remote_scan_result(scan_id=%q) 续取；不要重新发起扫描。", id)
}

func registerScanResultTool(s *server.MCPServer, cfg *Config, scans *ScanManager) {
	if !cfg.toolEnabled("remote_scan_result") {
		return
	}
	s.AddTool(mcp.NewTool("remote_scan_result",
		readOnlyHints(),
		mcp.WithDescription(fmt.Sprintf("续取 remote_search_content / remote_aggregate 的只读扫描结果，支持只读模式。scan_id 指向同一任务，续取不会重新扫描；省略 scan_id 列出近期任务，可找回断连前启动的扫描。最多同时运行 %d 个任务，最多保留最近 %d 个已完成结果，完成后 %d 分钟过期；服务重启清空。cancel=true 仅取消该读取任务，不修改目标文件。", maxActiveScans, maxRetainedScans, int(scanRetention/time.Minute))),
		mcp.WithString("scan_id", mcp.Description("原扫描返回的 scan_id；省略时列出近期扫描及状态。")),
		mcp.WithNumber("wait_ms", mcp.Description("本次最多等待的毫秒数，0 表示只查询状态；默认接近调用预算，超过预算会被限制。")),
		mcp.WithBoolean("cancel", mcp.Description("取消此 scan_id 对应的只读扫描，并取回已完成的部分结果。需要 scan_id；重复取消无额外影响。")),
		formatOption(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		wait, err := readScanWait(req, cfg)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		id := req.GetString("scan_id", "")
		if id == "" {
			if req.GetBool("cancel", false) {
				return mcp.NewToolResultError("cancel requires scan_id"), nil
			}
			type listedScan struct {
				ScanInfo
				Path  string `json:"path"`
				Error string `json:"error,omitempty"`
			}
			list := scans.List()
			infos := make([]listedScan, 0, len(list))
			for _, scan := range list {
				snap := scan.Snapshot()
				infos = append(infos, listedScan{snap.ScanInfo, scan.Path, snap.Error})
			}
			noteAudit(ctx, func(n *auditNote) { n.Command = "scan_list"; n.OutputBytes = len(infos) })
			if textFormat(req) {
				var b strings.Builder
				b.WriteString(header(fmt.Sprintf("%d retained scans", len(infos))) + "\n")
				for _, info := range infos {
					fmt.Fprintf(&b, "%s %s %s elapsed_ms=%d %s\n", info.ScanID, info.State, info.Tool, info.ElapsedMs, info.Path)
					if info.Error != "" {
						b.WriteString("error: " + info.Error + "\n")
					}
				}
				return textResult(b.String()), nil
			}
			return jsonResult(struct {
				Scans []listedScan `json:"scans"`
				Count int          `json:"count"`
			}{infos, len(infos)}), nil
		}
		scan := scans.Lookup(id)
		if scan == nil {
			return mcp.NewToolResultError(fmt.Sprintf("scan not found or expired: %s; omit scan_id to list retained scans", id)), nil
		}
		if req.GetBool("cancel", false) {
			scan.Cancel()
		}
		scan.Await(ctx, wait)
		snap := scan.Snapshot()
		noteAudit(ctx, func(n *auditNote) {
			n.Command = "scan_result " + id
			n.State, n.Error, n.TimedOut = snap.State, snap.Error, snap.State == ScanTimedOut
		})
		return renderScanResult(snap, textFormat(req)), nil
	})
}
