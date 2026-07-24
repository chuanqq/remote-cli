package main

import (
	"testing"
)

func TestParseProcStatLine(t *testing.T) {
	// comm contains spaces and parens — must split after the LAST ')'.
	line := "1234 (toad policy) S 1200 1234 1234 0 -1 4194304 100 0 0 0 5 2 0 0 20 0 1 0 987654321 1000000 50 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0"
	pid, comm, state, ppid, start, ok := parseProcStatLine(line)
	if !ok {
		t.Fatal("parse failed")
	}
	if pid != 1234 || comm != "toad policy" || state != "S" || ppid != 1200 {
		t.Errorf("pid=%d comm=%q state=%s ppid=%d", pid, comm, state, ppid)
	}
	if start != 987654321 {
		t.Errorf("starttime=%d", start)
	}

	if _, _, _, _, _, ok := parseProcStatLine("garbage"); ok {
		t.Error("expected parse failure on garbage")
	}
}

func TestDecodeProcHexAddr(t *testing.T) {
	// 127.0.0.1:8888 in /proc/net/tcp hex form.
	addr, port, ok := decodeProcHexAddr("0100007F", "22B8", false)
	if !ok || addr != "127.0.0.1" || port != 8888 {
		t.Errorf("v4: %s:%d ok=%v", addr, port, ok)
	}

	// 0.0.0.0:3222
	addr, port, ok = decodeProcHexAddr("00000000", "0C96", false)
	if !ok || addr != "0.0.0.0" || port != 3222 {
		t.Errorf("v4 any: %s:%d ok=%v", addr, port, ok)
	}

	// IPv6 loopback ::1 (word0 little-endian 0x00000001, rest 0).
	addr, _, ok = decodeProcHexAddr("00000000000000000000000001000000", "1F90", true)
	if !ok || addr != "0:0:0:0:0:0:0:1" {
		t.Errorf("v6 loopback: %s ok=%v", addr, ok)
	}
}

func TestParseProcNetLine(t *testing.T) {
	line := "   1: 0100007F:22B8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345678 1 0000000000000000 100 0 0 10 0"
	addr, port, state, inode, ok := parseProcNetLine(line, false)
	if !ok {
		t.Fatal("parse failed")
	}
	if addr != "127.0.0.1" || port != 8888 || state != "0A" || inode != 12345678 {
		t.Errorf("%s:%d st=%s inode=%d", addr, port, state, inode)
	}
}

func TestParseEtime(t *testing.T) {
	cases := map[string]int64{
		"05:30":      330,
		"01:05:30":   3930,
		"2-01:05:30": 176730,
		"42":         42,
		"":           0,
	}
	for in, want := range cases {
		if got := parseEtime(in); got != want {
			t.Errorf("parseEtime(%q)=%d, want %d", in, got, want)
		}
	}
}

func TestParsePsOutput(t *testing.T) {
	out := `  1     0 root     Ss   2-03:04:05 /sbin/init /sbin/init splash
  42    1 work     R+   00:01       toadpolicy ./bin/toadpolicy -f conf/x.conf
`
	procs := parsePsOutput(out)
	if len(procs) != 2 {
		t.Fatalf("procs=%d", len(procs))
	}
	if procs[0].PID != 1 || procs[0].User != "root" || procs[0].ElapsedSeconds != 2*86400+3*3600+4*60+5 {
		t.Errorf("p0: %+v", procs[0])
	}
	if procs[1].Cmdline != "/sbin/init splash" && procs[1].Cmdline != "./bin/toadpolicy -f conf/x.conf" {
		t.Errorf("p1 cmdline: %q", procs[1].Cmdline)
	}
}

func TestTruncateToLimit(t *testing.T) {
	s := "0123456789"
	if got := truncateToLimit(s, 4, false); got != "0123" {
		t.Errorf("head: %q", got)
	}
	if got := truncateToLimit(s, 4, true); got != "6789" {
		t.Errorf("tail: %q", got)
	}

	// UTF-8 boundary safety: 中 = 3 bytes.
	u := "ab中中中"
	got := truncateToLimit(u, 5, false)
	if len(got) > 5 {
		t.Errorf("head utf8 len=%d", len(got))
	}
	for _, r := range got {
		if r == 0xFFFD {
			t.Errorf("head utf8 produced replacement char: %q", got)
		}
	}
	got = truncateToLimit(u, 5, true)
	for _, r := range got {
		if r == 0xFFFD {
			t.Errorf("tail utf8 produced replacement char: %q", got)
		}
	}
}
