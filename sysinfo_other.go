//go:build !linux && !darwin

package main

// Platform stubs for systems without a /proc or sysctl implementation
// (e.g. Windows). They report zero values so status endpoints keep working.

func getLoadAverage() []float64 { return []float64{0, 0, 0} }

func getMemoryMB() uint64 { return 0 }
