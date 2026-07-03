//go:build !darwin

package main

import (
	"os"
	"strconv"
	"strings"
)

func getLoadAverage() []float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return []float64{0, 0, 0}
	}
	parts := strings.Fields(string(data))
	if len(parts) < 3 {
		return []float64{0, 0, 0}
	}
	load := make([]float64, 3)
	for i := 0; i < 3; i++ {
		load[i], _ = strconv.ParseFloat(parts[i], 64)
	}
	return load
}

func getMemoryMB() uint64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseUint(fields[1], 10, 64)
				return kb / 1024
			}
		}
	}
	return 0
}
