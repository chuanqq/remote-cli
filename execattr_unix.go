//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// setProcAttr puts the child process in its own process group so that
// killProcessGroup can terminate the whole tree on timeout/cancel.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the entire process group rooted at pid.
func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
