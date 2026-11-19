//go:build windows

package agent

import "errors"

var errEDQUOT = errors.New("disk quota exceeded")

func statfsDefault(string) (uint64, uint64, error) { return 0, 0, errors.ErrUnsupported }

func canAccess(string, uint32) bool { return false }
