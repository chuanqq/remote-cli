//go:build linux || darwin

package main

import (
	"os"
	"os/user"
	"strconv"
	"sync"
	"syscall"
)

// idCache memoizes UID/GID -> name lookups (list_dir of large directories
// would otherwise hammer user.LookupId).
var idCache sync.Map // "u:1000" / "g:1000" -> name string

func lookupUID(uid uint32) string {
	key := "u:" + strconv.FormatUint(uint64(uid), 10)
	if v, ok := idCache.Load(key); ok {
		return v.(string)
	}
	name := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(name); err == nil && u.Username != "" {
		name = u.Username
	}
	idCache.Store(key, name)
	return name
}

func lookupGID(gid uint32) string {
	key := "g:" + strconv.FormatUint(uint64(gid), 10)
	if v, ok := idCache.Load(key); ok {
		return v.(string)
	}
	name := strconv.FormatUint(uint64(gid), 10)
	if g, err := user.LookupGroupId(name); err == nil && g.Name != "" {
		name = g.Name
	}
	idCache.Store(key, name)
	return name
}

// ownerGroup resolves owner and group names for a file. Returns empty
// strings when the underlying stat data is unavailable.
func ownerGroup(info os.FileInfo) (string, string) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ""
	}
	return lookupUID(st.Uid), lookupGID(st.Gid)
}

// ownerGroupByPath is ownerGroup for paths (e.g. /proc/<pid> dirs).
func ownerGroupByPath(path string) (string, string) {
	info, err := os.Stat(path)
	if err != nil {
		return "", ""
	}
	return ownerGroup(info)
}

// nlinkOf returns the hard-link count, 0 when unavailable.
func nlinkOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 0
}
