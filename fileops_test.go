package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

func TestEncodingRoundTrip(t *testing.T) {
	cases := []struct {
		enc  string
		text string
	}{
		{"utf-8", "hello 世界"},
		{"", "plain ascii"},
		{"gbk", "计费系统 toad-policy2"},
		{"gb18030", "预算迁移 €符号测试"},
	}
	for _, c := range cases {
		encoded, err := encodeFromUTF8(c.text, c.enc)
		if err != nil {
			t.Fatalf("encode %q enc=%s: %v", c.text, c.enc, err)
		}
		decoded, err := decodeToUTF8(encoded, c.enc)
		if err != nil {
			t.Fatalf("decode enc=%s: %v", c.enc, err)
		}
		if decoded != c.text {
			t.Errorf("round trip enc=%s: got %q want %q", c.enc, decoded, c.text)
		}
	}

	if _, err := decodeToUTF8([]byte("x"), "latin1"); err == nil {
		t.Error("expected error for unknown encoding")
	}
}

func TestDetectEncoding(t *testing.T) {
	if got := detectEncoding([]byte("just utf8 世界")); got != "utf-8" {
		t.Errorf("utf8 detect: got %s", got)
	}
	gbk, _, _ := transform.Bytes(simplifiedchinese.GBK.NewEncoder(), []byte("中文编码"))
	if got := detectEncoding(gbk); got != "gbk" {
		t.Errorf("gbk detect: got %s", got)
	}
}

func TestLooksBinary(t *testing.T) {
	if looksBinary([]byte("normal text\nsecond line")) {
		t.Error("text misdetected as binary")
	}
	if !looksBinary([]byte{0x89, 0x50, 0x00, 0x4e}) {
		t.Error("NUL byte not detected as binary")
	}
}

func TestValidatePathSandbox(t *testing.T) {
	root := t.TempDir()

	inside := filepath.Join(root, "sub", "file.txt")
	if _, err := validatePath(inside, root); err != nil {
		t.Errorf("path inside root rejected: %v", err)
	}

	// Sibling directory sharing a prefix must not bypass the sandbox.
	bypass := root + "xxx/evil.txt"
	if _, err := validatePath(bypass, root); err == nil {
		t.Error("prefix-bypass path was allowed")
	}

	// Traversal escaping root.
	escape := filepath.Join(root, "..", "outside.txt")
	if _, err := validatePath(escape, root); err == nil {
		t.Error("traversal escape was allowed")
	}

	// No root => anything valid.
	if _, err := validatePath("/etc/hosts", ""); err != nil {
		t.Errorf("no-root path rejected: %v", err)
	}

	if _, err := validatePath("", root); err == nil {
		t.Error("empty path allowed")
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "nested", "note.txt")

	wr, err := WriteFileContent(WriteFileRequest{
		Path:     target,
		Content:  "第一行\n第二行\n第三行",
		Encoding: "gbk",
		MakeDirs: true,
	}, root)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !wr.Created {
		t.Error("expected Created=true for new file")
	}

	// On-disk bytes should be GBK, not UTF-8.
	raw, _ := os.ReadFile(target)
	if strings.Contains(string(raw), "第") {
		t.Error("file was not GBK-encoded on disk")
	}

	rd, err := ReadFileContent(ReadFileRequest{Path: target}, root)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if rd.Encoding != "gbk" {
		t.Errorf("expected detected encoding gbk, got %s", rd.Encoding)
	}
	if rd.Content != "第一行\n第二行\n第三行" {
		t.Errorf("content mismatch: %q", rd.Content)
	}

	// Line range slice.
	slice, err := ReadFileContent(ReadFileRequest{Path: target, StartLine: 2, EndLine: 2}, root)
	if err != nil {
		t.Fatalf("read slice: %v", err)
	}
	if slice.Content != "第二行" || slice.TotalLines != 3 || slice.ReturnedLines != 1 {
		t.Errorf("slice mismatch: content=%q total=%d returned=%d", slice.Content, slice.TotalLines, slice.ReturnedLines)
	}
}

func TestWriteAppend(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "log.txt")

	if _, err := WriteFileContent(WriteFileRequest{Path: target, Content: "a"}, root); err != nil {
		t.Fatal(err)
	}
	wr, err := WriteFileContent(WriteFileRequest{Path: target, Content: "b", Append: true}, root)
	if err != nil {
		t.Fatal(err)
	}
	if wr.Created {
		t.Error("append should not report Created")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "ab" {
		t.Errorf("append result: %q", got)
	}
}

func TestEditFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "code.txt")
	os.WriteFile(target, []byte("foo bar foo baz"), 0644)

	// Non-unique without replace_all => error.
	if _, err := EditFileContent(EditFileRequest{Path: target, OldString: "foo", NewString: "X"}, root); err == nil {
		t.Error("expected non-unique error")
	}

	res, err := EditFileContent(EditFileRequest{Path: target, OldString: "foo", NewString: "X", ReplaceAll: true}, root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Replacements != 2 {
		t.Errorf("expected 2 replacements, got %d", res.Replacements)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "X bar X baz" {
		t.Errorf("edit result: %q", got)
	}

	// Not found.
	if _, err := EditFileContent(EditFileRequest{Path: target, OldString: "missing", NewString: "Y"}, root); err == nil {
		t.Error("expected not-found error")
	}
}

func TestBase64RoundTripChunked(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "blob.bin")

	// Binary payload with NUL bytes.
	original := []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0x00, 0x42, 0x00}

	// Chunked upload: first half overwrite, second half append.
	half := len(original) / 2
	if _, err := UploadBase64(UploadBase64Request{
		Path:    target,
		DataB64: base64.StdEncoding.EncodeToString(original[:half]),
	}, root); err != nil {
		t.Fatalf("upload chunk1: %v", err)
	}
	if _, err := UploadBase64(UploadBase64Request{
		Path:    target,
		DataB64: base64.StdEncoding.EncodeToString(original[half:]),
		Append:  true,
	}, root); err != nil {
		t.Fatalf("upload chunk2: %v", err)
	}

	got, _ := os.ReadFile(target)
	if string(got) != string(original) {
		t.Errorf("uploaded bytes mismatch: %v", got)
	}

	// Chunked download: 4 bytes at a time.
	var assembled []byte
	var offset int64
	for {
		dl, err := DownloadBase64(DownloadBase64Request{Path: target, Offset: offset, MaxBytes: 4}, root)
		if err != nil {
			t.Fatalf("download: %v", err)
		}
		chunk, _ := base64.StdEncoding.DecodeString(dl.DataB64)
		assembled = append(assembled, chunk...)
		offset += int64(dl.BytesRead)
		if dl.EOF {
			break
		}
		if dl.BytesRead == 0 {
			t.Fatal("no progress on download")
		}
	}
	if string(assembled) != string(original) {
		t.Errorf("downloaded bytes mismatch: %v", assembled)
	}
}

func TestReadBinaryDetection(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "bin.dat")
	os.WriteFile(target, []byte{0x00, 0x01, 0x02}, 0644)

	rd, err := ReadFileContent(ReadFileRequest{Path: target}, root)
	if err != nil {
		t.Fatal(err)
	}
	if !rd.IsBinary {
		t.Error("binary file not flagged")
	}
}

func TestStatFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "exists.txt")
	os.WriteFile(target, []byte("hi"), 0644)

	st, err := StatFile(target, root)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || st.IsDir || st.Size != 2 {
		t.Errorf("stat: %+v", st)
	}

	missing, err := StatFile(filepath.Join(root, "nope.txt"), root)
	if err != nil {
		t.Fatalf("stat missing should not error: %v", err)
	}
	if missing.Exists {
		t.Error("missing file reported as existing")
	}
}

func TestListDirectory(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0644)
	os.Mkdir(filepath.Join(root, "d"), 0755)

	res, err := ListDirectory(root, root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 2 {
		t.Errorf("expected 2 entries, got %d", res.Count)
	}
}
