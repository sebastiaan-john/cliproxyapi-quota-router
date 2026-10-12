package router

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func decode(t *testing.T, raw []byte) pluginabi.Envelope {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope %s: %v", raw, err)
	}
	return env
}

func call(t *testing.T, h *Handler, method string, payload any) pluginabi.Envelope {
	t.Helper()
	var raw []byte
	switch p := payload.(type) {
	case nil:
	case []byte:
		raw = p
	default:
		var err error
		if raw, err = json.Marshal(p); err != nil {
			t.Fatal(err)
		}
	}
	return decode(t, h.Handle(method, raw))
}

func lifecycle(yaml string) map[string]any {
	return map[string]any{"config_yaml": []byte(yaml), "schema_version": 6}
}

func TestRegisterAdvertisesCapabilities(t *testing.T) {
	configured := 0
	h := &Handler{Router: New(), OnConfigured: func() { configured++ }}
	env := call(t, h, "plugin.register", lifecycle("enabled: true\npriority: 5\nmode: headroom\nacross_priorities: true\n"))
	if !env.OK {
		t.Fatalf("register failed: %+v", env.Error)
	}
	var reg map[string]any
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	caps := reg["capabilities"].(map[string]any)
	for _, key := range []string{"scheduler", "usage_plugin", "management_api", "scheduler_across_priorities"} {
		if caps[key] != true {
			t.Errorf("capability %s = %v", key, caps[key])
		}
	}
	if reg["schema_version"].(float64) != float64(pluginabi.SchemaVersion) {
		t.Errorf("schema_version = %v", reg["schema_version"])
	}
	meta := reg["metadata"].(map[string]any)
	if meta["Name"] != PluginName || len(meta["ConfigFields"].([]any)) == 0 {
		t.Errorf("metadata = %v", meta)
	}
	if h.Router.Config().Mode != ModeHeadroom || configured != 1 {
		t.Errorf("config not applied: %+v, callbacks=%d", h.Router.Config(), configured)
	}
}

// Every YAML key of Config is advertised to management clients, and nothing
// else is: the two lists cannot drift apart.
func TestConfigFieldsMatchConfigKeys(t *testing.T) {
	h := &Handler{Router: New()}
	fields := map[string]bool{}
	for _, f := range h.registration(DefaultConfig()).Metadata.ConfigFields {
		fields[f.Name] = true
	}
	keys := map[string]bool{}
	rt := reflect.TypeOf(Config{})
	for i := range rt.NumField() {
		keys[rt.Field(i).Tag.Get("yaml")] = true
	}
	for key := range keys {
		if !fields[key] {
			t.Errorf("config key %q has no ConfigField", key)
		}
	}
	for name := range fields {
		if !keys[name] {
			t.Errorf("ConfigField %q has no config key", name)
		}
	}
}

func TestReconfigureRejectsBadConfig(t *testing.T) {
	h := &Handler{Router: New()}
	env := call(t, h, "plugin.reconfigure", lifecycle("mode: fastest\n"))
	if env.OK || env.Error == nil || !strings.Contains(env.Error.Message, "mode") {
		t.Fatalf("env = %+v", env)
	}
	if h.Router.Config().Mode != ModeSmart {
		t.Fatal("bad config was applied")
	}
}

func TestPickAndUsageOverRPC(t *testing.T) {
	h := &Handler{Router: New()}
	rec := pluginapi.UsageRecord{AuthID: "a", Provider: "codex", ResponseHeaders: codexHeaders(win{used: 40, resetIn: 2 * time.Hour}, nil)}
	if env := call(t, h, "usage.handle", rec); !env.OK {
		t.Fatalf("usage.handle: %+v", env.Error)
	}
	env := call(t, h, "scheduler.pick", pickReq(codexCandidates("a")...))
	var resp pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if !env.OK || resp.AuthID != "a" || !resp.Handled {
		t.Fatalf("pick = %+v", resp)
	}
}

func TestMalformedPickIsUnhandledNotError(t *testing.T) {
	for name, h := range map[string]*Handler{
		"bad json":   {Router: New()},
		"nil router": {}, // forces a recovered panic
	} {
		env := call(t, h, "scheduler.pick", []byte(`{"Candidates":`))
		if name == "nil router" {
			env = call(t, h, "scheduler.pick", pickReq(codexCandidates("a")...))
		}
		var resp pluginapi.SchedulerPickResponse
		_ = json.Unmarshal(env.Result, &resp)
		if !env.OK || resp.Handled {
			t.Errorf("%s: env=%+v resp=%+v", name, env, resp)
		}
	}
}

func TestOtherMethods(t *testing.T) {
	h := &Handler{Router: New()}
	if env := call(t, h, "nope", nil); env.OK || env.Error.Code != "unknown_method" {
		t.Errorf("unknown method: %+v", env)
	}
	if env := call(t, h, "usage.handle", []byte("{")); env.OK {
		t.Error("bad usage record accepted")
	}
	for _, m := range []string{"plugin.quiesce", "plugin.shutdown"} {
		if env := call(t, h, m, nil); !env.OK {
			t.Errorf("%s: %+v", m, env.Error)
		}
	}
	env := call(t, h, "management.register", map[string]any{"BasePath": "/v0/management"})
	if !env.OK || !strings.Contains(string(env.Result), StatusPath) {
		t.Fatalf("management.register: %s", env.Result)
	}
}

func managementGet(t *testing.T, h *Handler, method, path string) pluginapi.ManagementResponse {
	t.Helper()
	env := call(t, h, "management.handle", pluginapi.ManagementRequest{Method: method, Path: path})
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestManagementStatus(t *testing.T) {
	h := &Handler{Router: New()}
	observe(h.Router, "a", 40, 2*time.Hour)
	h.Router.Pick(pickReq(codexCandidates("a")...))

	resp := managementGet(t, h, "GET", "/v0/management"+StatusPath)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, resp.Body)
	}
	var st Status
	if err := json.Unmarshal(resp.Body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Config.RefreshInterval != "5m0s" || len(st.Credentials) != 1 || len(st.Decisions) != 1 {
		t.Fatalf("status = %+v", st)
	}
	if managementGet(t, h, "GET", "/v0/management/other").StatusCode != 404 {
		t.Error("unknown path not 404")
	}
	if managementGet(t, h, "POST", "/v0/management"+StatusPath).StatusCode != 405 {
		t.Error("POST not rejected")
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte("enabled: true\npriority: 3\nrefresh_interval: 2m\nsession_affinity: false\nminimum_headroom: 0.1\nfallback: Fill-First\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RefreshInterval != 2*time.Minute || cfg.SessionAffinity || cfg.MinimumHeadroom != 0.1 || cfg.Fallback != FallbackFillFirst {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.StaleAfter != DefaultConfig().StaleAfter {
		t.Error("unset field lost its default")
	}
	for _, bad := range []string{"minimum_headroom: 1.5", "fallback: random", "stale_after: -1m", "mode: [x]"} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// NaN fails every comparison, so a range check alone would accept it.
	for _, key := range []string{"minimum_headroom", "switch_margin", "tie_tolerance"} {
		for _, v := range []string{".nan", ".NaN", ".inf", "-.inf", "+.Inf"} {
			bad := key + ": " + v
			if _, err := ParseConfig([]byte(bad)); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("%q: err = %v", bad, err)
			}
		}
	}
	if cfg, _ := ParseConfig([]byte("refresh_interval: 1s")); cfg.RefreshInterval != 10*time.Second {
		t.Errorf("refresh floor not applied: %v", cfg.RefreshInterval)
	}
	if cfg, err := ParseConfig(nil); err != nil || cfg != DefaultConfig() {
		t.Errorf("empty config: %+v %v", cfg, err)
	}
}

func TestHostClient(t *testing.T) {
	var gotMethod string
	var gotBody []byte
	ok := HostClient{Call: func(method string, body []byte) ([]byte, int) {
		gotMethod, gotBody = method, body
		return []byte(`{"ok":true,"result":{"files":[{"id":"a","status":"active","disabled":true}]}}`), 0
	}}
	files, err := ok.ListAuths(context.Background())
	if err != nil || len(files) != 1 || files[0].ID != "a" || !files[0].Disabled {
		t.Fatalf("files=%+v err=%v", files, err)
	}
	if gotMethod != "host.auth.list" || string(gotBody) != "{}" {
		t.Errorf("call = %s %s", gotMethod, gotBody)
	}

	for name, c := range map[string]HostCall{
		"error envelope": func(string, []byte) ([]byte, int) {
			return []byte(`{"ok":false,"error":{"code":"x","message":"denied"}}`), 1
		},
		"empty":     func(string, []byte) ([]byte, int) { return nil, 1 },
		"bad code":  func(string, []byte) ([]byte, int) { return []byte(`{"ok":true,"result":{}}`), 2 },
		"bad json":  func(string, []byte) ([]byte, int) { return []byte(`nope`), 0 },
		"nil value": nil,
	} {
		if _, err := (HostClient{Call: c}).ListAuths(context.Background()); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ok.ListAuths(ctx); err == nil {
		t.Error("cancelled context ignored")
	}

	var logged map[string]any
	HostClient{Call: func(method string, body []byte) ([]byte, int) {
		_ = json.Unmarshal(body, &logged)
		return []byte(`{"ok":true}`), 0
	}}.Log("info", "hello", map[string]any{"k": "v"})
	if logged["level"] != "info" || logged["message"] != "hello" {
		t.Errorf("log payload = %v", logged)
	}
}
