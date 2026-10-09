//go:build windows

package main

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// setProcAttr is a no-op on Windows (no process-group concept here).
func setProcAttr(cmd *exec.Cmd) {}

// setDetachedAttr is a no-op on Windows; spawned jobs are killed via taskkill /T.
func setDetachedAttr(cmd *exec.Cmd) {}

// killProcessGroup kills the process tree rooted at pid via taskkill.
func killProcessGroup(pid int) {
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}

// signalProcessGroup has no graceful variant on Windows: any signal kills the tree.
func signalProcessGroup(pid int, _ syscall.Signal) error {
	return exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}

// pidAlive asks tasklist whether pid exists.
func pidAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, errors.New("invalid pid")
	}
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").Output()
	if err != nil {
		return false, err
	}
	return strings.Contains(string(out), " "+strconv.Itoa(pid)+" "), nil
}
