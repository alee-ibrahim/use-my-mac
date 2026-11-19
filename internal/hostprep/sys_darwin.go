//go:build darwin

package hostprep

import "golang.org/x/sys/unix"

// BootTime returns kern.boottime in Unix seconds (0 if unknown).
func BootTime() int64 {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return 0
	}
	return tv.Sec
}
