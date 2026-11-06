//go:build windows

package main

import "errors"

// freeBytesFor is not implemented for windows yet: syscall.Statfs has no
// windows equivalent and pulling in a syscall dep for a warning-level check
// is not worth it. checkDiskSpace skips the check on windows instead.
func freeBytesFor(dir string) (uint64, error) {
	return 0, errors.New("disk space check is not supported on windows yet")
}
