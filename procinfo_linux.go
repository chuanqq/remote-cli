//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// clockTicks caches sysconf(_SC_CLK_TCK); 100 is the right fallback on
// virtually all Linux configurations.
var (
	clockTicksOnce sync.Once
	clockTicks     = int64(100)
)

func hz() int64 {
	clockTicksOnce.Do(func() {
		out, err := exec.Command("getconf", "CLK_TCK").Output()
		if err == nil {
			if v, parseErr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); parseErr == nil && v > 0 {
				clockTicks = v
			}
		}
	})
	return clockTicks
}

// bootTime reads btime from /proc/stat (unix seconds).
func bootTime() int64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "btime ") {
			v, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
			return v
		}
	}
	return 0
}

// ListProcesses enumerates processes from /proc. Processes that vanish or
// are unreadable mid-scan are skipped silently.
func ListProcesses(req ListProcessesRequest) (*ListProcessesResult, error) {
	var filter *regexp.Regexp
	if req.Filter != "" {
		re, err := regexp.Compile(req.Filter)
		if err != nil {
			return nil, err
		}
		filter = re
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	boot := bootTime()
	ticks := hz()
	now := time.Now()

	result := &ListProcessesResult{Processes: []ProcessInfo{}}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		pidDir := filepath.Join("/proc", e.Name())

		statData, err := os.ReadFile(filepath.Join(pidDir, "stat"))
		if err != nil {
			continue
		}
		pid, comm, state, ppid, startTicks, ok := parseProcStatLine(string(statData))
		if !ok {
			continue
		}

		owner, _ := ownerGroupByPath(pidDir)
		if req.User != "" && owner != req.User {
			continue
		}

		p := ProcessInfo{
			PID:   pid,
			PPID:  ppid,
			User:  owner,
			State: state,
			Cmd:   comm,
		}

		if boot > 0 && ticks > 0 {
			startUnix := boot + int64(startTicks)/ticks
			p.StartTime = time.Unix(startUnix, 0).Format(time.RFC3339)
			p.ElapsedSeconds = int64(now.Sub(time.Unix(startUnix, 0)).Seconds())
			if p.ElapsedSeconds < 0 {
				p.ElapsedSeconds = 0
			}
		}

		if cmdline, err := os.ReadFile(filepath.Join(pidDir, "cmdline")); err == nil {
			joined := strings.TrimRight(strings.ReplaceAll(string(cmdline), "\x00", " "), " ")
			p.Cmdline = truncateString(joined, 500)
		}

		if filter != nil {
			haystack := p.Cmdline
			if haystack == "" {
				haystack = p.Cmd
			}
			if !filter.MatchString(haystack) {
				continue
			}
		}

		result.Processes = append(result.Processes, p)
		if len(result.Processes) >= maxProcessList {
			result.Truncated = true
			break
		}
	}

	sort.Slice(result.Processes, func(i, j int) bool { return result.Processes[i].PID < result.Processes[j].PID })
	result.Count = len(result.Processes)
	return result, nil
}

// CheckPort lists listening sockets from /proc/net/{tcp,tcp6,udp,udp6} and
// best-effort maps them to PIDs by scanning /proc/<pid>/fd symlinks.
func CheckPort(req CheckPortRequest) (*CheckPortResult, error) {
	type rawListener struct {
		proto string
		addr  string
		port  int
		inode uint64
	}

	var raw []rawListener
	collect := func(path, proto string, v6, onlyListen bool) {
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			addr, port, state, inode, ok := parseProcNetLine(line, v6)
			if !ok {
				continue
			}
			// 0A = LISTEN for TCP; UDP has no listen state, all rows are bound.
			if onlyListen && state != "0A" {
				continue
			}
			raw = append(raw, rawListener{proto: proto, addr: addr, port: port, inode: inode})
		}
	}
	collect("/proc/net/tcp", "tcp", false, true)
	collect("/proc/net/tcp6", "tcp6", true, true)
	collect("/proc/net/udp", "udp", false, false)
	collect("/proc/net/udp6", "udp6", true, false)

	// Map socket inodes to PIDs (best effort: same-user processes only).
	inodeSet := make(map[uint64]bool, len(raw))
	for _, l := range raw {
		inodeSet[l.inode] = true
	}
	inodePID := mapSocketInodes(inodeSet)

	result := &CheckPortResult{Listeners: []PortListener{}}
	commCache := map[int]string{}
	for _, l := range raw {
		if req.Port > 0 && l.port != req.Port {
			continue
		}
		listener := PortListener{Proto: l.proto, Address: l.addr, Port: l.port}
		if pid, ok := inodePID[l.inode]; ok {
			listener.PID = pid
			name, cached := commCache[pid]
			if !cached {
				if data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm")); err == nil {
					name = strings.TrimSpace(string(data))
				}
				commCache[pid] = name
			}
			listener.Process = name
		}
		if req.ProcessName != "" &&
			!strings.Contains(strings.ToLower(listener.Process), strings.ToLower(req.ProcessName)) {
			continue
		}
		result.Listeners = append(result.Listeners, listener)
	}

	sort.Slice(result.Listeners, func(i, j int) bool { return result.Listeners[i].Port < result.Listeners[j].Port })
	result.Count = len(result.Listeners)
	return result, nil
}

// mapSocketInodes scans /proc/<pid>/fd for "socket:[inode]" links and
// returns inode -> pid for the inodes of interest. Stops early once all
// inodes are resolved.
func mapSocketInodes(want map[uint64]bool) map[uint64]int {
	found := make(map[uint64]int)
	if len(want) == 0 {
		return found
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return found
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // not our process / no permission
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 64)
			if err != nil {
				continue
			}
			if want[inode] {
				found[inode] = pid
				if len(found) == len(want) {
					return found
				}
			}
		}
	}
	return found
}
