package router

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func mustSelect(t *testing.T, resp pluginapi.SchedulerPickResponse, d Decision, want string) {
	t.Helper()
	if !resp.Handled || resp.AuthID != want {
		t.Fatalf("picked %q (handled=%v, outcome=%s, reason=%q), want %q", resp.AuthID, resp.Handled, d.Outcome, d.Reason, want)
	}
}

func TestPickSmartFixtures(t *testing.T) {
	r, _ := newTestRouter()
	observe(r, "a", 72, 14*time.Hour)
	observe(r, "b", 3, 8*time.Hour)
	observe(r, "c", 38, 2*day)
	observe(r, "d", 91, 5*day)
	resp, d := r.Pick(pickReq(codexCandidates("a", "b", "c", "d")...))
	mustSelect(t, resp, d, "a")
	if d.Outcome != OutcomeSelected || !strings.Contains(d.Reason, "smart") {
		t.Errorf("decision = %+v", d)
	}
}

func TestPickModes(t *testing.T) {
	for mode, want := range map[Mode]string{ModeExpiringFirst: "a", ModeHeadroom: "d", ModeSmart: "a"} {
		r, _ := newTestRouter()
		cfg, err := ParseConfig([]byte("mode: " + string(mode)))
		if err != nil {
			t.Fatal(err)
		}
		r.SetConfig(cfg)
		observe(r, "a", 72, 14*time.Hour)
		observe(r, "b", 3, 8*time.Hour)
		observe(r, "d", 91, 5*day)
		resp, d := r.Pick(pickReq(codexCandidates("a", "b", "d")...))
		if resp.AuthID != want {
			t.Errorf("%s picked %q (%s), want %q", mode, resp.AuthID, d.Reason, want)
		}
	}
}

func TestPickSkipsIneligible(t *testing.T) {
	r, clock := newTestRouter()
	observe(r, "exhausted", 0, 2*time.Hour)
	observe(r, "thin", 1.5, 3*time.Hour) // below the 2% headroom
	observe(r, "disabled", 90, time.Hour)
	observe(r, "hostoff", 90, time.Hour)
	observe(r, "ok", 20, 4*day)
	r.Store().ApplyHostStates(map[string]HostAuthState{
		"hostoff":   {Disabled: true},
		"exhausted": {}, "thin": {}, "disabled": {}, "ok": {},
	}, clock.Now())

	cands := codexCandidates("exhausted", "thin", "disabled", "hostoff", "ok")
	cands[2].Status = "disabled"
	resp, d := r.Pick(pickReq(cands...))
	mustSelect(t, resp, d, "ok")
	blocked := 0
	for _, v := range d.Candidates {
		if v.State == stateBlocked {
			blocked++
		}
	}
	if blocked != 4 {
		t.Fatalf("blocked = %d, want 4: %+v", blocked, d.Candidates)
	}
}

func TestHostStateAgesOut(t *testing.T) {
	r, clock := newTestRouter()
	observe(r, "a", 90, time.Hour)
	r.Store().ApplyHostStates(map[string]HostAuthState{"a": {Disabled: true}}, clock.Now())
	if resp, _ := r.Pick(pickReq(codexCandidates("a")...)); resp.AuthID == "a" {
		t.Fatal("disabled auth was picked")
	}
	clock.Advance(3*r.Config().RefreshInterval + time.Second)
	observe(r, "a", 90, time.Hour)
	if resp, d := r.Pick(pickReq(codexCandidates("a")...)); resp.AuthID != "a" {
		t.Fatalf("stale host state still blocks: %+v", d)
	}
}

// The host filters cooling-down credentials per model before calling the
// scheduler (availableAuthsForSelector upstream). A credential it offers is
// available now, whatever retry time an earlier host.auth.list reported: the
// host may have reset the cooldown, or it may apply to a different model.
func TestOfferedCandidateIsNotVetoedByCachedRetry(t *testing.T) {
	r, clock := newTestRouter()
	observe(r, "a", 80, 4*time.Hour)
	observe(r, "b", 20, 4*time.Hour)
	r.Store().ApplyHostStates(map[string]HostAuthState{
		"a": {NextRetryAfter: clock.Now().Add(29 * time.Hour)},
		"b": {},
	}, clock.Now())
	resp, d := r.Pick(pickReq(codexCandidates("a", "b")...))
	mustSelect(t, resp, d, "a")
	if v := candidate(t, d, "a"); v.State != stateEligible {
		t.Fatalf("a = %+v", v)
	}
}

func TestMissingDataProbesThenFallsBack(t *testing.T) {
	r, clock := newTestRouter()
	cands := codexCandidates("x", "y")
	first, d1 := r.Pick(pickReq(cands...))
	second, d2 := r.Pick(pickReq(cands...))
	if first.AuthID == "" || second.AuthID == "" || first.AuthID == second.AuthID {
		t.Fatalf("expected two distinct probes, got %q (%s) and %q (%s)", first.AuthID, d1.Reason, second.AuthID, d2.Reason)
	}
	// Both probed recently and no data arrived: nothing to rank, defer to host.
	resp, d := r.Pick(pickReq(cands...))
	if resp.DelegateBuiltin != string(FallbackRoundRobin) || d.Outcome != OutcomeDelegated {
		t.Fatalf("expected round-robin delegation, got %+v / %+v", resp, d)
	}
	// After the probe interval a missing credential is probed again.
	clock.Advance(r.Config().ProbeInterval + time.Second)
	if resp, d := r.Pick(pickReq(cands...)); resp.AuthID == "" || !strings.HasPrefix(d.Reason, "probe") {
		t.Fatalf("expected re-probe, got %+v / %+v", resp, d)
	}
}

func TestProbeEndsWhenDataArrives(t *testing.T) {
	r, _ := newTestRouter()
	resp, _ := r.Pick(pickReq(codexCandidates("x")...))
	observe(r, resp.AuthID, 50, time.Hour)
	if r.Store().ProbedAt("x") != (time.Time{}) {
		t.Fatal("observation should clear probe marker")
	}
	_, d := r.Pick(pickReq(codexCandidates("x")...))
	if d.Candidates[0].State != stateEligible {
		t.Fatalf("state = %s", d.Candidates[0].State)
	}
}

func TestStaleDataIsReprobed(t *testing.T) {
	r, clock := newTestRouter()
	observe(r, "old", 80, 3*day)
	observe(r, "fresh", 30, 3*day)
	clock.Advance(r.Config().StaleAfter - time.Minute)
	observe(r, "fresh", 30, 3*day)
	clock.Advance(2 * time.Minute)
	resp, d := r.Pick(pickReq(codexCandidates("old", "fresh")...))
	if resp.AuthID != "old" || !strings.Contains(d.Reason, "stale") {
		t.Fatalf("expected stale credential to be re-probed, got %q (%s)", resp.AuthID, d.Reason)
	}
}

func TestUnsupportedProviderDelegates(t *testing.T) {
	for fb, check := range map[Fallback]func(pluginapi.SchedulerPickResponse) bool{
		FallbackRoundRobin: func(r pluginapi.SchedulerPickResponse) bool { return r.Handled && r.DelegateBuiltin == "round-robin" },
		FallbackFillFirst:  func(r pluginapi.SchedulerPickResponse) bool { return r.Handled && r.DelegateBuiltin == "fill-first" },
		FallbackNone:       func(r pluginapi.SchedulerPickResponse) bool { return !r.Handled },
	} {
		r, _ := newTestRouter()
		cfg := DefaultConfig()
		cfg.Fallback = fb
		r.SetConfig(cfg)
		req := pluginapi.SchedulerPickRequest{Provider: "gemini", Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "g1", Provider: "gemini"}, {ID: "g2", Provider: "gemini"},
		}}
		if resp, d := r.Pick(req); !check(resp) {
			t.Errorf("fallback %s: resp=%+v decision=%+v", fb, resp, d)
		}
	}
}

func TestFallbackAvoidsBlockedCandidates(t *testing.T) {
	r, _ := newTestRouter()
	observe(r, "codex-empty", 0, time.Hour)
	req := pluginapi.SchedulerPickRequest{Provider: "", Providers: []string{"codex", "gemini"}, Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "codex-empty", Provider: "codex"}, {ID: "g1", Provider: "gemini"}, {ID: "g2", Provider: "gemini"},
	}}
	seen := map[string]bool{}
	for range 4 {
		resp, d := r.Pick(req)
		if resp.AuthID == "codex-empty" || resp.AuthID == "" {
			t.Fatalf("unexpected pick %+v (%s)", resp, d.Reason)
		}
		seen[resp.AuthID] = true
	}
	if !seen["g1"] || !seen["g2"] {
		t.Fatalf("round-robin fallback did not rotate: %v", seen)
	}
}

func TestAllBlockedRejectsUnderEveryFallback(t *testing.T) {
	for _, fb := range []Fallback{FallbackRoundRobin, FallbackFillFirst, FallbackNone} {
		r, clock := newTestRouter()
		cfg := DefaultConfig()
		cfg.Fallback = fb
		r.SetConfig(cfg)
		observe(r, "thin", 1, time.Hour)
		observe(r, "empty", 0, time.Hour)
		observe(r, "off", 90, time.Hour)
		r.Store().ApplyHostStates(map[string]HostAuthState{"off": {Disabled: true}, "thin": {}, "empty": {}}, clock.Now())
		resp, d := r.Pick(pickReq(codexCandidates("thin", "empty", "off")...))
		if !resp.Handled || !resp.Reject || resp.RejectCode != RejectCode || resp.AuthID != "" || resp.DelegateBuiltin != "" {
			t.Fatalf("fallback %s: expected rejection, got %+v (%s)", fb, resp, d.Reason)
		}
		if d.Outcome != OutcomeRejected {
			t.Errorf("fallback %s: outcome = %s", fb, d.Outcome)
		}
	}
}

func TestFallbackNoneNeverReopensBlockedCandidates(t *testing.T) {
	r, _ := newTestRouter()
	cfg := DefaultConfig()
	cfg.Fallback = FallbackNone
	r.SetConfig(cfg)
	observe(r, "codex-empty", 0, time.Hour)
	req := pluginapi.SchedulerPickRequest{Providers: []string{"codex", "gemini"}, Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "codex-empty", Provider: "codex"}, {ID: "g1", Provider: "gemini"},
	}}
	// Returning unhandled here would let the host pick codex-empty.
	resp, d := r.Pick(req)
	if !resp.Handled || resp.AuthID != "g1" {
		t.Fatalf("resp = %+v (%s)", resp, d.Reason)
	}
	// With nothing excluded, none still defers to the host.
	resp, d = r.Pick(pluginapi.SchedulerPickRequest{Provider: "gemini", Candidates: req.Candidates[1:]})
	if resp.Handled || d.Outcome != OutcomeUnhandled {
		t.Fatalf("resp = %+v (%s)", resp, d.Reason)
	}
}

func TestZeroHeadroomSkipsEmptyAccount(t *testing.T) {
	r, _ := newTestRouter()
	cfg, err := ParseConfig([]byte("minimum_headroom: 0"))
	if err != nil {
		t.Fatal(err)
	}
	r.SetConfig(cfg)
	observe(r, "empty", 0, 2*time.Hour)
	observe(r, "thin", 0.5, 2*time.Hour)
	resp, d := r.Pick(pickReq(codexCandidates("empty", "thin")...))
	mustSelect(t, resp, d, "thin")
	resp, d = r.Pick(pickReq(codexCandidates("empty")...))
	if !resp.Reject {
		t.Fatalf("empty account with headroom 0 was not rejected: %+v (%s)", resp, d.Reason)
	}
}

func TestHysteresisKeepsCurrentWithinMargin(t *testing.T) {
	r, _ := newTestRouter()
	observe(r, "a", 60, 10*time.Hour)
	observe(r, "b", 40, 10*time.Hour)
	cands := codexCandidates("a", "b")
	resp, d := r.Pick(pickReq(cands...))
	mustSelect(t, resp, d, "a")

	// b now scores ~10% higher than a: within the 15% margin, keep a.
	observe(r, "a", 50, 10*time.Hour)
	observe(r, "b", 55, 10*time.Hour)
	resp, d = r.Pick(pickReq(cands...))
	mustSelect(t, resp, d, "a")
	if !strings.HasPrefix(d.Reason, "kept current") {
		t.Errorf("reason = %q", d.Reason)
	}

	// b clearly better: switch.
	observe(r, "a", 30, 10*time.Hour)
	resp, d = r.Pick(pickReq(cands...))
	mustSelect(t, resp, d, "b")
}

func TestTiesRotateWhenChoosingAnew(t *testing.T) {
	r, _ := newTestRouter()
	for _, id := range []string{"a", "b", "c"} {
		observe(r, id, 50, 6*time.Hour)
	}
	cands := codexCandidates("c", "b", "a")
	first, d := r.Pick(pickReq(cands...))
	mustSelect(t, first, d, "a") // stable ID order regardless of host order
	if !strings.Contains(d.Reason, "tied") {
		t.Errorf("reason = %q", d.Reason)
	}
	// Sticky choice is kept while scores stay tied.
	if again, d := r.Pick(pickReq(cands...)); again.AuthID != "a" {
		t.Fatalf("tie flapped to %q (%s)", again.AuthID, d.Reason)
	}
	// Once the current one drops out, the next tied credential is chosen.
	observe(r, "a", 0, 6*time.Hour)
	next, d := r.Pick(pickReq(cands...))
	if next.AuthID != "b" && next.AuthID != "c" {
		t.Fatalf("picked %q (%s)", next.AuthID, d.Reason)
	}
}

func TestSessionAffinity(t *testing.T) {
	r, clock := newTestRouter()
	observe(r, "a", 80, 10*time.Hour)
	observe(r, "b", 20, 10*time.Hour)
	req := pickReq(codexCandidates("a", "b")...)
	req.Options.Headers = map[string][]string{"session_id": {"conversation-fixture-123"}}

	resp, d := r.Pick(req)
	mustSelect(t, resp, d, "a")
	if !d.Session {
		t.Fatal("session not detected")
	}
	// b becomes far better, but the session stays on a.
	observe(r, "b", 99, 3*time.Hour)
	resp, d = r.Pick(req)
	mustSelect(t, resp, d, "a")
	if d.Reason != "session affinity" {
		t.Errorf("reason = %q", d.Reason)
	}
	// A request without the session moves to b.
	if resp, _ := r.Pick(pickReq(codexCandidates("a", "b")...)); resp.AuthID != "b" {
		t.Fatalf("sessionless pick = %q", resp.AuthID)
	}
	// When a is exhausted the session is rebound.
	observe(r, "a", 0, 10*time.Hour)
	resp, d = r.Pick(req)
	mustSelect(t, resp, d, "b")
	observe(r, "a", 100, 10*time.Hour)
	resp, d = r.Pick(req)
	mustSelect(t, resp, d, "b")
	// Bindings expire.
	clock.Advance(r.Config().SessionAffinityTTL + time.Minute)
	observe(r, "a", 100, 10*time.Hour)
	observe(r, "b", 10, 10*time.Hour)
	if resp, d := r.Pick(req); resp.AuthID != "a" {
		t.Fatalf("expired binding still applied: %q (%s)", resp.AuthID, d.Reason)
	}
}

func TestSessionAffinityDisabled(t *testing.T) {
	r, _ := newTestRouter()
	cfg := DefaultConfig()
	cfg.SessionAffinity = false
	r.SetConfig(cfg)
	observe(r, "a", 80, 10*time.Hour)
	req := pickReq(codexCandidates("a")...)
	req.Options.Metadata = map[string]any{"canonical_session_id": "s-1"}
	if _, d := r.Pick(req); d.Session {
		t.Fatal("session tracked while disabled")
	}
	if r.aff.size() != 0 {
		t.Fatal("binding stored while disabled")
	}
}

func TestSessionKeySources(t *testing.T) {
	if sessionKey(nil, nil) != "" {
		t.Fatal("empty request produced a key")
	}
	fromMeta := sessionKey(nil, map[string]any{"execution_session_id": "abc"})
	fromHeader := sessionKey(map[string][]string{"x-session-id": {"abc"}}, nil)
	if fromMeta == "" || fromHeader == "" || fromMeta == fromHeader {
		t.Fatalf("keys: meta=%q header=%q", fromMeta, fromHeader)
	}
	if strings.Contains(fromMeta, "abc") {
		t.Fatal("raw session id leaked into key")
	}
	if sessionKey(map[string][]string{"X-Client-Request-Id": {"per-request"}}, nil) != "" {
		t.Fatal("per-request id must not create affinity")
	}
}

func TestAffinityEvictsWhenFull(t *testing.T) {
	a := newAffinity()
	for i := range maxAffinityEntries + 5 {
		a.bind(string(rune(i+1))+"k", "auth", t0, time.Hour)
	}
	if a.size() > maxAffinityEntries {
		t.Fatalf("size = %d", a.size())
	}
}

func TestEmptyCandidatesUnhandled(t *testing.T) {
	r, _ := newTestRouter()
	if resp, d := r.Pick(pickReq()); resp.Handled || d.Outcome != OutcomeUnhandled {
		t.Fatalf("resp=%+v d=%+v", resp, d)
	}
}

func TestStoreKeepsSnapshotOnHeaderlessOrOlderResponses(t *testing.T) {
	r, clock := newTestRouter()
	observe(r, "a", 70, time.Hour)
	if r.ObserveUsage(pluginapi.UsageRecord{AuthID: "a", Provider: "codex", ResponseHeaders: map[string][]string{"Content-Type": {"application/json"}}}) {
		t.Fatal("headerless response replaced the snapshot")
	}
	snap, _ := r.Store().Snapshot("a")
	if snap.Windows[0].Remaining != 0.7 {
		t.Fatalf("remaining = %v", snap.Windows[0].Remaining)
	}
	// An out-of-order older observation is dropped.
	newer := snap.ObservedAt
	if r.Store().Observe("a", "codex", codexHeaders(win{used: 90, resetIn: time.Hour}, nil), newer.Add(-time.Minute)) {
		t.Fatal("older observation accepted")
	}
	clock.Advance(time.Second)
	if !r.Store().Observe("a", "codex", codexHeaders(win{used: 90, resetIn: time.Hour}, nil), clock.Now()) {
		t.Fatal("newer observation rejected")
	}
}

func TestDecisionsAreBounded(t *testing.T) {
	r, _ := newTestRouter()
	for range recentDecisions + 7 {
		r.Pick(pickReq(codexCandidates("a")...))
	}
	if got := len(r.Decisions()); got != recentDecisions {
		t.Fatalf("decisions = %d", got)
	}
}

func TestLogDecisions(t *testing.T) {
	var lines []map[string]any
	r, _ := newTestRouter(WithLogger(func(_, _ string, f map[string]any) { lines = append(lines, f) }))
	observe(r, "a", 80, time.Hour)
	r.Pick(pickReq(codexCandidates("a")...))
	if len(lines) != 0 {
		t.Fatal("logged while log_decisions is off")
	}
	cfg := DefaultConfig()
	cfg.LogDecisions = true
	r.SetConfig(cfg)
	r.Pick(pickReq(codexCandidates("a")...))
	if len(lines) != 1 || lines[0]["outcome"] != OutcomeSelected {
		t.Fatalf("lines = %+v", lines)
	}
}

func TestPickRespectsLimitingWindow(t *testing.T) {
	r, _ := newTestRouter()
	weekly := func(used float64) *win { return &win{used: used, resetIn: 4 * day, length: 7 * day} }
	// a: fresh 5h window but the weekly window is almost spent.
	r.ObserveUsage(pluginapi.UsageRecord{AuthID: "a", Provider: "codex",
		ResponseHeaders: codexHeaders(win{used: 0, resetIn: 2 * time.Hour, length: 5 * time.Hour}, weekly(95))})
	// b: half the 5h window left with plenty of weekly room.
	r.ObserveUsage(pluginapi.UsageRecord{AuthID: "b", Provider: "codex",
		ResponseHeaders: codexHeaders(win{used: 50, resetIn: 3 * time.Hour, length: 5 * time.Hour}, weekly(30))})
	resp, d := r.Pick(pickReq(codexCandidates("a", "b")...))
	mustSelect(t, resp, d, "b")

	// c: weekly window below headroom blocks the account despite a full 5h window.
	r.ObserveUsage(pluginapi.UsageRecord{AuthID: "c", Provider: "codex",
		ResponseHeaders: codexHeaders(win{used: 0, resetIn: time.Hour, length: 5 * time.Hour}, weekly(99))})
	_, d = r.Pick(pickReq(codexCandidates("c")...))
	if d.Candidates[0].State != stateBlocked {
		t.Fatalf("c state = %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
}

// usage records one Codex response carrying exactly the given headers.
func usage(r *Router, id string, h map[string]string) bool {
	headers := http.Header{}
	for k, v := range h {
		headers.Set(k, v)
	}
	return r.ObserveUsage(pluginapi.UsageRecord{AuthID: id, Provider: "codex", ResponseHeaders: headers})
}

func TestPartialPrimaryUpdateKeepsExhaustedSecondary(t *testing.T) {
	r, clock := newTestRouter()
	weekly := &win{used: 100, resetIn: 2 * day, length: 7 * day}
	r.ObserveUsage(pluginapi.UsageRecord{AuthID: "a", Provider: "codex",
		ResponseHeaders: codexHeaders(win{used: 10, resetIn: 2 * time.Hour, length: 5 * time.Hour}, weekly)})
	observe(r, "b", 40, 3*day)
	clock.Advance(time.Minute)
	// A later response for a only mentions the 5-hour window.
	if !usage(r, "a", map[string]string{"X-Codex-Primary-Used-Percent": "5", "X-Codex-Primary-Reset-After-Seconds": "7200"}) {
		t.Fatal("partial update ignored")
	}
	snap, _ := r.Store().Snapshot("a")
	if len(snap.Windows) != 2 || snap.Windows[0].Remaining != 0.95 || snap.Windows[1].Remaining != 0 {
		t.Fatalf("windows after partial update = %+v", snap.Windows)
	}
	resp, d := r.Pick(pickReq(codexCandidates("a", "b")...))
	mustSelect(t, resp, d, "b")
	if d.Candidates[0].State != stateBlocked {
		t.Fatalf("a state = %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
}

func TestBooleanOnlyHeaders(t *testing.T) {
	r, clock := newTestRouter()
	// No prior data: Allowed: true says nothing about remaining quota, so the
	// credential is still probed instead of ranked with a score of zero.
	usage(r, "fresh", map[string]string{"X-Codex-Allowed": "true"})
	_, d := r.Pick(pickReq(codexCandidates("fresh")...))
	if d.Candidates[0].State != stateProbe {
		t.Fatalf("fresh state = %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
	// The probe response again carries only the flag: the probe marker stays,
	// so the credential is not probed on every request.
	usage(r, "fresh", map[string]string{"X-Codex-Allowed": "true"})
	if _, d = r.Pick(pickReq(codexCandidates("fresh")...)); d.Candidates[0].State != stateUnknown {
		t.Fatalf("fresh state after flag-only probe = %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}

	// Prior depleted window: a bare Allowed: true does not wipe it.
	r.ObserveUsage(pluginapi.UsageRecord{AuthID: "a", Provider: "codex",
		ResponseHeaders: codexHeaders(win{used: 100, resetIn: time.Hour}, nil)})
	clock.Advance(time.Second)
	usage(r, "a", map[string]string{"X-Codex-Allowed": "true"})
	if _, d = r.Pick(pickReq(codexCandidates("a")...)); d.Candidates[0].State != stateBlocked {
		t.Fatalf("a state = %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}

	// Allowed: false alone blocks a credential with healthy windows until stale.
	observe(r, "c", 80, 3*day)
	clock.Advance(time.Second)
	usage(r, "c", map[string]string{"X-Codex-Allowed": "false"})
	if _, d = r.Pick(pickReq(codexCandidates("c")...)); d.Candidates[0].State != stateBlocked {
		t.Fatalf("c state = %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
	// A primary-only update without the flag does not clear it early.
	usage(r, "c", map[string]string{"X-Codex-Primary-Used-Percent": "20"})
	if _, d = r.Pick(pickReq(codexCandidates("c")...)); d.Candidates[0].State != stateBlocked {
		t.Fatalf("c unblocked by a flagless update: %s", d.Candidates[0].Reason)
	}
	// A newer Allowed: true does.
	usage(r, "c", map[string]string{"X-Codex-Allowed": "true", "X-Codex-Primary-Used-Percent": "20"})
	if _, d = r.Pick(pickReq(codexCandidates("c")...)); d.Candidates[0].State != stateEligible {
		t.Fatalf("c state = %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
}

func TestRecoveryAfterResetWithPartialUpdates(t *testing.T) {
	r, clock := newTestRouter()
	r.ObserveUsage(pluginapi.UsageRecord{AuthID: "a", Provider: "codex",
		ResponseHeaders: codexHeaders(win{used: 0, resetIn: time.Hour, length: 5 * time.Hour},
			&win{used: 100, resetIn: 2 * time.Hour, length: 7 * day})})
	if _, d := r.Pick(pickReq(codexCandidates("a")...)); d.Candidates[0].State != stateBlocked {
		t.Fatalf("state = %s", d.Candidates[0].State)
	}
	clock.Advance(2*time.Hour + time.Minute)
	// Only the primary window is reported after the weekly reset passed.
	usage(r, "a", map[string]string{"X-Codex-Primary-Used-Percent": "30", "X-Codex-Primary-Reset-After-Seconds": "3600"})
	resp, d := r.Pick(pickReq(codexCandidates("a")...))
	mustSelect(t, resp, d, "a")
	if d.Candidates[0].State != stateEligible {
		t.Fatalf("state = %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
}

// primaryOnly reports only a's 5-hour window at 10% used, as some Codex
// responses do.
func primaryOnly(r *Router) {
	usage(r, "a", map[string]string{"X-Codex-Primary-Used-Percent": "10", "X-Codex-Primary-Reset-After-Seconds": "7200"})
}

func TestStalenessIsPerWindow(t *testing.T) {
	r, clock := newTestRouter()
	stale := r.Config().StaleAfter
	r.ObserveUsage(pluginapi.UsageRecord{AuthID: "a", Provider: "codex",
		ResponseHeaders: codexHeaders(win{used: 10, resetIn: 2 * time.Hour}, &win{used: 20, resetIn: 3 * day})})
	// Fresh 5-hour reports keep arriving; the weekly window is never repeated.
	for elapsed := time.Duration(0); elapsed <= stale; elapsed += 10 * time.Minute {
		clock.Advance(10 * time.Minute)
		primaryOnly(r)
	}
	_, d := r.Pick(pickReq(codexCandidates("a")...))
	if d.Candidates[0].State != stateProbe || !strings.Contains(d.Candidates[0].Reason, "stale") {
		t.Fatalf("primary fresh, secondary stale: %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
}

func TestLimitFlagsDoNotRefreshWindows(t *testing.T) {
	r, clock := newTestRouter()
	observe(r, "a", 80, 3*day)
	for elapsed := time.Duration(0); elapsed <= r.Config().StaleAfter; elapsed += 5 * time.Minute {
		clock.Advance(5 * time.Minute)
		usage(r, "a", map[string]string{"X-Codex-Allowed": "true"})
	}
	_, d := r.Pick(pickReq(codexCandidates("a")...))
	if d.Candidates[0].State != stateProbe || !strings.Contains(d.Candidates[0].Reason, "stale") {
		t.Fatalf("flag-only updates kept old window rankable: %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
}

func TestStaleDepletedWindowBlocksUntilReset(t *testing.T) {
	r, clock := newTestRouter()
	r.ObserveUsage(pluginapi.UsageRecord{AuthID: "a", Provider: "codex",
		ResponseHeaders: codexHeaders(win{used: 10, resetIn: 2 * time.Hour}, &win{used: 100, resetIn: 2 * day})})
	elapsed := r.Config().StaleAfter + time.Hour
	clock.Advance(elapsed)
	primaryOnly(r)
	if _, d := r.Pick(pickReq(codexCandidates("a")...)); d.Candidates[0].State != stateBlocked {
		t.Fatalf("old depleted weekly window with future reset: %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
	// After the weekly reset its fullness is known as of the reset itself.
	clock.Advance(2*day - elapsed + time.Minute)
	primaryOnly(r)
	resp, d := r.Pick(pickReq(codexCandidates("a")...))
	mustSelect(t, resp, d, "a")
	if d.Candidates[0].State != stateEligible {
		t.Fatalf("after reset: %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
	// Without a newer weekly report, the refill ages out like any observation.
	clock.Advance(r.Config().StaleAfter)
	primaryOnly(r)
	if _, d = r.Pick(pickReq(codexCandidates("a")...)); d.Candidates[0].State != stateProbe {
		t.Fatalf("refill should age out: %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
}

func TestStaleDepletedWindowWithoutResetIsReprobed(t *testing.T) {
	r, clock := newTestRouter()
	usage(r, "a", map[string]string{"X-Codex-Primary-Used-Percent": "10", "X-Codex-Primary-Reset-After-Seconds": "7200",
		"X-Codex-Secondary-Used-Percent": "100"})
	clock.Advance(r.Config().StaleAfter - time.Minute)
	primaryOnly(r)
	if _, d := r.Pick(pickReq(codexCandidates("a")...)); d.Candidates[0].State != stateBlocked {
		t.Fatalf("depleted window without reset: %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
	clock.Advance(2 * time.Minute)
	primaryOnly(r)
	if _, d := r.Pick(pickReq(codexCandidates("a")...)); d.Candidates[0].State != stateProbe || !strings.Contains(d.Candidates[0].Reason, "stale") {
		t.Fatalf("expired no-reset block should be probed, got %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
	// A probe answer that reports the window again makes it rankable.
	usage(r, "a", map[string]string{"X-Codex-Primary-Used-Percent": "10", "X-Codex-Primary-Reset-After-Seconds": "7200",
		"X-Codex-Secondary-Used-Percent": "40"})
	if _, d := r.Pick(pickReq(codexCandidates("a")...)); d.Candidates[0].State != stateEligible {
		t.Fatalf("after fresh report: %s (%s)", d.Candidates[0].State, d.Candidates[0].Reason)
	}
}
