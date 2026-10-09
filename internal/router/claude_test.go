package router

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// cwin is one Claude window: utilization share and time until reset.
type cwin struct {
	util    float64
	resetIn time.Duration
}

// claudeHeaders builds the unified rate-limit headers of a Claude OAuth
// response observed at now. A nil window is left out.
func claudeHeaders(now time.Time, five, week *cwin) http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	set := func(abbr string, w *cwin) {
		if w == nil {
			return
		}
		h.Set("Anthropic-Ratelimit-Unified-"+abbr+"-Utilization", strconv.FormatFloat(w.util, 'f', -1, 64))
		h.Set("Anthropic-Ratelimit-Unified-"+abbr+"-Reset", strconv.FormatInt(now.Add(w.resetIn).Unix(), 10))
		h.Set("Anthropic-Ratelimit-Unified-"+abbr+"-Status", "allowed")
	}
	set("5h", five)
	set("7d", week)
	h.Set("Request-Id", "req_fixture")
	return h
}

func observeClaude(r *Router, id string, h http.Header) {
	r.ObserveUsage(pluginapi.UsageRecord{AuthID: id, Provider: "claude", ResponseHeaders: h})
}

func claudeCandidates(ids ...string) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.SchedulerAuthCandidate{
			ID: id, Provider: "claude", Status: "active",
			Attributes: map[string]string{"auth_kind": "oauth"},
		})
	}
	return out
}

func claudePick(cands ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{Provider: "claude", Model: "claude-sonnet-5-5", Candidates: cands}
}

func candidate(t *testing.T, d Decision, id string) CandidateView {
	t.Helper()
	for _, v := range d.Candidates {
		if v.id == id {
			return v
		}
	}
	t.Fatalf("no candidate %s in %+v", id, d.Candidates)
	return CandidateView{}
}

func TestClaudeSmartRanking(t *testing.T) {
	r, clock := newTestRouter()
	now := clock.Now()
	// a: plenty of 5h quota that resets soon, and room in the week.
	observeClaude(r, "a", claudeHeaders(now, &cwin{0.20, 2 * time.Hour}, &cwin{0.30, 3 * day}))
	// b: same 5h window, but the week is nearly spent, which caps what the 5h window can use.
	observeClaude(r, "b", claudeHeaders(now, &cwin{0.20, 2 * time.Hour}, &cwin{0.95, 3 * day}))
	// c: fresh 5h window, week resets later.
	observeClaude(r, "c", claudeHeaders(now, &cwin{0.0, 5 * time.Hour}, &cwin{0.50, 6 * day}))

	resp, d := r.Pick(claudePick(claudeCandidates("a", "b", "c")...))
	mustSelect(t, resp, d, "a")
	a, b := candidate(t, d, "a"), candidate(t, d, "b")
	if b.Score >= a.Score || b.Remaining == nil || *b.Remaining > 0.051 {
		t.Errorf("multi-window cap not applied: a=%+v b=%+v", a, b)
	}
}

func TestClaudeRejectedWindowBlocksUntilReset(t *testing.T) {
	r, clock := newTestRouter()
	now := clock.Now()
	h := claudeHeaders(now, &cwin{1.0, 90 * time.Minute}, &cwin{0.4, 4 * day})
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	observeClaude(r, "spent", h)
	observeClaude(r, "ok", claudeHeaders(now, &cwin{0.9, 4 * time.Hour}, &cwin{0.6, 4 * day}))

	resp, d := r.Pick(claudePick(claudeCandidates("spent", "ok")...))
	mustSelect(t, resp, d, "ok")
	if v := candidate(t, d, "spent"); v.State != stateBlocked || !strings.Contains(v.Reason, now.Add(90*time.Minute).UTC().Format(time.RFC3339)) {
		t.Fatalf("spent = %+v", v)
	}

	// Past the reset the 5h window counts as full again without a new response.
	// The weekly figure is older than stale_after by then, so the account is
	// probed to refresh it rather than ranked.
	clock.Advance(91 * time.Minute)
	_, d = r.Pick(claudePick(claudeCandidates("spent", "ok")...))
	if v := candidate(t, d, "spent"); v.State != stateProbe {
		t.Fatalf("after reset spent = %+v", v)
	}
}

// A later response that reports only the 5h window must not clear a depleted
// weekly window.
func TestClaudePartialUpdateKeepsDepletedWeek(t *testing.T) {
	r, clock := newTestRouter()
	observeClaude(r, "x", claudeHeaders(clock.Now(), &cwin{0.1, 4 * time.Hour}, &cwin{1.0, 2 * day}))
	clock.Advance(time.Minute)
	observeClaude(r, "x", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, nil))
	observeClaude(r, "y", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 2 * day}))

	resp, d := r.Pick(claudePick(claudeCandidates("x", "y")...))
	mustSelect(t, resp, d, "y")
	if v := candidate(t, d, "x"); v.State != stateBlocked {
		t.Fatalf("x = %+v", v)
	}
}

func TestClaudeStaleDataIsProbed(t *testing.T) {
	r, clock := newTestRouter()
	observeClaude(r, "old", claudeHeaders(clock.Now(), &cwin{0.1, 4 * time.Hour}, &cwin{0.1, 6 * day}))
	clock.Advance(31 * time.Minute)
	observeClaude(r, "fresh", claudeHeaders(clock.Now(), &cwin{0.1, 4 * time.Hour}, &cwin{0.1, 6 * day}))

	resp, d := r.Pick(claudePick(claudeCandidates("old", "fresh")...))
	mustSelect(t, resp, d, "old")
	if !strings.Contains(d.Reason, "stale") {
		t.Fatalf("reason = %q, want a stale probe", d.Reason)
	}
	// The probe is spent; the next pick ranks the fresh account.
	resp, d = r.Pick(claudePick(claudeCandidates("old", "fresh")...))
	mustSelect(t, resp, d, "fresh")
}

func TestClaudeModelScopedRejectionKeepsAccount(t *testing.T) {
	r, clock := newTestRouter()
	h := claudeHeaders(clock.Now(), &cwin{0.0, 5 * time.Hour}, &cwin{0.69, 2 * day})
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day_overage_included")
	h.Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
	observeClaude(r, "team", h)

	resp, d := r.Pick(claudePick(claudeCandidates("team")...))
	mustSelect(t, resp, d, "team")
	if v := candidate(t, d, "team"); v.State != stateEligible {
		t.Fatalf("team = %+v", v)
	}
}

func TestClaudeCredentialRejectionHonorsUnifiedReset(t *testing.T) {
	r, clock := newTestRouter()
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(clock.Now().Add(2*time.Hour).Unix(), 10))
	observeClaude(r, "x", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 4 * day}))
	clock.Advance(time.Minute)
	observeClaude(r, "x", h)

	resp, d := r.Pick(claudePick(claudeCandidates("x")...))
	if !resp.Reject || resp.RejectCode != RejectCode {
		t.Fatalf("resp = %+v (%s)", resp, d.Reason)
	}
	// The block outlives stale_after because Anthropic named the reset time.
	clock.Advance(90 * time.Minute)
	if resp, _ := r.Pick(claudePick(claudeCandidates("x")...)); !resp.Reject {
		t.Fatalf("block lifted before the unified reset: %+v", resp)
	}
	clock.Advance(31 * time.Minute)
	if _, d := r.Pick(claudePick(claudeCandidates("x")...)); candidate(t, d, "x").State == stateBlocked {
		t.Fatalf("still blocked after the unified reset: %+v", d)
	}
}

func TestClaudeAllExhaustedRejects(t *testing.T) {
	r, clock := newTestRouter()
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{1.0, time.Hour}, &cwin{0.5, day}))
	observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.3, time.Hour}, &cwin{0.99, day}))
	resp, d := r.Pick(claudePick(claudeCandidates("a", "b")...))
	if !resp.Handled || !resp.Reject || d.Outcome != OutcomeRejected {
		t.Fatalf("resp = %+v, decision = %+v", resp, d)
	}
}

// Extra usage keeps an exhausted account serving, billed to the account. By
// default it is paid capacity: never chosen, and never handed to the host.
func TestClaudeExtraUsageIsPaidByDefault(t *testing.T) {
	r, clock := newTestRouter()
	h := claudeHeaders(clock.Now(), &cwin{1.0, 2 * time.Hour}, &cwin{0.5, day})
	h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "allowed")
	observeClaude(r, "credits", h)
	observeClaude(r, "ok", claudeHeaders(clock.Now(), &cwin{0.5, 2 * time.Hour}, &cwin{0.5, day}))

	resp, d := r.Pick(claudePick(claudeCandidates("credits", "ok")...))
	mustSelect(t, resp, d, "ok")
	if v := candidate(t, d, "credits"); v.State != statePaid || !strings.Contains(v.Reason, "extra usage") {
		t.Fatalf("credits = %+v", v)
	}

	// Alone, it is rejected with the paid code rather than delegated: a
	// delegated pick would let the host spend on it.
	resp, d = r.Pick(claudePick(claudeCandidates("credits")...))
	if !resp.Reject || resp.RejectCode != RejectCodePaidOnly || d.Outcome != OutcomeRejected {
		t.Fatalf("resp = %+v, decision = %+v", resp, d)
	}

	// Next to a blocked account the answer is the same.
	spent := claudeHeaders(clock.Now(), &cwin{1.0, 2 * time.Hour}, &cwin{0.5, day})
	spent.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
	observeClaude(r, "spent", spent)
	resp, d = r.Pick(claudePick(claudeCandidates("credits", "spent")...))
	if !resp.Reject || resp.RejectCode != RejectCodePaidOnly {
		t.Fatalf("resp = %+v, decision = %+v", resp, d)
	}
}

func TestClaudeAPIKeysArePaidAndNotProbed(t *testing.T) {
	r, _ := newTestRouter()
	cands := claudeCandidates("oauth", "key")
	cands[1].Attributes = map[string]string{"auth_kind": "apikey"}

	// Mixed pool: the OAuth account is probed, the key is never touched.
	resp, d := r.Pick(claudePick(cands...))
	mustSelect(t, resp, d, "oauth")
	if v := candidate(t, d, "key"); v.State != statePaid || !strings.Contains(v.Reason, "API key") {
		t.Fatalf("key = %+v", v)
	}
	// Key-only pool, as when the host has filtered every OAuth account out:
	// rejected, not delegated.
	resp, d = r.Pick(claudePick(cands[1]))
	if !resp.Reject || resp.RejectCode != RejectCodePaidOnly || d.Outcome != OutcomeRejected {
		t.Fatalf("resp = %+v, decision = %+v", resp, d)
	}
}

// Codex and Claude snapshots live side by side and are routed per provider.
func TestClaudeAndCodexSideBySide(t *testing.T) {
	r, clock := newTestRouter()
	observe(r, "cx", 80, 3*time.Hour)
	observeClaude(r, "cl", claudeHeaders(clock.Now(), &cwin{0.1, 3 * time.Hour}, &cwin{0.1, 3 * day}))
	if resp, d := r.Pick(pickReq(codexCandidates("cx")...)); resp.AuthID != "cx" {
		t.Fatalf("codex pick = %+v (%s)", resp, d.Reason)
	}
	if resp, d := r.Pick(claudePick(claudeCandidates("cl")...)); resp.AuthID != "cl" || !strings.Contains(d.Reason, "smart") {
		t.Fatalf("claude pick = %+v (%s)", resp, d.Reason)
	}
}
