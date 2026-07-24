package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerFileTools adds file operation tools to the MCP server. All tools
// honor cfg.FSRoots as an optional sandbox, respect the cfg tool blacklist,
// and reuse the shared audit log.
func registerFileTools(s *server.MCPServer, audit *AuditLogger, cfg *Config) {
	roots := cfg.FSRoots

	// auditFileOp records a file operation in the shared audit log, reusing the
	// AuditEntry shape (Command carries a synthetic "op path" descriptor).
	auditFileOp := func(header http.Header, op, path string, ok bool, bytes int, truncated bool) {
		exit := 0
		if !ok {
			exit = 1
		}
		workDir := ""
		if len(roots) > 0 {
			workDir = strings.Join(roots, ",")
		}
		audit.Log(AuditEntry{
			SourceIP:         sourceIP(header),
			Tool:             "remote_" + op,
			Command:          op + " " + path,
			WorkingDirectory: workDir,
			ExitCode:         exit,
			OutputBytes:      bytes,
			Truncated:        truncated,
		})
	}

	registerWriteTool(s, cfg, roots, auditFileOp)
	registerReadTool(s, cfg, roots, auditFileOp)
	registerEditTool(s, cfg, roots, auditFileOp)
	registerListStatTools(s, cfg, roots, auditFileOp)
	registerBase64Tools(s, cfg, roots, auditFileOp)
	registerSearchTools(s, cfg, roots, auditFileOp)
	registerTailLogTool(s, cfg, roots, auditFileOp)
	registerManageTools(s, cfg, roots, auditFileOp)
}

type fileAuditFunc func(header http.Header, op, path string, ok bool, bytes int, truncated bool)

func registerWriteTool(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if !cfg.toolEnabled("remote_write_file") {
		return
	}
	s.AddTool(mcp.NewTool("remote_write_file",
		mcp.WithDescription("Write UTF-8 text content to a file on the remote server. Supports target encoding conversion (utf-8/gbk/gb2312/gb18030), append mode, and auto-creating parent directories. For binary data use remote_upload_base64. Do NOT fall back to shell `echo ... > file` or heredocs: this tool returns sha256/mode for verification and can lint the result (bash -n / py_compile)."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Absolute or relative file path on the remote server.")),
		mcp.WithString("content", mcp.Required(), mcp.Description("UTF-8 text content to write.")),
		mcp.WithString("encoding", mcp.Description("On-disk encoding: utf-8 (default), gbk, gb2312, gb18030."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
		mcp.WithBoolean("append", mcp.Description("Append to the file instead of overwriting. Default false.")),
		mcp.WithBoolean("make_dirs", mcp.Description("Create parent directories if missing. Default false.")),
		mcp.WithString("mode", mcp.Description("Octal permission for newly created files, e.g. \"0644\". Default 0644.")),
		mcp.WithString("lint", mcp.Description("Post-write syntax check: \"bash\" (bash -n) or \"python\" (py_compile). Result is advisory and returned in the response."), mcp.Enum("bash", "python", "none")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}
		content := req.GetString("content", "")

		result, err := WriteFileContent(WriteFileRequest{
			Path:     path,
			Content:  content,
			Encoding: req.GetString("encoding", ""),
			Append:   req.GetBool("append", false),
			MakeDirs: req.GetBool("make_dirs", false),
			Mode:     req.GetString("mode", ""),
			Lint:     req.GetString("lint", ""),
		}, roots)
		if err != nil {
			auditFileOp(req.Header, "write_file", path, false, 0, false)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(req.Header, "write_file", result.Path, true, result.BytesWritten, false)
		return jsonResult(result), nil
	})
}

func registerReadTool(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if !cfg.toolEnabled("remote_read_file") {
		return
	}
	s.AddTool(mcp.NewTool("remote_read_file",
		mcp.WithDescription("Read a file from the remote server and return its content as UTF-8. Auto-detects source encoding unless specified, detects binary files (use remote_download_base64 for those). Reads are seek-based: large files are never loaded wholesale. Prefer this over shell cat/head/sed: cat <file> = call with just path; sed -n 'X,Yp' = start_line/end_line; tail -n +N = start_line=N; tail -N = tail_lines=N; head -c N = max_bytes=N. For growing logs with follow/filter use remote_tail_log."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Absolute or relative file path on the remote server.")),
		mcp.WithString("encoding", mcp.Description("Source encoding; auto-detected if omitted."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
		mcp.WithNumber("start_line", mcp.Description("1-based start line (inclusive). Omit or 0 for start of file.")),
		mcp.WithNumber("end_line", mcp.Description("1-based end line (inclusive). Omit or 0 for end of file.")),
		mcp.WithNumber("tail_lines", mcp.Description("Read the last N lines (seek-based, works on huge files). Wins over start_line/end_line.")),
		mcp.WithNumber("offset_bytes", mcp.Description("Start reading at this byte offset. Default 0.")),
		mcp.WithNumber("max_bytes", mcp.Description("Maximum bytes to read. Default 1MB.")),
		mcp.WithString("truncate_mode", mcp.Description("When the file exceeds max_bytes: \"head\" keeps the first bytes (default), \"tail\" keeps the last bytes."), mcp.Enum("head", "tail")),
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
		}, roots)
		if err != nil {
			auditFileOp(req.Header, "read_file", path, false, 0, false)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(req.Header, "read_file", result.Path, true, len(result.Content), result.Truncated)
		return jsonResult(result), nil
	})
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
			auditFileOp(req.Header, "edit_file", path, false, 0, false)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(req.Header, "edit_file", result.Path, true, 0, false)
		return jsonResult(result), nil
	})
}

func registerListStatTools(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if cfg.toolEnabled("remote_list_dir") {
		s.AddTool(mcp.NewTool("remote_list_dir",
			mcp.WithDescription("List directory entries on the remote server with name, type, size, mode, owner/group, mtime and symlink targets — the structured equivalent of `ls -la`. Supports glob filtering, hidden files, and sorting."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Directory path on the remote server.")),
			mcp.WithString("sort_by", mcp.Description("Sort order: \"name\" (default, ascending), \"mtime\" (newest first), \"size\" (largest first)."), mcp.Enum("name", "mtime", "size")),
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
				auditFileOp(req.Header, "list_dir", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(req.Header, "list_dir", result.Path, true, result.Count, false)
			return jsonResult(result), nil
		})
	}

	if cfg.toolEnabled("remote_stat") {
		s.AddTool(mcp.NewTool("remote_stat",
			mcp.WithDescription("Return metadata about a path on the remote server: existence, type, size, mode, owner/group, nlink, mtime, symlink target, and (for regular files) binary sniff. Optionally include a content hash (md5/sha256) and detected text encoding — one call replaces the `stat -c ...; file -I; md5sum` combo. A missing path returns exists=false without error."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File or directory path on the remote server.")),
			mcp.WithString("include_hash", mcp.Description("Compute a streaming content hash (regular files up to 256MB)."), mcp.Enum("md5", "sha256")),
			mcp.WithBoolean("include_encoding", mcp.Description("Detect text encoding (utf-8/gbk/unknown) from the file head.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			path := req.GetString("path", "")
			if path == "" {
				return mcp.NewToolResultError("path is required"), nil
			}

			result, err := StatFile(StatRequest{
				Path:            path,
				IncludeHash:     req.GetString("include_hash", ""),
				IncludeEncoding: req.GetBool("include_encoding", false),
			}, roots)
			if err != nil {
				auditFileOp(req.Header, "stat", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(req.Header, "stat", result.Path, true, 0, false)
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
				auditFileOp(req.Header, "upload_base64", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(req.Header, "upload_base64", result.Path, true, result.BytesWritten, false)
			return jsonResult(result), nil
		})
	}

	if cfg.toolEnabled("remote_download_base64") {
		s.AddTool(mcp.NewTool("remote_download_base64",
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
				auditFileOp(req.Header, "download_base64", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(req.Header, "download_base64", result.Path, true, result.BytesRead, false)
			return jsonResult(result), nil
		})
	}
}

// registerSearchTools adds the server-side content/file search tools.
func registerSearchTools(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if cfg.toolEnabled("remote_search_content") {
		s.AddTool(mcp.NewTool("remote_search_content",
			mcp.WithDescription("Search file contents on the remote server with an RE2 regex — the structured equivalent of `grep -rn pattern path`, implemented server-side (no grep/rg needed on the target). No matches returns an empty list, NOT an error. Binary and oversized files are skipped automatically; hidden files are excluded unless requested."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File or directory to search (directories are searched recursively).")),
			mcp.WithString("pattern", mcp.Required(), mcp.Description("RE2 regular expression, e.g. \"FATAL|ERROR|failed\".")),
			mcp.WithArray("include_glob", mcp.Description("Only search files whose name matches any glob, e.g. [\"*.conf\", \"*.log\"]."), mcp.WithStringItems()),
			mcp.WithBoolean("ignore_case", mcp.Description("Case-insensitive matching. Default false.")),
			mcp.WithNumber("context_lines", mcp.Description("Include N lines before and after each match. Default 0, max 20.")),
			mcp.WithNumber("max_results", mcp.Description("Maximum matches to return. Default 200, max 5000; the truncated flag signals more.")),
			mcp.WithNumber("max_file_size", mcp.Description("Skip files larger than this many bytes. Default 32MB.")),
			mcp.WithBoolean("include_hidden", mcp.Description("Search dot-files/dot-directories too. Default false.")),
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
				IgnoreCase:    req.GetBool("ignore_case", false),
				ContextLines:  req.GetInt("context_lines", 0),
				MaxResults:    req.GetInt("max_results", 0),
				MaxFileSize:   int64(req.GetInt("max_file_size", 0)),
				IncludeHidden: req.GetBool("include_hidden", false),
			}, roots)
			if err != nil {
				auditFileOp(req.Header, "search_content", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(req.Header, "search_content", path, true, result.TotalMatches, result.Truncated)
			return jsonResult(result), nil
		})
	}

	if cfg.toolEnabled("remote_find_files") {
		s.AddTool(mcp.NewTool("remote_find_files",
			mcp.WithDescription("Find files and directories by name on the remote server — the structured equivalent of `find <path> -name/-iname`, but sandboxed to the FS roots (a stray `find /` is impossible) and explicit about truncation."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Directory to search recursively.")),
			mcp.WithArray("name_glob", mcp.Description("Name globs to match, e.g. [\"*.cpp\", \"*.h\"]. Empty matches everything."), mcp.WithStringItems()),
			mcp.WithString("type", mcp.Description("Entry type filter."), mcp.Enum("file", "dir", "any")),
			mcp.WithNumber("max_depth", mcp.Description("Maximum directory depth (1 = direct children only). Default unlimited.")),
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
				Type:          req.GetString("type", ""),
				MaxDepth:      req.GetInt("max_depth", 0),
				MaxResults:    req.GetInt("max_results", 0),
				IgnoreCase:    req.GetBool("ignore_case", false),
				IncludeHidden: req.GetBool("include_hidden", false),
			}, roots)
			if err != nil {
				auditFileOp(req.Header, "find_files", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(req.Header, "find_files", path, true, result.Count, result.Truncated)
			return jsonResult(result), nil
		})
	}
}

// registerTailLogTool adds the log-viewing tool (tail/cursor/follow/filter).
func registerTailLogTool(s *server.MCPServer, cfg *Config, roots []string, auditFileOp fileAuditFunc) {
	if !cfg.toolEnabled("remote_tail_log") {
		return
	}
	s.AddTool(mcp.NewTool("remote_tail_log",
		mcp.WithDescription("Read the tail of a (possibly huge, growing) log file on the remote server without loading it into memory. Replaces `tail -n N`, `tail -n +N` (use since_line=N-1), and `sleep 70; tail ...` (use follow_seconds=70). Supports regex filtering and two cursor styles: since_line (line numbers) or since_offset (byte offsets, cheapest for repeated polling — take end_offset from the previous response)."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Log file path on the remote server.")),
		mcp.WithNumber("lines", mcp.Description("Tail mode: return the last N lines. Default 100, max 10000.")),
		mcp.WithString("filter_regex", mcp.Description("RE2: return only matching lines, e.g. \"FATAL|ERROR\". Cursors still advance past non-matching lines.")),
		mcp.WithNumber("since_line", mcp.Description("Line cursor: return lines after this 1-based line number (use end_line of the previous response).")),
		mcp.WithNumber("since_offset", mcp.Description("Byte cursor: return content after this offset (use end_offset of the previous response). Wins over since_line.")),
		mcp.WithNumber("follow_seconds", mcp.Description("Wait up to N seconds (max 300) for new content before returning; timed_out=true when nothing arrived.")),
		mcp.WithString("encoding", mcp.Description("Source encoding; auto-detected if omitted."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
		mcp.WithNumber("max_bytes", mcp.Description("Cap on collected content bytes. Default 1MB.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}

		result, err := TailLog(TailLogRequest{
			Path:          path,
			Lines:         req.GetInt("lines", 0),
			FilterRegex:   req.GetString("filter_regex", ""),
			SinceLine:     req.GetInt("since_line", 0),
			SinceOffset:   int64(req.GetInt("since_offset", 0)),
			FollowSeconds: req.GetInt("follow_seconds", 0),
			Encoding:      req.GetString("encoding", ""),
			MaxBytes:      req.GetInt("max_bytes", 0),
		}, roots)
		if err != nil {
			auditFileOp(req.Header, "tail_log", path, false, 0, false)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(req.Header, "tail_log", path, true, len(result.Content), result.Truncated)
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
				auditFileOp(req.Header, "move_file", src+" -> "+dst, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(req.Header, "move_file", src+" -> "+dst, true, 0, false)
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
				auditFileOp(req.Header, "copy_file", src+" -> "+dst, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(req.Header, "copy_file", src+" -> "+dst, true, result.EntriesCopied+int(result.BytesCopied), false)
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
					auditFileOp(req.Header, "delete_file", path, false, 0, false)
					return mcp.NewToolResultError(err.Error()), nil
				}
				auditFileOp(req.Header, "delete_file", path, true, result.Entries, false)
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
				auditFileOp(req.Header, "make_dir", path, false, 0, false)
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditFileOp(req.Header, "make_dir", result.Path, true, 0, false)
			return jsonResult(result), nil
		})
	}
}
