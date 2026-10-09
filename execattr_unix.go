//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcAttr puts the child process in its own process group so that
// killProcessGroup can terminate the whole tree on timeout/cancel.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// setDetachedAttr starts the child in a new session (setsid): it has no
// controlling terminal, survives this server exiting, and leads its own
// process group, so killProcessGroup(pid) still reaches the whole job tree.
func setDetachedAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// killProcessGroup SIGKILLs the entire process group rooted at pid.
func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// signalProcessGroup sends sig to the process group rooted at pid.
func signalProcessGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

// pidAlive reports whether a process with this pid exists (signal 0 probe).
// EPERM means it exists but belongs to another user.
func pidAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, errors.New("invalid pid")
	}
	err := syscall.Kill(pid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}
