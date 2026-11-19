//go:build windows

package runtimes

import "os"

func owner(string) (int, bool) { return -1, false }

func openNoFollow(p string) (*os.File, error) { return os.Open(p) }
