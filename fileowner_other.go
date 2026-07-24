//go:build !linux && !darwin

package main

import "os"

// Platform stubs: no POSIX owner/group concept.

func ownerGroup(info os.FileInfo) (string, string) { return "", "" }

func ownerGroupByPath(path string) (string, string) { return "", "" }

func nlinkOf(info os.FileInfo) uint64 { return 0 }
