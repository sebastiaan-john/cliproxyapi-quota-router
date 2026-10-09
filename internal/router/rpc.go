package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Plugin identity. Version is overridden at release time with -ldflags -X.
var Version = "0.1.0"

const (
	PluginName = "cliproxyapi-quota-router"
	repository = "https://github.com/OlivierDijkstra/cliproxyapi-quota-router"
	logoURL    = "https://raw.githubusercontent.com/OlivierDijkstra/cliproxyapi-quota-router/main/assets/icon.svg"

	// StatusPath is registered under /v0/management, so it requires the
	// management key. Resource routes are unauthenticated and are not used.
	StatusPath = "/quota-router/status"
	// InvalidatePath (POST) drops this plugin's cached quota for one credential.
	// It is also under /v0/management and needs the management key.
	InvalidatePath = "/quota-router/invalidate"
)

// InvalidateRequest names the credential by the label shown in the status
// route. With reveal_auth_ids on, the raw auth ID is accepted as well.
type InvalidateRequest struct {
	Auth string `json:"auth"`
}

// InvalidateResponse states exactly what was cleared. PluginOnly and
// HostCooldownCleared are constants that make the boundary explicit: this
// route cannot touch host cooldowns or disabled flags.
type InvalidateResponse struct {
	Auth                string `json:"auth"`
	SnapshotDropped     bool   `json:"snapshot_dropped"`
	ProbeMarkerDropped  bool   `json:"probe_marker_dropped"`
	PluginOnly          bool   `json:"plugin_only"`
	HostCooldownCleared bool   `json:"host_cooldown_cleared"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

// capabilities uses the host's JSON keys (internal/pluginhost/rpc_schema.go).
type capabilities struct {
	Scheduler                 bool `json:"scheduler"`
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities,omitempty"`
	UsagePlugin               bool `json:"usage_plugin"`
	ManagementAPI             bool `json:"management_api"`
}

type managementRoute struct {
	Method      string
	Path        string
	Description string
}

type managementRegistration struct {
	Routes []managementRoute `json:"routes"`
}

// Handler dispatches host RPC methods to the router.
type Handler struct {
	Router *Router
	// OnConfigured runs after a successful register or reconfigure.
	OnConfigured func()
}

// Handle returns the JSON envelope for one host call. Malformed requests become
// error envelopes; Handle never panics on input.
//
// A scheduler.pick failure is reported as an unhandled pick rather than an
// error, because the host fails the client request on scheduler errors. A
// panic must not escape either: in a c-shared library it would abort the host.
func (h *Handler) Handle(method string, request []byte) (out []byte) {
	defer func() {
		if p := recover(); p != nil {
			out = h.failure(method, fmt.Errorf("internal error: %v", p))
		}
	}()
	result, err := h.dispatch(method, request)
	if err != nil {
		return h.failure(method, err)
	}
	return result
}

func (h *Handler) failure(method string, err error) []byte {
	if h.Router != nil && h.Router.log != nil {
		h.Router.log("warn", "quota-router call failed", map[string]any{"method": method, "error": err.Error()})
	}
	if method == pluginabi.MethodSchedulerPick {
		if raw, errEnv := okEnvelope(pluginapi.SchedulerPickResponse{}); errEnv == nil {
			return raw
		}
	}
	return errorEnvelope("plugin_error", err.Error())
}

func (h *Handler) dispatch(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", err)
			}
		}
		cfg, err := ParseConfig(req.ConfigYAML)
		if err != nil {
			return nil, err
		}
		// SetConfig restores bindings from a new state_file before any save can
		// reach it, and logs a load failure without failing registration.
		h.Router.SetConfig(cfg)
		if h.OnConfigured != nil {
			h.OnConfigured()
		}
		return okEnvelope(h.registration(cfg))
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		h.Router.StopRefresher()
		if err := h.Router.SaveState(); err != nil && h.Router.log != nil {
			h.Router.log("warn", "quota-router state save failed", map[string]any{"error": err.Error()})
		}
		return okEnvelope(struct{}{})
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode scheduler pick: %w", err)
		}
		resp, _ := h.Router.Pick(req)
		return okEnvelope(resp)
	case pluginabi.MethodUsageHandle:
		var rec pluginapi.UsageRecord
		if err := json.Unmarshal(request, &rec); err != nil {
			return nil, fmt.Errorf("decode usage record: %w", err)
		}
		h.Router.ObserveUsage(rec)
		return okEnvelope(struct{}{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{Routes: []managementRoute{
			{
				Method:      http.MethodGet,
				Path:        StatusPath,
				Description: "Quota router configuration, cached quota and recent routing decisions",
			},
			{
				Method:      http.MethodPost,
				Path:        InvalidatePath,
				Description: "Drop the plugin's cached quota snapshot for one credential (by hashed label); host cooldowns are untouched",
			},
		}})
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
		return okEnvelope(h.management(req))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func (h *Handler) registration(cfg Config) registration {
	field := func(name string, typ pluginapi.ConfigFieldType, desc string, enum ...string) pluginapi.ConfigField {
		return pluginapi.ConfigField{Name: name, Type: typ, Description: desc, EnumValues: enum}
	}
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             PluginName,
			Version:          Version,
			Author:           "Olivier Dijkstra",
			GitHubRepository: repository,
			Logo:             logoURL,
			ConfigFields: []pluginapi.ConfigField{
				field("mode", pluginapi.ConfigFieldTypeEnum, "Ranking: smart (usable quota per hour until it resets; expiring-first is another name for it) or headroom (most quota left).",
					string(ModeSmart), string(ModeExpiringFirst), string(ModeHeadroom)),
				field("minimum_headroom", pluginapi.ConfigFieldTypeNumber, "New sessions skip credentials while any window has less than this fraction left. An empty window is always skipped. Default 0.02."),
				field("session_minimum_headroom", pluginapi.ConfigFieldTypeNumber, "A bound session keeps its credential down to this fraction, preserving its prompt cache. At most minimum_headroom. Default 0 (until actually exhausted)."),
				field("paid_fallback", pluginapi.ConfigFieldTypeBoolean, "Allow per-request billed credentials (API keys, Anthropic extra usage) as a last resort when no subscription quota is usable. Default false: such picks are rejected."),
				field("fallback", pluginapi.ConfigFieldTypeEnum, "Strategy when quota data cannot rank candidates. none leaves the pick to the host only while no candidate is excluded.",
					string(FallbackRoundRobin), string(FallbackFillFirst), string(FallbackNone)),
				field("refresh_interval", pluginapi.ConfigFieldTypeString, "How often to sync host auth state, e.g. 5m."),
				field("stale_after", pluginapi.ConfigFieldTypeString, "A credential with any quota window reported longer ago than this is re-probed, e.g. 30m."),
				field("probe_interval", pluginapi.ConfigFieldTypeString, "Minimum gap between probe picks of one credential without data, e.g. 1m."),
				field("min_reset_horizon", pluginapi.ConfigFieldTypeString, "Floor for time-to-reset in scoring so near resets cannot dominate, e.g. 1h."),
				field("switch_margin", pluginapi.ConfigFieldTypeNumber, "Keep the current credential unless another scores this fraction higher. Default 0.15."),
				field("tie_tolerance", pluginapi.ConfigFieldTypeNumber, "Scores within this fraction of the best rotate. Default 0.05."),
				field("session_affinity", pluginapi.ConfigFieldTypeBoolean, "Keep a client session on the same credential while it stays eligible."),
				field("session_affinity_ttl", pluginapi.ConfigFieldTypeString, "Inactivity TTL of a session binding, renewed on every request, e.g. 1h."),
				field("state_file", pluginapi.ConfigFieldTypeString, "Absolute path of a private file persisting session bindings across restarts (session hashes, SHA-256 auth digests, expiries only). Empty disables it."),
				field("across_priorities", pluginapi.ConfigFieldTypeBoolean, "Receive candidates from all priority tiers instead of only the highest."),
				field("log_decisions", pluginapi.ConfigFieldTypeBoolean, "Log one line per routing decision through the host logger."),
				field("reveal_auth_ids", pluginapi.ConfigFieldTypeBoolean, "Show raw auth IDs in diagnostics instead of hashed labels. Auth IDs can contain e-mail addresses."),
			},
		},
		Capabilities: capabilities{
			Scheduler:                 true,
			SchedulerAcrossPriorities: cfg.AcrossPriorities,
			UsagePlugin:               true,
			ManagementAPI:             true,
		},
	}
}

// Status is the body of the management status route.
type Status struct {
	Plugin      string           `json:"plugin"`
	Version     string           `json:"version"`
	Config      ConfigView       `json:"config"`
	Providers   []string         `json:"quota_providers"`
	Store       StoreStats       `json:"store"`
	Sessions    int              `json:"session_bindings"`
	State       StateStatus      `json:"state_file"`
	Credentials []CredentialView `json:"credentials"`
	Decisions   []Decision       `json:"recent_decisions"`
}

// CredentialView is the cached quota of one credential, labelled like CandidateView.
type CredentialView struct {
	Auth            string       `json:"auth"`
	Provider        string       `json:"provider"`
	ObservedAt      time.Time    `json:"observed_at"`
	LimitReached    bool         `json:"limit_reached,omitempty"`
	LimitObservedAt time.Time    `json:"limit_observed_at,omitzero"`
	LimitUntil      time.Time    `json:"limit_until,omitzero"`
	ExtraUsage      bool         `json:"extra_usage,omitempty"`
	Windows         []WindowView `json:"windows"`
}

// WindowView is one quota window for diagnostics.
type WindowView struct {
	Name       string    `json:"name"`
	Remaining  float64   `json:"remaining"`
	Length     string    `json:"length,omitempty"`
	ResetAt    time.Time `json:"reset_at,omitzero"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
}

// Status assembles the diagnostic snapshot.
func (r *Router) Status() Status {
	cfg := r.Config()
	providers := r.registry.Providers()
	sort.Strings(providers)
	st := Status{
		Plugin:    PluginName,
		Version:   Version,
		Config:    cfg.View(),
		Providers: providers,
		Store:     r.store.Stats(),
		Sessions:  r.aff.size(),
		State:     r.State(),
		Decisions: r.Decisions(),
	}
	for id, snap := range r.store.snapshotsCopy() {
		cv := CredentialView{
			Auth:            r.label(id, cfg),
			Provider:        snap.Provider,
			ObservedAt:      snap.ObservedAt,
			LimitReached:    snap.LimitReached,
			LimitObservedAt: snap.LimitObservedAt,
			LimitUntil:      snap.LimitUntil,
			ExtraUsage:      snap.ExtraUsage,
		}
		for _, w := range snap.Windows {
			wv := WindowView{Name: w.Name, Remaining: w.Remaining, ResetAt: w.ResetAt, ObservedAt: w.ObservedAt}
			if w.Length > 0 {
				wv.Length = w.Length.String()
			}
			cv.Windows = append(cv.Windows, wv)
		}
		st.Credentials = append(st.Credentials, cv)
	}
	sort.Slice(st.Credentials, func(i, j int) bool { return st.Credentials[i].Auth < st.Credentials[j].Auth })
	return st
}

func jsonResponse(status int, body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}},
		Body:       body,
	}
}

func jsonError(status int, msg string) pluginapi.ManagementResponse {
	body, _ := json.Marshal(map[string]string{"error": msg})
	return jsonResponse(status, body)
}

func (h *Handler) management(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimRight(req.Path, "/")
	switch {
	case strings.HasSuffix(path, StatusPath):
		if req.Method != "" && !strings.EqualFold(req.Method, http.MethodGet) {
			return jsonError(http.StatusMethodNotAllowed, "method not allowed")
		}
		body, err := json.MarshalIndent(h.Router.Status(), "", "  ")
		if err != nil {
			return jsonError(http.StatusInternalServerError, "encode status")
		}
		return jsonResponse(http.StatusOK, body)
	case strings.HasSuffix(path, InvalidatePath):
		if !strings.EqualFold(req.Method, http.MethodPost) {
			return jsonError(http.StatusMethodNotAllowed, "method not allowed")
		}
		return h.invalidate(req.Body)
	default:
		return jsonError(http.StatusNotFound, "not found")
	}
}

// invalidate handles POST InvalidatePath. The body names one credential by
// label. Unknown and ambiguous labels are refused before anything changes.
func (h *Handler) invalidate(body []byte) pluginapi.ManagementResponse {
	var req InvalidateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "body must be JSON like {\"auth\": \"auth-…\"}")
	}
	label := strings.TrimSpace(req.Auth)
	if label == "" {
		return jsonError(http.StatusBadRequest, "auth is required: the label shown in the status route")
	}
	ids := h.Router.resolveLabel(label)
	switch len(ids) {
	case 0:
		return jsonError(http.StatusNotFound, "no known credential has that label")
	case 1:
	default:
		return jsonError(http.StatusConflict, "label matches more than one credential")
	}
	snapshotDropped, probeDropped := h.Router.Store().Invalidate(ids[0])
	if h.Router.log != nil {
		h.Router.log("info", "quota-router snapshot invalidated", map[string]any{
			"auth": authLabel(ids[0]), "snapshot_dropped": snapshotDropped, "probe_marker_dropped": probeDropped,
		})
	}
	out, _ := json.Marshal(InvalidateResponse{
		Auth:                authLabel(ids[0]),
		SnapshotDropped:     snapshotDropped,
		ProbeMarkerDropped:  probeDropped,
		PluginOnly:          true,
		HostCooldownCleared: false,
	})
	return jsonResponse(http.StatusOK, out)
}

// resolveLabel returns the known auth IDs a label refers to: by hashed label
// always, and by raw ID only when reveal_auth_ids is on.
func (r *Router) resolveLabel(label string) []string {
	cfg := r.Config()
	var ids []string
	for _, id := range r.store.KnownIDs() {
		if authLabel(id) == label || (cfg.RevealAuthIDs && id == label) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := pluginabi.NewErrorEnvelope(code, message)
	return raw
}
