//go:build !unix

package selfupdate

import "os"

func checkRootOwned(string, os.FileInfo) error { return nil }

func chownRoot(string) {}

// Lock is a no-op outside Unix (the updater only runs on macOS).
func Lock(dir string) (func(), error) {
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	return func() {}, nil
}
