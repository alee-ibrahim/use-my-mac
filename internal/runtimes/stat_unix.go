//go:build unix

package runtimes

import (
	"os"
	"syscall"
)

func owner(p string) (int, bool) {
	fi, err := os.Lstat(p)
	if err != nil {
		return -1, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, false
	}
	return int(st.Uid), true
}

func openNoFollow(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
