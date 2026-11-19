//go:build !darwin

package hostprep

// BootTime returns 0 off macOS: pf rules are only used on macOS.
func BootTime() int64 { return 0 }
