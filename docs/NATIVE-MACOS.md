# Native macOS lifecycle: release blocker

The module's race-enabled unit tests and a single native-loader pass are not sufficient to establish safe macOS hot-unload. No production proxy was changed during this investigation.

## Reproduction

The plugin was built as a darwin/arm64 `c-shared` library and exercised through the real CLIProxyAPI native host in isolated source copies. Repeated independent test-process runs on unmodified v8.0.10 reproduced an intermittent hang after `plugin unloaded`, while the fake Anthropic `httptest.Server` was closing. An earlier, pre-correction library artifact also hung; this is not established as a regression in the recent routing/persistence corrections.

The bounded command is:

```sh
QUOTA_ROUTER_LIB=/absolute/path/to/cliproxyapi-quota-router.dylib CGO_ENABLED=1 go test -tags quotarouter_smoke -run '^TestQuotaRouterLoaderSmoke$' -count=1 -timeout 20s -v ./internal/pluginhost
```

Run that command in the isolated host checkout after `scripts/host-smoke.sh` has copied the fixture. Some failures can affect runtime timeout handling too, so an external process deadline is required. Run independent test processes, not repeated library loading in a single process.

## Differential evidence

Only the isolated v8.0.10 host copy was instrumented, with a diagnostic C macro that leaves the shared library mapped instead of executing `dlclose`. With that macro enabled, 15 consecutive independent native smoke processes passed. This is diagnostic evidence pointing to native unloading, **not validation against an unmodified host** and not a proposed silent test workaround.

The host's `internal/pluginhost/loader_unix.go` calls the plugin shutdown export and then `dlclose`. Plugin shutdown stops the refresher and saves affinity, but stopping plugin-owned goroutines is not equivalent to stopping the Go runtime embedded in the shared library.

Go's primary upstream tracker still has the dlclose support request open:

- https://github.com/golang/go/issues/11100 — `runtime: support dlclose with -buildmode=c-shared`.
- https://github.com/golang/go/issues/65050 — related multiple-c-shared-runtime investigation; not assumed to be the exact cause of this reproduction.

## Deployment boundary

Do not deploy this native plugin on the live macOS proxy while its loader may unload Go shared libraries. A safe loader policy must retain their mappings for the process lifetime, or use a supported isolation mechanism that does not dynamically unmap an active Go runtime. Such a policy needs its own repeated shutdown/reconfiguration validation. Reconfiguration/hot reload can accumulate retained versions and runtime resources; process restart is the bounded lifecycle boundary. This document is a blocker report, not an implemented host fix.

The plugin-only repository remains the fork under development. There is no deployed host patch, no live service restart, and no claim that single smoke passes prove production lifecycle safety. Subscription cache counters and T3 session identity propagation remain separate live-validation requirements.
