package router

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Paid credentials: API keys on a supported provider, and subscription accounts
// whose windows are exhausted but still served from Anthropic extra usage.
// Both bill the account per request, so they are never chosen unless
// paid_fallback is enabled, and then only as a last resort.

func TestPaidFallbackOffRejectsKeyOnlyPool(t *testing.T) {
	r, _ := newTestRouter()
	cands := claudeCandidates("key")
	cands[0].Attributes = map[string]string{"auth_kind": "apikey"}
	resp, d := r.Pick(claudePick(cands...))
	if !resp.Handled || !resp.Reject || resp.RejectCode != RejectCodePaidOnly {
		t.Fatalf("key-only pool with paid_fallback off: resp=%+v decision=%+v", resp, d)
	}
	if v := candidate(t, d, "key"); v.State != statePaid {
		t.Fatalf("key = %+v, want state %s", v, statePaid)
	}
}

func paidOptIn(t *testing.T) (*Router, *fakeClock) {
	t.Helper()
	r, clock := newTestRouter()
	cfg, err := ParseConfig([]byte("paid_fallback: true"))
	if err != nil {
		t.Fatal(err)
	}
	r.SetConfig(cfg)
	return r, clock
}

// With paid_fallback on, paid credentials are still the last resort: a ranked
// subscription account wins, an unranked (recently probed) subscription account
// wins, an account that needs a probe wins, and only an all-paid pool picks paid.
func TestPaidFallbackOptInIsLastResort(t *testing.T) {
	r, clock := paidOptIn(t)
	key := claudeCandidates("key")[0]
	key.Attributes = map[string]string{"auth_kind": "apikey"}
	credits := claudeHeaders(clock.Now(), &cwin{1.0, 2 * time.Hour}, &cwin{0.5, day})
	credits.Set("Anthropic-Ratelimit-Unified-Overage-Status", "allowed")
	observeClaude(r, "credits", credits)

	// Eligible subscription beats paid.
	observeClaude(r, "sub", claudeHeaders(clock.Now(), &cwin{0.9, 2 * time.Hour}, &cwin{0.9, day}))
	resp, d := r.Pick(claudePick(append(claudeCandidates("credits", "sub"), key)...))
	mustSelect(t, resp, d, "sub")

	// A subscription account that only needs a probe beats paid.
	resp, d = r.Pick(claudePick(append(claudeCandidates("credits", "fresh"), key)...))
	mustSelect(t, resp, d, "fresh")
	if !strings.HasPrefix(d.Reason, "probe") {
		t.Fatalf("reason = %q", d.Reason)
	}
	// Just probed, no data yet: still a subscription account, still preferred.
	resp, d = r.Pick(claudePick(append(claudeCandidates("credits", "fresh"), key)...))
	mustSelect(t, resp, d, "fresh")
	if v := candidate(t, d, "fresh"); v.State != stateUnknown {
		t.Fatalf("fresh = %+v", v)
	}

	// All paid: picked, and recorded as paid fallback.
	resp, d = r.Pick(claudePick(append(claudeCandidates("credits"), key)...))
	if !resp.Handled || resp.Reject || resp.AuthID == "" || !strings.Contains(d.Reason, "paid fallback") {
		t.Fatalf("all-paid pool: resp=%+v decision=%+v", resp, d)
	}
	// Key-only pool with opt-in: the key is used.
	resp, d = r.Pick(claudePick(key))
	mustSelect(t, resp, d, "key")
	// Paid next to a blocked account: paid wins over rejecting.
	observeClaude(r, "spent", claudeHeaders(clock.Now(), &cwin{1.0, time.Hour}, &cwin{0.5, day}))
	resp, d = r.Pick(claudePick(claudeCandidates("credits", "spent")...))
	mustSelect(t, resp, d, "credits")
}

// Codex has no extra usage, but a Codex API key is paid in the same way.
func TestPaidFallbackCoversCodexKeys(t *testing.T) {
	r, _ := newTestRouter()
	cands := codexCandidates("key")
	cands[0].Attributes = map[string]string{"auth_kind": "apikey"}
	resp, d := r.Pick(pickReq(cands...))
	if !resp.Reject || resp.RejectCode != RejectCodePaidOnly {
		t.Fatalf("resp=%+v decision=%+v", resp, d)
	}
}

// Providers without a quota parser keep the old behaviour: an API key there is
// just a credential the plugin cannot rank, so the pick is delegated.
func TestUnsupportedProviderKeysStillDelegate(t *testing.T) {
	r, _ := newTestRouter()
	resp, d := r.Pick(pluginapi.SchedulerPickRequest{Provider: "gemini", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "g1", Provider: "gemini", Attributes: map[string]string{"auth_kind": "apikey"}},
	}})
	if resp.Reject || d.Outcome != OutcomeDelegated {
		t.Fatalf("resp=%+v decision=%+v", resp, d)
	}
}

// A bound session on an account that has become paid moves to a usable
// subscription account and then stays there, even after the old account recovers.
func TestAffinityLeavesPaidAccountAndSticks(t *testing.T) {
	r, clock := paidOptIn(t)
	req := claudePick(claudeCandidates("a", "b")...)
	req.Options.Metadata = map[string]any{"canonical_session_id": "s-paid"}
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.1, 2 * time.Hour}, &cwin{0.1, day}))
	observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.5, 2 * time.Hour}, &cwin{0.5, day}))
	resp, d := r.Pick(req)
	mustSelect(t, resp, d, "a")

	// a runs dry but extra usage keeps it serving: paid. The session must not
	// keep billing a while b has subscription quota.
	clock.Advance(time.Minute)
	paid := claudeHeaders(clock.Now(), &cwin{1.0, 2 * time.Hour}, &cwin{0.5, day})
	paid.Set("Anthropic-Ratelimit-Unified-Overage-Status", "allowed")
	observeClaude(r, "a", paid)
	resp, d = r.Pick(req)
	mustSelect(t, resp, d, "b")

	// a recovers; the session stays on b, whose cache it has been building.
	clock.Advance(time.Minute)
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.0, 5 * time.Hour}, &cwin{0.5, day}))
	resp, d = r.Pick(req)
	mustSelect(t, resp, d, "b")
	if d.Reason != "session affinity" {
		t.Fatalf("reason = %q", d.Reason)
	}

	// With paid_fallback on and nothing else usable, a paid binding is kept.
	clock.Advance(time.Minute)
	observeClaude(r, "b", paid)
	observeClaude(r, "a", paid)
	resp, d = r.Pick(req)
	mustSelect(t, resp, d, "b")
}

// Default config: a paid binding is dropped and the pick rejected rather than
// billed, even though the session was bound to that account.
func TestAffinityNeverRetainsPaidByDefault(t *testing.T) {
	r, clock := newTestRouter()
	req := claudePick(claudeCandidates("a")...)
	req.Options.Metadata = map[string]any{"canonical_session_id": "s-default"}
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.5, 2 * time.Hour}, &cwin{0.5, day}))
	resp, d := r.Pick(req)
	mustSelect(t, resp, d, "a")
	clock.Advance(time.Minute)
	paid := claudeHeaders(clock.Now(), &cwin{1.0, 2 * time.Hour}, &cwin{0.5, day})
	paid.Set("Anthropic-Ratelimit-Unified-Overage-Status", "allowed")
	observeClaude(r, "a", paid)
	resp, d = r.Pick(req)
	if !resp.Reject || resp.RejectCode != RejectCodePaidOnly {
		t.Fatalf("resp=%+v decision=%+v", resp, d)
	}
}
