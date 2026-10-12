# Upstream contract

This plugin targets the CLIProxyAPI native plugin ABI. Everything below was read from upstream source, not inferred. Re-check it when bumping the `github.com/router-for-me/CLIProxyAPI/v8` dependency.

## Inspected revisions

| Source | Revision | Date |
| --- | --- | --- |
| `router-for-me/CLIProxyAPI` `main` | `8ef43e4df3b216a42493105d31c2873b69191473` | 2026-10-04 |
| `router-for-me/CLIProxyAPI` tag `v8.0.13` (the `go.mod` dependency) | `d7914afdedca7af95ee974a42453dc49fc1388ce` | 2026-10-03 |
| `router-for-me/CLIProxyAPI-Plugins-Store` `main` | `f37b6d61e276a69d53ce8662aae8a6cef0989adf` | 2026-10-04 |

Between `v8.0.13` and `main`, nothing changed in `sdk/pluginapi`, `sdk/pluginabi`, `internal/pluginhost/scheduler.go`, `internal/pluginhost/rpc_*.go` or the scheduler candidate code in `sdk/cliproxy/auth/conductor_selection.go`.

## C ABI

From `examples/plugin/*/go/main.go` and `internal/pluginhost/loader_unix.go` / `loader_windows.go`:

- The library exports `cliproxy_plugin_init(const cliproxy_host_api*, cliproxy_plugin_api*)`. It fills `abi_version` (`pluginabi.ABIVersion`, 1), `call`, `free_buffer` and `shutdown`.
- `call(method, request, len, *response)` takes and returns JSON. Responses use the envelope `{"ok": bool, "result": ..., "error": {"code", "message", "http_status"}}` (`pluginabi.Envelope`).
- The host API table holds `host_ctx`, `call` and `free_buffer`. Plugins call host methods such as `host.auth.list` and `host.log` through it.
- Unix: the loader needs a cgo build of the host (`//go:build cgo && (linux || darwin || freebsd)`). Without cgo, `loader_unsupported.go` refuses to load plugins. Windows loads DLLs through `syscall` and needs no cgo in the host.
- On Unix the host marks the plugin's callback instance closed, calls `shutdown`, then frees the host API table and `dlclose`s the library (`dynamicLibraryClient.Shutdown` in `loader_unix.go`). The Windows loader calls `shutdown` and keeps the DLL mapped (`loader_windows.go`, "Windows Go DLLs are not safe to hot-unload").
- Go's linker passes `-z nodelete` for `-buildmode=c-shared` on ELF targets (`cmd/link/internal/ld/lib.go`), so on Linux the `dlclose` does nothing and the Go runtime stays loaded. A later load of the same file reinitializes the same runtime.
- Inside `shutdown` the plugin takes the write side of a lock that every host callback holds for its full duration. That waits for in-flight `host.auth.list` and `host.log` calls with no timeout, clears the stored table pointer, and makes later callbacks fail without touching it. Only then does `shutdown` stop the refresh goroutine and return. The wait can't deadlock against the host: `callHostAuthList` and `callHostLog` take only the auth manager's lock and briefly `Host.mu`, and the host releases `Host.mu` before calling `shutdown`, `plugin.register` or `plugin.reconfigure` (`host.go`, `ApplyConfig`, `UnloadPluginContext`, `ShutdownAllContext`). Callbacks already started return once the host answers.
- `scripts/host-smoke.sh` runs this path in CI with the real loader at the `go.mod` version.

## Methods this plugin implements

| Method | Purpose |
| --- | --- |
| `plugin.register`, `plugin.reconfigure` | Request `{"config_yaml": <bytes>, "schema_version": n}`. The YAML is the `plugins.configs.<id>` section, including the host's own `enabled` and `priority`. The response is the registration below. |
| `plugin.quiesce`, `plugin.shutdown` | Return `{}`. |
| `scheduler.pick` | Request: `pluginapi.SchedulerPickRequest`. Response: `pluginapi.SchedulerPickResponse`. |
| `usage.handle` | Request: `pluginapi.UsageRecord`. |
| `management.register`, `management.handle` | One authenticated GET route under `/v0/management`. |

Registration (`internal/pluginhost/rpc_schema.go`, `rpcRegistration`):

```json
{
  "schema_version": 6,
  "metadata": {"Name": "...", "Version": "...", "ConfigFields": [...]},
  "capabilities": {"scheduler": true, "scheduler_across_priorities": false, "usage_plugin": true, "management_api": true}
}
```

## Scheduler semantics

`sdk/cliproxy/auth/conductor_selection.go` and `internal/pluginhost/scheduler.go`:

- The host calls the scheduler only after filtering candidates: disabled auths, auths already tried for this request, auths that do not serve the model, and auths blocked or cooling down for the model (`availableAuthsForSelector`). By default only the highest priority tier is offered. `scheduler_across_priorities` widens that.
- Each candidate carries `ID`, `Provider`, `Priority`, `Status` and `Attributes` with credential-like keys removed (`schedulerSafeAttributes`). `Metadata` is never populated, so quota data cannot come from the candidate.
- `Options.Headers` holds the client request headers. `Options.Metadata` holds request metadata such as `canonical_session_id` and `execution_session_id` when the host has derived them.
- A response must set `Handled`. Then exactly one of: an `AuthID` that is in the candidate list, `DelegateBuiltin` of `round-robin` or `fill-first`, or `Reject` with optional `RejectCode`/`RejectReason`. Anything else is logged as invalid and treated as unhandled.
- Unhandled and delegated picks let the host choose from the full candidate list (`pickViaBuiltinScheduler`). This plugin therefore answers unhandled or delegated only when it excluded no candidate. A rejection becomes an `auth.Error` with the reject code, which fails the client request (`conductor_selection.go`).
- An error envelope from `scheduler.pick` fails the client request. This plugin therefore answers decode errors and internal panics with an unhandled pick.
- Only one scheduler plugin is active: the first active record in priority order (`schedulerRecord`).
- While a plugin scheduler is installed, `Manager.LookupSessionAffinity` returns `unsupported`, so `host.affinity.lookup` is of no use here. This plugin keeps its own session bindings.

## Where quota data comes from

- `internal/runtime/executor/helps/usage_helpers.go` attaches the upstream response headers of each attempt to the usage record (`record.ResponseHeaders = GetResponseHeaders(ctx)`), and `internal/pluginhost/adapters_usage_translation.go` passes them to `usage.handle` unfiltered.
- For Codex websocket traffic, `helps.ParseCodexQuotaEventHeaders` turns `codex.rate_limits` events into the same `X-Codex-*` header names and merges them into those headers.
- `sdk/cliproxy/auth/quota_signals.go` lists the Codex header families: `X-Codex-{Primary,Secondary}-{Used-Percent,Window-Minutes,Reset-After-Seconds,Reset-At}`, `X-Codex-Limit-Reached`, `X-Codex-Allowed`, `X-Codex-Credits-*`, `X-Codex-Plan-Type`, `X-Codex-Active-Limit`, plus per-model `X-Codex-<name>-*` and `X-Codex-Additional-<name>-*`. The host keeps its own copy in `QuotaState.Signals` but no plugin API exposes it.
- `host.auth.list` returns `pluginapi.HostAuthFileEntry` records with `id`, `auth_index`, `provider`, `disabled`, `unavailable`, `status`, `next_retry_after`, and also `email` and `path`. When the host has no auth manager it falls back to reading auth files from disk, and those entries have no `id`. `buildHostAuthFileEntry` (`internal/pluginhost/auth_callbacks.go`) skips auths without a `path` attribute unless they are runtime-only, so config-sourced credentials (`claude-api-key`, `codex-api-key` and similar) never appear. The plugin therefore forgets a snapshot only when an auth it saw listed before drops out of the list. The plugin never logs or persists an entry; it keeps only `id`, `disabled` and `next_retry_after`, and uses only `disabled` for routing.
- `next_retry_after` in that list is the credential-level `auth.NextRetryAfter`. The host's own candidate filter (`availableAuthsForSelector`, `retryRoundAvailabilityForAuth` in `conductor_selection.go`; `availabilityBlock` in `selector.go`) applies both the credential-level and the per-model cooldown before the scheduler is called. An offered candidate has passed that filter, so this fork does not re-apply a cached retry time.

## Host callbacks this plugin does not use

Verified present in `v8.0.13`, listed so nobody has to guess:

- `host.routing.reset_cooldown` (`pluginabi.MethodHostRoutingResetCooldown`, `internal/pluginhost/routing_callbacks.go`): takes `{"auth_index": ...}` and calls `Manager.ResetQuota` for that credential, skipping persistence of token files. This is the host-side primitive a cooldown-recovery feature would use. The fork does not call it; see `DESIGN.md`.
- `host.affinity.lookup`: returns `unsupported` while a plugin scheduler is installed.
- `host.auth.get`, `host.auth.get_runtime`, `host.auth.save`: credential access. Never called.

## Lifecycle

- `plugin.register` and `plugin.reconfigure` are sent with the plugin's YAML section. `plugin.quiesce` is sent to a plugin being replaced during `ApplyConfig`. `plugin.shutdown` is *not* sent as an RPC in `v8.0.13`: `ShutdownAllContext` and `UnloadPluginContext` call the native `shutdown` export directly (`shutdownPluginClient` in `internal/pluginhost/host.go`). The fork therefore flushes its state file inside `cliproxyPluginShutdown` as well as on `plugin.quiesce`.
- `management.handle` receives `pluginapi.ManagementRequest` with `Method`, `Path`, `Headers`, `Query` and `Body []byte` (`ServeManagementHTTP` reads the HTTP body in full). Routes are declared with a `Method`; `POST` is accepted (`normalizeManagementRoute`). All of them sit under `/v0/management` behind the management key.

## Claude

Read from `main` at the revision above unless noted. The same files exist unchanged at `v8.0.13`.

- The Claude executor calls `helps.RecordAPIResponseMetadata(ctx, cfg, status, httpResp.Header.Clone())` right after the upstream round trip, before it looks at the status code (`claude_executor_execute.go`, `claude_executor_stream.go`, `claude_executor_tokens.go`). That stores the headers in the request context. `UsageReporter.publishRecord` copies them into `record.ResponseHeaders` for both success and failure records (`TrackFailure`), so 429 responses reach `usage.handle` with their headers.
- The reporter is built with `NewExecutorUsageReporter(ctx, e, ...)`, and `ClaudeExecutor.Identifier()` is `"claude"`, so `UsageRecord.Provider` is `claude`.
- `sdk/cliproxy/auth/quota_signals.go` keeps `anthropic-ratelimit-unified-*` and `Retry-After` as quota signals for provider `claude` only.
- `internal/runtime/executor/helps/claude_ratelimit.go` reads the unified headers: per-window `5h`, `7d` and `7d_oi` status, utilization and reset; the overall `status`, `representative-claim` and `reset`; and `overage-status` and `overage-disabled-reason`. `ClaudeHeadersIndicateUnifiedRateLimitRejection` treats a `5h` or `7d` rejection as credential-wide. A rejection that is only on `7d_oi` or overage, while the shared windows are `allowed`/`allowed_warning` or report utilization below 1, is scoped to the requested model. The plugin applies the same rule.
- Reset values are parsed as unix seconds, with RFC 3339 and HTTP dates as fallbacks (`parseUnixOrTimestamp`).
- Candidates keep their `auth_kind` attribute (it isn't matched by `schedulerAttributeSensitive`). The watcher sets it to `oauth` for auth files and `apikey` for `claude-api-key` and other config keys (`internal/watcher/synthesizer/file.go`, `config.go`).

Anthropic doesn't publish these headers. Two other sources agree with the CLIProxyAPI code:

- Claude Code 2.1.289 (`@anthropic-ai/claude-code-linux-x64` from npm) maps the claims `five_hour`, `seven_day`, `seven_day_overage_included` and `overage` to the header infixes `5h`, `7d`, `7d_oi` and `overage`. It reads `-utilization`, `-reset` and `-surpassed-threshold` for each as numbers and rounds `-reset` to whole unix seconds. Its schema describes `seven_day_overage_included` as a per-model bucket and lists the status values `allowed`, `allowed_warning` and `rejected`. It also knows `seven_day_opus` and `seven_day_sonnet` claims, but maps no header infix to them.
- [CLIProxyAPI issue #5915](https://github.com/router-for-me/CLIProxyAPI/issues/5915) quotes real headers: utilization `0.69` and `1.02`, `representative-claim` `seven_day_overage_included`, and a later 200 response on the same account with `representative-claim` `five_hour` and `status` `allowed`.

The upstream tests (`claude_ratelimit_test.go`, `claude_executor_fable_ratelimit_test.go`) are synthetic, but they use the same names and units.

## Gemini

Not supported. `quota_signals.go` ignores every header except Codex and Claude ones, and the Gemini, Vertex and Antigravity executors report no remaining-quota figure. Antigravity reads `QUOTA_EXHAUSTED` and retry delays from 429 bodies (`antigravity_executor_credits.go`) and turns them into host cooldowns, which the host applies before the scheduler sees the candidates. CLIProxyAPI does have quota *provider* plugins (`internal/api/handlers/management/plugin_quota.go`), but those are management routes that call a plugin to fetch quota. They don't feed quota to a scheduler.

## Plugin store and file naming

From `internal/pluginhost/platform.go` and `internal/pluginstore/{github,install,checksum,registry}.go`:

- The host scans `plugins/<GOOS>/<GOARCH>/` and then `plugins/`, for `.so` on Linux, `.dylib` on macOS and `.dll` on Windows. The plugin ID is the file name without extension and without an optional `-v<version>` suffix. That ID is the key under `plugins.configs`.
- A GitHub-release install looks for the asset `<id>_<version>_<goos>_<goarch>.zip` and `checksums.txt` (sha256, `sha256sum` format). The zip must contain exactly one library named `<id>.<ext>` or `<id>-v<version>.<ext>` at its root. The release tag, minus a leading `v`, must match `^[0-9][0-9A-Za-z.+-]*$`.
- The official registry is `https://raw.githubusercontent.com/router-for-me/CLIProxyAPI-Plugins-Store/main/registry.json`. Extra registries go in `plugins.store-sources`.

## Platforms

- Go supports `-buildmode=c-shared` on linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64 and windows/arm64 (`src/internal/platform/supported.go`, Go 1.26.8).
- CLIProxyAPI's own release ships plugin-capable builds for those six targets. Its `_no-plugin` Linux archives and the FreeBSD arm64 build are compiled without cgo and cannot load any plugin. Its Linux builds target glibc 2.17 through manylinux2014, and this plugin's Linux builds do the same.
- The plugin talks to the host only through the C ABI and JSON. It does not have to match the host's Go version.
