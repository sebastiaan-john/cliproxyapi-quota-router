// Package router implements quota-aware credential selection for the
// CLIProxyAPI scheduler capability.
package router

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const recentDecisions = 20

// Candidate states shown in diagnostics.
const (
	stateEligible = "eligible" // fresh quota data, above headroom
	stateProbe    = "probe"    // quota-aware provider without fresh data
	stateUnknown  = "unknown"  // no usable data; only used by the fallback
	stateBlocked  = "blocked"  // excluded from selection
	// statePaid marks a credential that is positively known to bill per request:
	// an API key on a quota-aware provider, or a subscription account that is
	// out of quota but still served from extra usage. It is only ever chosen
	// when paid_fallback is enabled and nothing else is usable.
	statePaid = "paid"
)

// Scheduler reject codes. The host turns them into the client-facing error code.
const (
	// RejectCode is returned when no candidate is usable.
	RejectCode = "quota_exhausted"
	// RejectCodePaidOnly is returned when the only usable candidates would be
	// billed per request and paid_fallback is off.
	RejectCodePaidOnly = "paid_fallback_disabled"
)

// Decision outcomes.
const (
	OutcomeSelected  = "selected"
	OutcomeDelegated = "delegated"
	OutcomeUnhandled = "unhandled"
	OutcomeRejected  = "rejected"
)

// LogFunc receives one structured diagnostic line.
type LogFunc func(level, msg string, fields map[string]any)

// CandidateView explains how one candidate was classified. Auth is a label,
// never a credential, and is a hash of the auth ID unless reveal_auth_ids is set.
type CandidateView struct {
	Auth      string    `json:"auth"`
	Provider  string    `json:"provider,omitempty"`
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	Score     float64   `json:"score,omitempty"`
	Remaining *float64  `json:"remaining,omitempty"`
	NextReset time.Time `json:"next_reset,omitzero"`

	id string
}

// Decision records why a pick ended the way it did.
type Decision struct {
	Time       time.Time       `json:"time"`
	Mode       Mode            `json:"mode"`
	Provider   string          `json:"provider,omitempty"`
	Model      string          `json:"model,omitempty"`
	Outcome    string          `json:"outcome"`
	Selected   string          `json:"selected,omitempty"`
	Delegate   string          `json:"delegate,omitempty"`
	Reason     string          `json:"reason"`
	Session    bool            `json:"session,omitempty"`
	Candidates []CandidateView `json:"candidates"`
}

// Router holds configuration, cached quota data and selection state.
type Router struct {
	store    *Store
	registry *quota.Registry
	aff      *affinity
	now      func() time.Time
	log      LogFunc

	mu        sync.Mutex
	cfg       Config
	sticky    map[string]string
	rr        map[string]uint64
	decisions []Decision

	refreshMu   sync.Mutex
	lister      AuthLister
	refreshStop chan struct{}
	refreshDone chan struct{}

	// configMu serializes SetConfig transitions. persistMu guards the state
	// file bookkeeping and is taken before r.mu, never after it.
	configMu   sync.Mutex
	persistMu  sync.Mutex
	state      StateStatus
	stateSaved bool
	savedGen   uint64
	savedPath  string
}

// Option customizes a Router.
type Option func(*Router)

// WithClock overrides time.Now, for tests.
func WithClock(now func() time.Time) Option { return func(r *Router) { r.now = now } }

// WithLogger sets the diagnostic sink.
func WithLogger(log LogFunc) Option { return func(r *Router) { r.log = log } }

// WithAuthLister enables the periodic host auth refresh.
func WithAuthLister(l AuthLister) Option { return func(r *Router) { r.lister = l } }

// New returns a router using the default config and quota parsers.
func New(opts ...Option) *Router {
	registry := quota.DefaultRegistry()
	r := &Router{
		store:    NewStore(registry),
		registry: registry,
		aff:      newAffinity(),
		now:      time.Now,
		cfg:      DefaultConfig(),
		sticky:   make(map[string]string),
		rr:       make(map[string]uint64),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Config returns the active configuration.
func (r *Router) Config() Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

// SetConfig applies cfg in an order that keeps persisted bindings safe. A
// running refresher is stopped while the old configuration is still active,
// so its last save goes to the old destination. The new configuration is then
// published and, if state_file changed, the save bookkeeping is reset and the
// new file is loaded, all under the persistence lock, so no save can reach the
// new file before its bindings are restored. The refresher restarts after.
func (r *Router) SetConfig(cfg Config) {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	old := r.Config()
	restart := old.RefreshInterval != cfg.RefreshInterval && r.refreshing()
	if restart {
		r.StopRefresher()
	}
	r.persistMu.Lock()
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
	var loadErr error
	if cfg.StateFile != old.StateFile {
		r.stateSaved, r.savedGen, r.savedPath = false, 0, ""
		r.state = StateStatus{}
		loadErr = r.loadLocked(cfg)
	}
	r.persistMu.Unlock()
	if loadErr != nil && r.log != nil {
		r.log("warn", "quota-router state load failed", map[string]any{"error": loadErr.Error()})
	}
	if restart {
		r.StartRefresher()
	}
}

// Store exposes the quota cache.
func (r *Router) Store() *Store { return r.store }

// ObserveUsage feeds one usage record's response headers into the cache.
func (r *Router) ObserveUsage(rec pluginapi.UsageRecord) bool {
	return r.store.Observe(rec.AuthID, rec.Provider, rec.ResponseHeaders, r.now())
}

// Pick chooses a candidate for one request.
func (r *Router) Pick(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, Decision) {
	now := r.now()
	r.mu.Lock()
	cfg := r.cfg
	resp, d := r.pickLocked(req, now, cfg)
	r.decisions = append(r.decisions, d)
	if len(r.decisions) > recentDecisions {
		r.decisions = r.decisions[len(r.decisions)-recentDecisions:]
	}
	r.mu.Unlock()
	if cfg.LogDecisions {
		r.logDecision(d)
	}
	return resp, d
}

func (r *Router) pickLocked(req pluginapi.SchedulerPickRequest, now time.Time, cfg Config) (pluginapi.SchedulerPickResponse, Decision) {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" {
		provider = strings.Join(req.Providers, ",")
	}
	pool := provider + "|" + req.Model
	d := Decision{Time: now, Mode: cfg.Mode, Provider: provider, Model: req.Model}

	views := make([]CandidateView, 0, len(req.Candidates))
	byState := map[string][]int{}
	for _, c := range req.Candidates {
		v := r.classify(c, now, cfg)
		byState[v.State] = append(byState[v.State], len(views))
		views = append(views, v)
	}
	d.Candidates = views

	selected, reason := "", ""
	key := ""
	if cfg.SessionAffinity {
		key = sessionKey(req.Options.Headers, req.Options.Metadata)
		d.Session = key != ""
		if bound, ok := r.aff.lookup(key, now); ok {
			for i, v := range views {
				if !bound.matches(v.id) {
					continue
				}
				affinityReason := "session affinity"
				if v.State == stateBlocked && cfg.SessionMinimumHeadroom < cfg.MinimumHeadroom {
					// The new-session headroom keeps fresh sessions off a nearly
					// empty account. A session already cached there may run it
					// down to the session headroom instead. Any state the session
					// headroom leaves usable counts, including stale data that
					// would be probed; exhaustion, rejection and disabled stay
					// blocked at every headroom. byState still lists this index
					// as blocked; that only matters when nothing is selected, and
					// a selection is made right here.
					if sv := r.classifyWith(req.Candidates[i], now, cfg, cfg.SessionMinimumHeadroom); sv.State != stateBlocked {
						sv.Reason = "kept for bound session below new-session headroom"
						views[i], v = sv, sv
						affinityReason = "session affinity (kept for bound session below new-session headroom)"
					}
				}
				switch {
				case v.State == stateBlocked:
				case v.State == statePaid:
					// A session never keeps billing a credential while a
					// subscription account could serve it. With paid_fallback on
					// and nothing else usable, the existing binding is kept so the
					// session does not bounce between paid credentials.
					if cfg.PaidFallback && !anySubscriptionUsable(views) {
						selected, reason = v.id, "session affinity (paid, nothing else usable)"
					}
				default:
					selected, reason = v.id, affinityReason
				}
			}
		}
	}
	if selected == "" && len(byState[stateProbe]) > 0 {
		v := views[r.rotate("probe|"+pool, views, byState[stateProbe])]
		selected, reason = v.id, "probe: "+v.Reason
		r.store.MarkProbed(v.id, now)
	}
	if selected == "" && len(byState[stateEligible]) > 0 {
		selected, reason = r.best(pool, views, byState[stateEligible], cfg)
	}

	// A delegated or unhandled answer lets the host choose from the full list,
	// so it is only given when this plugin excluded nothing. Paid candidates
	// count as excluded: the host would happily pick one.
	excluded := len(byState[stateBlocked]) > 0 || len(byState[statePaid]) > 0

	resp := pluginapi.SchedulerPickResponse{}
	switch {
	case selected != "":
		resp = pluginapi.SchedulerPickResponse{AuthID: selected, Handled: true}
		d.Outcome, d.Selected, d.Reason = OutcomeSelected, r.label(selected, cfg), reason
		r.aff.bind(key, selected, now, cfg.SessionAffinityTTL)
	case len(byState[stateUnknown]) > 0:
		resp = r.fallback(pool, views, byState[stateUnknown], excluded, cfg, &d)
		if resp.AuthID != "" {
			r.aff.bind(key, resp.AuthID, now, cfg.SessionAffinityTTL)
		}
	case len(byState[statePaid]) > 0 && cfg.PaidFallback:
		// Last resort, by explicit opt-in only.
		i := r.rotate("paid|"+pool, views, byState[statePaid])
		id := views[i].id
		resp = pluginapi.SchedulerPickResponse{AuthID: id, Handled: true}
		d.Outcome, d.Selected = OutcomeSelected, r.label(id, cfg)
		d.Reason = fmt.Sprintf("paid fallback among %d billed candidates; no subscription quota usable", len(byState[statePaid]))
		r.aff.bind(key, id, now, cfg.SessionAffinityTTL)
	case len(byState[statePaid]) > 0:
		resp = pluginapi.SchedulerPickResponse{
			Handled:      true,
			Reject:       true,
			RejectCode:   RejectCodePaidOnly,
			RejectReason: "the only usable credentials are billed per request (API key or extra usage) and paid_fallback is off",
		}
		d.Outcome, d.Reason = OutcomeRejected, "only paid candidates remain and paid_fallback is off"
	case len(views) == 0:
		d.Outcome, d.Reason = OutcomeUnhandled, "no candidates"
	default:
		// Every candidate is blocked. Any answer other than a rejection would let
		// the host pick one of them.
		resp = pluginapi.SchedulerPickResponse{
			Handled:      true,
			Reject:       true,
			RejectCode:   RejectCode,
			RejectReason: "every candidate credential is out of quota, below minimum_headroom or disabled",
		}
		d.Outcome, d.Reason = OutcomeRejected, "every candidate is blocked"
	}

	return resp, d
}

func (r *Router) classify(c pluginapi.SchedulerAuthCandidate, now time.Time, cfg Config) CandidateView {
	return r.classifyWith(c, now, cfg, cfg.MinimumHeadroom)
}

// classifyWith classifies c with an explicit headroom threshold.
func (r *Router) classifyWith(c pluginapi.SchedulerAuthCandidate, now time.Time, cfg Config, headroom float64) CandidateView {
	v := CandidateView{id: c.ID, Auth: r.label(c.ID, cfg), Provider: c.Provider}
	block := func(reason string) CandidateView {
		v.State, v.Reason = stateBlocked, reason
		return v
	}
	switch strings.ToLower(c.Status) {
	case "disabled", "error":
		return block("host status " + strings.ToLower(c.Status))
	}
	// The host filters cooled-down and unavailable auths per model before calling
	// us (availableAuthsForSelector upstream), so an offered candidate is
	// available now. The cached host list adds only the disabled flag; a retry
	// time it reported earlier is not a veto, because the host may have reset
	// that cooldown since, or it may belong to another model.
	if hs, ok := r.store.HostState(c.ID, now, 3*cfg.RefreshInterval); ok && hs.Disabled {
		return block("disabled")
	}
	if !r.registry.Supports(c.Provider) {
		v.State, v.Reason = stateUnknown, "no quota parser for provider"
		return v
	}
	// API-key credentials reach the provider's public API, which bills every
	// request and sends no subscription quota headers. They are paid capacity:
	// never probed and never ranked against subscription accounts.
	if strings.EqualFold(strings.TrimSpace(c.Attributes["auth_kind"]), "apikey") {
		v.State, v.Reason = statePaid, "API key: billed per request"
		return v
	}
	unknown := func(reason string) CandidateView {
		v.State, v.Reason = stateProbe, reason
		if now.Sub(r.store.ProbedAt(c.ID)) < cfg.ProbeInterval {
			v.State, v.Reason = stateUnknown, "awaiting quota data from recent probe"
		}
		return v
	}
	snap, ok := r.store.Snapshot(c.ID)
	if !ok {
		return unknown("no quota data yet")
	}
	a := assess(snap, now, headroom, cfg.MinResetHorizon, cfg.StaleAfter)
	rem := a.Remaining
	v.Remaining, v.NextReset = &rem, a.NextReset
	if !a.BlockedUntil.IsZero() {
		until := a.BlockedUntil.UTC().Format(time.RFC3339)
		// Exhaustion is judged without any configured headroom: a window at
		// zero, a rejected window or a limit flag. Quota that is merely below
		// the configured headroom is a routing exclusion for new sessions; a
		// bound session may still use it, and it is never paid.
		exhausted := headroom == 0 || !assess(snap, now, 0, cfg.MinResetHorizon, cfg.StaleAfter).BlockedUntil.IsZero()
		if !exhausted {
			return block(fmt.Sprintf("below %.0f%% headroom until %s", headroom*100, until))
		}
		// Anthropic keeps serving an exhausted account with extra usage enabled,
		// billed to the account. That is paid capacity.
		if !snap.LimitReached && snap.ExtraUsage && now.Sub(snap.ExtraUsageObservedAt) <= cfg.StaleAfter {
			v.State, v.Reason = statePaid, "windows exhausted until "+until+", extra usage billed per request"
			return v
		}
		return block("out of quota until " + until)
	}
	if len(snap.Windows) == 0 {
		// Only a limit flag was reported, e.g. X-Codex-Allowed: true. That is not
		// enough to rank the credential.
		return unknown("no quota windows reported")
	}
	if a.Stale {
		return unknown("quota data stale")
	}
	v.State = stateEligible
	v.Score = score(cfg.Mode, a)
	return v
}

// anySubscriptionUsable reports whether some candidate could serve the request
// without billing: ranked, awaiting a probe, or unranked but not paid.
func anySubscriptionUsable(views []CandidateView) bool {
	for _, v := range views {
		switch v.State {
		case stateEligible, stateProbe, stateUnknown:
			return true
		}
	}
	return false
}

// best applies hysteresis and tie rotation among eligible candidates.
func (r *Router) best(pool string, views []CandidateView, idx []int, cfg Config) (string, string) {
	sort.SliceStable(idx, func(i, j int) bool {
		a, b := views[idx[i]], views[idx[j]]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.id < b.id
	})
	top := views[idx[0]].Score
	if cur, ok := r.sticky[pool]; ok {
		for _, i := range idx {
			if views[i].id == cur && views[i].Score*(1+cfg.SwitchMargin) >= top {
				return cur, fmt.Sprintf("kept current: within %.0f%% of best score", cfg.SwitchMargin*100)
			}
		}
	}
	tied := idx[:0:0]
	for _, i := range idx {
		if views[i].Score >= top*(1-cfg.TieTolerance) || nearlyEqual(views[i].Score, top) {
			tied = append(tied, i)
		}
	}
	chosen := views[r.rotate("tie|"+pool, views, tied)].id
	r.sticky[pool] = chosen
	if len(tied) > 1 {
		return chosen, fmt.Sprintf("best %s score, rotating among %d tied", cfg.Mode, len(tied))
	}
	return chosen, "best " + string(cfg.Mode) + " score"
}

// fallback picks among candidates that have no usable quota data. It defers to
// the host only when no candidate is excluded: a delegated or unhandled pick
// lets the host choose from the full candidate list, blocked ones included.
func (r *Router) fallback(pool string, views []CandidateView, idx []int, anyBlocked bool, cfg Config, d *Decision) pluginapi.SchedulerPickResponse {
	if !anyBlocked {
		return r.delegate(cfg, d, "no quota data to rank candidates")
	}
	strategy := cfg.Fallback
	if strategy == FallbackNone {
		strategy = FallbackRoundRobin
	}
	i := idx[0] // fill-first: the first candidate in host order
	if strategy == FallbackRoundRobin {
		i = r.rotate("fallback|"+pool, views, idx)
	}
	id := views[i].id
	d.Outcome, d.Selected = OutcomeSelected, r.label(id, cfg)
	d.Reason = fmt.Sprintf("%s fallback among %d candidates without quota data", strategy, len(idx))
	return pluginapi.SchedulerPickResponse{AuthID: id, Handled: true}
}

// delegate hands the pick to the host. Only call it when no candidate is blocked.
func (r *Router) delegate(cfg Config, d *Decision, reason string) pluginapi.SchedulerPickResponse {
	if cfg.Fallback == FallbackNone {
		d.Outcome, d.Reason = OutcomeUnhandled, reason
		return pluginapi.SchedulerPickResponse{}
	}
	d.Outcome, d.Delegate, d.Reason = OutcomeDelegated, string(cfg.Fallback), reason
	return pluginapi.SchedulerPickResponse{DelegateBuiltin: string(cfg.Fallback), Handled: true}
}

// rotate returns the next element of idx for counter key, cycling in auth ID
// order so the sequence does not depend on how the host ordered candidates.
func (r *Router) rotate(key string, views []CandidateView, idx []int) int {
	ordered := append([]int(nil), idx...)
	sort.Slice(ordered, func(i, j int) bool { return views[ordered[i]].id < views[ordered[j]].id })
	n := r.rr[key]
	r.rr[key] = n + 1
	return ordered[n%uint64(len(ordered))]
}

func (r *Router) logDecision(d Decision) {
	if r.log == nil {
		return
	}
	eligible := 0
	for _, v := range d.Candidates {
		if v.State == stateEligible {
			eligible++
		}
	}
	r.log("info", "quota-router pick", map[string]any{
		"outcome":    d.Outcome,
		"selected":   d.Selected,
		"delegate":   d.Delegate,
		"reason":     d.Reason,
		"mode":       string(d.Mode),
		"provider":   d.Provider,
		"model":      d.Model,
		"candidates": len(d.Candidates),
		"eligible":   eligible,
	})
}

// Decisions returns the most recent decisions, oldest first.
func (r *Router) Decisions() []Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Decision(nil), r.decisions...)
}

func (r *Router) label(id string, cfg Config) string {
	if cfg.RevealAuthIDs {
		return id
	}
	return authLabel(id)
}

// authLabel is a stable, non-reversible label for an auth ID. Auth IDs are
// often file names that contain account e-mail addresses.
func authLabel(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "auth-" + hex.EncodeToString(sum[:5])
}

func nearlyEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-12
}
