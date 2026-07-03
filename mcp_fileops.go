package main

import (
	"context"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerFileTools adds file operation tools to the MCP server. All tools
// honor cfg.FSRoot as an optional sandbox and reuse the shared audit log.
func registerFileTools(s *server.MCPServer, audit *AuditLogger, cfg *Config) {
	root := cfg.FSRoot

	// auditFileOp records a file operation in the shared audit log, reusing the
	// AuditEntry shape (Command carries a synthetic "op path" descriptor).
	auditFileOp := func(header http.Header, op, path string, ok bool, bytes int) {
		exit := 0
		if !ok {
			exit = 1
		}
		audit.Log(AuditEntry{
			SourceIP:         sourceIP(header),
			Command:          op + " " + path,
			WorkingDirectory: root,
			ExitCode:         exit,
			OutputBytes:      bytes,
		})
	}

	registerWriteTool(s, root, auditFileOp)
	registerReadTool(s, root, auditFileOp)
	registerEditTool(s, root, auditFileOp)
	registerListStatTools(s, root, auditFileOp)
	registerBase64Tools(s, root, auditFileOp)
}

type fileAuditFunc func(header http.Header, op, path string, ok bool, bytes int)

func registerWriteTool(s *server.MCPServer, root string, auditFileOp fileAuditFunc) {
	s.AddTool(mcp.NewTool("remote_write_file",
		mcp.WithDescription("Write UTF-8 text content to a file on the remote server. Supports target encoding conversion (utf-8/gbk/gb2312/gb18030), append mode, and auto-creating parent directories. For binary data use remote_upload_base64."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Absolute or relative file path on the remote server.")),
		mcp.WithString("content", mcp.Required(), mcp.Description("UTF-8 text content to write.")),
		mcp.WithString("encoding", mcp.Description("On-disk encoding: utf-8 (default), gbk, gb2312, gb18030."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
		mcp.WithBoolean("append", mcp.Description("Append to the file instead of overwriting. Default false.")),
		mcp.WithBoolean("make_dirs", mcp.Description("Create parent directories if missing. Default false.")),
		mcp.WithString("mode", mcp.Description("Octal permission for newly created files, e.g. \"0644\". Default 0644.")),
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
		}, root)
		if err != nil {
			auditFileOp(req.Header, "write_file", path, false, 0)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(req.Header, "write_file", result.Path, true, result.BytesWritten)
		return jsonResult(result), nil
	})
}

func registerReadTool(s *server.MCPServer, root string, auditFileOp fileAuditFunc) {
	s.AddTool(mcp.NewTool("remote_read_file",
		mcp.WithDescription("Read a file from the remote server and return its content as UTF-8. Auto-detects source encoding (utf-8/gbk) unless specified, supports 1-based line ranges, and detects binary files (use remote_download_base64 for those)."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Absolute or relative file path on the remote server.")),
		mcp.WithString("encoding", mcp.Description("Source encoding; auto-detected if omitted."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
		mcp.WithNumber("start_line", mcp.Description("1-based start line (inclusive). Omit or 0 for start of file.")),
		mcp.WithNumber("end_line", mcp.Description("1-based end line (inclusive). Omit or 0 for end of file.")),
		mcp.WithNumber("max_bytes", mcp.Description("Maximum bytes to read. Default 1MB.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}

		result, err := ReadFileContent(ReadFileRequest{
			Path:      path,
			Encoding:  req.GetString("encoding", ""),
			StartLine: req.GetInt("start_line", 0),
			EndLine:   req.GetInt("end_line", 0),
			MaxBytes:  req.GetInt("max_bytes", 0),
		}, root)
		if err != nil {
			auditFileOp(req.Header, "read_file", path, false, 0)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(req.Header, "read_file", result.Path, true, len(result.Content))
		return jsonResult(result), nil
	})
}

func registerEditTool(s *server.MCPServer, root string, auditFileOp fileAuditFunc) {
	s.AddTool(mcp.NewTool("remote_edit_file",
		mcp.WithDescription("Perform an exact string replacement in a remote file. old_string must match exactly; by default it must be unique (set replace_all to replace every occurrence). Preserves the file's encoding and permissions."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Absolute or relative file path on the remote server.")),
		mcp.WithString("old_string", mcp.Required(), mcp.Description("Exact text to find (must be unique unless replace_all is true).")),
		mcp.WithString("new_string", mcp.Required(), mcp.Description("Replacement text.")),
		mcp.WithBoolean("replace_all", mcp.Description("Replace all occurrences instead of requiring uniqueness. Default false.")),
		mcp.WithString("encoding", mcp.Description("Source encoding; auto-detected if omitted. Written back with the same encoding."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}
		oldStr := req.GetString("old_string", "")
		if oldStr == "" {
			return mcp.NewToolResultError("old_string is required"), nil
		}

		result, err := EditFileContent(EditFileRequest{
			Path:       path,
			OldString:  oldStr,
			NewString:  req.GetString("new_string", ""),
			ReplaceAll: req.GetBool("replace_all", false),
			Encoding:   req.GetString("encoding", ""),
		}, root)
		if err != nil {
			auditFileOp(req.Header, "edit_file", path, false, 0)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(req.Header, "edit_file", result.Path, true, 0)
		return jsonResult(result), nil
	})
}

func registerListStatTools(s *server.MCPServer, root string, auditFileOp fileAuditFunc) {
	s.AddTool(mcp.NewTool("remote_list_dir",
		mcp.WithDescription("List the entries of a directory on the remote server with name, type, size, mode, and modification time."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Directory path on the remote server.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}

		result, err := ListDirectory(path, root)
		if err != nil {
			auditFileOp(req.Header, "list_dir", path, false, 0)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(req.Header, "list_dir", result.Path, true, result.Count)
		return jsonResult(result), nil
	})

	s.AddTool(mcp.NewTool("remote_stat",
		mcp.WithDescription("Return metadata about a path on the remote server: existence, type, size, mode, modification time, and (for regular files) whether it looks binary. A missing path returns exists=false without error."),
		mcp.WithString("path", mcp.Required(), mcp.Description("File or directory path on the remote server.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}

		result, err := StatFile(path, root)
		if err != nil {
			auditFileOp(req.Header, "stat", path, false, 0)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(req.Header, "stat", result.Path, true, 0)
		return jsonResult(result), nil
	})
}

func registerBase64Tools(s *server.MCPServer, root string, auditFileOp fileAuditFunc) {
	s.AddTool(mcp.NewTool("remote_upload_base64",
		mcp.WithDescription("Upload binary content to a remote file by decoding a base64 payload. Supports append mode for chunked uploads of large files and auto-creating parent directories."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Destination file path on the remote server.")),
		mcp.WithString("data_b64", mcp.Required(), mcp.Description("Base64-encoded (standard encoding) file content or chunk.")),
		mcp.WithBoolean("append", mcp.Description("Append this chunk instead of overwriting. Use for chunked uploads. Default false.")),
		mcp.WithBoolean("make_dirs", mcp.Description("Create parent directories if missing. Default false.")),
		mcp.WithString("mode", mcp.Description("Octal permission for newly created files, e.g. \"0644\". Default 0644.")),
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
		}, root)
		if err != nil {
			auditFileOp(req.Header, "upload_base64", path, false, 0)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(req.Header, "upload_base64", result.Path, true, result.BytesWritten)
		return jsonResult(result), nil
	})

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
		}, root)
		if err != nil {
			auditFileOp(req.Header, "download_base64", path, false, 0)
			return mcp.NewToolResultError(err.Error()), nil
		}
		auditFileOp(req.Header, "download_base64", result.Path, true, result.BytesRead)
		return jsonResult(result), nil
	})
}

