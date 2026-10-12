# TDD evidence

Strict vertical cycles: one behaviour test, a captured failing run, the minimal
implementation, a captured passing run. Commands ran from the repository root
with Go 1.26.0. Output is trimmed to the relevant lines; nothing is edited.
No credential files, live hosts or provider endpoints were touched.

Baseline before any change:

```
$ go test ./...
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/quota	(cached)
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	(cached)
?   	github.com/OlivierDijkstra/cliproxyapi-quota-router/plugin	[no test files]
```

## Cycle 1: paid_fallback off rejects a key-only pool

Test: `TestPaidFallbackOffRejectsKeyOnlyPool` in `internal/router/paid_test.go`.

Red:

```
$ go test -run 'TestPaidFallbackOffRejectsKeyOnlyPool' ./internal/router
internal/router/paid_test.go:17:57: undefined: RejectCodePaidOnly
internal/router/paid_test.go:20:45: undefined: statePaid
internal/router/paid_test.go:21:43: undefined: statePaid
FAIL	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router [build failed]
```

Implementation: `statePaid`, `RejectCodePaidOnly`, `Config.PaidFallback`; API keys
and exhausted extra-usage accounts classify as paid; paid candidates count as
excluded so the pick is never delegated to the host.

Green for the new test, then the package run showed exactly the two old tests
that encoded the previous contract (paid treated as unknown and delegated):

```
$ go test -run 'TestPaidFallbackOffRejectsKeyOnlyPool' ./internal/router
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	0.471s

$ go test ./internal/router
--- FAIL: TestClaudeExtraUsageIsLastResort (0.00s)
    claude_test.go:206: credits = {... State:paid Reason:windows exhausted until 2026-10-01T14:00:00Z, extra usage billed per request ...}
--- FAIL: TestClaudeAPIKeysAreNotProbed (0.00s)
    claude_test.go:231: key = {... State:paid Reason:API key: billed per request ...}
FAIL
```

Those two were rewritten as `TestClaudeExtraUsageIsPaidByDefault` and
`TestClaudeAPIKeysArePaidAndNotProbed` (same fixtures, new expectations:
rejected with `paid_fallback_disabled`, never delegated).

```
$ go test ./internal/router
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	0.334s
```

## Cycle 2: paid opt-in is last resort; affinity never retains paid by default

Tests in `internal/router/paid_test.go`: `TestPaidFallbackOptInIsLastResort`,
`TestPaidFallbackCoversCodexKeys`, `TestUnsupportedProviderKeysStillDelegate`,
`TestAffinityLeavesPaidAccountAndSticks`, `TestAffinityNeverRetainsPaidByDefault`.

Red:

```
$ go test -run 'TestPaidFallback|TestUnsupportedProviderKeysStillDelegate|TestAffinity(LeavesPaid|NeverRetainsPaid)' ./internal/router
--- FAIL: TestAffinityLeavesPaidAccountAndSticks (0.00s)
    paid_test.go:115: picked "b" (handled=true, outcome=selected, reason="best smart score"), want "a"
--- FAIL: TestAffinityNeverRetainsPaidByDefault (0.00s)
    paid_test.go:158: resp={AuthID:a ... Reject:false ...} decision={... Reason:session affinity ... State:paid ...}
FAIL
```

The first failure was a fixture mistake (the account meant to win the first
pick had less quota); the fixture was corrected. The second is the intended red:
the affinity branch accepted any non-blocked binding, paid included.

Implementation: the affinity branch skips a paid binding unless `paid_fallback`
is on and `anySubscriptionUsable` is false.

```
$ go test -run 'TestPaidFallback|TestUnsupportedProviderKeysStillDelegate|TestAffinity(LeavesPaid|NeverRetainsPaid)' ./internal/router
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	0.219s
$ go test ./...
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/quota	(cached)
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	0.181s
?   	github.com/OlivierDijkstra/cliproxyapi-quota-router/plugin	[no test files]
```

## Cycle 3: cache-binding protection and session_minimum_headroom

Tests in `internal/router/affinity_test.go`. Regression guards for behaviour
that already existed: `TestSessionAffinitySlidesWithActivity`,
`TestSessionIsNotMigratedByBetterScore`,
`TestProbePickBindsAndSurvivesBetterAlternatives`,
`TestFailoverStaysOnReplacementAfterRecovery`,
`TestBoundSessionStillLeavesOnHardSignals`. New behaviour:
`TestBoundSessionSurvivesBelowNewSessionHeadroom`,
`TestSessionMinimumHeadroomConfig`.

Red (the new field does not exist, so the file does not compile):

```
$ go test -run 'TestSession|TestProbePickBinds|TestFailoverStays|TestBoundSession' ./internal/router
internal/router/affinity_test.go:122:16: r.Config().SessionMinimumHeadroom undefined (type Config has no field or method SessionMinimumHeadroom)
internal/router/affinity_test.go:123:72: r.Config().SessionMinimumHeadroom undefined (type Config has no field or method SessionMinimumHeadroom)
internal/router/affinity_test.go:197:23: cfg.SessionMinimumHeadroom undefined (type Config has no field or method SessionMinimumHeadroom)
FAIL	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router [build failed]
```

Implementation: `Config.SessionMinimumHeadroom` (default 0, validated to
`[0, minimum_headroom]`), `classifyWith(..., headroom)`, and the affinity branch
re-assesses a blocked bound candidate at the session headroom. Hard signals
(zero quota, rejection, limit flag, disabled, host status) still block because
`depleted(remaining, 0)` is true at zero and the other checks run before the
headroom check.

```
$ go test -run 'TestSession|TestProbePickBinds|TestFailoverStays|TestBoundSession' ./internal/router
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	0.219s
$ go test ./...
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/quota	(cached)
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	0.178s
?   	github.com/OlivierDijkstra/cliproxyapi-quota-router/plugin	[no test files]
```

## Cycle 4: an offered candidate is not vetoed by a cached retry time

Contract checked in the pinned SDK (`sdk/cliproxy/auth/conductor_selection.go`
`availableAuthsForSelector`, `retryRoundAvailabilityForAuth`, and
`selector.go` `availabilityBlock`): the host removes credentials whose
`NextRetryAfter`, `Unavailable` or quota state blocks them for the requested
model before it calls `scheduler.pick`. `host.auth.list` entries
(`internal/pluginhost/auth_callbacks.go` `buildHostAuthFileEntry`) carry the
credential-level `NextRetryAfter`, not the per-model one.

Test: `TestOfferedCandidateIsNotVetoedByCachedRetry` in `internal/router/router_test.go`.

Red:

```
$ go test -run 'TestOfferedCandidateIsNotVetoedByCachedRetry' ./internal/router
--- FAIL: TestOfferedCandidateIsNotVetoedByCachedRetry (0.00s)
    router_test.go:106: picked "b" (handled=true, outcome=selected, reason="best smart score"), want "a"
FAIL
```

Implementation: `classifyWith` blocks on the cached `Disabled` flag only. The
`NextRetryAfter` field stays in `HostAuthState` as informational data.
`TestPickSkipsIneligible` lost its `cooling` fixture (that veto no longer
exists; blocked count 5 to 4). This does not restore host-excluded candidates:
the plugin only ever answers with an ID from the offered list.

```
$ go test ./internal/router
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	1.285s
$ go vet ./...
(no output)
```

## Cycle 5: state_file round trip restores a binding in a fresh Router

Test: `TestStateFileRoundTripRestoresBinding` in `internal/router/persist_test.go`.

Red:

```
$ go test -run 'TestStateFileRoundTripRestoresBinding' ./internal/router
internal/router/persist_test.go:27:6: cfg.StateFile undefined (type Config has no field or method StateFile)
internal/router/persist_test.go:45:15: r1.SaveState undefined (type *Router has no field or method SaveState)
internal/router/persist_test.go:65:15: r2.LoadState undefined (type *Router has no field or method LoadState)
internal/router/persist_test.go:76:15: r2.SaveState undefined (type *Router has no field or method SaveState)
FAIL	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router [build failed]
```

Implementation: `Config.StateFile` (absolute path required), `persist.go`
(`SaveState`, `LoadState`, `State`; temp file + rename, 0600, symlink and
group/other-readable files refused, 4 MiB cap, schema version 1), affinity
bindings carry a full SHA-256 digest of the auth ID and restored bindings are
resolved against offered candidates by digest. Hooks: save at the end of every
`Refresh`, on `plugin.quiesce`/`plugin.shutdown`, and in the native
`cliproxyPluginShutdown`; load on `plugin.register`/`plugin.reconfigure`.

```
$ go test -run 'TestStateFileRoundTripRestoresBinding' ./internal/router
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	0.592s
$ go vet ./...
(no output)
$ go test ./...
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/quota	(cached)
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	0.207s
?   	github.com/OlivierDijkstra/cliproxyapi-quota-router/plugin	[no test files]
```

## Cycle 6: persistence edge cases (guards, not reds)

These were written after the cycle 5 implementation, which already had the
guards in place, so they passed on their first run. They are regression guards
rather than TDD reds and are recorded as such: `TestStateFileExpiredEntriesAreDropped`,
`TestStateFileRemovedAccountIsNotForced`, `TestStateFileBlockedAccountIsNotForced`,
`TestStateFileRefusesBadFiles` (corrupt JSON, wrong version, symlink,
world-readable), `TestStateFileSkipsMalformedEntries`,
`TestStateFileSaveSkipsWhenUnchanged`, `TestStateFileDisabledByDefault`,
`TestStateFileConfigRequiresAbsolutePath`, `TestStateFileStatusDoesNotLeakPath`,
`TestStateFileConcurrentSaveAndBind`, `TestStateFileLifecycleOverRPC`.

```
$ go test -race -run 'TestStateFile' ./internal/router
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	1.387s
```

## Cycle 7: POST /quota-router/invalidate, and config metadata alignment

Contract checked in the pinned SDK: `pluginapi.ManagementRequest.Body` is
`[]byte` (base64 over the JSON RPC), `ManagementRoute.Method` may be `POST`,
and `internal/pluginhost/management.go` `ServeManagementHTTP` reads the HTTP
body and passes it through under the management key.

Tests: `internal/router/invalidate_test.go` (`TestInvalidateDropsSnapshotAndKeepsBindings`,
`TestInvalidateRejectsBadRequests`, `TestInvalidateRefusesAmbiguousLabel`,
`TestInvalidateRouteIsRegistered`) and `TestConfigFieldsMatchConfigKeys` in
`internal/router/rpc_test.go`, which reflects over the YAML tags of `Config`
and the advertised `ConfigFields`.

Red:

```
$ go test -run 'TestInvalidate|TestConfigFieldsMatchConfigKeys|TestExampleConfigParses' ./internal/router
internal/router/invalidate_test.go:56:46: undefined: InvalidatePath
internal/router/invalidate_test.go:60:10: undefined: InvalidateResponse
internal/router/invalidate_test.go:100:29: undefined: InvalidatePath
internal/router/invalidate_test.go:146:46: undefined: InvalidatePath
internal/router/invalidate_test.go:169:49: undefined: InvalidatePath
FAIL	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router [build failed]
```

Implementation: `Store.Invalidate`, `Store.KnownIDs`, `InvalidatePath`,
`InvalidateRequest`/`InvalidateResponse`, route registration, JSON responses
for 200/400/404/405/409, label resolution by hash (raw ID only with
`reveal_auth_ids`), and the three missing `ConfigFields`. `config.example.yaml`
gained the new keys so `TestExampleConfigParses` still equals the defaults.

```
$ go test -run 'TestInvalidate|TestConfigFieldsMatchConfigKeys|TestExampleConfigParses|TestManagementStatus|TestOtherMethods' ./internal/router
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	0.224s
$ go test -race -count=1 ./...
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/quota	1.321s
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	1.943s
?   	github.com/OlivierDijkstra/cliproxyapi-quota-router/plugin	[no test files]
$ go vet ./...
(no output)
$ gofmt -l internal plugin
internal/router/config.go
$ gofmt -w internal/router/config.go
```

## Final runs and build

`scripts/build-plugin.sh` could not be run in this session (the tool sandbox
refused the script's piped shell), so the exact `go build` it performs was run
directly. The script's `nm` export check was not run. The cgo-generated
`dist/cliproxyapi-quota-router.h` declares all four entry points
(`cliproxy_plugin_init`, `cliproxyPluginCall`, `cliproxyPluginFree`,
`cliproxyPluginShutdown`, lines 144 to 147), which cgo only emits for
`//export` functions it compiled into the library. The reviewer should still
run the script for the `nm` check and the host smoke test.

```
$ go build -trimpath -buildvcs=false -buildmode=c-shared -o dist/cliproxyapi-quota-router.dylib ./plugin
(no output; dist/cliproxyapi-quota-router.dylib and .h written)

$ go test -race -count=1 ./...
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/quota	1.277s
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	1.643s
?   	github.com/OlivierDijkstra/cliproxyapi-quota-router/plugin	[no test files]
```

Not run here, for the reviewer: `golangci-lint run ./...`,
`scripts/host-smoke.sh dist/cliproxyapi-quota-router.dylib <CLIProxyAPI v8.0.13 checkout>`.

# Review round 1: four findings

The independent review failed the implementation above on four findings. Each
was fixed with a regression test that failed first. Tests for findings 1, 3 and
4 use only pre-existing API, so their reds are behavioural failures, not
compile errors.

## Finding 1 (high): below-headroom quota with overage allowed classified as paid

Test: `TestBelowHeadroomWithOverageAllowedIsNotPaid` in
`internal/router/affinity_test.go` (subtests `mixed pool`, `single bound account`).

Red:

```
$ go test -run 'TestBelowHeadroomWithOverageAllowedIsNotPaid' ./internal/router
--- FAIL: TestBelowHeadroomWithOverageAllowedIsNotPaid (0.03s)
    --- FAIL: TestBelowHeadroomWithOverageAllowedIsNotPaid/mixed_pool (0.00s)
        affinity_test.go:223: picked "b" (handled=true, outcome=selected, reason="best smart score"), want "a"
    --- FAIL: TestBelowHeadroomWithOverageAllowedIsNotPaid/single_bound_account (0.02s)
        affinity_test.go:254: picked "" (handled=true, outcome=rejected, reason="only paid candidates remain and paid_fallback is off"), want "a"
FAIL
```

Fix: `classifyWith` now decides exhaustion with `assess(..., headroom 0, ...)`
before looking at extra usage. Quota below the configured headroom but above
zero is a headroom exclusion (blocked for new sessions, re-assessed for bound
ones); only actual exhaustion can be paid. After the fix the pick was right
but the decision reason read `session affinity`; the test asserts the
headroom note on the decision, so the reason became
`session affinity (kept for bound session below new-session headroom)`.

Green run is shared with findings 3 and 4 below.

## Finding 3 (medium): save bookkeeping not tied to the destination

Test: `TestStateFileSaveFollowsReconfiguredPath` in `internal/router/persist_test.go`.

Red:

```
$ go test -run 'TestStateFileSaveFollowsReconfiguredPath' ./internal/router
--- FAIL: TestStateFileSaveFollowsReconfiguredPath (0.06s)
    persist_test.go:357: B after reconfigure: st=<nil> err=<nil>
FAIL
```

Fix: `SaveState` reads the configuration under `persistMu` and skips only when
generation and `savedPath` both match; `SetConfig` resets the bookkeeping when
`state_file` changes.

## Finding 4 (medium): reconfigure saved to the new file before loading it

Test: `TestReconfigureRestoresBeforeFirstSave` in `internal/router/persist_test.go`.

Red:

```
$ go test -run 'TestReconfigureRestoresBeforeFirstSave' ./internal/router
--- FAIL: TestReconfigureRestoresBeforeFirstSave (0.03s)
    persist_test.go:405: after reconfigure: bindings=1 state={Enabled:true Restored:0 LastSave:0001-01-01 00:00:00 +0000 UTC LastError:}
FAIL
```

Fix: `SetConfig` is serialized by `configMu` and ordered: stop a running
refresher under the old configuration, then under `persistMu` publish the new
configuration, reset save bookkeeping and load the new file, then restart the
refresher. The RPC handler no longer calls `LoadState` separately, so every
`SetConfig` caller gets the same ordering. Lock order is `configMu` →
`persistMu` → `r.mu`; `StopRefresher` is called before `persistMu` is taken
because the refresh loop's deferred save needs `persistMu`.

Green for all three, then the package under the race detector:

```
$ go test -run 'TestBelowHeadroomWithOverageAllowedIsNotPaid|TestStateFileSaveFollowsReconfiguredPath|TestReconfigureRestoresBeforeFirstSave' ./internal/router
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	0.239s
$ go test -race -count=1 ./internal/router
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	1.835s
```

## Finding 2 (medium): Lstat-then-ReadFile race and unbounded read

Tests in `internal/router/persist_test.go`: `TestStateFileReadIsBounded`,
`TestStateFileGrowthAfterOpenIsRejected` (file grown past the cap after the
descriptor was opened, refused via the descriptor's own Stat),
`TestStateFileOpenRefusesSymlink`, `TestStateFileDescriptorPermissionCheck`.
They name new helpers, so the red is a build failure:

```
$ go test -run 'TestStateFileReadIsBounded|TestStateFileGrowthAfterOpenIsRejected|TestStateFileOpenRefusesSymlink|TestStateFileDescriptorPermissionCheck' ./internal/router
internal/router/persist_test.go:434:17: undefined: readBounded
internal/router/persist_test.go:437:15: undefined: readBounded
internal/router/persist_test.go:447:12: undefined: openStateFile
internal/router/persist_test.go:463:15: undefined: readOpenStateFile
internal/router/persist_test.go:478:15: undefined: openStateFile
internal/router/persist_test.go:495:12: undefined: openStateFile
internal/router/persist_test.go:500:15: undefined: readOpenStateFile
FAIL	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router [build failed]
```

Fix: `go.sum` has no `golang.org/x/sys`, so the standard `syscall` package is
used. `persist_nofollow.go` (`darwin || linux || freebsd`) opens with
`O_RDONLY|O_NOFOLLOW` and maps `ELOOP`/`EMLINK` to `errStateSymlink`;
`persist_follow.go` (every other target, Windows among the release builds)
keeps an Lstat before the open and documents the remaining window. Type,
permissions and size are then checked on the open descriptor
(`readOpenStateFile`), the read goes through `readBounded` with
`maxStateFileBytes+1`, and the descriptor is closed by `readStateFile`. Only
the final path component is protected from symlinks; parent directories are
resolved normally. There is no fallback to an unbounded read.

```
$ go test -run 'TestStateFileReadIsBounded|TestStateFileGrowthAfterOpenIsRejected|TestStateFileOpenRefusesSymlink|TestStateFileDescriptorPermissionCheck|TestStateFileRefusesBadFiles' ./internal/router
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	0.289s
```

## Final runs for review round 1

```
$ go test -race -count=1 ./...
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/quota	1.720s
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	2.544s
?   	github.com/OlivierDijkstra/cliproxyapi-quota-router/plugin	[no test files]
$ go vet ./...
(no output)
$ GOOS=windows go vet ./internal/router
(no output)
$ gofmt -l internal plugin
(no output)
```

Not run in this round: the c-shared build and the host loader smoke; the
reviewer reported both passing on 8.0.10 and 8.0.13 before these fixes and
will rerun them.

# Review round 2: two findings and a Windows qualification

## Finding 1 (medium): stale below-headroom binding discarded

The affinity re-assessment accepted only `stateEligible`. A bound account with
positive quota below the new-session headroom, a future reset and headers
older than `stale_after` re-assesses as `probe`/`unknown`, which was thrown
away, so the session migrated or was rejected.

Test: `TestBoundSessionKeepsStaleBelowHeadroomAccount` in
`internal/router/affinity_test.go` (overage allowed and rejected, mixed and
single pools, plus a guard that stale true zero stays blocked).

Red:

```
$ go test -run 'TestBoundSessionKeepsStaleBelowHeadroomAccount' ./internal/router
--- FAIL: TestBoundSessionKeepsStaleBelowHeadroomAccount (0.00s)
    --- FAIL: TestBoundSessionKeepsStaleBelowHeadroomAccount/overage_allowed_mixed_pool (0.00s)
        affinity_test.go:296: picked "b" (handled=true, outcome=selected, reason="best smart score"), want "a"
    --- FAIL: TestBoundSessionKeepsStaleBelowHeadroomAccount/overage_allowed_single_bound_account (0.00s)
        affinity_test.go:314: picked "" (handled=true, outcome=rejected, reason="every candidate is blocked"), want "a"
    --- FAIL: TestBoundSessionKeepsStaleBelowHeadroomAccount/overage_rejected_mixed_pool (0.00s)
        affinity_test.go:296: picked "b" (handled=true, outcome=selected, reason="best smart score"), want "a"
    --- FAIL: TestBoundSessionKeepsStaleBelowHeadroomAccount/overage_rejected_single_bound_account (0.00s)
        affinity_test.go:314: picked "" (handled=true, outcome=rejected, reason="every candidate is blocked"), want "a"
FAIL
```

The `stale true zero stays blocked` subtest passed before the fix and guards it.

Fix: one condition, `sv.State != stateBlocked` instead of
`sv.State == stateEligible`. Exhaustion, rejection, limit flags and disabled
block at every headroom, so they are unaffected; the paid guard in the switch
below still applies to whatever state is accepted.

## Finding 2 (medium): blocking open on a FIFO

Test: `TestStateFileRefusesFIFO` in `internal/router/persist_fifo_test.go`
(`darwin || linux || freebsd`). It runs the read in a goroutine with a
two-second deadline and, on timeout, connects a non-blocking writer so the
blocked reader returns before the test fails; the red run exited in 2.17s with
no leaked goroutine.

Red:

```
$ go test -run 'TestStateFileRefusesFIFO' ./internal/router
--- FAIL: TestStateFileRefusesFIFO (2.00s)
    persist_fifo_test.go:41: opening a FIFO blocked instead of being refused
FAIL
```

Fix: `O_NONBLOCK` added to the open flags in `persist_nofollow.go`. The
descriptor type, permission and size checks and the bounded read are
unchanged, so the FIFO is refused as "not a regular file".

## Windows qualification

The `world readable` case of `TestStateFileRefusesBadFiles` now skips on
Windows, because the implementation does not enforce owner-only permissions
there and the 0600 mode used when writing does not create an ACL. README and
DESIGN.md state the Unix-only privacy guarantee, the Windows pre-open symlink
window, and that the strict descriptor validation is the macOS/Linux
behaviour. Nothing in this round touched the native ABI code in `plugin/`.

## Final runs for review round 2

```
$ go test -run 'TestBoundSessionKeepsStaleBelowHeadroomAccount|TestStateFileRefusesFIFO' ./internal/router
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	0.327s
$ go test -race -count=1 ./...
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/quota	1.340s
ok  	github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router	1.701s
?   	github.com/OlivierDijkstra/cliproxyapi-quota-router/plugin	[no test files]
$ go vet ./...
(no output)
$ GOOS=windows go vet ./internal/router
(no output)
$ gofmt -l internal plugin
(no output)
```

The Windows build was cross-vetted only, not executed. The c-shared build and
host loader smoke were not rerun in this round.

# Cache identity round: real host ingress

`internal/router/testdata/ingress-v8.0.23.json` holds scheduler.pick requests
recorded by CLIProxyAPI's `test/scheduler_ingress_identity_test.go`, which sends
Claude Code shaped HTTP requests through the real v8.0.23 Claude handler, auth
manager and executors. `ingress_test.go` replays them through scheduler.pick.

## Cycle 8: one model's move rewrote the session's binding

Tests: `TestIngressModelSwitchKeepsMainModelPin` and
`TestIngressProviderSwitchKeepsPin` in `internal/router/ingress_test.go`.

Red:

```
$ go test -count=1 -run 'TestIngress' ./internal/router/
--- FAIL: TestIngressModelSwitchKeepsMainModelPin (0.00s)
    ingress_test.go:254: opus after haiku moved: picked "acct-b" (session affinity), want "acct-a"
--- FAIL: TestIngressProviderSwitchKeepsPin (0.00s)
    ingress_test.go:277: back on claude: picked "acct-b" (best smart score), want "acct-a"
FAIL
```

Implementation: `bindingKey(sessionKey(...), req.Model)`; the thinking suffix
is dropped from the model. Green: `go test -count=1 ./...` passes.

## Cycle 9: an offered candidate with host status "error" was blocked

Found by `TestQuotaRouterEndToEnd` (CLIProxyAPI `test/quota_router_e2e_test.go`,
build tag `quotarouter_e2e`) with the built library: after a 429 on the haiku
helper, the host offered acct-a for opus with status `error`, and the plugin
classified it `blocked` ("host status error"), moving the opus session.

Test: `TestOfferedErrorStatusKeepsBoundSession` in `internal/router/affinity_test.go`.

Red:

```
$ go test -count=1 -run TestOfferedErrorStatusKeepsBoundSession ./internal/router
--- FAIL: TestOfferedErrorStatusKeepsBoundSession (0.00s)
    affinity_test.go:369: picked "b" (handled=true, outcome=selected, reason="best smart score"), want "a"
FAIL
```

Implementation: only `disabled` vetoes an offered candidate. Green: the unit
test and the end-to-end run pass; the same end-to-end run against the library
built from 9d44b64 still fails at "S1 opus keeps its account after haiku moved".
