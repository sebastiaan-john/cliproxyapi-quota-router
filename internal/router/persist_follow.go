//go:build !(darwin || linux || freebsd)

package router

import (
	"io/fs"
	"os"
)

// openStateFile on platforms without an O_NOFOLLOW open flag in the syscall
// package (Windows among the release targets). The symlink check is a
// separate Lstat before the open, so a link planted between the two calls is
// followed. Everything after the open (type, permissions, size, bounded read)
// is still validated on the descriptor. This limitation is documented in
// docs/DESIGN.md.
func openStateFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, errStateSymlink
	}
	return os.Open(path)
}
