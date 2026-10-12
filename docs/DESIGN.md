# Design notes for the cache-safe fork

This records what was changed, what was deliberately not changed, where the
author of this fork disagreed with an earlier proposal, and what would have to
happen on the host side to go further. Everything about the host is taken from
the `v8.0.13` SDK source pinned in `go.mod`, read directly; nothing is inferred.

## Priority: the prompt cache lives on one account

Anthropic prompt caching is keyed per account. Moving a session to another
account throws the cache away and pays to rebuild it. So the fork treats an
existing, healthy binding as the first thing to honour, before any score:

- A bound session stays on its account while that account can serve it. The
  "can serve" test uses `session_minimum_headroom` (default 0) instead of the
  new-session `minimum_headroom` (default 2%). At 0, only actual exhaustion
  (remaining 0, a rejected window, a limit flag), a disabled credential, or the
  host dropping the candidate moves the session.
- Probe picks bind. An earlier proposal was "probe without binding, rank on the
  second request". That was rejected: the probe request is a real request that
  already started the cache on that account; switching on request two is the
  exact waste the fork exists to avoid. The cost of keeping the probe binding is
  that a session may live on an account that ranked lower for new sessions.
  That is accepted.
- No automatic re-probing of blocked accounts with user traffic. Another
  proposal was to send one request every so often to an account the plugin has
  blocked, to learn that it recovered. Rejected: it spends a user's request on
  a likely 429, and after a manual allowance reset the operator knows better
  than a timer. The `invalidate` route replaces it.
- A binding belongs to a session and a model. Prompt caches belong to one
  account and one model, and Claude Code sends its haiku helper requests
  (titles, quota probes) under the main session's ID, as it does any model the
  user switches to, including models another provider serves. With one binding
  per session, a helper request the host routed elsewhere (a cooldown for that
  model only) or a request to another provider rewrote the session's binding,
  and the main model then left the account holding its cache. Each model now
  keeps its own binding; a thinking suffix such as `(high)` does not start a
  new one. The session identity itself is the host's `canonical_session_id`,
  so a subagent (`claude:<sid>:agent:<agent>`) has its own binding and is
  ranked like a new session on its first request.
- After a failover the session stays on the replacement. The host excludes
  already-tried auths within a request, the plugin binds to whatever served
  the request, and nothing in the plugin ever prefers the previous account.

## Paid capacity

Two kinds of credential bill per request: API keys (`auth_kind=apikey`) on a
provider the plugin understands, and Claude accounts whose shared windows are
exhausted but whose `overage-status` header says `allowed`. The original
treated the first as "unknown, do not probe" and the second as "last resort",
both of which could be chosen by the round-robin fallback or kept by affinity.

The fork gives them a state of their own, `paid`, and:

- rejects the pick with `paid_fallback_disabled` when paid candidates are the
  only ones left and `paid_fallback` is off (the default), including a pool
  that holds only keys because the host filtered every OAuth account out;
- with `paid_fallback: true`, uses them only when no eligible, probe-able or
  unranked subscription account is offered;
- never lets affinity keep a paid account while a subscription account is
  usable, and with opt-in keeps a paid binding only when nothing else is, so a
  session does not bounce between two paid credentials;
- never delegates to the host when a paid candidate is in the list, since the
  host's built-in selector would pick it.

"Exhausted" is decided without any configured headroom: a window at zero, a
rejected window or a limit flag. An account at 1% remaining with
`minimum_headroom: 0.02` and `overage-status: allowed` is not exhausted and
not paid; it is a headroom exclusion, blocked for new sessions and kept for a
session already bound to it. The first review round caught the earlier code
classifying that case as paid, which would have moved or rejected a session
with real quota left.

Observability limit, stated plainly: the plugin only knows about extra usage
after it has seen a response from that account. The first probe of an account
with extra usage enabled is billed. Providers without a quota parser (Gemini
and others) keep the original fallback behaviour; the plugin cannot tell what
bills there.

## The host's candidate list is authoritative

`sdk/cliproxy/auth/conductor_selection.go` (`availableAuthsForSelector`,
`retryRoundAvailabilityForAuth`) and `selector.go` (`availabilityBlock`) show
the host removing credentials that are disabled, unavailable, quota-exceeded or
inside `NextRetryAfter` for the requested model before `scheduler.pick` runs.
The original plugin then re-applied the `next_retry_after` it had cached from
`host.auth.list` up to three refresh intervals earlier. That cached value is
credential-wide (`buildHostAuthFileEntry` copies `auth.NextRetryAfter`), can be
hours stale, and can be reset at the host in between. The fork removes that
veto entirely rather than ageing it, because any age rule is a guess about
host behaviour the plugin cannot observe. The disabled flag is kept: a disabled
credential is never offered anyway, so the check is harmless and documents
intent. This does not and cannot restore a candidate the host did not offer.

## Persistence

`state_file` is off by default. When on, the file holds the schema version, a
save time, and for each binding the key the router already uses (a truncated
SHA-256 of the session identity and the model), the full SHA-256 of the auth ID,
and the expiry. Quota snapshots and cooldowns are not persisted: they age out
in half an hour anyway and a restart is a fine moment to re-probe. The file is
written to a 0600 temporary in the same directory and renamed into place.
Loading opens the file with `O_NOFOLLOW` on Linux, macOS and FreeBSD, so a
symlink at the final path component is refused by the open call itself; on
other targets (Windows among the release builds) the symlink check is a
separate `Lstat` before the open and a link planted between the two is
followed. The open also sets `O_NONBLOCK` on those three platforms so a FIFO
at the path cannot stall the plugin; the descriptor's type check then refuses
it. Type, owner-only permissions and size are then validated on the open
descriptor, the read is bounded at 4 MiB plus one byte so a file that grows
after the size check is still refused, other schema versions and malformed
JSON are refused as a whole, and individual malformed entries are skipped.
The owner-only permission check is Unix-only: on Windows it is skipped, and
the 0600 mode used when writing does not establish an owner-only ACL, so the
plugin makes no privacy claim for the file there beyond what the directory's
ACL provides. The strict descriptor validation is the behaviour on the
primary macOS and Linux builds. Only the final component is protected: a symlinked parent directory
is resolved like any other directory, and the plugin does not try to defend
the directory itself. Restored bindings are matched by digesting each offered
candidate, so a removed, disabled or cooling-down account is simply never
matched.

Flushes happen on every refresh tick, on `plugin.quiesce`, and in the native
`shutdown` export, because `v8.0.13` does not send `plugin.shutdown` as an RPC
before unloading. A crash between flushes loses recent bindings. The
"nothing changed" check that skips a write is tied to the destination path as
well as the binding generation, so moving `state_file` always writes the new
file. A reconfiguration is an ordered transition: a running refresher is
stopped while the old configuration is still active, the new configuration is
published and the new file loaded under the persistence lock, and the
refresher restarts afterwards, so nothing can overwrite a new state file
before its bindings are restored. Restoration adds only sessions that are not
already bound live.

## Scoring on fractions

Scores compare remaining fractions across accounts. The headers carry no
request counts, so a Max account and a Pro account at 20% remaining look the
same. Explicit relative weights per account were considered and deferred: they
would have to be keyed by auth ID (which puts e-mail addresses in the
operator's config) and calibrated against real capacities that are not
published. Nothing here infers absolute quota from a percentage.

## The reset-recovery boundary

What happened in the field: a Claude account was cooling down at the
host; after a manual allowance reset upstream the usage endpoint showed 0%,
but the host kept rejecting the account on a cooldown of about 29 hours. That
block is in the host's `NextRetryAfter`/model state. The plugin never saw those
accounts as candidates and therefore could not have picked them, whatever it
did.

What this fork does about it: nothing at the host, by design. The
`invalidate` route clears the plugin's own cached snapshot so that, once the
host offers the account again, the plugin probes it instead of trusting a
stale "blocked until" time. The response says so explicitly:
`plugin_only: true`, `host_cooldown_cleared: false`.

There are existing host reset bridges, but neither is sufficiently scoped
for automatic verified recovery:

- Both inspected tags (`v8.0.10` and `v8.0.13`) have the authenticated
  quota-provider management bridge in
  `internal/api/handlers/management/plugin_quota.go`. A successful
  `quota.reset` response causes the handler to call `Manager.ResetQuota`.
  The handler and `internal/pluginhost/quota_provider.go` are byte-identical
  in those tags. Calling a provider by itself does not clear host state.
- `v8.0.13` additionally implements `host.routing.reset_cooldown` in
  `internal/pluginhost/routing_callbacks.go`. This callback takes
  `auth_index` and skips token-file persistence. The callback file is absent
  in `v8.0.10`.
- `Manager.ResetQuota` in
  `sdk/cliproxy/auth/conductor_cooldown.go` resets every model entry for the
  credential, including unavailable/error state. It does not restrict the
  reset to an upstream usage window, verify the failure reason, or compare
  an expected generation. A healthy usage response cannot prove that an
  unrelated model failure or RPM cooldown is safe to clear, and a new failure
  can arrive between verification and reset.
- The quota-provider bridge reads credential storage before dispatching the
  plugin, even when metadata is supplied. It cannot meet a strict no-token-file
  access policy merely by ignoring `StorageJSON` inside the plugin.

The fork invokes neither bridge. A safe recovery extension needs a host-side
reason/model-scoped compare-and-reset operation under the manager lock,
plus fresh, complete quota evidence for precisely those scopes. Missing
scope evidence must fail closed. It must preserve unrelated auth errors,
model failures, disabled state, and healthy session bindings. This is a
follow-up host contract, not working recovery functionality in this fork.

Independent parent verification built the darwin/arm64 shared library,
checked all four ABI exports, and passed `TestQuotaRouterLoaderSmoke` through
isolated real host source copies of both `v8.0.10` and `v8.0.13`. No running
proxy or credential store was changed. Loader fixtures verify ABI/routing,
not live quota recovery or real T3 session-identity propagation.

Mock tests in this repository prove the plugin's own state transitions. They
prove nothing about live cooldown recovery at the host.

## Things verified against the pinned SDK

- `HostAuthFileEntry` has `id`, `auth_index`, `provider`, `status`, `disabled`,
  `unavailable`, `next_retry_after`, and also `email` and `path`. The plugin
  reads only `id`, `disabled`, `status`, `next_retry_after`, and logs none.
- `SchedulerAuthCandidate.Attributes` has credential-like keys removed by
  `schedulerSafeAttributes`; `auth_kind` survives.
- `ManagementRequest.Body` is `[]byte`; `ManagementRoute.Method` can be POST;
  management routes sit under `/v0/management` behind the management key.
- A response to `scheduler.pick` that names an ID outside the candidate list is
  logged as invalid and treated as unhandled (`normalizeSchedulerResponse`).
