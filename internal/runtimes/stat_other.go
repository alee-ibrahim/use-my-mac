//go:build !darwin && !linux

package runtimes

import "io/fs"

type fileID struct {
	size  int64
	mtime int64
}

// identity without ctime: platforms other than macOS and Linux only run
// the agent for development.
func identity(fi fs.FileInfo) fileID {
	return fileID{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
}
