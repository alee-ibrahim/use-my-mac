//go:build !unix

package runtimes

import (
	"context"
	"os"
)

// lockFile only creates the file outside Unix (agents run on macOS; this
// keeps tests building elsewhere). The in-process lock still applies.
func lockFile(_ context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return func() { f.Close() }, nil
}
