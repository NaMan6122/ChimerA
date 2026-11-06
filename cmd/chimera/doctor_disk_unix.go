//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// freeBytesFor walks up from dir to the nearest existing ancestor and reads
// that filesystem's free space for unprivileged writes (Bavail). statfs
// needs an existing path, and doctor must not create directories as a side
// effect — the gateway's own EnsureDirs does that at startup.
func freeBytesFor(dir string) (uint64, error) {
	path := dir
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			return 0, fmt.Errorf("no existing ancestor of %s", dir)
		}
		path = parent
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, err
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), nil
}
