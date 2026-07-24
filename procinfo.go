package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Shared types (data collection is per-platform: procinfo_{linux,darwin,other}.go)
// ---------------------------------------------------------------------------

type ProcessInfo struct {
	PID            int    `json:"pid"`
	PPID           int    `json:"ppid"`
	User           string `json:"user"`
	State          string `json:"state"`
	ElapsedSeconds int64  `json:"elapsed_seconds"`
	StartTime      string `json:"start_time,omitempty"` // RFC3339, Linux only
	Cmd            string `json:"cmd"`
	Cmdline        string `json:"cmdline,omitempty"`
}

type ListProcessesRequest struct {
	Filter string // RE2 matched against cmdline (falls back to comm)
	User   string // exact username match
}

type ListProcessesResult struct {
	Processes []ProcessInfo `json:"processes"`
	Count     int           `json:"count"`
	Truncated bool          `json:"truncated"`
}

const maxProcessList = 1000

type PortListener struct {
	Proto   string `json:"proto"` // "tcp" | "tcp6" | "udp" | "udp6"
	Address string `json:"address"`
	Port    int    `json:"port"`
	PID     int    `json:"pid,omitempty"`
	Process string `json:"process,omitempty"`
}

type CheckPortRequest struct {
	Port        int    // 0 = all listening ports
	ProcessName string // case-insensitive substring match on process name
}

type CheckPortResult struct {
	Listeners []PortListener `json:"listeners"`
	Count     int            `json:"count"`
}

// ---------------------------------------------------------------------------
// Parsers (pure functions, unit-tested)
// ---------------------------------------------------------------------------

// parseProcStatLine parses /proc/<pid>/stat. The comm field may contain
// spaces and parentheses, so we split after the LAST ')'.
// Layout after comm: state ppid pgrp session tty_nr tpgid flags minflt
// cminflt majflt cmajflt utime stime cutime cstime priority nice num_threads
// itrealvalue starttime ...
func parseProcStatLine(line string) (pid int, comm, state string, ppid int, startTimeTicks uint64, ok bool) {
	open := strings.Index(line, "(")
	close := strings.LastIndex(line, ")")
	if open < 0 || close < 0 || close < open {
		return 0, "", "", 0, 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line[:open]))
	if err != nil {
		return 0, "", "", 0, 0, false
	}
	comm = line[open+1 : close]
	fields := strings.Fields(line[close+1:])
	if len(fields) < 20 {
		return 0, "", "", 0, 0, false
	}
	state = fields[0]
	ppid, _ = strconv.Atoi(fields[1])
	startTimeTicks, _ = strconv.ParseUint(fields[19], 10, 64)
	return pid, comm, state, ppid, startTimeTicks, true
}

// decodeProcHexAddr converts the hex address found in /proc/net/{tcp,tcp6}
// into dotted/colon notation. IPv4 is a little-endian u32; IPv6 is four
// little-endian u32 words.
func decodeProcHexAddr(hexIP string, hexPort string, v6 bool) (string, int, bool) {
	port64, err := strconv.ParseUint(hexPort, 16, 32)
	if err != nil {
		return "", 0, false
	}
	if !v6 {
		if len(hexIP) != 8 {
			return "", 0, false
		}
		v, err := strconv.ParseUint(hexIP, 16, 32)
		if err != nil {
			return "", 0, false
		}
		ip := fmt.Sprintf("%d.%d.%d.%d", v&0xFF, (v>>8)&0xFF, (v>>16)&0xFF, (v>>24)&0xFF)
		return ip, int(port64), true
	}
	if len(hexIP) != 32 {
		return "", 0, false
	}
	var words [4]uint32
	for i := 0; i < 4; i++ {
		v, err := strconv.ParseUint(hexIP[i*8:(i+1)*8], 16, 32)
		if err != nil {
			return "", 0, false
		}
		words[i] = uint32(v)
	}
	// Each 32-bit word is stored little-endian; rebuild the 16 bytes.
	b := make([]byte, 16)
	for i, w := range words {
		b[i*4+0] = byte(w)
		b[i*4+1] = byte(w >> 8)
		b[i*4+2] = byte(w >> 16)
		b[i*4+3] = byte(w >> 24)
	}
	// Format as compressed IPv6.
	groups := make([]string, 8)
	for i := 0; i < 8; i++ {
		groups[i] = strconv.FormatUint(uint64(b[i*2])<<8|uint64(b[i*2+1]), 16)
	}
	return strings.Join(groups, ":"), int(port64), true
}

// parseProcNetLine parses one data line of /proc/net/{tcp,tcp6,udp,udp6}:
//
//	sl  local_address rem_address   st tx_queue:rx_queue tr:tm->when retrnsmt
//	uid  timeout inode
func parseProcNetLine(line string, v6 bool) (addr string, port int, state string, inode uint64, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 10 {
		return "", 0, "", 0, false
	}
	local := strings.SplitN(fields[1], ":", 2)
	if len(local) != 2 {
		return "", 0, "", 0, false
	}
	addr, port, ok = decodeProcHexAddr(local[0], local[1], v6)
	if !ok {
		return "", 0, "", 0, false
	}
	state = fields[3]
	inode, _ = strconv.ParseUint(fields[9], 10, 64)
	return addr, port, state, inode, true
}

// parseEtime converts ps(1) etime ([[dd-]hh:]mm:ss) to seconds.
func parseEtime(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var days int64
	if i := strings.Index(s, "-"); i >= 0 {
		days, _ = strconv.ParseInt(s[:i], 10, 64)
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var h, m, sec int64
	switch len(parts) {
	case 3:
		h, _ = strconv.ParseInt(parts[0], 10, 64)
		m, _ = strconv.ParseInt(parts[1], 10, 64)
		sec, _ = strconv.ParseInt(parts[2], 10, 64)
	case 2:
		m, _ = strconv.ParseInt(parts[0], 10, 64)
		sec, _ = strconv.ParseInt(parts[1], 10, 64)
	case 1:
		sec, _ = strconv.ParseInt(parts[0], 10, 64)
	}
	return days*86400 + h*3600 + m*60 + sec
}

// parsePsOutput parses `ps -axo pid=,ppid=,user=,stat=,etime=,comm=,args=`
// (header-less). comm has no spaces; args is the remainder.
func parsePsOutput(out string) []ProcessInfo {
	var procs []ProcessInfo
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		p := ProcessInfo{
			PID:            pid,
			PPID:           ppid,
			User:           fields[2],
			State:          fields[3],
			ElapsedSeconds: parseEtime(fields[4]),
			Cmd:            fields[5],
		}
		if len(fields) > 6 {
			p.Cmdline = truncateString(strings.Join(fields[6:], " "), 500)
		}
		procs = append(procs, p)
	}
	return procs
}

func truncateString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
