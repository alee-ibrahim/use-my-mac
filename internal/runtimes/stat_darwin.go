//go:build darwin

package runtimes

import (
	"io/fs"
	"syscall"
)

type fileID struct {
	dev, ino     uint64
	size         int64
	mtime, ctime int64
}

func identity(fi fs.FileInfo) fileID {
	id := fileID{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		id.dev, id.ino = uint64(st.Dev), st.Ino
		id.ctime = st.Ctimespec.Nano()
	}
	return id
}
