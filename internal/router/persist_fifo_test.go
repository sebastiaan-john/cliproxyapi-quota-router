//go:build darwin || linux || freebsd

package router

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Review finding: a blocking open of a FIFO never reaches the descriptor
// checks. The open must not block, and the FIFO must then be refused as not a
// regular file. If the open does block, the deferred writer below unblocks it
// so the failing run does not leak a stuck goroutine.
func TestStateFileRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readStateFile(path)
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		// Connect a non-blocking writer so the blocked reader returns.
		if w, werr := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); werr == nil {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
			_ = w.Close()
		}
		t.Fatal("opening a FIFO blocked instead of being refused")
	}
	if err == nil || !strings.Contains(err.Error(), "not a regular file") || strings.Contains(err.Error(), dir) {
		t.Fatalf("FIFO: err=%v", err)
	}
}
