//go:build darwin || linux || freebsd

package router

import (
	"errors"
	"os"
	"syscall"
)

// openStateFile opens path read-only and refuses to follow a symbolic link at
// the final component, in the open call itself rather than in a prior check.
// Linux and macOS report that as ELOOP, FreeBSD as EMLINK. Both become
// errStateSymlink. O_NONBLOCK keeps the open from hanging on a FIFO planted
// at the path, so the descriptor's type check can refuse it; it has no effect
// on a regular file. Only the final component is protected; parent
// directories are resolved normally.
func openStateFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, errStateSymlink
		}
		return nil, err
	}
	return f, nil
}
