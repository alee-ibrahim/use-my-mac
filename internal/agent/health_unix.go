//go:build unix

package agent

import (
	"syscall"

	"golang.org/x/sys/unix"
)

var errEDQUOT error = syscall.EDQUOT

// statfsDefault returns the free (available to unprivileged users) and
// total bytes of the file system holding path.
func statfsDefault(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize) //nolint:unconvert // int64 on linux, uint32 on darwin
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil
}

// canAccess reports whether this process may access path with mode (4 read,
// 2 write, 1 execute/search). It evaluates ACLs on macOS.
func canAccess(path string, mode uint32) bool {
	return unix.Access(path, mode) == nil
}
