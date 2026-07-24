//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

// setProcAttr is a no-op on Windows (no process-group concept here).
func setProcAttr(cmd *exec.Cmd) {}

// killProcessGroup kills the process tree rooted at pid via taskkill.
func killProcessGroup(pid int) {
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}
