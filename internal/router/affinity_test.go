package router

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Cache protection: a session that is building a prompt cache on one account
// stays there while that account can still serve it. Only exhaustion, a
// provider rejection, a disabled credential, or the host dropping the
// candidate moves it. Scores, probes and the new-session headroom do not.

func sessionReq(ids ...string) pluginapi.SchedulerPickRequest {
	req := claudePick(claudeCandidates(ids...)...)
	req.Options.Metadata = map[string]any{"canonical_session_id": "s-cache"}
	return req
}

func TestSessionAffinitySlidesWithActivity(t *testing.T) {
	r, clock := newTestRouter()
	ttl := r.Config().SessionAffinityTTL
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
	resp, d := r.Pick(sessionReq("a", "b"))
	mustSelect(t, resp, d, "a")

	// Keep the session active just inside the TTL, three times over. Total
	// elapsed time is far beyond one TTL; the binding must still hold.
	for i := range 3 {
		clock.Advance(ttl - time.Minute)
		observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.9, 4 * time.Hour}, &cwin{0.9, 3 * day}))
		observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.1, 4 * time.Hour}, &cwin{0.1, 3 * day}))
		resp, d = r.Pick(sessionReq("a", "b"))
		if resp.AuthID != "a" || d.Reason != "session affinity" {
			t.Fatalf("round %d: picked %q (%s), want a", i, resp.AuthID, d.Reason)
		}
	}
	// An idle gap past the TTL does release it.
	clock.Advance(ttl + time.Minute)
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.9, 4 * time.Hour}, &cwin{0.9, 3 * day}))
	observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.1, 4 * time.Hour}, &cwin{0.1, 3 * day}))
	if resp, d := r.Pick(sessionReq("a", "b")); resp.AuthID != "b" {
		t.Fatalf("after idle gap picked %q (%s), want b", resp.AuthID, d.Reason)
	}
}

func TestSessionIsNotMigratedByBetterScore(t *testing.T) {
	r, clock := newTestRouter()
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.3, 4 * time.Hour}, &cwin{0.3, 3 * day}))
	observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.6, 4 * time.Hour}, &cwin{0.6, 3 * day}))
	resp, d := r.Pick(sessionReq("a", "b"))
	mustSelect(t, resp, d, "a")
	// b becomes overwhelmingly better, far past switch_margin. a still serves.
	clock.Advance(time.Minute)
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.9, 4 * time.Hour}, &cwin{0.9, 3 * day}))
	observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.0, 4 * time.Hour}, &cwin{0.0, 3 * day}))
	resp, d = r.Pick(sessionReq("a", "b"))
	mustSelect(t, resp, d, "a")
	// A new session goes to b.
	if resp, _ := r.Pick(claudePick(claudeCandidates("a", "b")...)); resp.AuthID != "b" {
		t.Fatalf("new session picked %q", resp.AuthID)
	}
}

// The first request of a session often lands on an account without quota data
// (a probe). That pick is a real request that starts the cache, so it binds,
// and the binding survives the data that arrives afterwards.
func TestProbePickBindsAndSurvivesBetterAlternatives(t *testing.T) {
	r, clock := newTestRouter()
	resp, d := r.Pick(sessionReq("x", "y"))
	if resp.AuthID == "" || !strings.HasPrefix(d.Reason, "probe") {
		t.Fatalf("resp=%+v decision=%+v", resp, d)
	}
	first := resp.AuthID
	other := "y"
	if first == "y" {
		other = "x"
	}
	clock.Advance(time.Second)
	observeClaude(r, first, claudeHeaders(clock.Now(), &cwin{0.8, 4 * time.Hour}, &cwin{0.8, 3 * day}))
	observeClaude(r, other, claudeHeaders(clock.Now(), &cwin{0.0, 4 * time.Hour}, &cwin{0.0, 3 * day}))
	resp, d = r.Pick(sessionReq("x", "y"))
	if resp.AuthID != first || d.Reason != "session affinity" {
		t.Fatalf("picked %q (%s), want %s", resp.AuthID, d.Reason, first)
	}
}

func TestFailoverStaysOnReplacementAfterRecovery(t *testing.T) {
	r, clock := newTestRouter()
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
	resp, d := r.Pick(sessionReq("a", "b"))
	mustSelect(t, resp, d, "a")

	// a is rejected by the provider: credential-wide 5h rejection.
	clock.Advance(time.Minute)
	rej := claudeHeaders(clock.Now(), &cwin{1.0, 2 * time.Hour}, &cwin{0.2, 3 * day})
	rej.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	rej.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	observeClaude(r, "a", rej)
	resp, d = r.Pick(sessionReq("a", "b"))
	mustSelect(t, resp, d, "b")

	// a recovers fully. The session has moved its cache to b and stays there.
	clock.Advance(time.Minute)
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.0, 5 * time.Hour}, &cwin{0.0, 3 * day}))
	for range 3 {
		resp, d = r.Pick(sessionReq("a", "b"))
		if resp.AuthID != "b" || d.Reason != "session affinity" {
			t.Fatalf("picked %q (%s), want b", resp.AuthID, d.Reason)
		}
	}
}

// A bound session keeps its account below the new-session headroom, until the
// quota is actually gone. New sessions are kept off such an account.
func TestBoundSessionSurvivesBelowNewSessionHeadroom(t *testing.T) {
	r, clock := newTestRouter()
	if r.Config().SessionMinimumHeadroom != 0 {
		t.Fatalf("session_minimum_headroom default = %v, want 0", r.Config().SessionMinimumHeadroom)
	}
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
	resp, d := r.Pick(sessionReq("a", "b"))
	mustSelect(t, resp, d, "a")

	// 1% left: below minimum_headroom (2%) but above zero.
	clock.Advance(time.Minute)
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.99, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	resp, d = r.Pick(sessionReq("a", "b"))
	mustSelect(t, resp, d, "a")
	if !strings.Contains(d.Reason, "session") {
		t.Fatalf("reason = %q", d.Reason)
	}
	// A new session must not start on a.
	if resp, d := r.Pick(claudePick(claudeCandidates("a", "b")...)); resp.AuthID != "b" || candidate(t, d, "a").State != stateBlocked {
		t.Fatalf("new session: picked %q, a=%+v", resp.AuthID, candidate(t, d, "a"))
	}

	// True exhaustion moves the bound session.
	clock.Advance(time.Minute)
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{1.0, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	resp, d = r.Pick(sessionReq("a", "b"))
	mustSelect(t, resp, d, "b")
}

// The session headroom relaxes only the headroom threshold. Rejections, limit
// flags, disabled credentials and host status still move the session.
func TestBoundSessionStillLeavesOnHardSignals(t *testing.T) {
	bindToA := func(t *testing.T) (*Router, *fakeClock) {
		t.Helper()
		r, clock := newTestRouter()
		observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
		observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
		resp, d := r.Pick(sessionReq("a", "b"))
		mustSelect(t, resp, d, "a")
		clock.Advance(time.Minute)
		return r, clock
	}
	t.Run("unified rejection without window data", func(t *testing.T) {
		r, clock := bindToA(t)
		h := claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, nil)
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		h.Set("Anthropic-Ratelimit-Unified-Reset", "0")
		h.Del("Anthropic-Ratelimit-Unified-5h-Status")
		h.Del("Anthropic-Ratelimit-Unified-5h-Utilization")
		h.Del("Anthropic-Ratelimit-Unified-5h-Reset")
		observeClaude(r, "a", h)
		resp, d := r.Pick(sessionReq("a", "b"))
		mustSelect(t, resp, d, "b")
	})
	t.Run("host disabled", func(t *testing.T) {
		r, clock := bindToA(t)
		r.Store().ApplyHostStates(map[string]HostAuthState{"a": {Disabled: true}, "b": {}}, clock.Now())
		resp, d := r.Pick(sessionReq("a", "b"))
		mustSelect(t, resp, d, "b")
	})
	t.Run("candidate status disabled", func(t *testing.T) {
		r, _ := bindToA(t)
		req := sessionReq("a", "b")
		req.Candidates[0].Status = "disabled"
		resp, d := r.Pick(req)
		mustSelect(t, resp, d, "b")
	})
	t.Run("host dropped the candidate", func(t *testing.T) {
		r, _ := bindToA(t)
		resp, d := r.Pick(sessionReq("b"))
		mustSelect(t, resp, d, "b")
	})
}

// Review finding: an account with quota left but below the new-session
// headroom, whose headers also say extra usage is allowed, is not exhausted.
// It must classify as a headroom exclusion (blocked for new sessions, kept for
// a bound session), not as paid. Paid stays reserved for actual exhaustion.
func TestBelowHeadroomWithOverageAllowedIsNotPaid(t *testing.T) {
	lowButPositive := func(now time.Time) http.Header {
		h := claudeHeaders(now, &cwin{0.99, 4 * time.Hour}, &cwin{0.2, 3 * day})
		h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "allowed")
		return h
	}
	exhausted := func(now time.Time) http.Header {
		h := claudeHeaders(now, &cwin{1.0, 4 * time.Hour}, &cwin{0.2, 3 * day})
		h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "allowed")
		return h
	}

	t.Run("mixed pool", func(t *testing.T) {
		r, clock := newTestRouter()
		observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
		observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
		resp, d := r.Pick(sessionReq("a", "b"))
		mustSelect(t, resp, d, "a")

		clock.Advance(time.Minute)
		observeClaude(r, "a", lowButPositive(clock.Now()))
		// The bound session keeps its cache on a.
		resp, d = r.Pick(sessionReq("a", "b"))
		mustSelect(t, resp, d, "a")
		if !strings.Contains(d.Reason, "kept for bound session") {
			t.Fatalf("reason = %q", d.Reason)
		}
		// A new session goes to b, and a is a headroom exclusion, not paid.
		resp, d = r.Pick(claudePick(claudeCandidates("a", "b")...))
		mustSelect(t, resp, d, "b")
		if v := candidate(t, d, "a"); v.State != stateBlocked || !strings.Contains(v.Reason, "headroom") {
			t.Fatalf("a = %+v", v)
		}
		// Actual exhaustion with extra usage allowed is paid and, by default,
		// moves the session off a.
		clock.Advance(time.Minute)
		observeClaude(r, "a", exhausted(clock.Now()))
		resp, d = r.Pick(sessionReq("a", "b"))
		mustSelect(t, resp, d, "b")
		if v := candidate(t, d, "a"); v.State != statePaid {
			t.Fatalf("a = %+v", v)
		}
	})

	t.Run("single bound account", func(t *testing.T) {
		r, clock := newTestRouter()
		observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
		resp, d := r.Pick(sessionReq("a"))
		mustSelect(t, resp, d, "a")

		clock.Advance(time.Minute)
		observeClaude(r, "a", lowButPositive(clock.Now()))
		for range 3 {
			resp, d = r.Pick(sessionReq("a"))
			mustSelect(t, resp, d, "a")
		}
		// A new session has nothing usable: rejected as out of quota, not as paid.
		resp, d = r.Pick(claudePick(claudeCandidates("a")...))
		if !resp.Reject || resp.RejectCode != RejectCode {
			t.Fatalf("new session: resp=%+v decision=%+v", resp, d)
		}
		// Actual exhaustion: paid, rejected by default even for the bound session.
		clock.Advance(time.Minute)
		observeClaude(r, "a", exhausted(clock.Now()))
		resp, d = r.Pick(sessionReq("a"))
		if !resp.Reject || resp.RejectCode != RejectCodePaidOnly {
			t.Fatalf("exhausted: resp=%+v decision=%+v", resp, d)
		}
	})
}

// Review finding: a bound account with positive quota below the new-session
// headroom, a future reset, and an observation older than stale_after is
// usable for the bound session. Re-assessed at the session headroom it is
// "probe"/"unknown" (stale), not "eligible"; the session must still keep it.
// Known exhaustion stays blocked however stale the headers are.
func TestBoundSessionKeepsStaleBelowHeadroomAccount(t *testing.T) {
	stale := DefaultConfig().StaleAfter
	for name, overage := range map[string]string{"overage allowed": "allowed", "overage rejected": "rejected"} {
		lowStale := func(now time.Time) http.Header {
			h := claudeHeaders(now, &cwin{0.99, 4 * time.Hour}, &cwin{0.2, 3 * day})
			h.Set("Anthropic-Ratelimit-Unified-Overage-Status", overage)
			return h
		}
		t.Run(name+" mixed pool", func(t *testing.T) {
			r, clock := newTestRouter()
			observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
			observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
			resp, d := r.Pick(sessionReq("a", "b"))
			mustSelect(t, resp, d, "a")

			clock.Advance(time.Minute)
			observeClaude(r, "a", lowStale(clock.Now()))
			clock.Advance(stale + time.Minute)
			observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
			resp, d = r.Pick(sessionReq("a", "b"))
			mustSelect(t, resp, d, "a")
			if !strings.Contains(d.Reason, "kept for bound session") {
				t.Fatalf("reason = %q", d.Reason)
			}
			// New sessions still avoid a.
			if resp, _ := r.Pick(claudePick(claudeCandidates("a", "b")...)); resp.AuthID != "b" {
				t.Fatalf("new session picked %q", resp.AuthID)
			}
		})
		t.Run(name+" single bound account", func(t *testing.T) {
			r, clock := newTestRouter()
			observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
			resp, d := r.Pick(sessionReq("a"))
			mustSelect(t, resp, d, "a")
			clock.Advance(time.Minute)
			observeClaude(r, "a", lowStale(clock.Now()))
			clock.Advance(stale + time.Minute)
			resp, d = r.Pick(sessionReq("a"))
			mustSelect(t, resp, d, "a")
			if resp, d := r.Pick(claudePick(claudeCandidates("a")...)); !resp.Reject || resp.RejectCode != RejectCode {
				t.Fatalf("new session: resp=%+v decision=%+v", resp, d)
			}
		})
	}

	t.Run("stale true zero stays blocked", func(t *testing.T) {
		r, clock := newTestRouter()
		observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
		observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
		resp, d := r.Pick(sessionReq("a", "b"))
		mustSelect(t, resp, d, "a")
		clock.Advance(time.Minute)
		zero := claudeHeaders(clock.Now(), &cwin{1.0, 4 * time.Hour}, &cwin{0.2, 3 * day})
		zero.Set("Anthropic-Ratelimit-Unified-Overage-Status", "allowed")
		observeClaude(r, "a", zero)
		clock.Advance(stale + time.Minute)
		observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
		resp, d = r.Pick(sessionReq("a", "b"))
		mustSelect(t, resp, d, "b")
		if v := candidate(t, d, "a"); v.State != stateBlocked {
			t.Fatalf("a = %+v", v)
		}
	})
}

func TestSessionMinimumHeadroomConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte("session_minimum_headroom: 0.01\nminimum_headroom: 0.05\n"))
	if err != nil || cfg.SessionMinimumHeadroom != 0.01 {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	for _, bad := range []string{"session_minimum_headroom: 1", "session_minimum_headroom: -0.1", "session_minimum_headroom: .nan", "session_minimum_headroom: 0.5\nminimum_headroom: 0.1"} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A model-scoped failure sets the host's aggregate auth status to "error"
// (MarkResult in sdk/cliproxy/auth/conductor_cooldown.go) while the host keeps
// offering the credential for every model it did not cool down. Measured end
// to end: a 429 on the haiku helper left the account offered for opus with
// status "error". The offer is the host's per-model verdict, so the bound opus
// session must stay, and the account still ranks for new sessions.
func TestOfferedErrorStatusKeepsBoundSession(t *testing.T) {
	r, clock := newTestRouter()
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
	resp, d := r.Pick(sessionReq("a", "b"))
	mustSelect(t, resp, d, "a")

	req := sessionReq("a", "b")
	req.Candidates[0].Status = "error"
	resp, d = r.Pick(req)
	mustSelect(t, resp, d, "a")
	if d.Reason != "session affinity" {
		t.Fatalf("reason = %q", d.Reason)
	}
	fresh := claudePick(claudeCandidates("a", "b")...)
	fresh.Candidates[0].Status = "error"
	if resp, d := r.Pick(fresh); resp.AuthID != "a" {
		t.Fatalf("new session picked %q: %+v", resp.AuthID, d.Candidates)
	}
}
