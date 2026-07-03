//go:build darwin

package main

/*
#include <stdlib.h>
#include <sys/sysctl.h>
#include <mach/mach.h>
*/
import "C"
import (
	"os/exec"
	"strconv"
	"strings"
	"unsafe"
)

func getLoadAverage() []float64 {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return []float64{0, 0, 0}
	}
	// output: "{ 1.23 4.56 7.89 }"
	s := strings.Trim(string(out), "{ }\n")
	parts := strings.Fields(s)
	load := make([]float64, 3)
	for i := 0; i < 3 && i < len(parts); i++ {
		load[i], _ = strconv.ParseFloat(parts[i], 64)
	}
	return load
}

func getMemoryMB() uint64 {
	var size C.uint64_t
	sizeLen := C.size_t(unsafe.Sizeof(size))
	name := C.CString("hw.memsize")
	defer C.free(unsafe.Pointer(name))
	C.sysctlbyname(name, unsafe.Pointer(&size), &sizeLen, nil, 0)
	return uint64(size) / 1024 / 1024
}
