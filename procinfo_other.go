//go:build !linux && !darwin

package main

import "errors"

// Process/port inspection is only implemented for Linux (/proc) and macOS
// (ps/lsof) so far.

func ListProcesses(req ListProcessesRequest) (*ListProcessesResult, error) {
	return nil, errors.New("remote_list_processes is not supported on this platform")
}

func CheckPort(req CheckPortRequest) (*CheckPortResult, error) {
	return nil, errors.New("remote_check_port is not supported on this platform")
}
