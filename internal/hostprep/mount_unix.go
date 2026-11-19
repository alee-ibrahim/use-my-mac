//go:build unix

package hostprep

import (
	"os"
	"path/filepath"
	"syscall"
)

// IsMountPoint reports whether dir is on a different device than its
// parent, i.e. a file system is mounted there.
func IsMountPoint(dir string) bool {
	a, err := os.Stat(dir)
	if err != nil {
		return false
	}
	b, err := os.Stat(filepath.Dir(filepath.Clean(dir)))
	if err != nil {
		return false
	}
	sa, ok1 := a.Sys().(*syscall.Stat_t)
	sb, ok2 := b.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && sa.Dev != sb.Dev
}

// Owner returns the uid and gid that own path (-1 if unknown).
func Owner(path string) (int, int) {
	fi, err := os.Stat(path)
	if err != nil {
		return -1, -1
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}
