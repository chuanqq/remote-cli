package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// cappedBuffer is an io.Writer that retains at most limit bytes while counting
// everything written. Head mode keeps the first bytes and discards the rest;
// tail mode keeps the most recent bytes in a ring. Either way memory is
// bounded by limit, unlike the bytes.Buffer it replaces, which held a
// command's entire output before truncating it (VmHWM 159 MB vs 27 MB RSS in
// production).
type cappedBuffer struct {
	limit int
	tail  bool
	buf   []byte // head mode: retained prefix; tail mode: ring storage
	start int    // tail mode: index of the oldest byte once the ring is full
	full  bool   // tail mode: ring has wrapped
	total int64  // bytes written, including discarded ones
}

func newCappedBuffer(limit int, tail bool) *cappedBuffer {
	if limit < 0 {
		limit = 0
	}
	return &cappedBuffer{limit: limit, tail: tail}
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += int64(n)
	if b.limit == 0 {
		return n, nil
	}
	if !b.tail {
		if room := b.limit - len(b.buf); room > 0 {
			if len(p) > room {
				p = p[:room]
			}
			b.buf = append(b.buf, p...)
		}
		return n, nil
	}

	// Tail mode: only the last `limit` bytes of p can survive.
	if len(p) >= b.limit {
		if cap(b.buf) < b.limit {
			b.buf = make([]byte, b.limit)
		}
		b.buf = b.buf[:b.limit]
		copy(b.buf, p[len(p)-b.limit:])
		b.start, b.full = 0, true
		return n, nil
	}
	if !b.full {
		if len(b.buf)+len(p) <= b.limit {
			b.buf = append(b.buf, p...)
			return n, nil
		}
		// Grow to the full ring, then fall through to the wrapping copy.
		fill := b.limit - len(b.buf)
		b.buf = append(b.buf, p[:fill]...)
		p = p[fill:]
		b.full, b.start = true, 0
	}
	for len(p) > 0 {
		c := copy(b.buf[b.start:], p)
		p = p[c:]
		b.start = (b.start + c) % b.limit
	}
	return n, nil
}

// Bytes returns the retained bytes in write order.
func (b *cappedBuffer) Bytes() []byte {
	if !b.tail || !b.full || b.start == 0 {
		return b.buf
	}
	out := make([]byte, 0, len(b.buf))
	out = append(out, b.buf[b.start:]...)
	return append(out, b.buf[:b.start]...)
}

// truncated reports whether anything was discarded.
func (b *cappedBuffer) truncated() bool {
	return b.total > int64(len(b.buf))
}

// normalizeOutputEncoding validates the output_encoding parameter.
func normalizeOutputEncoding(enc string) (string, error) {
	switch e := strings.ToLower(strings.TrimSpace(enc)); e {
	case "", "utf-8", "utf8":
		return "", nil
	case "gbk", "gb2312", "gb18030", "auto":
		return e, nil
	default:
		return "", fmt.Errorf("unsupported output_encoding %q (want utf-8, gbk, gb2312, gb18030 or auto)", enc)
	}
}

// finishOutput turns captured bytes into the response string: rune-safe
// truncation to max bytes, then optional decoding to UTF-8. It returns the
// text, whether anything was dropped, and the encoding actually decoded from
// ("" when no conversion was requested).
//
// Decoding happens after truncation so max_output_bytes keeps meaning "bytes
// the command produced". A cut can split a GBK double-byte char; the decoder
// then emits one U+FFFD at the edge, which is acceptable.
func finishOutput(b *cappedBuffer, max int, tail bool, encoding string) (string, bool, string) {
	raw := b.Bytes()
	truncated := b.truncated()
	if len(raw) > max {
		truncated = true
	}
	if encoding == "" {
		return truncateToLimit(string(raw), max, tail), truncated, ""
	}

	if len(raw) > max {
		if tail {
			raw = raw[len(raw)-max:]
		} else {
			raw = raw[:max]
		}
	}
	src := encoding
	if src == "auto" {
		if utf8.Valid(raw) {
			return string(raw), truncated, "utf-8"
		}
		src = "gbk"
	}
	out, err := decodeToUTF8(raw, src)
	if err != nil {
		// Undecodable: hand back the raw bytes rather than nothing.
		return string(raw), truncated, ""
	}
	return out, truncated, src
}
