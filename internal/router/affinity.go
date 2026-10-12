package router

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxAffinityEntries = 10000

// sessionMetadataKeys are host-populated scheduler metadata keys that carry a
// session identity (sdk/cliproxy/executor/types.go upstream), strongest first.
var sessionMetadataKeys = []string{
	"canonical_session_id",
	"execution_session_id",
	"lcp_affinity_session_id",
	"derived_session_id",
}

// sessionHeaders are client headers the host itself treats as session IDs
// (sdk/cliproxy/session/info.go upstream). Per-request IDs are excluded.
var sessionHeaders = []string{
	"X-Claude-Code-Session-Id",
	"Session-Id",
	"Session_id",
	"X-Session-Id",
	"X-Session-Affinity",
	"Thread-Id",
	"X-Thread-Id",
	"X-Conversation-Id",
}

// sessionKey derives an opaque key for the request's session. The raw session
// identifier is hashed immediately and never stored or logged.
func sessionKey(headers map[string][]string, metadata map[string]any) string {
	raw := ""
	for _, key := range sessionMetadataKeys {
		if v, ok := metadata[key].(string); ok && strings.TrimSpace(v) != "" {
			raw = "m:" + strings.TrimSpace(v)
			break
		}
	}
	if raw == "" && len(headers) > 0 {
		h := http.Header(headers)
		for _, name := range sessionHeaders {
			if v := firstHeader(h, name); v != "" {
				raw = "h:" + v
				break
			}
		}
	}
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

// bindingKey scopes a session key to one model. Prompt caches belong to one
// account and one model, so each model a session uses keeps its own binding:
// when one model has to move (the host excluded the bound account for that
// model only, or another provider serves it) the others stay where their
// caches are. A thinking suffix such as "(high)" does not start a new binding.
func bindingKey(session, model string) string {
	if session == "" {
		return ""
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if base, _, ok := strings.Cut(model, "("); ok {
		model = strings.TrimSpace(base)
	}
	sum := sha256.Sum256([]byte(session + "\x00" + model))
	return hex.EncodeToString(sum[:16])
}

// firstHeader matches case-insensitively because the map crossed a JSON
// boundary and may not use canonical keys.
func firstHeader(h http.Header, name string) string {
	for key, values := range h {
		if strings.EqualFold(key, name) {
			for _, v := range values {
				if v = strings.TrimSpace(v); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// authDigest is the full SHA-256 of an auth ID, in hex. It is what the state
// file stores instead of the ID, which is often a file name with an e-mail
// address in it. It is not reversible; a binding restored from disk is matched
// by digesting each offered candidate.
func authDigest(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// binding is one session's credential. authID is empty for a binding restored
// from disk until a pick resolves it against a live candidate.
type binding struct {
	authID  string
	digest  string
	expires time.Time
}

// matches reports whether the binding refers to the given auth ID.
func (b binding) matches(id string) bool {
	if b.authID != "" {
		return b.authID == id
	}
	return b.digest != "" && b.digest == authDigest(id)
}

// affinity maps binding keys (session and model) to the auth they were last
// routed to.
type affinity struct {
	mu       sync.Mutex
	bindings map[string]binding
	// gen counts mutations so a save can be skipped when nothing changed.
	gen uint64
}

func newAffinity() *affinity {
	return &affinity{bindings: make(map[string]binding)}
}

func (a *affinity) lookup(key string, now time.Time) (binding, bool) {
	if key == "" {
		return binding{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.bindings[key]
	if !ok || now.After(b.expires) {
		return binding{}, false
	}
	return b, true
}

func (a *affinity) bind(key, authID string, now time.Time, ttl time.Duration) {
	if key == "" || authID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.bindings[key]; !exists && len(a.bindings) >= maxAffinityEntries {
		a.evictLocked(now)
	}
	a.bindings[key] = binding{authID: authID, digest: authDigest(authID), expires: now.Add(ttl)}
	a.gen++
}

// evictLocked drops expired bindings, or the one closest to expiry if none are.
func (a *affinity) evictLocked(now time.Time) {
	oldestKey := ""
	var oldest time.Time
	for key, b := range a.bindings {
		if now.After(b.expires) {
			delete(a.bindings, key)
			continue
		}
		if oldestKey == "" || b.expires.Before(oldest) {
			oldestKey, oldest = key, b.expires
		}
	}
	if len(a.bindings) >= maxAffinityEntries && oldestKey != "" {
		delete(a.bindings, oldestKey)
	}
	a.gen++
}

func (a *affinity) prune(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, b := range a.bindings {
		if now.After(b.expires) {
			delete(a.bindings, key)
			a.gen++
		}
	}
}

func (a *affinity) size() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.bindings)
}

// snapshot returns the live bindings in their on-disk form and the mutation
// generation they correspond to.
func (a *affinity) snapshot(now time.Time) ([]stateBinding, uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]stateBinding, 0, len(a.bindings))
	for key, b := range a.bindings {
		if now.After(b.expires) || b.digest == "" {
			continue
		}
		out = append(out, stateBinding{Session: key, Auth: b.digest, Expires: b.expires})
	}
	return out, a.gen
}

// restore adds bindings read from disk for sessions not already bound live.
// Expired entries are skipped. It returns how many were added.
func (a *affinity) restore(entries []stateBinding, now time.Time) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	added := 0
	for _, e := range entries {
		if now.After(e.Expires) || e.Session == "" || e.Auth == "" {
			continue
		}
		if _, live := a.bindings[e.Session]; live {
			continue
		}
		if len(a.bindings) >= maxAffinityEntries {
			break
		}
		a.bindings[e.Session] = binding{digest: e.Auth, expires: e.Expires}
		added++
	}
	if added > 0 {
		a.gen++
	}
	return added
}
