//go:build darwin

package main

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ListProcesses wraps `ps` on macOS (no /proc available).
func ListProcesses(req ListProcessesRequest) (*ListProcessesResult, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,user=,stat=,etime=,comm=,args=").Output()
	if err != nil {
		return nil, err
	}
	procs := parsePsOutput(string(out))

	var filtered []ProcessInfo
	for _, p := range procs {
		if req.User != "" && p.User != req.User {
			continue
		}
		if req.Filter != "" {
			haystack := p.Cmdline
			if haystack == "" {
				haystack = p.Cmd
			}
			matched, err := regexp.MatchString(req.Filter, haystack)
			if err != nil {
				return nil, err
			}
			if !matched {
				continue
			}
		}
		filtered = append(filtered, p)
	}
	truncated := false
	if len(filtered) > maxProcessList {
		filtered = filtered[:maxProcessList]
		truncated = true
	}
	if filtered == nil {
		filtered = []ProcessInfo{}
	}
	return &ListProcessesResult{Processes: filtered, Count: len(filtered), Truncated: truncated}, nil
}

// CheckPort wraps `lsof` on macOS.
func CheckPort(req CheckPortRequest) (*CheckPortResult, error) {
	out, err := exec.Command("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-iUDP").Output()
	if err != nil && len(out) == 0 {
		return nil, err
	}

	result := &CheckPortResult{Listeners: []PortListener{}}
	for i, line := range strings.Split(string(out), "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue // header
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		name := fields[len(fields)-1] // e.g. 127.0.0.1:8888 or *:3222
		proto := strings.ToLower(fields[7])
		if proto != "tcp" && proto != "udp" {
			continue
		}
		idx := strings.LastIndex(name, ":")
		if idx < 0 {
			continue
		}
		port, err := strconv.Atoi(name[idx+1:])
		if err != nil {
			continue
		}
		if req.Port > 0 && port != req.Port {
			continue
		}
		if req.ProcessName != "" &&
			!strings.Contains(strings.ToLower(fields[0]), strings.ToLower(req.ProcessName)) {
			continue
		}
		pid, _ := strconv.Atoi(fields[1])
		addr := name[:idx]
		if addr == "*" {
			addr = "0.0.0.0"
		}
		result.Listeners = append(result.Listeners, PortListener{
			Proto:   proto,
			Address: addr,
			Port:    port,
			PID:     pid,
			Process: fields[0],
		})
	}
	result.Count = len(result.Listeners)
	return result, nil
}
