package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The tests in this file restart the plugin for real: each step runs in a
// new OS process (this test binary re-executed into
// TestPersistHelperProcess), which registers through the RPC entry point with
// state_file set, feeds usage, picks, and shuts down the way the host does.
// The clock of each process is fixed from the environment, so expiry is
// checked without waiting.

const persistHelperEnv = "QUOTA_ROUTER_PERSIST_HELPER"

type persistStep struct {
	Restored  int               `json:"restored"`
	LoadError string            `json:"load_error"`
	SaveError string            `json:"save_error"`
	Picks     map[string]string `json:"picks"`
	Reasons   map[string]string `json:"reasons"`
}

// persistSessions are the sessions every step picks for, as the host
// presents them: canonical session ID plus model.
var persistSessions = []struct{ name, session, model string }{
	{"s1-opus", "claude:aaaaaaaa-0000-4000-8000-000000000001", "claude-opus-4-6"},
	{"s1-haiku", "claude:aaaaaaaa-0000-4000-8000-000000000001", "claude-haiku-4-5-20251001"},
	{"s2-opus", "claude:aaaaaaaa-0000-4000-8000-000000000002", "claude-opus-4-6"},
}

// runPersistStep runs one plugin process. util maps auth ID to 5h and 7d
// utilization reported before the picks.
func runPersistStep(t *testing.T, state string, now time.Time, util map[string]float64) persistStep {
	t.Helper()
	utilJSON, _ := json.Marshal(util)
	cmd := exec.Command(os.Args[0], "-test.run=^TestPersistHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		persistHelperEnv+"=1",
		"QUOTA_ROUTER_PERSIST_STATE="+state,
		"QUOTA_ROUTER_PERSIST_NOW="+now.Format(time.RFC3339Nano),
		"QUOTA_ROUTER_PERSIST_UTIL="+string(utilJSON),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("plugin process failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "PERSIST-STEP "); ok {
			var out persistStep
			if err := json.Unmarshal([]byte(rest), &out); err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatalf("no result from plugin process:\n%s\n%s", stdout.String(), stderr.String())
	return persistStep{}
}

// TestPersistHelperProcess is one plugin process lifetime. It does nothing
// unless started by runPersistStep.
func TestPersistHelperProcess(t *testing.T) {
	if os.Getenv(persistHelperEnv) == "" {
		t.Skip("helper process for the restart tests")
	}
	now, err := time.Parse(time.RFC3339Nano, os.Getenv("QUOTA_ROUTER_PERSIST_NOW"))
	if err != nil {
		t.Fatal(err)
	}
	var util map[string]float64
	if err := json.Unmarshal([]byte(os.Getenv("QUOTA_ROUTER_PERSIST_UTIL")), &util); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Router: New(WithClock(func() time.Time { return now }))}
	yaml := fmt.Sprintf("enabled: true\nsession_affinity: true\nstate_file: %q\n", os.Getenv("QUOTA_ROUTER_PERSIST_STATE"))
	if env := call(t, h, "plugin.register", lifecycle(yaml)); !env.OK {
		t.Fatalf("register: %+v", env.Error)
	}
	out := persistStep{Picks: map[string]string{}, Reasons: map[string]string{}}
	st := h.Router.State()
	out.Restored, out.LoadError = st.Restored, st.LastError
	ids := make([]string, 0, len(util))
	for id, u := range util {
		ids = append(ids, id)
		rec := pluginapi.UsageRecord{AuthID: id, Provider: "claude", ResponseHeaders: claudeHeaders(now, &cwin{u, 4 * time.Hour}, &cwin{u, 3 * day})}
		if env := call(t, h, "usage.handle", rec); !env.OK {
			t.Fatalf("usage: %+v", env.Error)
		}
	}
	for _, s := range persistSessions {
		req := pluginapi.SchedulerPickRequest{
			Providers: []string{"claude"}, Model: s.model,
			Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"canonical_session_id": s.session}},
			Candidates: claudeCandidates(ids...),
		}
		out.Picks[s.name], out.Reasons[s.name] = pickRPC(t, h, req)
	}
	if env := call(t, h, "plugin.shutdown", nil); !env.OK {
		t.Fatalf("shutdown: %+v", env.Error)
	}
	if e := h.Router.State().LastError; strings.HasPrefix(e, "save") {
		out.SaveError = e
	}
	raw, _ := json.Marshal(out)
	fmt.Printf("PERSIST-STEP %s\n", raw)
}

func TestPinsSurviveProcessRestartAndExpire(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	first := runPersistStep(t, state, t0, map[string]float64{"acct-a": 0.2, "acct-b": 0.5})
	for name, got := range first.Picks {
		if got != "acct-a" {
			t.Fatalf("first process: %s picked %s", name, got)
		}
	}

	// Thirty minutes later, in a new process, acct-b is far better. Every
	// binding comes back and holds.
	second := runPersistStep(t, state, t0.Add(30*time.Minute), map[string]float64{"acct-a": 0.9, "acct-b": 0.0})
	if second.Restored != len(persistSessions) || second.LoadError != "" {
		t.Fatalf("second process: %+v", second)
	}
	for name, got := range second.Picks {
		if got != "acct-a" || second.Reasons[name] != "session affinity" {
			t.Fatalf("second process: %s picked %s (%s)", name, got, second.Reasons[name])
		}
	}

	// The second process renewed the bindings to t0+90m. Three hours after t0
	// they have expired: nothing is restored and sessions are ranked afresh.
	third := runPersistStep(t, state, t0.Add(3*time.Hour), map[string]float64{"acct-a": 0.9, "acct-b": 0.0})
	if third.Restored != 0 || third.LoadError != "" {
		t.Fatalf("third process: %+v", third)
	}
	for name, got := range third.Picks {
		if got != "acct-b" {
			t.Fatalf("third process: %s picked %s (%s)", name, got, third.Reasons[name])
		}
	}
}

// A state file the plugin refuses to read, or a directory it cannot write,
// costs the bindings and nothing else: the process registers, picks and
// shuts down normally and reports the error.
func TestStateFileFailuresAcrossProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission checks are Unix-only")
	}
	util := map[string]float64{"acct-a": 0.2, "acct-b": 0.5}

	t.Run("unreadable by policy", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "state.json")
		runPersistStep(t, state, t0, util)
		if err := os.Chmod(state, 0o644); err != nil {
			t.Fatal(err)
		}
		step := runPersistStep(t, state, t0.Add(time.Minute), map[string]float64{"acct-a": 0.9, "acct-b": 0.0})
		if step.Restored != 0 || !strings.Contains(step.LoadError, "readable by other users") {
			t.Fatalf("step = %+v", step)
		}
		if step.Picks["s1-opus"] != "acct-b" {
			t.Fatalf("picks = %v", step.Picks)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(state, []byte(`{"version":1,"bindings":[`), 0o600); err != nil {
			t.Fatal(err)
		}
		step := runPersistStep(t, state, t0, util)
		if step.Restored != 0 || !strings.Contains(step.LoadError, "not valid JSON") || step.Picks["s1-opus"] != "acct-a" {
			t.Fatalf("step = %+v", step)
		}
	})

	t.Run("newer schema", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(state, []byte(`{"version":2,"bindings":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		step := runPersistStep(t, state, t0, util)
		if step.Restored != 0 || !strings.Contains(step.LoadError, "schema version") || step.Picks["s1-opus"] != "acct-a" {
			t.Fatalf("step = %+v", step)
		}
	})

	t.Run("save fails", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := t.TempDir()
		state := filepath.Join(dir, "state.json")
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		step := runPersistStep(t, state, t0, util)
		if step.SaveError == "" || strings.Contains(step.SaveError, dir) || step.Picks["s1-opus"] != "acct-a" {
			t.Fatalf("step = %+v", step)
		}
	})
}
