package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Persisted affinity: session bindings survive a quick proxy restart so the
// prompt caches built on each account are not thrown away. The file holds the
// opaque session hash, a full SHA-256 of the auth ID and an expiry; nothing
// else. Fixture IDs below are shaped like real auth file names.
const (
	persistAuthA = "claude-alice@example.com.json"
	persistAuthB = "claude-bob@example.com.json"
	persistSess  = "s-restore-fixture-0001"
)

func stateRouter(t *testing.T, path string) (*Router, *fakeClock) {
	t.Helper()
	r, clock := newTestRouter()
	cfg := DefaultConfig()
	cfg.StateFile = path
	r.SetConfig(cfg)
	return r, clock
}

func restoreReq() pluginapi.SchedulerPickRequest {
	req := claudePick(claudeCandidates(persistAuthA, persistAuthB)...)
	req.Options.Metadata = map[string]any{"canonical_session_id": persistSess}
	return req
}

func TestStateFileRoundTripRestoresBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	r1, clock := stateRouter(t, path)
	observeClaude(r1, persistAuthA, claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	observeClaude(r1, persistAuthB, claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
	resp, d := r1.Pick(restoreReq())
	mustSelect(t, resp, d, persistAuthA)
	if err := r1.SaveState(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %o, want 600", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	for _, private := range []string{"alice", "bob", "example.com", persistSess, persistAuthA} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("state file contains %q: %s", private, raw)
		}
	}

	// A fresh router, as after a restart, with b now far better than a.
	r2, _ := stateRouter(t, path)
	if err := r2.LoadState(); err != nil {
		t.Fatal(err)
	}
	observeClaude(r2, persistAuthA, claudeHeaders(clock.Now(), &cwin{0.9, 4 * time.Hour}, &cwin{0.9, 3 * day}))
	observeClaude(r2, persistAuthB, claudeHeaders(clock.Now(), &cwin{0.0, 4 * time.Hour}, &cwin{0.0, 3 * day}))
	resp, d = r2.Pick(restoreReq())
	mustSelect(t, resp, d, persistAuthA)
	if !strings.Contains(d.Reason, "session affinity") {
		t.Fatalf("reason = %q", d.Reason)
	}
	// The pick resolved the digest back to a live ID; a second save still holds one entry.
	if err := r2.SaveState(); err != nil {
		t.Fatal(err)
	}
	if r2.aff.size() != 1 {
		t.Fatalf("bindings = %d, want 1", r2.aff.size())
	}
}

// writeBinding persists one binding for persistSess to the given auth digest.
func writeBinding(t *testing.T, path, digest string, expires time.Time) {
	t.Helper()
	st := stateFile{Version: stateVersion, SavedAt: t0, Bindings: []stateBinding{
		{Session: bindingKey(sessionKey(nil, map[string]any{"canonical_session_id": persistSess}), restoreReq().Model), Auth: digest, Expires: expires},
	}}
	if err := writeStateFile(path, st); err != nil {
		t.Fatal(err)
	}
}

func TestStateFileExpiredEntriesAreDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	r, clock := stateRouter(t, path)
	writeBinding(t, path, authDigest(persistAuthA), clock.Now().Add(-time.Second))
	if err := r.LoadState(); err != nil {
		t.Fatal(err)
	}
	if r.aff.size() != 0 || r.State().Restored != 0 {
		t.Fatalf("expired binding restored: size=%d state=%+v", r.aff.size(), r.State())
	}
}

// A restored binding whose account is no longer offered (removed, disabled or
// cooling down at the host) is simply unmatched: the session is ranked afresh
// and rebound to a live account.
func TestStateFileRemovedAccountIsNotForced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	r, clock := stateRouter(t, path)
	writeBinding(t, path, authDigest("claude-gone@example.com.json"), clock.Now().Add(time.Hour))
	if err := r.LoadState(); err != nil {
		t.Fatal(err)
	}
	observeClaude(r, persistAuthA, claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	observeClaude(r, persistAuthB, claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
	resp, d := r.Pick(restoreReq())
	mustSelect(t, resp, d, persistAuthA)
	if d.Reason == "session affinity" {
		t.Fatal("unmatched restored binding reported as affinity")
	}
	// Now bound live to a; a later pick is affinity.
	if _, d := r.Pick(restoreReq()); d.Reason != "session affinity" {
		t.Fatalf("reason = %q", d.Reason)
	}
}

// A restored binding to an account the plugin itself has blocked is not honoured.
func TestStateFileBlockedAccountIsNotForced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	r, clock := stateRouter(t, path)
	writeBinding(t, path, authDigest(persistAuthA), clock.Now().Add(time.Hour))
	if err := r.LoadState(); err != nil {
		t.Fatal(err)
	}
	observeClaude(r, persistAuthA, claudeHeaders(clock.Now(), &cwin{1.0, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	observeClaude(r, persistAuthB, claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
	resp, d := r.Pick(restoreReq())
	mustSelect(t, resp, d, persistAuthB)
}

func TestStateFileRefusesBadFiles(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"corrupt json": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"wrong version": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"version":99,"bindings":[]}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, path string) {
			target := filepath.Join(filepath.Dir(path), "target.json")
			if err := os.WriteFile(target, []byte(`{"version":1,"bindings":[]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Skip("symlinks unsupported:", err)
			}
		},
		"world readable": func(t *testing.T, path string) {
			if runtime.GOOS == "windows" {
				// Unix permission bits are not enforced on Windows and chmod 0600
				// does not create an owner-only ACL; the plugin does not claim it.
				t.Skip("owner-only permission check is Unix-only")
			}
			if err := os.WriteFile(path, []byte(`{"version":1,"bindings":[]}`), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			setup(t, path)
			r, _ := stateRouter(t, path)
			err := r.LoadState()
			if err == nil {
				t.Fatal("bad state file accepted")
			}
			if strings.Contains(err.Error(), dir) || strings.Contains(r.State().LastError, dir) {
				t.Fatalf("error leaks the path: %v / %+v", err, r.State())
			}
			if r.aff.size() != 0 {
				t.Fatal("bindings restored from a refused file")
			}
			// Saving over a symlink is refused as well; other bad files are replaced.
			if saveErr := r.SaveState(); name == "symlink" && saveErr == nil {
				t.Fatal("saved through a symlink")
			}
		})
	}
}

// Entries with malformed fields are skipped individually; the rest load.
func TestStateFileSkipsMalformedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	r, clock := stateRouter(t, path)
	good := sessionKey(nil, map[string]any{"canonical_session_id": persistSess})
	st := stateFile{Version: stateVersion, Bindings: []stateBinding{
		{Session: good, Auth: authDigest(persistAuthA), Expires: clock.Now().Add(time.Hour)},
		{Session: "not-hex", Auth: authDigest(persistAuthA), Expires: clock.Now().Add(time.Hour)},
		{Session: good + "00", Auth: "short", Expires: clock.Now().Add(time.Hour)},
		{Session: good, Auth: authDigest(persistAuthB)}, // zero expiry
	}}
	if err := writeStateFile(path, st); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadState(); err != nil {
		t.Fatal(err)
	}
	if r.aff.size() != 1 || r.State().Restored != 1 {
		t.Fatalf("size=%d state=%+v", r.aff.size(), r.State())
	}
}

func TestStateFileSaveSkipsWhenUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	r, clock := stateRouter(t, path)
	observeClaude(r, persistAuthA, claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	r.Pick(restoreReq())
	if err := r.SaveState(); err != nil {
		t.Fatal(err)
	}
	first, _ := os.Stat(path)
	// Make any rewrite visible regardless of filesystem timestamp resolution.
	if err := os.WriteFile(path, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveState(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "sentinel" {
		t.Fatal("unchanged bindings were rewritten")
	}
	_ = first
	// A new binding does trigger a write.
	clock.Advance(time.Second)
	req := restoreReq()
	req.Options.Metadata["canonical_session_id"] = "s-other"
	r.Pick(req)
	if err := r.SaveState(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) == "sentinel" {
		t.Fatal("changed bindings were not written")
	}
}

func TestStateFileDisabledByDefault(t *testing.T) {
	r, clock := newTestRouter()
	observeClaude(r, persistAuthA, claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	r.Pick(restoreReq())
	if err := r.SaveState(); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadState(); err != nil {
		t.Fatal(err)
	}
	if st := r.State(); st.Enabled || !st.LastSave.IsZero() {
		t.Fatalf("state = %+v", st)
	}
}

func TestStateFileConfigRequiresAbsolutePath(t *testing.T) {
	if _, err := ParseConfig([]byte("state_file: relative/state.json")); err == nil {
		t.Fatal("relative state_file accepted")
	}
	cfg, err := ParseConfig([]byte("state_file: " + filepath.Join(string(filepath.Separator), "var", "lib", "x", "..", "state.json")))
	if err != nil || cfg.StateFile != filepath.Join(string(filepath.Separator), "var", "lib", "state.json") {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if cfg, _ := ParseConfig([]byte("state_file: \"  \"")); cfg.StateFile != "" {
		t.Fatalf("blank state_file = %q", cfg.StateFile)
	}
}

// Status reports that persistence is on, and nothing about where the file is.
func TestStateFileStatusDoesNotLeakPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	r, _ := stateRouter(t, path)
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = r.LoadState()
	st := r.Status()
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), dir) || strings.Contains(string(raw), "state.json") {
		t.Fatalf("status leaks the state file path: %s", raw)
	}
	if !st.Config.StateFileEnabled || !st.State.Enabled || st.State.LastError == "" {
		t.Fatalf("status = %+v", st.State)
	}
}

// Picks, refreshes and saves run concurrently in the host. The race detector
// covers the locking; this just drives them together.
func TestStateFileConcurrentSaveAndBind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	r, clock := stateRouter(t, path)
	observeClaude(r, persistAuthA, claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			req := restoreReq()
			req.Options.Metadata["canonical_session_id"] = fmt.Sprintf("s-%d", i)
			for range 20 {
				r.Pick(req)
			}
		}()
		go func() {
			defer wg.Done()
			for range 20 {
				_ = r.SaveState()
			}
		}()
	}
	wg.Wait()
	if err := r.SaveState(); err != nil {
		t.Fatal(err)
	}
	r2, _ := stateRouter(t, path)
	if err := r2.LoadState(); err != nil {
		t.Fatal(err)
	}
	if r2.aff.size() != 8 {
		t.Fatalf("restored %d bindings, want 8", r2.aff.size())
	}
}

// Review finding: the "nothing changed" check must be tied to the destination.
// Saving to A and then reconfiguring to B with no new bindings must still
// write B.
func TestStateFileSaveFollowsReconfiguredPath(t *testing.T) {
	dir := t.TempDir()
	pathA, pathB := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	r, clock := stateRouter(t, pathA)
	observeClaude(r, persistAuthA, claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	r.Pick(restoreReq())
	if err := r.SaveState(); err != nil {
		t.Fatal(err)
	}
	cfg := r.Config()
	cfg.StateFile = pathB
	r.SetConfig(cfg)
	if err := r.SaveState(); err != nil {
		t.Fatal(err)
	}
	st, err := readStateFile(pathB)
	if err != nil || st == nil || len(st.Bindings) != 1 {
		t.Fatalf("B after reconfigure: st=%+v err=%v", st, err)
	}
	// Back to A with no changes: A is current already and is left alone.
	if err := os.WriteFile(pathA, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.StateFile = pathA
	r.SetConfig(cfg)
	if err := r.SaveState(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(pathA); string(raw) == "sentinel" {
		t.Fatal("switching back to A did not rewrite it")
	}
}

// Review finding: a reconfigure that enables or moves state_file while the
// refresher runs must restore the file's bindings before anything saves to
// it, stop the old refresher under the old config, and restart it after.
func TestReconfigureRestoresBeforeFirstSave(t *testing.T) {
	pathB := filepath.Join(t.TempDir(), "b.json")
	lister := &fakeLister{entries: []pluginapi.HostAuthFileEntry{{ID: persistAuthA}, {ID: persistAuthB}}}
	r, clock := newTestRouter(WithAuthLister(lister))
	// B already holds a binding from a previous run.
	writeBinding(t, pathB, authDigest(persistAuthB), clock.Now().Add(time.Hour))

	cfg := DefaultConfig()
	cfg.RefreshInterval = 10 * time.Second
	r.SetConfig(cfg)
	r.StartRefresher()
	defer r.StopRefresher()
	deadline := time.Now().Add(2 * time.Second)
	for lister.Calls() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// A live session, bound while no state file is configured.
	observeClaude(r, persistAuthA, claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	live := restoreReq()
	live.Options.Metadata["canonical_session_id"] = "s-live"
	r.Pick(live)

	calls := lister.Calls()
	cfg.StateFile = pathB
	cfg.RefreshInterval = 20 * time.Second
	r.SetConfig(cfg)

	// Restoration is part of the transition, so it is visible immediately.
	if r.aff.size() != 2 || r.State().Restored != 1 {
		t.Fatalf("after reconfigure: bindings=%d state=%+v", r.aff.size(), r.State())
	}
	if !r.refreshing() {
		t.Fatal("refresher not running after reconfigure")
	}
	// The restarted refresher runs once immediately and saves the merged table.
	deadline = time.Now().Add(2 * time.Second)
	for (lister.Calls() <= calls || r.State().LastSave.IsZero()) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if lister.Calls() <= calls {
		t.Fatal("restarted refresher never ran")
	}
	st, err := readStateFile(pathB)
	if err != nil || st == nil {
		t.Fatalf("B: st=%+v err=%v", st, err)
	}
	if len(st.Bindings) != 2 {
		t.Fatalf("B holds %d bindings, want the restored one and the live one: %+v", len(st.Bindings), st.Bindings)
	}
}

// Review finding: the old Lstat-then-ReadFile had a window in which the path
// could be swapped or the file grown. Validation now happens on the open
// descriptor and the read is bounded.
func TestStateFileReadIsBounded(t *testing.T) {
	exact := strings.Repeat("x", maxStateFileBytes)
	if raw, err := readBounded(strings.NewReader(exact), maxStateFileBytes); err != nil || len(raw) != maxStateFileBytes {
		t.Fatalf("exact size: len=%d err=%v", len(raw), err)
	}
	if _, err := readBounded(strings.NewReader(exact+"x"), maxStateFileBytes); !errors.Is(err, errStateTooLarge) {
		t.Fatalf("one byte over: err=%v", err)
	}
}

func TestStateFileGrowthAfterOpenIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"bindings":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	// The file grows past the cap after it was opened but before it is read.
	w, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, maxStateFileBytes+1)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readOpenStateFile(f); !errors.Is(err, errStateTooLarge) {
		t.Fatalf("grown file: err=%v", err)
	}
}

func TestStateFileOpenRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"bindings":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	if _, err := openStateFile(path); !errors.Is(err, errStateSymlink) {
		t.Fatalf("open through symlink: err=%v", err)
	}
	if _, err := readStateFile(path); !errors.Is(err, errStateSymlink) || strings.Contains(err.Error(), dir) {
		t.Fatalf("read through symlink: err=%v", err)
	}
}

// Permissions are checked on the opened descriptor, not on a prior Lstat.
func TestStateFileDescriptorPermissionCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on windows")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"bindings":[]}`), 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := openStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := readOpenStateFile(f); !errors.Is(err, errStatePerms) {
		t.Fatalf("group-readable: err=%v", err)
	}
}

// The RPC lifecycle loads on register and saves on quiesce.
func TestStateFileLifecycleOverRPC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	yaml := "state_file: " + path + "\n"
	h := &Handler{Router: New()}
	if env := call(t, h, "plugin.register", lifecycle(yaml)); !env.OK {
		t.Fatalf("register: %+v", env.Error)
	}
	observeClaude(h.Router, persistAuthA, claudeHeaders(time.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	h.Router.Pick(restoreReq())
	if env := call(t, h, "plugin.quiesce", nil); !env.OK {
		t.Fatalf("quiesce: %+v", env.Error)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("quiesce did not save: %v", err)
	}

	h2 := &Handler{Router: New()}
	if env := call(t, h2, "plugin.register", lifecycle(yaml)); !env.OK {
		t.Fatalf("register: %+v", env.Error)
	}
	if h2.Router.aff.size() != 1 || h2.Router.State().Restored != 1 {
		t.Fatalf("register did not restore: size=%d state=%+v", h2.Router.aff.size(), h2.Router.State())
	}
}
