package router

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// testdata/ingress-v8.0.23.json holds scheduler.pick requests recorded by
// CLIProxyAPI's test/scheduler_ingress_identity_test.go: Claude Code shaped
// HTTP requests sent through the real Claude handler, auth manager and
// executors of host v8.0.23, as the scheduler capability received them.
// Regenerate with QUOTA_ROUTER_INGRESS_FIXTURE=<path> go test -run
// TestSchedulerReceivesIngressSessionIdentity ./test in the host checkout.
//
// The tests below replay those exact shapes through the plugin's RPC entry
// point, with the candidates and quota each scenario needs.

type ingressPick struct {
	Case      string              `json:"case"`
	Stream    bool                `json:"stream"`
	Provider  string              `json:"provider"`
	Providers []string            `json:"providers"`
	Model     string              `json:"model"`
	Metadata  map[string]string   `json:"metadata"`
	Headers   map[string][]string `json:"headers"`
}

func loadIngress(t *testing.T) map[string]ingressPick {
	t.Helper()
	raw, err := os.ReadFile("testdata/ingress-v8.0.23.json")
	if err != nil {
		t.Fatal(err)
	}
	var picks []ingressPick
	if err := json.Unmarshal(raw, &picks); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]ingressPick, len(picks))
	for _, p := range picks {
		key := p.Case
		if p.Stream {
			key += "+stream"
		}
		out[key] = p
	}
	return out
}

// ingressReq rebuilds the recorded request with the given candidates. name is
// a case name, with "+stream" for the streaming recording.
func ingressReq(t *testing.T, fx map[string]ingressPick, name string, cands ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	t.Helper()
	p, ok := fx[name]
	if !ok {
		t.Fatalf("no recorded pick %q", name)
	}
	meta := make(map[string]any, len(p.Metadata))
	for k, v := range p.Metadata {
		meta[k] = v
	}
	return pluginapi.SchedulerPickRequest{
		Provider: p.Provider, Providers: p.Providers, Model: p.Model, Stream: p.Stream,
		Options:    pluginapi.SchedulerOptions{Headers: p.Headers, Metadata: meta},
		Candidates: cands,
	}
}

// pickRPC sends req through the plugin's scheduler.pick RPC as JSON, the way
// it crosses the native ABI, and returns the chosen auth and the reason.
func pickRPC(t *testing.T, h *Handler, req pluginapi.SchedulerPickRequest) (string, string) {
	t.Helper()
	env := call(t, h, "scheduler.pick", req)
	if !env.OK {
		t.Fatalf("scheduler.pick failed: %+v", env.Error)
	}
	var resp pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	ds := h.Router.Decisions()
	return resp.AuthID, ds[len(ds)-1].Reason
}

func ingressRouter(t *testing.T) (*Handler, *fakeClock) {
	t.Helper()
	r, clock := newTestRouter()
	return &Handler{Router: r}, clock
}

// claudeQuota feeds one Claude account's unified headers: 5h and 7d
// utilization, both resetting at a fixed distance.
func claudeQuota(r *Router, clock *fakeClock, id string, five, week float64) {
	observeClaude(r, id, claudeHeaders(clock.Now(), &cwin{five, 4 * time.Hour}, &cwin{week, 3 * day}))
}

func mustPick(t *testing.T, step, got, reason, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: picked %q (%s), want %q", step, got, reason, want)
	}
}

// TestIngressSessionKeys pins down what the plugin keys affinity on for every
// recorded Claude Code shape: the host's canonical_session_id. Streaming and
// non-streaming requests share a key, every subagent has its own, and a
// request without any session identity falls back to the host's derived ID.
func TestIngressSessionKeys(t *testing.T) {
	fx := loadIngress(t)
	key := func(name string) string {
		p := fx[name]
		meta := map[string]any{}
		for k, v := range p.Metadata {
			meta[k] = v
		}
		return sessionKey(p.Headers, meta)
	}
	parent := key("initial")
	if parent == "" {
		t.Fatal("no session key for the initial request")
	}
	for _, name := range []string{"resumed", "helper-haiku-same-session", "same-session-other-provider", "legacy-user-id", "claude-header-outranks-body", "failover-initial", "failover-retry"} {
		if key(name) != parent {
			t.Errorf("%s does not share the main session key", name)
		}
		if _, ok := fx[name+"+stream"]; ok && key(name+"+stream") != parent {
			t.Errorf("%s (stream) does not share the main session key", name)
		}
	}
	own := map[string]string{"main": parent}
	for _, name := range []string{"subagent", "sibling-subagent", "nested-subagent", "body-subagent-json-user-id", "legacy-user-id-subagent", "forked-session", "claude-body-outranks-generic-header", "no-identity"} {
		k := key(name)
		if k == "" || k != key(name+"+stream") {
			t.Errorf("%s: key %q, stream key %q", name, k, key(name+"+stream"))
		}
		for other, ok := range own {
			if ok == k {
				t.Errorf("%s shares a key with %s", name, other)
			}
		}
		own[name] = k
	}
}

// A healthy bound session stays on its account even when another scores far
// better; a new session goes to the better one.
func TestIngressHealthyPinBeatsBetterScore(t *testing.T) {
	fx := loadIngress(t)
	h, clock := ingressRouter(t)
	r := h.Router
	claudeQuota(r, clock, "acct-a", 0.2, 0.2)
	claudeQuota(r, clock, "acct-b", 0.5, 0.5)
	got, why := pickRPC(t, h, ingressReq(t, fx, "initial", claudeCandidates("acct-a", "acct-b")...))
	mustPick(t, "initial", got, why, "acct-a")

	clock.Advance(time.Minute)
	claudeQuota(r, clock, "acct-a", 0.9, 0.9)
	claudeQuota(r, clock, "acct-b", 0.0, 0.0)
	for _, name := range []string{"resumed+stream", "resumed", "claude-header-outranks-body", "legacy-user-id+stream"} {
		got, why = pickRPC(t, h, ingressReq(t, fx, name, claudeCandidates("acct-a", "acct-b")...))
		mustPick(t, name, got, why, "acct-a")
	}
	got, why = pickRPC(t, h, ingressReq(t, fx, "forked-session", claudeCandidates("acct-a", "acct-b")...))
	mustPick(t, "forked session (new)", got, why, "acct-b")
}

// Every subagent has its own pin. A subagent's first request is a new session
// and is ranked; afterwards it stays put, independent of its parent.
func TestIngressSubagentsHaveOwnPins(t *testing.T) {
	fx := loadIngress(t)
	h, clock := ingressRouter(t)
	r := h.Router
	cands := claudeCandidates("acct-a", "acct-b")
	claudeQuota(r, clock, "acct-a", 0.2, 0.2)
	claudeQuota(r, clock, "acct-b", 0.5, 0.5)
	got, why := pickRPC(t, h, ingressReq(t, fx, "initial", cands...))
	mustPick(t, "parent", got, why, "acct-a")

	clock.Advance(time.Minute)
	claudeQuota(r, clock, "acct-a", 0.6, 0.6)
	claudeQuota(r, clock, "acct-b", 0.1, 0.1)
	got, why = pickRPC(t, h, ingressReq(t, fx, "subagent", cands...))
	mustPick(t, "first subagent request (new session)", got, why, "acct-b")
	got, why = pickRPC(t, h, ingressReq(t, fx, "sibling-subagent", cands...))
	mustPick(t, "sibling subagent", got, why, "acct-b")
	got, why = pickRPC(t, h, ingressReq(t, fx, "resumed", cands...))
	mustPick(t, "parent after subagents", got, why, "acct-a")

	// acct-a becomes far better; each subagent stays on its own account.
	clock.Advance(time.Minute)
	claudeQuota(r, clock, "acct-a", 0.0, 0.0)
	claudeQuota(r, clock, "acct-b", 0.8, 0.8)
	got, why = pickRPC(t, h, ingressReq(t, fx, "subagent+stream", cands...))
	mustPick(t, "subagent resumed", got, why, "acct-b")
	// The parent losing its account does not move a subagent.
	got, why = pickRPC(t, h, ingressReq(t, fx, "resumed", claudeCandidates("acct-b")...))
	mustPick(t, "parent failover", got, why, "acct-b")
	got, why = pickRPC(t, h, ingressReq(t, fx, "sibling-subagent", cands...))
	mustPick(t, "sibling after parent failover", got, why, "acct-b")
}

// The recorded failover: the host asks again for the same request without the
// failed account. The session then stays on the replacement, also after the
// original recovers and the operator invalidates its cached quota.
func TestIngressFailoverPinsReplacement(t *testing.T) {
	fx := loadIngress(t)
	h, clock := ingressRouter(t)
	r := h.Router
	claudeQuota(r, clock, "acct-a", 0.2, 0.2)
	claudeQuota(r, clock, "acct-b", 0.5, 0.5)
	got, why := pickRPC(t, h, ingressReq(t, fx, "initial", claudeCandidates("acct-a", "acct-b")...))
	mustPick(t, "initial", got, why, "acct-a")

	got, why = pickRPC(t, h, ingressReq(t, fx, "failover-initial", claudeCandidates("acct-a", "acct-b")...))
	mustPick(t, "failover first attempt", got, why, "acct-a")
	got, why = pickRPC(t, h, ingressReq(t, fx, "failover-retry", claudeCandidates("acct-b")...))
	mustPick(t, "failover retry", got, why, "acct-b")

	// acct-a is offered again and scores far better.
	clock.Advance(time.Minute)
	claudeQuota(r, clock, "acct-a", 0.0, 0.0)
	claudeQuota(r, clock, "acct-b", 0.7, 0.7)
	got, why = pickRPC(t, h, ingressReq(t, fx, "resumed", claudeCandidates("acct-a", "acct-b")...))
	mustPick(t, "after recovery", got, why, "acct-b")
	r.Store().Invalidate("acct-a")
	got, why = pickRPC(t, h, ingressReq(t, fx, "resumed+stream", claudeCandidates("acct-a", "acct-b")...))
	mustPick(t, "after invalidate", got, why, "acct-b")
}
