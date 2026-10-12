# CLIProxyAPI Quota Router (cache-safe fork)

Use Codex and Claude subscription quota before it expires, keep each client session on the account that holds its prompt cache, and reject known paid capacity unless explicitly enabled.

> **Development branch — not ready for live macOS deployment.** Repeated native-loader tests reproduce an intermittent shutdown/unload hang, including with a pre-correction artifact. A diagnostic isolated host that retained the library mapping passed 15 consecutive runs; the unmodified host is not proven safe. See [the native lifecycle blocker](docs/NATIVE-MACOS.md). No host fix or production deployment is included here.

This is a fork of [OlivierDijkstra/cliproxyapi-quota-router](https://github.com/OlivierDijkstra/cliproxyapi-quota-router), a [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) scheduler plugin by Olivier Dijkstra. The fork lives at [sebastiaan-john/cliproxyapi-quota-router](https://github.com/sebastiaan-john/cliproxyapi-quota-router). Go module path and plugin ID are unchanged so it drops in where the original did.

The plugin reads quota/reset headers from upstream responses, ranks accounts for new sessions by usable quota at risk of expiring, and binds each session to one account for as long as that account can serve it. It does not read credentials or call provider usage APIs. Providers other than Codex and Claude use the fallback strategy.

## What the fork changes

- **Sessions stay put.** An existing binding beats every score, probe and the new-session headroom. Only real exhaustion, a provider rejection, a disabled credential, or the host no longer offering the account moves a session. After a failover the session stays on the replacement; it does not bounce back when the old account recovers. `session_minimum_headroom` (default 0) governs bound sessions; `minimum_headroom` (default 2%) governs new ones.
- **Paid capacity is opt-in.** API keys on Claude or Codex, and Claude accounts whose windows are exhausted but still served through Anthropic extra usage, are classified as paid. By default such a pick is rejected with code `paid_fallback_disabled`, including when the host offers only keys because every OAuth account is cooling down. With `paid_fallback: true` they are a last resort behind every subscription account, and a session already on a now-paid account moves to a subscription account when one is usable.
- **The host's candidate list is authoritative.** A retry time cached from an earlier `host.auth.list` no longer blocks a candidate the host currently offers. The disabled flag still does.
- **Bindings can survive restarts.** Optional `state_file` stores session hashes, SHA-256 digests of auth IDs and expiries in a private 0600 file. Nothing else is written.
- **Operators can drop a stale snapshot.** `POST /v0/management/quota-router/invalidate` clears the plugin's cached quota for one credential after a manual allowance reset upstream. It cannot clear host cooldowns.

## Install

1. Use a plugin-capable CLIProxyAPI build (not a Linux `_no-plugin` build). Build with `scripts/build-plugin.sh dist` or download a release library and `checksums.txt`; verify the checksum.
2. Put the library in CLIProxyAPI's `plugins/` directory (or `plugins/<os>/<arch>/`). Name it `cliproxyapi-quota-router.so` on Linux, `.dylib` on macOS, or `.dll` on Windows. If using Docker, mount that directory at `/CLIProxyAPI/plugins`.
3. Add this to CLIProxyAPI's `config.yaml` and **restart** CLIProxyAPI (Go shared libraries cannot be safely hot-reloaded):

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cliproxyapi-quota-router:
      enabled: true
      priority: 10
      mode: smart
      minimum_headroom: 0.02
      session_minimum_headroom: 0
      paid_fallback: false
      fallback: round-robin
      session_affinity: true
      session_affinity_ttl: 1h
      # state_file: /var/lib/cliproxyapi/quota-router-state.json
```

`smart` scores quota left above the headroom against time until reset; `headroom` chooses the account with the most quota left. The session TTL is an inactivity TTL: every request on a session renews it. [All options and defaults](config.example.yaml).

## Paid usage, and what the plugin can and cannot see

The plugin learns that an account is paid only from headers it has already seen: `auth_kind=apikey` on the candidate, or `anthropic-ratelimit-unified-overage-status: allowed` on a response from an exhausted account. Before the first response from an account arrives, the plugin cannot know whether extra usage is enabled there. A probe request to such an account can therefore be billed. Keep extra usage disabled at Anthropic if that matters; the plugin reduces paid spend, it cannot make it impossible.

## Session bindings across restarts

With `state_file` set, bindings are flushed every `refresh_interval`, on `plugin.quiesce`, and in the native shutdown entry point the host calls before unloading. A crash between flushes loses the bindings made since the last one. On Linux, macOS and FreeBSD the file is opened without following a symlink at its final path component and without blocking on a FIFO, then refused as a whole if it is not a regular file, is readable by group or others, is over 4 MiB, has a different schema version, or is not JSON. Those checks run on the opened file, not on a prior lookup, and the read is bounded. Parent directories are not checked. On Windows the symlink check happens before the open (a link planted in between is followed) and the owner-only check is skipped: the file is created with mode 0600, which does not set a Windows ACL, so privacy there depends on the directory's ACL. Restored bindings are matched against the candidates the host offers; an account that was removed, disabled or is cooling down is never forced. Diagnostics report whether persistence is on, never the path.

## Management routes

Both routes require the CLIProxyAPI management key.

- `GET /v0/management/quota-router/status`: configuration, cached quota per credential, persistence state and recent decisions. Auth IDs are hashed labels unless `reveal_auth_ids` is on.
- `POST /v0/management/quota-router/invalidate` with body `{"auth": "<label from status>"}`: drops that credential's cached snapshot and probe marker. The response carries `plugin_only: true` and `host_cooldown_cleared: false`. Unknown labels return 404, ambiguous ones 409, bad bodies 400. Session bindings are not touched.

## Limits that come from the host

CLIProxyAPI filters candidates before calling the scheduler: disabled auths, auths already tried for the request, auths that do not serve the model, and auths it has in cooldown for that model. The plugin never sees those and cannot bring them back. If an account is blocked at the host after its allowance was reset upstream, this plugin cannot fix that; the invalidate route only clears the plugin's own view once the host offers the account again. See [docs/DESIGN.md](docs/DESIGN.md) for the host-side contract that would close that gap.

Scores compare remaining fractions, not request counts: a 20% window on a Max plan and a 20% window on a Pro plan score the same. Explicit per-account weights are deferred until there is a real capacity calibration to base them on.

Race-enabled module tests and vet pass against the `v8.0.13` SDK contract in `go.mod`. Individual real-loader smoke passes were observed on isolated `v8.0.10` and `v8.0.13` host copies, but repeated macOS runs exposed the native shutdown/unload blocker above; those single passes do not establish production compatibility. See [docs/upstream.md](docs/upstream.md) and [docs/NATIVE-MACOS.md](docs/NATIVE-MACOS.md).

MIT licensed. Original work copyright Olivier Dijkstra.
