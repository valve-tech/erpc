# eRPC fork — code review of the reconcile and the upstream catch-up

Branch `reconcile/ws-plus-main`, head `151c29c`. **0 behind upstream `erpc/erpc`**,
18 fork commits on top. Build clean, vet clean, tests green. Not pushed.

Two merges got here:

| commit | what |
|---|---|
| `b684745` | valve's WebSocket line onto the fork's main (12 of 18 commits kept) |
| `151c29c` | 39 upstream commits, up to `a301305` (2026-08-13) |

## Scope of this review

Covered in full: every conflict resolution, every post-merge repair, the 12
carried valve commits, and each upstream change that touches fork code.

Covered by shape only: the bulk of upstream's +48,398 lines. I read the commit
subjects and rationale for all 39, and read the code for the ones that collide
with fork surfaces. I did not line-review upstream's SVM implementation or its
data-integrity module. Say so if you want that.

## The finding that matters most

**Upstream now has a registry for exactly the thing the polyglot brief asks for.**

`common/architecture.go` defines `ArchitectureHandler` — five pipeline hooks
plus `NewJsonRpcErrorExtractor()` — and `ArchitectureRegistry`, populated by
each architecture package's `init()`. Its own comment: *"Pipeline files dispatch
through the registry — they never need to change for a new chain."*

And `architecture/svm/svm_state_poller.go:137` calls
`tracker.SetLatestBlockNumber(...)` — the exact seam the feasibility map named
as the cleanest in the tree. An independent implementation confirmed the
prediction.

But upstream's shape is a **compiled Go package per architecture**, not config.
That conflicts with the brief's bar ("config, not a Go type"). Recommendation
change: copy `architecture/svm` → `architecture/btc`, register a handler, extend
the enums. You inherit a working reference and stop fighting every upstream
pull. Add a config layer only if a third chain proves the duplication real.

Migration is partial. The registry is used at 3 sites; **11 `case
common.Architecture*` switches remain**, including `prepareRequest`, the client
factory, `IsValidArchitecture`, and `IsValidNetworkId`. Those are the same gates
the feasibility map listed — upstream extended them by hand for SVM rather than
routing them through the registry.

## Conflict resolutions — the ones worth challenging

**`erpc/networks.go` → upstream (merge 1).** Every valve change here was the
cross-pod monotonic block mechanism (`latestBlockShared`, `finalizedBlockShared`,
`evmHighestBlockNumber`, `tierUpstreamsByGroup`), superseded by the served-tip
design. I checked explicitly that none of it was WebSocket code before dropping
it. This is the single largest thing discarded — worth your eye.

**`clients/registry.go` → hand-merged twice.** Merge 1 took valve's structure
(`clientCreations` keyed by `UniqueUpstreamKey`, plus the ws/wss client) with
upstream's gRPC pool size. Merge 2 ported upstream's new `UpstreamTypeSvm` arm
into that structure — upstream wrote it against the old `newClient`/`clientErr`
shape, so it could not be taken verbatim. Verified the extractor it passes is
`NewCompositeJsonRpcErrorExtractor()`, which dispatches by architecture; the
field is still named `evmExtractor`, which now reads as a lie. Upstream's naming
debt, not worth a fork-local rename.

**`erpc/http_server.go` gzip middleware.** Ordering is load-bearing: the
WS-upgrade bypass must return *before* upstream's new `Vary: Accept-Encoding`
and before the writer is wrapped, or the upgrade 500s. Verified in the merged
file.

**`docs/pages/**` → upstream (merge 1).** This dropped valve's WebSocket
annotations from four upstream doc pages, keeping shared pages conflict-free on
future pulls. The fork's standalone `docs/pages/operation/websocket.mdx`
survives. Reversible if you want the annotations back.

## Post-merge repairs the auto-merge could not see

Each of these compiled only because I went looking; none surfaced as a conflict.

1. Duplicate `SharedStateRegistry` accessor (twice — valve added one, then
   upstream re-added one).
2. Orphaned `evmStatePollerMu.Unlock()` left after taking upstream's `sync.Once`
   poller guard.
3. Network-level shared-counter wiring in `networks_registry.go`, referencing
   fields the `networks.go` resolution removed.
4. `architecture/evm/block_ref.go` calling `EvmHighestLatestBlockNumber` as a
   method. Upstream moved it off `common.Network` onto `EvmNetwork` and added
   package-level helpers that type-assert; the call now routes through those.
5. Fork tests on the pre-`Tags` `Group` API, and on older
   `NewUpstreamsRegistry` / `NewNetwork` / `NewHttpServer` / `NewERPC`
   signatures.

Item 4 is the one that would have bitten silently in a less careful merge —
`ResolveCacheBlockRef` wraps the call in a `recover()` that swallows failures and
falls back to the tag literal, so a broken resolution degrades cache keying
rather than erroring.

## Valve's 12 commits — all verified present after both merges

Two of these fix bugs **still live on upstream today**, and are worth sending back:

1. **Breaker wedges open until process restart.** `failsafe/breaker.go`
   returned early on `OutcomeIgnore` without releasing the half-open trial
   permit, so `halfOpenInflight` leaks. Once it saturates trial capacity every
   `TryAcquirePermit` in HalfOpen is denied — real traffic and recovery probes
   both fail, permanently. Ignorable outcomes (timeouts, cancellations) are
   exactly what a transient blip produces during a trial.
2. **A batch entry can be forwarded to the wrong chain.** The per-entry
   goroutine captured `architecture`/`chainId` by reference while the body
   assigns to them, so the first entry to resolve published its network to every
   sibling.

The rest: WS support + indexer, WS self-heal, sharedState keep-latest, four
upstream-instance leak paths, gzip source-close (now on upstream's
`klauspost/compress`), `tx_replay_attack` normalization, and the 5-method
`TranslateLatestTag` set (a strict superset of upstream's 1).

## Fork code quality — spot review

Read `erpc/ws_server.go`, `erpc/subscription_manager.go`, `indexer/`.

Good: writes to the client conn are serialized under `writeMu` with deadlines,
and `pingLoop` correctly uses `WriteControl` (gorilla's separate control lock) so
a slow data write cannot delay the liveness signal. Per-message handling runs in
its own goroutine with its own `recover()`, so a panic cannot kill `readLoop`.

One nit: `wsc.Close()` at `ws_server.go:131` is not deferred — it runs after
`readLoop()` returns. The panic path is already covered by the per-message
recover, so this is latent fragility rather than a live defect, but `defer`
would cost nothing.

One gap: `SubscriptionManager` is EVM-only (`eth_subscribe`/`eth_unsubscribe`).
On an SVM network it fails cleanly — SVM upstreams are http-only so
`GetWsUpstreams` is empty — but Solana's `slotSubscribe`/`accountSubscribe` are
unhandled. Not a bug today; ours to own if we ever serve Solana over WebSocket.

## Two operational notes

1. **`6cd1b98` flips no-upstreams from 503 to 404.** Terminal state, not
   transient. Caddy or alerting logic keyed on 5xx will stop seeing this
   condition. Upstream's guidance: alert on
   `erpc_network_no_upstreams_available_total`.
2. **`1c89ac9` adds `counterDropLabels` / `counterLabelOverrides`.** This is the
   upstream fix for unbounded counter labels — the mechanism behind the fork's
   phantom-server metric noise.

## Build environment gotcha

`GOPROXY` is `direct` on this machine. The dependency bump in `a0200ff` made
`go build` stall for **20+ minutes at 0% CPU**, fetching pseudo-version modules
from source. It is not a slow compile and not a lock:

```
GOPROXY=https://proxy.golang.org,direct go mod download all   # ~7s
go build ./...                                                # ~12s
```

Worth setting `GOPROXY` persistently before the next upstream pull.

## Verification

- `go build ./...` clean. `go vet ./...` clean — the one `copylocks` warning at
  `erpc/query_executor_test.go:341` pre-dates this work (present on the base
  branch).
- Green: `common`, `common/legacy`, `upstream`, `clients`, `failsafe`, `health`,
  `internal/policy`, `internal/policy/stdlib`, `internal/simulator`,
  `architecture/evm`, `architecture/evm/integrity`, `architecture/svm`, `util`,
  `indexer`, `indexer/adapters/wsupstream`, `consensus`, `auth`, `thirdparty`.
- Every WebSocket and SVM suite passes.

> **CORRECTION — 2026-08-13.** This section first listed `erpc` as green and
> claimed the only failures were container-backed. Both statements were wrong,
> and the cause was the measurement, not the merge.
>
> I measured with `go test ./erpc/`. That package holds 457 tests and 927 `gock`
> mocks that share global HTTP-transport state, so it cannot run in parallel in
> one process. Serially it needs more than 950 seconds. Go's default timeout is
> 600 seconds, so the run was cut off and reported nothing about the tests it
> never reached. A truncated run is not a passing run, and I read it as one.
>
> Use the project's own harness instead. `make test-fast` compiles the erpc test
> binary once and shards it across processes — each process gets its own gock
> state — and covers the whole repo in about **424 seconds**. The repo's CI
> already uses it (`.github/workflows/test.yml:38`).
>
> The true failure set on `ec2e7b5` under `make test-fast` is three:
> `TestEvmJsonRpcCache_DynamoDB` and `TestEvmJsonRpcCache_Redis`, both needing a
> Docker daemon, and one real regression —
> `TestNetwork_HighestFinalizedBlockNumber/EvmHighestFinalizedBlockNumber_FallbackExcludedWhenPrimariesUp`.
>
> That regression belongs to this merge. The test came from valve's own
> `c8b22f9` and does not exist upstream. The merge kept valve's test and dropped
> valve's implementation: `tier:fallback`, `TierFallback` and `isFallback` have
> no remaining references in `erpc/networks.go` or `architecture/evm/`. It fails
> under process isolation too, so it is not test pollution. See
> [erpc-fallback-escape-decision.md](erpc-fallback-escape-decision.md) — the two
> share a root cause.

## Still open — your call

**Does the fork need the per-request fallback escape (`5a46e14`)?** Still
deferred. Upstream's policy engine does the tier split per *tick*
(`preferTag('!tier:fallback', {fallback:'tier:fallback'})` plus a `whenEmpty`
net); valve's fires per *request* after exhaustion. A request that exhausts its
primaries mid-tick still fails.

`FailoverConfig` is live — `erpc/subscription_manager.go:387` reads
`Failover.Enabled()` — but its `onDefaultsExhausted` suppression path has nothing
left to suppress, since `NewDefaultNetworkConfig` no longer carries a
`SelectionPolicy`. Pinned in `common/config_test.go` with a reconcile note
rather than quietly deleted.

Restoring it means porting `5a46e14` and `454c54d` onto the served-tip world —
their old integration point (`evmHighestBlockNumber`) is gone.
