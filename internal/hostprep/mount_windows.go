//go:build windows

package hostprep

// IsMountPoint is not supported on Windows.
func IsMountPoint(string) bool { return false }

// Owner is not supported on Windows.
func Owner(string) (int, int) { return -1, -1 }
