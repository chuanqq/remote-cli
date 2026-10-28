package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerFileTools adds file operation tools to the MCP server. All tools
// honor cfg.FSRoots as an optional sandbox, respect the cfg tool blacklist,
// and are audited by the withAudit tool middleware.
func registerFileTools(s *server.MCPServer, cfg *Config) {
	roots := cfg.FSRoots

	// auditFileOp describes a file operation for the audit record (Command
	// carries a synthetic "op path" descriptor). Tool name, duration and the
	// failure reason are filled in by withAudit.
	auditFileOp := func(ctx context.Context, op, path string, ok bool, bytes int, truncated bool) {
		noteAudit(ctx, func(n *auditNote) {
			n.Command = op + " " + path
			if len(roots) > 0 {
				n.WorkingDirectory = strings.Join(roots, ",")
			}
			if !ok {
				n.ExitCode = 1
			}
			n.OutputBytes = bytes
			n.Truncated = truncated
		})
	}

	registerWriteTool(s, cfg, roots, auditFileOp)
	registerReadTool(s, cfg, roots, auditFileOp)
	registerReadFilesTool(s, cfg, roots, auditFileOp)
	registerEditTool(s, cfg, roots, auditFileOp)
	registerListStatTools(s, cfg, roots, auditFileOp)
	registerBase64Tools(s, cfg, roots, auditFileOp)
	registerSearchTools(s, cfg, roots, auditFileOp)
	registerTailLogTool(s, cfg, roots, auditFileOp)
	registerDiffTool(s, cfg, roots, auditFileOp)
	registerManageTools(s, cfg, roots, auditFileOp)
}

type fileAuditFunc func(ctx context.Context, op, path string, ok bool, bytes int, truncated bool)

func registerWriteTool(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if !cfg.toolEnabled("remote_write_file") {
		return
	}
	s.AddTool(mcp.NewTool("remote_write_file",
		mcp.WithDescription("Write UTF-8 text content to a file on the remote server. Supports target encoding conversion (utf-8/gbk/gb2312/gb18030), append mode, and auto-creating parent directories. For binary data use remote_upload_base64. Do NOT fall back to shell `echo ... > file` or heredocs: this tool returns sha256/mode for verification and can lint the result (bash -n / py_compile). Use dry_run=true to preview an overwrite of a config file: nothing is written and preview.diff shows the unified diff against the current content."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Absolute or relative file path on the remote server.")),
		mcp.WithString("content", mcp.Required(), mcp.Description("UTF-8 text content to write.")),
		mcp.WithString("encoding", mcp.Description("On-disk encoding: utf-8 (default), gbk, gb2312, gb18030."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
		mcp.WithBoolean("append", mcp.Description("Append to the file instead of overwriting. Default false.")),
		mcp.WithBoolean("make_dirs", mcp.Description("Create parent directories if missing. Default false.")),
		mcp.WithString("mode", mcp.Description("Octal permission for newly created files, e.g. \"0644\". Default 0644.")),
		mcp.WithString("lint", mcp.Description("Post-write syntax check: \"bash\" (bash -n) or \"python\" (py_compile). Result is advisory and returned in the response."), mcp.Enum("bash", "python", "none")),
		mcp.WithBoolean("dry_run", mcp.Description("Write nothing; report what the write would do (bytes, sha256, created, mode) plus preview.diff against the current content. Default false.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}
		content := req.GetString("content", "")
		dryRun := req.GetBool("dry_run", false)
		op := "write_file"
		if dryRun {
			op = "write_file(dry_run)"
		}

		result, err := WriteFileContent(WriteFileRequest{
			Path:     path,
			Content:  content,
			Encoding: req.GetString("encoding", ""),
			Append:   req.GetBool("append", false),
			MakeDirs: req.GetBool("make_dirs", false),
			Mode:     req.GetString("mode", ""),
			Lint:     req.GetString("lint", ""),
			DryRun:   dryRun,
		}, roots)
		if err != nil {
			auditFileOp(ctx, op, path, false, 0, false)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(ctx, op, result.Path, true, result.BytesWritten, false)
		return jsonResult(result), nil
	})
}

func registerReadTool(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if !cfg.toolEnabled("remote_read_file") {
		return
	}
	s.AddTool(mcp.NewTool("remote_read_file",
		readOnlyHints(),
		mcp.WithDescription("Read a file from the remote server and return its content as UTF-8. Auto-detects source encoding (utf-8 / gbk) unless specified, detects binary files (use remote_download_base64 for those). "+
			"Reads one window per call, 64KB by default: start_line/end_line stream to any line of any file; tail_lines reads the last N lines; offset_bytes/max_bytes read a byte range. "+
			"A window never ends inside a character and, when the file continues, the result has truncated=true plus next_offset / next_start_line to read on. "+
			"line_numbers=true prefixes each line with its number like `cat -n`. "+
			"Prefer this over shell cat/head/sed: cat <file> = call with just path; sed -n 'X,Yp' = start_line/end_line; nl -ba = line_numbers; tail -N = tail_lines=N; head -c N = max_bytes=N. For growing logs with follow/filter use remote_tail_log."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Absolute or relative file path on the remote server.")),
		mcp.WithString("encoding", mcp.Description("Source encoding; auto-detected if omitted."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
		mcp.WithNumber("start_line", mcp.Description("1-based start line (inclusive). Omit or 0 for start of file.")),
		mcp.WithNumber("end_line", mcp.Description("1-based end line (inclusive). Omit or 0 for end of file.")),
		mcp.WithNumber("tail_lines", mcp.Description("Read the last N lines (seek-based, works on huge files). Wins over start_line/end_line.")),
		mcp.WithNumber("offset_bytes", mcp.Description("Start reading at this byte offset (e.g. next_offset of the previous call). Default 0.")),
		mcp.WithNumber("max_bytes", mcp.Description("Maximum bytes to read. Default 65536 (64KB), max 16MB; larger results risk being cut by the client.")),
		mcp.WithString("truncate_mode", mcp.Description("When the file exceeds max_bytes: \"head\" keeps the first bytes (default), \"tail\" keeps the last bytes."), mcp.Enum("head", "tail")),
		mcp.WithBoolean("line_numbers", mcp.Description("Prefix every line with its 1-based line number and a tab, like `cat -n`. Default false.")),
		mcp.WithArray("lines", mcp.Description("Read only these line numbers (any order, up to 1000), returned ascending and numbered like `cat -n`; e.g. [604, 620, 1658] replaces `for L in ...; do sed -n \"${L}p\" f; done`. Wins over every other mode."), mcp.WithNumberItems()),
		formatOption(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}

		result, err := ReadFileContent(ReadFileRequest{
			Path:         path,
			Encoding:     req.GetString("encoding", ""),
			StartLine:    req.GetInt("start_line", 0),
			EndLine:      req.GetInt("end_line", 0),
			TailLines:    req.GetInt("tail_lines", 0),
			OffsetBytes:  int64(req.GetInt("offset_bytes", 0)),
			MaxBytes:     req.GetInt("max_bytes", 0),
			TruncateMode: req.GetString("truncate_mode", ""),
			LineNumbers:  req.GetBool("line_numbers", false),
			Lines:        req.GetIntSlice("lines", nil),
		}, roots)
		if err != nil {
			auditFileOp(ctx, "read_file", path, false, 0, false)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(ctx, "read_file", result.Path, true, len(result.Content), result.Truncated)
		if textFormat(req) {
			return textResult(readFileText(result)), nil
		}
		return jsonResult(result), nil
	})
}

// readFilesSpec is one entry of remote_read_files' files[] argument.
type readFilesSpec struct {
	Path        string `json:"path"`
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	TailLines   int    `json:"tail_lines"`
	OffsetBytes int64  `json:"offset_bytes"`
	MaxBytes    int    `json:"max_bytes"`
	Lines       []int  `json:"lines"`
	Encoding    string `json:"encoding"`
}

func registerReadFilesTool(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if !cfg.toolEnabled("remote_read_files") {
		return
	}
	s.AddTool(mcp.NewTool("remote_read_files",
		readOnlyHints(),
		mcp.WithDescription(fmt.Sprintf("Read several files in one call — instead of `for f in ...; do echo \"=== $f\"; cat $f; done` or one remote_read_file per file. "+
			"Pass paths (plain paths or globs such as \"conf/*.conf\") and/or files[] with a window per file (start_line/end_line, tail_lines, lines, offset_bytes, max_bytes). "+
			"Up to %d files share one max_bytes budget: small files come back whole, large ones are cut (truncated=true with next_offset / next_start_line, as in remote_read_file). "+
			"Each file's result carries its own error, so one missing file does not fail the call; files left unread are listed in not_read.", maxReadFiles)),
		mcp.WithArray("paths", mcp.Description("Files to read whole (up to the budget), in order; a path that does not exist but holds * ? [ is expanded as a glob (base-name globs per directory, sorted)."), mcp.WithStringItems()),
		mcp.WithArray("files", mcp.Description("Files with a window each: {path, start_line?, end_line?, tail_lines?, lines?, offset_bytes?, max_bytes?, encoding?} — same meaning as in remote_read_file."),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":         map[string]any{"type": "string", "description": "File path (or glob)."},
					"start_line":   map[string]any{"type": "number", "description": "1-based first line."},
					"end_line":     map[string]any{"type": "number", "description": "1-based last line (inclusive)."},
					"tail_lines":   map[string]any{"type": "number", "description": "Last N lines."},
					"lines":        map[string]any{"type": "array", "items": map[string]any{"type": "number"}, "description": "Only these line numbers."},
					"offset_bytes": map[string]any{"type": "number", "description": "Start at this byte offset."},
					"max_bytes":    map[string]any{"type": "number", "description": "Cap for this file (within its share of the budget)."},
					"encoding":     map[string]any{"type": "string", "enum": []string{"utf-8", "gbk", "gb2312", "gb18030"}},
				},
				"required": []string{"path"},
			})),
		mcp.WithBoolean("line_numbers", mcp.Description("Number the lines of every file like `cat -n`. Default false.")),
		mcp.WithNumber("max_bytes", mcp.Description("Content budget shared by all files. Default 65536 (64KB), max 16MB.")),
		formatOption(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var files []ReadFileRequest
		for _, p := range req.GetStringSlice("paths", nil) {
			files = append(files, ReadFileRequest{Path: p})
		}
		if raw, ok := req.GetArguments()["files"]; ok && raw != nil {
			var specs []readFilesSpec
			data, err := json.Marshal(raw)
			if err == nil {
				err = json.Unmarshal(data, &specs)
			}
			if err != nil {
				return mcp.NewToolResultError("invalid files (want [{path, start_line?, end_line?, ...}]): " + err.Error()), nil
			}
			for _, sp := range specs {
				files = append(files, ReadFileRequest{Path: sp.Path, StartLine: sp.StartLine, EndLine: sp.EndLine,
					TailLines: sp.TailLines, OffsetBytes: sp.OffsetBytes, MaxBytes: sp.MaxBytes, Lines: sp.Lines,
					Encoding: sp.Encoding})
			}
		}
		if len(files) == 0 {
			return mcp.NewToolResultError("paths or files is required"), nil
		}
		result, err := ReadFiles(ReadFilesRequest{
			Files:       files,
			LineNumbers: req.GetBool("line_numbers", false),
			MaxBytes:    req.GetInt("max_bytes", 0),
		}, roots)
		descr := fmt.Sprintf("%d requested", len(files))
		if err != nil {
			auditFileOp(ctx, "read_files", descr, false, 0, false)
			return mcp.NewToolResultError(err.Error()), nil
		}
		total := 0
		for _, f := range result.Files {
			if f.ReadFileResult != nil {
				total += len(f.Content)
			}
		}
		auditFileOp(ctx, "read_files", descr+": "+readFilesAuditPaths(result), true, total, result.Truncated)
		if textFormat(req) {
			return textResult(readFilesText(result)), nil
		}
		return jsonResult(result), nil
	})
}

// readFilesAuditPaths lists the files a remote_read_files call returned, for
// the audit record (capped, so a 50-file call stays one readable line).
func readFilesAuditPaths(r *ReadFilesResult) string {
	var b strings.Builder
	for i, f := range r.Files {
		if b.Len() > 600 {
			fmt.Fprintf(&b, " ... (+%d)", len(r.Files)-i)
			break
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(f.Path)
	}
	return b.String()
}

func registerEditTool(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if !cfg.toolEnabled("remote_edit_file") {
		return
	}
	s.AddTool(mcp.NewTool("remote_edit_file",
		mcp.WithDescription("Apply exact-string replacements in a remote file, preserving its encoding and permissions. Single edit: pass old_string/new_string (must be unique unless replace_all). Multi-edit: pass edits[] — applied in order, atomically (any failure writes nothing). Set use_regex for RE2 patterns with $1 group expansion. Use dry_run to preview changes without writing. Returns sha256/mode and an optional lint result. Prefer this over sed -i or python heredocs."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Absolute or relative file path on the remote server.")),
		mcp.WithString("old_string", mcp.Description("Exact text to find (single-edit mode; must be unique unless replace_all is true).")),
		mcp.WithString("new_string", mcp.Description("Replacement text (single-edit mode).")),
		mcp.WithArray("edits", mcp.Description("Multi-edit mode: list of {old_string, new_string, replace_all?} applied in order; atomic — any failure writes nothing."),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"old_string":  map[string]any{"type": "string", "description": "Text or pattern to find."},
					"new_string":  map[string]any{"type": "string", "description": "Replacement text."},
					"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences (default false: require uniqueness)."},
				},
				"required": []string{"old_string", "new_string"},
			})),
		mcp.WithBoolean("replace_all", mcp.Description("Single-edit mode: replace all occurrences instead of requiring uniqueness. Default false.")),
		mcp.WithBoolean("use_regex", mcp.Description("Treat old_string as an RE2 pattern; new_string supports $1 group expansion. Implies replace-all semantics.")),
		mcp.WithBoolean("dry_run", mcp.Description("Report the changes that would be applied without writing. Default false.")),
		mcp.WithString("encoding", mcp.Description("Source encoding; auto-detected if omitted. Written back with the same encoding."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
		mcp.WithString("lint", mcp.Description("Post-write syntax check: \"bash\" (bash -n) or \"python\" (py_compile)."), mcp.Enum("bash", "python", "none")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}

		editReq := EditFileRequest{
			Path:       path,
			OldString:  req.GetString("old_string", ""),
			NewString:  req.GetString("new_string", ""),
			ReplaceAll: req.GetBool("replace_all", false),
			UseRegex:   req.GetBool("use_regex", false),
			DryRun:     req.GetBool("dry_run", false),
			Encoding:   req.GetString("encoding", ""),
			Lint:       req.GetString("lint", ""),
		}

		if raw, ok := req.GetArguments()["edits"]; ok && raw != nil {
			data, err := json.Marshal(raw)
			if err != nil {
				return mcp.NewToolResultError("invalid edits: " + err.Error()), nil
			}
			if err := json.Unmarshal(data, &editReq.Edits); err != nil {
				return mcp.NewToolResultError("invalid edits (want [{old_string,new_string,replace_all?}]): " + err.Error()), nil
			}
		}

		if len(editReq.Edits) == 0 && editReq.OldString == "" {
			return mcp.NewToolResultError("old_string (or edits) is required"), nil
		}

		result, err := EditFileContent(editReq, roots)
		if err != nil {
			auditFileOp(ctx, "edit_file", path, false, 0, false)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(ctx, "edit_file", result.Path, true, 0, false)
		return jsonResult(result), nil
	})
}

func registerListStatTools(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if cfg.toolEnabled("remote_list_dir") {
		s.AddTool(mcp.NewTool("remote_list_dir",
			readOnlyHints(),
			mcp.WithDescription("List directory entries on the remote server with name, type, size, mode, owner/group, mtime and symlink targets — the structured equivalent of `ls -la`. Supports glob filtering, hidden files, and sorting."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Directory path on the remote server.")),
			mcp.WithString("sort_by", mcp.Description("Sort order: \"name\" (default, ascending), \"mtime\" (newest first), \"size\" (files largest first, directories last — directory sizes are filesystem artifacts and are not compared)."), mcp.Enum("name", "mtime", "size")),
			mcp.WithArray("filter_glob", mcp.Description("Keep only entries whose name matches any of these globs, e.g. [\"*.conf\"]."), mcp.WithStringItems()),
			mcp.WithBoolean("include_hidden", mcp.Description("Include dot-files. Default false.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			path := req.GetString("path", "")
			if path == "" {
				return mcp.NewToolResultError("path is required"), nil
			}

			result, err := ListDirectory(ListDirRequest{
				Path:          path,
				SortBy:        req.GetString("sort_by", ""),
				FilterGlob:    req.GetStringSlice("filter_glob", nil),
				IncludeHidden: req.GetBool("include_hidden", false),
			}, roots)
			if err != nil {
				auditFileOp(ctx, "list_dir", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(ctx, "list_dir", result.Path, true, result.Count, false)
			return jsonResult(result), nil
		})
	}

	if cfg.toolEnabled("remote_stat") {
		s.AddTool(mcp.NewTool("remote_stat",
			readOnlyHints(),
			mcp.WithDescription("Return metadata about a path on the remote server: existence, type, size, mode, owner/group, nlink, mtime, symlink target, and (for regular files) binary sniff. "+
				"Optionally a content hash (md5/sha256), the detected text encoding, and text facts (include_text: line count, line endings lf/crlf/mixed, final newline, BOM, longest line) — "+
				"one call replaces the `stat -c ...; file -I; md5sum; wc -l` combo. Pass paths[] to stat up to 200 paths at once (each result carries its own error). A missing path returns exists=false without error."),
			mcp.WithString("path", mcp.Description("File or directory path on the remote server. Required unless paths is given.")),
			mcp.WithArray("paths", mcp.Description("Stat several paths in one call (up to 200); returns {results: [...]} in order."), mcp.WithStringItems()),
			mcp.WithString("include_hash", mcp.Description("Compute a streaming content hash (regular files up to 256MB)."), mcp.Enum("md5", "sha256")),
			mcp.WithBoolean("include_encoding", mcp.Description("Detect text encoding (utf-8/gbk/unknown) from the file head.")),
			mcp.WithBoolean("include_text", mcp.Description("Scan regular text files (up to 256MB) for line count (like wc -l), line endings, final newline, BOM and longest line.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sreq := StatRequest{
				IncludeHash:     req.GetString("include_hash", ""),
				IncludeEncoding: req.GetBool("include_encoding", false),
				IncludeText:     req.GetBool("include_text", false),
			}
			if paths := req.GetStringSlice("paths", nil); len(paths) > 0 {
				if p := req.GetString("path", ""); p != "" {
					paths = append([]string{p}, paths...)
				}
				results, err := StatFiles(paths, sreq, roots)
				descr := fmt.Sprintf("%d paths", len(paths))
				if err != nil {
					auditFileOp(ctx, "stat", descr, false, 0, false)
					return mcp.NewToolResultError(err.Error()), nil
				}
				auditFileOp(ctx, "stat", descr, true, len(results), false)
				return jsonResult(struct {
					Count   int           `json:"count"`
					Results []*StatResult `json:"results"`
				}{Count: len(results), Results: results}), nil
			}
			path := req.GetString("path", "")
			if path == "" {
				return mcp.NewToolResultError("path (or paths) is required"), nil
			}
			sreq.Path = path
			result, err := StatFile(sreq, roots)
			if err != nil {
				auditFileOp(ctx, "stat", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(ctx, "stat", result.Path, true, 0, false)
			return jsonResult(result), nil
		})
	}
}

func registerBase64Tools(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if cfg.toolEnabled("remote_upload_base64") {
		s.AddTool(mcp.NewTool("remote_upload_base64",
			mcp.WithDescription("Upload binary content to a remote file by decoding a base64 payload. Supports append mode for chunked uploads of large files and auto-creating parent directories. Returns sha256/mode of the written bytes for verification."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Destination file path on the remote server.")),
			mcp.WithString("data_b64", mcp.Required(), mcp.Description("Base64-encoded (standard encoding) file content or chunk.")),
			mcp.WithBoolean("append", mcp.Description("Append this chunk instead of overwriting. Use for chunked uploads. Default false.")),
			mcp.WithBoolean("make_dirs", mcp.Description("Create parent directories if missing. Default false.")),
			mcp.WithString("mode", mcp.Description("Octal permission for newly created files, e.g. \"0755\". Default 0644.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			path := req.GetString("path", "")
			if path == "" {
				return mcp.NewToolResultError("path is required"), nil
			}
			dataB64 := req.GetString("data_b64", "")
			if dataB64 == "" {
				return mcp.NewToolResultError("data_b64 is required"), nil
			}

			result, err := UploadBase64(UploadBase64Request{
				Path:     path,
				DataB64:  dataB64,
				Append:   req.GetBool("append", false),
				MakeDirs: req.GetBool("make_dirs", false),
				Mode:     req.GetString("mode", ""),
			}, roots)
			if err != nil {
				auditFileOp(ctx, "upload_base64", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(ctx, "upload_base64", result.Path, true, result.BytesWritten, false)
			return jsonResult(result), nil
		})
	}

	if cfg.toolEnabled("remote_download_base64") {
		s.AddTool(mcp.NewTool("remote_download_base64",
			readOnlyHints(),
			mcp.WithDescription("Download a byte range from a remote file as a base64 payload. Supports offset + max_bytes for chunked downloads of large or binary files; the eof flag signals the last chunk."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Source file path on the remote server.")),
			mcp.WithNumber("offset", mcp.Description("Starting byte offset. Default 0.")),
			mcp.WithNumber("max_bytes", mcp.Description("Maximum bytes to read this call. Default 4MB.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			path := req.GetString("path", "")
			if path == "" {
				return mcp.NewToolResultError("path is required"), nil
			}

			result, err := DownloadBase64(DownloadBase64Request{
				Path:     path,
				Offset:   int64(req.GetInt("offset", 0)),
				MaxBytes: req.GetInt("max_bytes", 0),
			}, roots)
			if err != nil {
				auditFileOp(ctx, "download_base64", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(ctx, "download_base64", result.Path, true, result.BytesRead, false)
			return jsonResult(result), nil
		})
	}
}

// scanBudget is how long a tool that scans trees (remote_search_content,
// remote_diff) may run: the call budget minus a margin for encoding and
// sending the result, so a huge tree returns partial results (truncated, with
// a hint) instead of a client-side timeout.
func scanBudget(callBudget time.Duration) time.Duration {
	return max(callBudget-5*time.Second, callBudget*3/4)
}

// registerSearchTools adds the server-side content/file search tools.
func registerSearchTools(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if cfg.toolEnabled("remote_search_content") {
		s.AddTool(mcp.NewTool("remote_search_content",
			readOnlyHints(),
			mcp.WithDescription("Search file contents on the remote server with an RE2 regex — the structured equivalent of `grep -rn pattern path`, implemented server-side (no grep/rg needed on the target). "+
				"RE2 alternation is a bare | (GNU grep's \\| matches a literal |); fixed_strings=true searches literal text. "+
				"output_mode: content (default: matching lines, with context_lines or before_lines/after_lines like grep -B/-A; overlapping context is emitted once), "+
				"files_with_matches (grep -l) or count (grep -c). Globs: \"*.conf\" matches base names, \"conf/*.conf\" and \"**/*.conf\" match paths below path, \"*.{h,cpp}\" alternates. "+
				"Files are streamed; GBK and UTF-8 are decoded per line. No matches is a success (files_matched=0), NOT an error; skipped files are counted per reason. "+
				"When truncated, call again with offset=next_offset for the next page."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File or directory to search (directories are searched recursively; a symlinked directory given as path is followed).")),
			mcp.WithString("pattern", mcp.Required(), mcp.Description("RE2 regular expression, e.g. \"FATAL|ERROR|failed\" (or literal text with fixed_strings).")),
			mcp.WithArray("include_glob", mcp.Description("Only search files matching any glob, e.g. [\"*.conf\", \"*.{h,cpp}\", \"conf/**/*.conf\"]."), mcp.WithStringItems()),
			mcp.WithArray("exclude_glob", mcp.Description("Skip files and directories matching any glob, e.g. [\"log\", \"*.bak*\", \"third_party/**\"]; excluded directories are not entered."), mcp.WithStringItems()),
			mcp.WithString("output_mode", mcp.Description("content (default), files_with_matches (only file paths, like grep -l) or count (matching lines per file, like grep -c)."),
				mcp.Enum(SearchContentMode, SearchFilesMode, SearchCountMode)),
			mcp.WithBoolean("ignore_case", mcp.Description("Case-insensitive matching. Default false.")),
			mcp.WithBoolean("fixed_strings", mcp.Description("Treat pattern as literal text, not a regex (grep -F). Default false.")),
			mcp.WithNumber("context_lines", mcp.Description("content mode: N lines before and after each match (grep -C). Default 0, max 20.")),
			mcp.WithNumber("before_lines", mcp.Description("content mode: N lines before each match (grep -B); overrides context_lines.")),
			mcp.WithNumber("after_lines", mcp.Description("content mode: N lines after each match (grep -A); overrides context_lines.")),
			mcp.WithNumber("max_results", mcp.Description("Maximum results per call: matches (content) or files. Default 200, max 5000; output is also capped at 64KB. truncated=true with next_offset signals more.")),
			mcp.WithNumber("offset", mcp.Description("Skip this many results first: pass next_offset of a truncated call (same other arguments) for the next page.")),
			mcp.WithNumber("max_file_size", mcp.Description("Skip files larger than this many bytes. Default 256MB.")),
			mcp.WithBoolean("include_hidden", mcp.Description("Search dot-files/dot-directories too. Default false.")),
			mcp.WithString("encoding", mcp.Description("Force the source encoding; by default it is detected per file and per line."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
			formatOption(),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			path := req.GetString("path", "")
			if path == "" {
				return mcp.NewToolResultError("path is required"), nil
			}
			if req.GetString("pattern", "") == "" {
				return mcp.NewToolResultError("pattern is required"), nil
			}

			result, err := SearchContent(SearchContentRequest{
				Path:          path,
				Pattern:       req.GetString("pattern", ""),
				IncludeGlob:   req.GetStringSlice("include_glob", nil),
				ExcludeGlob:   req.GetStringSlice("exclude_glob", nil),
				OutputMode:    req.GetString("output_mode", ""),
				IgnoreCase:    req.GetBool("ignore_case", false),
				FixedStrings:  req.GetBool("fixed_strings", false),
				ContextLines:  req.GetInt("context_lines", 0),
				BeforeLines:   req.GetInt("before_lines", 0),
				AfterLines:    req.GetInt("after_lines", 0),
				MaxResults:    req.GetInt("max_results", 0),
				Offset:        req.GetInt("offset", 0),
				MaxFileSize:   int64(req.GetInt("max_file_size", 0)),
				IncludeHidden: req.GetBool("include_hidden", false),
				Encoding:      req.GetString("encoding", ""),
				Deadline:      time.Now().Add(scanBudget(cfg.callBudget())),
			}, roots)
			if err != nil {
				auditFileOp(ctx, "search_content", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(ctx, "search_content", path, true, result.TotalMatches, result.Truncated)
			if textFormat(req) {
				return textResult(searchText(result)), nil
			}
			return jsonResult(result), nil
		})
	}

	if cfg.toolEnabled("remote_find_files") {
		s.AddTool(mcp.NewTool("remote_find_files",
			readOnlyHints(),
			mcp.WithDescription("Find files and directories on the remote server — the structured equivalent of `find <path> -name/-iname/-newer/-size`, but sandboxed to the FS roots (a stray `find /` is impossible) and explicit about truncation. "+
				"Globs: \"*.conf\" matches base names, \"conf/*.conf\" and \"**/*.conf\" match paths below path, \"*.{h,cpp}\" alternates. "+
				"Filter by modification time (newer_than / older_than: RFC3339 or an age like 30m, 2h, 7d) and size; sort_by mtime or size returns the newest / largest first."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Directory to search recursively (a symlinked directory given as path is followed).")),
			mcp.WithArray("name_glob", mcp.Description("Globs to match, e.g. [\"*.cpp\", \"*.h\"] or [\"conf/**/*.conf\"]. Empty matches everything."), mcp.WithStringItems()),
			mcp.WithArray("exclude_glob", mcp.Description("Skip entries matching any glob, e.g. [\"log\", \"*.bak*\"]; excluded directories are not entered."), mcp.WithStringItems()),
			mcp.WithString("type", mcp.Description("Entry type filter."), mcp.Enum("file", "dir", "symlink", "any")),
			mcp.WithNumber("max_depth", mcp.Description("Maximum directory depth (1 = direct children only). Default unlimited.")),
			mcp.WithString("newer_than", mcp.Description("Only entries modified after this: RFC3339 time or an age such as \"30m\", \"2h\", \"7d\" (find -mmin -N / -newer).")),
			mcp.WithString("older_than", mcp.Description("Only entries modified before this: RFC3339 time or an age such as \"7d\".")),
			mcp.WithNumber("min_size", mcp.Description("Only files of at least this many bytes (find -size +N).")),
			mcp.WithNumber("max_size", mcp.Description("Only files of at most this many bytes.")),
			mcp.WithString("sort_by", mcp.Description("path (default: walk order), mtime (newest first) or size (largest first)."), mcp.Enum("path", "mtime", "size")),
			mcp.WithNumber("max_results", mcp.Description("Maximum entries to return. Default 500, max 10000; the truncated flag signals more.")),
			mcp.WithBoolean("ignore_case", mcp.Description("Case-insensitive name matching (like find -iname). Default false.")),
			mcp.WithBoolean("include_hidden", mcp.Description("Include dot-files/dot-directories. Default false.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			path := req.GetString("path", "")
			if path == "" {
				return mcp.NewToolResultError("path is required"), nil
			}

			result, err := FindFiles(FindFilesRequest{
				Path:          path,
				NameGlob:      req.GetStringSlice("name_glob", nil),
				ExcludeGlob:   req.GetStringSlice("exclude_glob", nil),
				Type:          req.GetString("type", ""),
				MaxDepth:      req.GetInt("max_depth", 0),
				NewerThan:     req.GetString("newer_than", ""),
				OlderThan:     req.GetString("older_than", ""),
				MinSize:       int64(req.GetInt("min_size", 0)),
				MaxSize:       int64(req.GetInt("max_size", 0)),
				SortBy:        req.GetString("sort_by", ""),
				MaxResults:    req.GetInt("max_results", 0),
				IgnoreCase:    req.GetBool("ignore_case", false),
				IncludeHidden: req.GetBool("include_hidden", false),
			}, roots)
			if err != nil {
				auditFileOp(ctx, "find_files", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(ctx, "find_files", path, true, result.Count, result.Truncated)
			return jsonResult(result), nil
		})
	}
}

// registerDiffTool adds remote_diff: file-to-file unified diff, or a
// directory-tree comparison (added / removed / changed).
func registerDiffTool(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if !cfg.toolEnabled("remote_diff") {
		return
	}
	s.AddTool(mcp.NewTool("remote_diff",
		readOnlyHints(),
		mcp.WithDescription("Compare two files or two directory trees on the remote server — instead of diff / cmp / `sha256sum` loops over two deployments. "+
			"Two files: a unified diff from left to right (3 lines of context; GBK and UTF-8 are decoded, so an encoding-only change shows as identical text). "+
			"Two directories: relative paths only in right (added), only in left (removed), and in both with different content (changed), compared by size then SHA-256; "+
			"show_diffs=true adds the diff of up to 20 changed text files. Files over 8MB and binary files are compared by content only."),
		mcp.WithString("left", mcp.Required(), mcp.Description("Old file or directory (the - side).")),
		mcp.WithString("right", mcp.Required(), mcp.Description("New file or directory (the + side).")),
		mcp.WithBoolean("ignore_space", mcp.Description("Ignore changes in the amount of whitespace within lines (diff -b). Default false.")),
		mcp.WithArray("exclude_glob", mcp.Description("Directory mode: skip entries matching any glob, e.g. [\"log\", \"*.bak*\", \"data/**\"]."), mcp.WithStringItems()),
		mcp.WithBoolean("include_hidden", mcp.Description("Directory mode: include dot-files/dot-directories. Default false.")),
		mcp.WithBoolean("show_diffs", mcp.Description("Directory mode: include the unified diff of changed text files (up to 20, within max_bytes). Default false.")),
		mcp.WithNumber("max_bytes", mcp.Description("Cap on the diff text. Default 65536 (64KB).")),
		formatOption(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		left, right := req.GetString("left", ""), req.GetString("right", "")
		if left == "" || right == "" {
			return mcp.NewToolResultError("left and right are required"), nil
		}
		result, err := Diff(DiffRequest{
			Left:          left,
			Right:         right,
			IgnoreSpace:   req.GetBool("ignore_space", false),
			ExcludeGlob:   req.GetStringSlice("exclude_glob", nil),
			IncludeHidden: req.GetBool("include_hidden", false),
			ShowDiffs:     req.GetBool("show_diffs", false),
			MaxBytes:      req.GetInt("max_bytes", 0),
			Deadline:      time.Now().Add(scanBudget(cfg.callBudget())),
		}, roots)
		if err != nil {
			auditFileOp(ctx, "diff", left+" "+right, false, 0, false)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(ctx, "diff", left+" "+right, true, len(result.Diff), result.Truncated)
		if textFormat(req) {
			return textResult(diffText(result)), nil
		}
		return jsonResult(result), nil
	})
}

// registerTailLogTool adds the log-viewing tool (tail/cursor/follow/filter).
func registerTailLogTool(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if !cfg.toolEnabled("remote_tail_log") {
		return
	}
	s.AddTool(mcp.NewTool("remote_tail_log",
		readOnlyHints(),
		mcp.WithDescription("Read the tail of a (possibly huge, growing) log file on the remote server without loading it into memory. Replaces `tail -n N`, `grep X log | tail -n N` (lines=N + filter_regex=X: the last N MATCHING lines, found by scanning backwards), `tail -n +N` (use since_line=N-1), and `sleep 70; tail ...` (use follow_seconds=70). Two cursor styles: since_line (line numbers) or since_offset (byte offsets, cheapest for repeated polling — take end_offset from the previous response)."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Log file path on the remote server.")),
		mcp.WithNumber("lines", mcp.Description("Tail mode: return the last N lines — with filter_regex, the last N matching lines. Default 100, max 10000.")),
		mcp.WithString("filter_regex", mcp.Description("RE2: return only matching lines, e.g. \"FATAL|ERROR\" (RE2 alternation is a bare |, not GNU grep's \\|). Cursors still advance past non-matching lines.")),
		mcp.WithNumber("since_line", mcp.Description("Line cursor: return lines after this 1-based line number (use end_line of the previous response).")),
		mcp.WithNumber("since_offset", mcp.Description("Byte cursor: return content after this offset (use end_offset of the previous response). Wins over since_line.")),
		mcp.WithNumber("follow_seconds", mcp.Description(fmt.Sprintf("Wait up to N seconds (max %d, the call budget) for new content before returning; timed_out=true when nothing arrived — call again with since_offset=end_offset to keep following. To wait for a specific line use remote_wait_for(condition=log_regex).", cfg.callBudgetSec()))),
		mcp.WithString("encoding", mcp.Description("Source encoding; auto-detected if omitted."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
		mcp.WithNumber("max_bytes", mcp.Description("Cap on collected content bytes. Default 65536 (64KB); larger results risk being cut by the client.")),
		formatOption(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}

		follow := req.GetInt("follow_seconds", 0)
		if max := cfg.callBudgetSec(); follow > max {
			follow = max // a longer follow would outlive the client's request
		}
		result, err := TailLog(TailLogRequest{
			Path:          path,
			Lines:         req.GetInt("lines", 0),
			FilterRegex:   req.GetString("filter_regex", ""),
			SinceLine:     req.GetInt("since_line", 0),
			SinceOffset:   int64(req.GetInt("since_offset", 0)),
			FollowSeconds: follow,
			Encoding:      req.GetString("encoding", ""),
			MaxBytes:      req.GetInt("max_bytes", 0),
		}, roots)
		if err != nil {
			auditFileOp(ctx, "tail_log", path, false, 0, false)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(ctx, "tail_log", path, true, len(result.Content), result.Truncated)
		if textFormat(req) {
			return textResult(tailLogText(result)), nil
		}
		return jsonResult(result), nil
	})
}

// registerManageTools adds move/copy/delete/mkdir. remote_delete_file is
// only registered when FSRoots is non-empty: deleting with an unbounded
// filesystem is considered too dangerous (disable it explicitly via
// SHELL_API_DISABLED_TOOLS otherwise).
func registerManageTools(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if cfg.toolEnabled("remote_move_file") {
		s.AddTool(mcp.NewTool("remote_move_file",
			mcp.WithDescription("Move/rename a file or directory on the remote server (cross-filesystem moves fall back to copy+remove). Both paths must stay inside the FS roots."),
			mcp.WithString("src", mcp.Required(), mcp.Description("Source path.")),
			mcp.WithString("dst", mcp.Required(), mcp.Description("Destination path.")),
			mcp.WithBoolean("overwrite", mcp.Description("Replace an existing destination. Default false.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			src, dst := req.GetString("src", ""), req.GetString("dst", "")
			if src == "" || dst == "" {
				return mcp.NewToolResultError("src and dst are required"), nil
			}
			result, err := MoveFile(MoveFileRequest{Src: src, Dst: dst, Overwrite: req.GetBool("overwrite", false)}, roots)
			if err != nil {
				auditFileOp(ctx, "move_file", src+" -> "+dst, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(ctx, "move_file", src+" -> "+dst, true, 0, false)
			return jsonResult(result), nil
		})
	}

	if cfg.toolEnabled("remote_copy_file") {
		s.AddTool(mcp.NewTool("remote_copy_file",
			mcp.WithDescription("Copy a file (preserving mode) or a directory tree on the remote server. Both paths must stay inside the FS roots."),
			mcp.WithString("src", mcp.Required(), mcp.Description("Source path.")),
			mcp.WithString("dst", mcp.Required(), mcp.Description("Destination path. Must not exist unless overwrite is set; existing directories are never merged.")),
			mcp.WithBoolean("overwrite", mcp.Description("Replace an existing destination file. Default false.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			src, dst := req.GetString("src", ""), req.GetString("dst", "")
			if src == "" || dst == "" {
				return mcp.NewToolResultError("src and dst are required"), nil
			}
			result, err := CopyFile(CopyFileRequest{Src: src, Dst: dst, Overwrite: req.GetBool("overwrite", false)}, roots)
			if err != nil {
				auditFileOp(ctx, "copy_file", src+" -> "+dst, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(ctx, "copy_file", src+" -> "+dst, true, result.EntriesCopied+int(result.BytesCopied), false)
			return jsonResult(result), nil
		})
	}

	if cfg.toolEnabled("remote_delete_file") {
		if len(roots) == 0 {
			// Safety: refuse to expose deletion over the whole filesystem.
			// Operators who really want it must set SHELL_API_FS_ROOT.
			log.Printf("remote_delete_file not registered: SHELL_API_FS_ROOT is empty (delete requires a sandbox)")
		} else {
			s.AddTool(mcp.NewTool("remote_delete_file",
				mcp.WithDescription("Delete a file or directory tree inside the FS roots. SAFEGUARDS: requires confirm=true to delete anything; call with dry_run=true first to see the affected entries; refuses to delete an FS root itself."),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path to delete (file or directory).")),
				mcp.WithBoolean("recursive", mcp.Description("Required for non-empty directories. Default false.")),
				mcp.WithBoolean("dry_run", mcp.Description("List what would be deleted without deleting. Default false.")),
				mcp.WithBoolean("confirm", mcp.Description("Required true to actually delete. Default false.")),
			), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				path := req.GetString("path", "")
				if path == "" {
					return mcp.NewToolResultError("path is required"), nil
				}
				result, err := DeleteFile(DeleteFileRequest{
					Path:      path,
					Recursive: req.GetBool("recursive", false),
					DryRun:    req.GetBool("dry_run", false),
					Confirm:   req.GetBool("confirm", false),
				}, roots)
				if err != nil {
					auditFileOp(ctx, "delete_file", path, false, 0, false)
					return mcp.NewToolResultError(err.Error()), nil
				}
				auditFileOp(ctx, "delete_file", path, true, result.Entries, false)
				return jsonResult(result), nil
			})
		}
	}

	if cfg.toolEnabled("remote_make_dir") {
		s.AddTool(mcp.NewTool("remote_make_dir",
			mcp.WithDescription("Create a directory on the remote server (like mkdir -p by default), with optional octal mode."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Directory path to create.")),
			mcp.WithString("mode", mcp.Description("Octal permission, e.g. \"0755\". Default 0755.")),
			mcp.WithBoolean("parents", mcp.Description("Create parent directories as needed (mkdir -p). Default true.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			path := req.GetString("path", "")
			if path == "" {
				return mcp.NewToolResultError("path is required"), nil
			}
			result, err := MakeDir(MakeDirRequest{
				Path:    path,
				Mode:    req.GetString("mode", ""),
				Parents: req.GetBool("parents", true),
			}, roots)
			if err != nil {
				auditFileOp(ctx, "make_dir", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(ctx, "make_dir", result.Path, true, 0, false)
			return jsonResult(result), nil
		})
	}
}
