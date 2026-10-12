package router

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// stateVersion is the on-disk schema. Any other version is refused, not migrated.
const stateVersion = 1

// maxStateFileBytes bounds what LoadState will read. 10000 entries are well
// under 2 MiB; anything larger is not a file this plugin wrote.
const maxStateFileBytes = 4 << 20

// stateFile is the persisted affinity table. It carries no quota data, no
// cooldowns, no auth IDs, no session IDs and no request content: only the
// opaque session hash the router already keys on, the SHA-256 of the auth ID,
// and when the binding expires.
type stateFile struct {
	Version  int            `json:"version"`
	SavedAt  time.Time      `json:"saved_at"`
	Bindings []stateBinding `json:"bindings"`
}

type stateBinding struct {
	Session string    `json:"session"`
	Auth    string    `json:"auth"`
	Expires time.Time `json:"expires"`
}

// StateStatus is the diagnostic view of persistence. It never includes the path.
type StateStatus struct {
	Enabled   bool      `json:"enabled"`
	Restored  int       `json:"restored,omitempty"`
	LastSave  time.Time `json:"last_save,omitzero"`
	LastError string    `json:"last_error,omitempty"`
}

// SaveState writes the live session bindings to state_file atomically. It is a
// no-op without a state_file or when nothing changed since the last save.
// Errors are recorded for diagnostics and returned without the file path.
func (r *Router) SaveState() error {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	// The destination is read under the lock so a save can never straddle a
	// configuration change: it either sees the old path entirely or the new.
	cfg := r.Config()
	if cfg.StateFile == "" {
		return nil
	}
	now := r.now()
	entries, gen := r.aff.snapshot(now)
	if r.stateSaved && gen == r.savedGen && cfg.StateFile == r.savedPath {
		return nil
	}
	err := writeStateFile(cfg.StateFile, stateFile{Version: stateVersion, SavedAt: now, Bindings: entries})
	if err != nil {
		r.state.LastError = "save: " + describeFileError(err)
		return errors.New(r.state.LastError)
	}
	r.stateSaved, r.savedGen, r.savedPath = true, gen, cfg.StateFile
	r.state.LastSave, r.state.LastError = now, ""
	return nil
}

// LoadState reads state_file and restores unexpired bindings for sessions that
// are not already bound. A missing file is not an error. A symlink, a file
// readable by others, an oversized file, a different schema version or
// malformed content is refused as a whole. SetConfig does this itself when
// state_file changes; calling it again only adds bindings not bound live.
func (r *Router) LoadState() error {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	return r.loadLocked(r.Config())
}

// loadLocked is LoadState with persistMu held and the configuration given.
func (r *Router) loadLocked(cfg Config) error {
	if cfg.StateFile == "" {
		return nil
	}
	now := r.now()
	r.state.Enabled = true
	st, err := readStateFile(cfg.StateFile)
	if err != nil {
		r.state.LastError = "load: " + describeFileError(err)
		return errors.New(r.state.LastError)
	}
	if st == nil {
		return nil
	}
	valid := make([]stateBinding, 0, len(st.Bindings))
	for _, b := range st.Bindings {
		if !isHex(b.Session, 32) || !isHex(b.Auth, 64) || b.Expires.IsZero() {
			continue
		}
		valid = append(valid, b)
	}
	r.state.Restored = r.aff.restore(valid, now)
	r.state.LastError = ""
	return nil
}

// State returns the persistence diagnostics.
func (r *Router) State() StateStatus {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	st := r.state
	st.Enabled = r.Config().StateFile != ""
	return st
}

var errStateSymlink = errors.New("state file is a symbolic link")
var errStatePerms = errors.New("state file is readable by other users")
var errStateTooLarge = errors.New("state file is larger than the plugin would write")
var errStateVersion = errors.New("state file schema version is not supported")

// readStateFile opens the state file without following a symlink at the final
// path component (openStateFile, platform-specific) and validates the opened
// descriptor rather than a prior Lstat, so the file cannot be swapped between
// check and read. The parent directories are not checked: a symlinked
// directory on the way to the file is followed like any other directory.
func readStateFile(path string) (*stateFile, error) {
	f, err := openStateFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readOpenStateFile(f)
}

// readOpenStateFile validates and parses an already open descriptor. Type,
// permissions and size come from the descriptor; the read is bounded so a
// file that grows after the size check is still refused.
func readOpenStateFile(f *os.File) (*stateFile, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("state file is not a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errStatePerms
	}
	if info.Size() > maxStateFileBytes {
		return nil, errStateTooLarge
	}
	raw, err := readBounded(f, maxStateFileBytes)
	if err != nil {
		return nil, err
	}
	var st stateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("state file is not valid JSON")
	}
	if st.Version != stateVersion {
		return nil, errStateVersion
	}
	return &st, nil
}

// readBounded reads at most limit bytes and fails if the source holds more.
// It never falls back to an unbounded read.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errStateTooLarge
	}
	return raw, nil
}

// writeStateFile writes to a private temporary file in the same directory and
// renames it over path. Rename replaces a symlink at path with the new file
// rather than following it; an existing symlink is refused up front anyway.
func writeStateFile(path string, st stateFile) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return errStateSymlink
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".quota-router-state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// describeFileError strips file paths from OS errors so diagnostics never show
// where the state file lives.
func describeFileError(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Op + ": " + pathErr.Err.Error()
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Op + ": " + linkErr.Err.Error()
	}
	return err.Error()
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
