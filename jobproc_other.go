//go:build !linux && !darwin

package main

// processAlive cannot be checked portably here; recovered jobs are treated as
// ended with an unknown state instead of being guessed.
func processAlive(pid int) bool { return false }

func processCommandLine(pid int) string { return "" }
