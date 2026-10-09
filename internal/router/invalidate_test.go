package router

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// POST /quota-router/invalidate drops the plugin's own quota snapshot and probe
// marker for one credential, named by its hashed label. It exists for the
// moment after an operator has reset an account's allowance upstream: the
// plugin would otherwise keep the account blocked until the reset time it
// cached. It touches nothing at the host and leaves session bindings alone.

func managementPost(t *testing.T, h *Handler, path string, body []byte) pluginapi.ManagementResponse {
	t.Helper()
	env := call(t, h, "management.handle", pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    path,
		Headers: http.Header{"Content-Type": {"application/json"}},
		Body:    body,
	})
	if !env.OK {
		t.Fatalf("management.handle: %+v", env.Error)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestInvalidateDropsSnapshotAndKeepsBindings(t *testing.T) {
	r, clock := newTestRouter()
	h := &Handler{Router: r}
	sess := claudePick(claudeCandidates("a", "b")...)
	sess.Options.Metadata = map[string]any{"canonical_session_id": "s-inv"}
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{0.2, 3 * day}))
	observeClaude(r, "b", claudeHeaders(clock.Now(), &cwin{0.5, 4 * time.Hour}, &cwin{0.5, 3 * day}))
	resp, d := r.Pick(sess)
	mustSelect(t, resp, d, "a")

	// a's weekly window is reported spent for five days. The session fails over.
	clock.Advance(time.Minute)
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.2, 4 * time.Hour}, &cwin{1.0, 5 * day}))
	resp, d = r.Pick(sess)
	mustSelect(t, resp, d, "b")
	r.Store().MarkProbed("a", clock.Now())

	// The operator resets the allowance upstream and tells the plugin.
	body, _ := json.Marshal(map[string]string{"auth": authLabel("a")})
	mr := managementPost(t, h, "/v0/management"+InvalidatePath, body)
	if mr.StatusCode != http.StatusOK || mr.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d headers=%v body=%s", mr.StatusCode, mr.Headers, mr.Body)
	}
	var out InvalidateResponse
	if err := json.Unmarshal(mr.Body, &out); err != nil {
		t.Fatal(err)
	}
	if !out.PluginOnly || out.HostCooldownCleared || !out.SnapshotDropped || !out.ProbeMarkerDropped || out.Auth != authLabel("a") {
		t.Fatalf("response = %+v", out)
	}
	if strings.Contains(string(mr.Body), `"a"`) {
		t.Fatalf("raw auth ID in response: %s", mr.Body)
	}
	if _, ok := r.Store().Snapshot("a"); ok {
		t.Fatal("snapshot kept")
	}
	if !r.Store().ProbedAt("a").IsZero() {
		t.Fatal("probe marker kept")
	}

	// The failed-over session keeps its new binding: no bounce-back.
	resp, d = r.Pick(sess)
	mustSelect(t, resp, d, "b")
	if d.Reason != "session affinity" {
		t.Fatalf("reason = %q", d.Reason)
	}
	// A new session probes a, which is how fresh headers arrive.
	resp, d = r.Pick(claudePick(claudeCandidates("a", "b")...))
	mustSelect(t, resp, d, "a")
	if !strings.HasPrefix(d.Reason, "probe") {
		t.Fatalf("reason = %q", d.Reason)
	}
	// Fresh headers rank it again.
	observeClaude(r, "a", claudeHeaders(clock.Now(), &cwin{0.0, 5 * time.Hour}, &cwin{0.0, 7 * day}))
	if _, d := r.Pick(claudePick(claudeCandidates("a", "b")...)); candidate(t, d, "a").State != stateEligible {
		t.Fatalf("a = %+v", candidate(t, d, "a"))
	}
}

func TestInvalidateRejectsBadRequests(t *testing.T) {
	r, _ := newTestRouter()
	h := &Handler{Router: r}
	observe(r, "known", 50, time.Hour)
	path := "/v0/management" + InvalidatePath

	if got := managementGet(t, h, http.MethodGet, path).StatusCode; got != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d", got)
	}
	for name, body := range map[string][]byte{
		"not json":   []byte("{"),
		"no auth":    []byte(`{}`),
		"blank auth": []byte(`{"auth":"  "}`),
		"wrong type": []byte(`{"auth":5}`),
	} {
		if mr := managementPost(t, h, path, body); mr.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d body=%s", name, mr.StatusCode, mr.Body)
		}
	}
	// Unknown label, and a raw ID while reveal_auth_ids is off.
	for name, auth := range map[string]string{"unknown": authLabel("nobody"), "raw id": "known"} {
		body, _ := json.Marshal(map[string]string{"auth": auth})
		if mr := managementPost(t, h, path, body); mr.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d body=%s", name, mr.StatusCode, mr.Body)
		}
	}
	if _, ok := r.Store().Snapshot("known"); !ok {
		t.Fatal("a rejected request dropped a snapshot")
	}
	// With reveal_auth_ids on, the raw ID is accepted as well.
	cfg := DefaultConfig()
	cfg.RevealAuthIDs = true
	r.SetConfig(cfg)
	body, _ := json.Marshal(map[string]string{"auth": "known"})
	if mr := managementPost(t, h, path, body); mr.StatusCode != http.StatusOK {
		t.Fatalf("raw id with reveal on: status = %d body=%s", mr.StatusCode, mr.Body)
	}
}

// With reveal_auth_ids on, a label can match one credential by hash and
// another whose raw ID happens to equal that label. That is refused.
func TestInvalidateRefusesAmbiguousLabel(t *testing.T) {
	r, _ := newTestRouter()
	cfg := DefaultConfig()
	cfg.RevealAuthIDs = true
	r.SetConfig(cfg)
	h := &Handler{Router: r}
	observe(r, "x", 50, time.Hour)
	observe(r, authLabel("x"), 50, time.Hour)
	body, _ := json.Marshal(map[string]string{"auth": authLabel("x")})
	mr := managementPost(t, h, "/v0/management"+InvalidatePath, body)
	if mr.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d body=%s", mr.StatusCode, mr.Body)
	}
	if _, ok := r.Store().Snapshot("x"); !ok {
		t.Fatal("ambiguous request dropped a snapshot")
	}
}

func TestInvalidateRouteIsRegistered(t *testing.T) {
	h := &Handler{Router: New()}
	env := call(t, h, "management.register", map[string]any{"BasePath": "/v0/management"})
	var reg struct {
		Routes []struct {
			Method string
			Path   string
		}
	}
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rt := range reg.Routes {
		if rt.Method == http.MethodPost && rt.Path == InvalidatePath {
			found = true
		}
	}
	if !found {
		t.Fatalf("routes = %+v", reg.Routes)
	}
}
