# eRPC polyglot proxy — Step 1 feasibility map

Repo read: `/Users/michaelmclaughlin/Documents/valve-tech/github/erpc`, branch
`fix/empty-result-rotation` at `b2a8302`, 4 commits ahead of `origin/main`.
`go build ./...` is green. 83,473 non-test Go lines.

All evidence below is `file:line` in that repo.

## Verdict

**Extend. The config-driven pattern is achievable, and most of it already
exists.** No hard blocker for Bitcoin. One real shape problem for the beacon —
see "The beacon exception".

> **Update (2026-08-13, after the upstream catch-up).** Upstream eRPC has since
> shipped Solana and a `common.ArchitectureRegistry` — a pluggable
> `ArchitectureHandler` interface whose own comment reads *"Pipeline files
> dispatch through the registry — they never need to change for a new chain."*
> Their SVM state poller feeds `tracker.SetLatestBlockNumber`, confirming the
> central seam this map identified. But their shape is a **compiled Go package
> per architecture**, not config. Recommendation now: copy `architecture/svm` →
> `architecture/btc` and register a handler, rather than building a config
> layer beside upstream's switches. Details in
> [erpc-merge-review.md](erpc-merge-review.md).

Three facts drive the verdict.

1. **The ranking and rotation core is already chain-agnostic.** The health
   tracker stores a plain `int64` height and a derived lag
   (`health/tracker.go:1314`, `health/tracker.go:110`). The selection-policy
   engine reads only those tracker numbers (`internal/policy/eval.go:74`,
   `internal/policy/eval.go:290`). Nothing on that path knows about Ethereum.
2. **The declarative config layer is already built.** The fork embeds a JS
   runtime (`sobek`, `internal/policy/runtime_pool.go:8`) and evaluates an
   operator-supplied policy program each tick
   (`internal/policy/default_policy.js`, 1,655-line stdlib at
   `internal/policy/stdlib/stdlib.js`). The default program already drops a
   lagging node with `blockNumberLagAbove(16)` /
   `blockSecondsLagAbove(30)` (`internal/policy/default_policy.js:41`). That is
   the Dshackle behaviour, in config, today.
3. **The per-architecture strategy seam already exists and is already
   injected.** `common.JsonRpcErrorExtractor` (`common/error_extractor.go:7`) is
   an interface chosen per architecture and wired in at
   `upstream/registry.go:87`. A second implementation is an additive file, not a
   rewrite.

The EVM coupling is real but concentrated. It sits in a **feeder**, a **request
adapter**, and four **validators**. Each one is replaceable additively.

---

## 1. Upstream model

`UpstreamConfig` carries a `Type` plus one optional per-family block
(`common/config.go:817-845`):

```go
Type UpstreamType          // common/config.go:819
Evm  *EvmUpstreamConfig    // common/config.go:843
JsonRpc *JsonRpcUpstreamConfig
Grpc *GrpcUpstreamConfig
```

`UpstreamType` has exactly one value: `UpstreamTypeEvm`
(`common/architecture_evm.go:11`). `NetworkArchitecture` likewise has one:
`ArchitectureEvm` (`common/network.go:15`). An unset type defaults to `evm`
(`common/defaults.go:1678`, with a `TODO make actual calls to detect other types
(solana, btc, etc)?`).

**The interfaces above the config are already generic.** `common.Upstream`
(`common/upstream.go:40-60`) names no chain. `clients.ClientInterface`
(`clients/registry.go:20-23`) is two methods, `GetType` and `SendRequest`.

The URL grammar is already polyglot. `parseUrlPath` treats architecture as a
free string in `/<project>/<architecture>/<chainId>`
(`erpc/http_server.go:884-947`), and `resolveNetworkConfig` splits `networkId`
on `:` and only then switches on the architecture
(`erpc/networks_registry.go:344-356`).

**Concrete upstream struct coupling:** `upstream.Upstream` holds one named
field, `evmStatePoller` (`upstream/upstream.go:187`), constructed only when
`Type == UpstreamTypeEvm` (`upstream/upstream.go:291-300`).

## 2. Health / liveness

Two separate systems. Only one of them is EVM-coupled.

**Chain-agnostic (keep as is):** `health.Tracker` records latency, error rate,
throttle rate, cordon state and block-head lag per upstream and method
(`health/tracker.go`). Its head input is a bare integer:

```go
func (t *Tracker) SetLatestBlockNumber(upstream common.Upstream, blockNumber int64, blockTimestamp int64)
// health/tracker.go:1314
```

Cordon/uncordon (`health/tracker.go:793`, `:820`) is likewise chain-free.

**EVM-hardwired:** the operator health endpoint.
- `checkEvmChainId` calls `eth_chainId` per upstream and fails the upstream when
  it mismatches (`erpc/healthcheck.go:815-870`).
- The upstream filter switches on architecture and only handles `evm`
  (`erpc/healthcheck.go:207-217`). A non-EVM upstream silently drops out of the
  filtered list.
- Diagnostics come from the EVM poller only (`erpc/healthcheck.go:266-270`).
- The static-upstream count for `EvalAllActiveUpstreams` compares
  `upsCfg.Evm.ChainId` (`erpc/healthcheck.go:516-521`).

`eth_syncing` lives in the poller, not the health endpoint
(`architecture/evm/evm_state_poller.go:1171`, `fetchSyncingState`).

## 3. Tip / height

One producer, one consumer, and a clean pipe between them.

**Producer (EVM-only):** `EvmStatePoller`
(`architecture/evm/evm_state_poller.go`, 1,581 lines). It calls
`eth_getBlockByNumber` for latest/finalized (`:1116`, `fetchBlock`) and
`eth_syncing` (`:1171`). Its interface is 17 methods, all EVM-named
(`common/architecture_evm.go:104-123`).

**The pipe:** the poller writes into the tracker via `SetLatestBlockNumber`
(above). That call is the entire contract between "how you learn the tip" and
"how the tip is used".

**Consumers:** the policy engine's `blockHeadLag` /
`blockHeadLagSeconds` (`internal/policy/eval.go:74`, `:95`, `:290`), the
Prometheus head-lag gauge (`health/tracker.go:428`), the network-wide head
recompute (`health/tracker.go:1279`), and the served-tip machinery
(`erpc/networks.go:1210`, `:1759`, `:1804`).

**This is the cleanest seam in the tree.** A Bitcoin poller that calls
`getblockchaininfo`, reads `blocks`, and calls `SetLatestBlockNumber` gets
head-lag exclusion, the lag gauge, and the "pick the most caught-up upstream"
ranking for free — with zero change to the consumers.

Separate EVM-only paths that a Bitcoin pattern simply does not enter: finality
(`IsBlockFinalized`, `common/architecture_evm.go:112`), earliest-block probes
(`architecture/evm/evm_state_poller.go:744`, `:1255-1535`), and block-availability
bounds (`erpc/networks.go:1863`, which returns early for any non-EVM
architecture).

## 4. Failover / failsafe

Four layers. Three are generic; one holds an EVM method list.

- **Selection policy (generic).** The JS program excludes and ranks upstreams
  per tick from tracker metrics only (`internal/policy/engine.go:261`,
  `:343`, `:424`). The prober re-admits an excluded upstream by mirroring
  sampled real traffic (`internal/policy/prober.go`).
- **Failsafe policies (generic).** Retry, hedge, timeout, circuit breaker,
  consensus — matched per `(method, finality)` (`upstream/upstream.go:206-226`,
  `failsafe/`).
- **Rotation (generic mechanism, EVM-specific list).**
  `NormalizedRequest.MarkUpstreamCompleted` decides whether a response frees the
  upstream for reuse or records `ErrEndpointMissingData` and rotates
  (`common/request.go:1596-1640`). The rule it consults is
  `IsEmptyResultAccepted(netCfg, method)` (`common/request.go:1619`), resolved
  through `common/empty_result.go:16-70` — the branch's own work.
  The default list is nine hardcoded `eth_*` methods
  (`common/defaults.go:2083-2096`).
- **Error classification (already pluggable).**
  `common.JsonRpcErrorExtractor` (`common/error_extractor.go:7-15`), implemented
  at `architecture/evm/extractor.go:11`, injected at `upstream/registry.go:87`.

**The brief's `ClassifyResponse` maps onto two existing hooks, not one new one:**
the extractor decides "is this an error and which kind", and
`emptyResultAccept` decides "is an empty result final or a rotate signal".
Generalizing means making the default list per-family instead of per-build. The
branch's work does not fight this; it is the exact insertion point.

## 5. Request shape

This is where the real constraint sits.

- **Ingress assumes a JSON body with JSON-RPC batch semantics.**
  `isBatch := len(body) > 0 && body[0] == '['` (`erpc/http_server.go:419`).
- **`NormalizedRequest` carries a body and nothing else.** No HTTP verb, no
  path, no query string (`common/request.go:379-421`). `Method()` reads
  `"method"` out of the JSON body, though it will short-circuit on a
  pre-set `r.method` (`common/request.go:1083-1108`).
- **`NormalizedResponse` is friendlier.** It holds the raw `io.ReadCloser` and
  parses the JSON-RPC view lazily (`common/response.go:14-33`), so a non-JSON-RPC
  response body can ride through unparsed.
- **The transport switch has one arm.** `clients/registry.go:85` builds an HTTP
  JSON-RPC client or a gRPC BDS client, both under `case common.UpstreamTypeEvm`.
- **The EVM hooks are method-dispatched and default to pass-through**
  (`architecture/evm/hooks.go:13`, `:61`, `:86`, `:109`). A Bitcoin method name
  falls to `default` and is a no-op. Note that
  `evm.HandleUpstreamPostForward` and `evm.HandleNetworkPostForward` are called
  *outside* the architecture switch (`erpc/networks.go:1703`,
  `erpc/projects.go:268`) — harmless today, but it should move inside the switch.

## The four hard gates

These reject a non-EVM architecture today. Each is a small, additive change.

| # | Gate | Site |
|---|------|------|
| 1 | `IsValidArchitecture` returns `architecture == "evm"` | `common/network.go:34-36` (called at `erpc/http_server.go:1028`) |
| 2 | `IsValidNetworkId` requires the `evm:` prefix | `util/ids.go:21-27` (called at `erpc/networks_registry.go:201`) |
| 3 | `Network.prepareRequest` returns "unsupported architecture" in its default branch | `erpc/networks.go:1553-1576` |
| 4 | `clients.CreateClient` builds no client for a non-`evm` upstream type | `clients/registry.go:85` |

Gate 3 is the load-bearing one. It is also the natural home for the pattern's
request adapter.

## The beacon exception

Bitcoin fits. It is JSON-RPC over HTTP POST, so it reuses the whole ingress,
`NormalizedRequest`, transport, and rotation machinery. The work is a poller, an
extractor, an empty-result list, and a `prepareRequest` arm.

The beacon does not fit as cleanly. It is REST: the request identity is
`GET /eth/v1/node/health`, and eRPC has nowhere to put a verb, a path, or a
query string (`common/request.go:379-421`), while ingress reads the first byte
of the body to decide batching (`erpc/http_server.go:419`). Supporting it means
adding a request shape and a transport in Go — real work, not a config block.
Config alone cannot express it.

**This contradicts nothing that was already decided — it confirms it.**
`docs/superpowers/plans/beacon-routing.md` already routes `beacon` around eRPC,
straight to the beacon client through the Caddy front, with
`/eth/v1/node/health` as the pool health gate. That plan is the cheaper and
still-correct answer. My recommendation: keep it, and do not pull the beacon
into eRPC for v1.

## Recommended v1 scope

**In:** a declarative chain pattern covering health, tip, rotation rule, and
JSON-RPC routing. Bitcoin as the first non-EVM pattern. EVM re-expressed as a
pattern, with its existing defaults preserved so no existing config changes.

**Deferred:** the beacon (stays in the Caddy front); family-specific caching;
family-specific finality; the earliest-block availability probes.

## Merge risk

Low, and mostly confined to five files. New pattern code lands in new packages
(`architecture/btc/`, plus a pattern registry). The shared-code edits are the
four gates plus the two misplaced hook calls — each one or two lines inside a
switch that upstream eRPC is unlikely to restructure. The largest single risk is
`common/defaults.go:2083`, where the empty-result list becomes per-family;
upstream edits that list, so expect a conflict there on merge.

## Which branch to build on — RESOLVED

This map was read on `fix/empty-result-rotation`, which has no WebSocket
support. The fork's WebSocket work lived on a separate line (`origin/valve-ws`,
the branch that actually ships as tag `valve-ws-v1`), and the two lines had
diverged with no reachable merge base.

That is now reconciled: **`reconcile/ws-plus-main`** (merge commit `b684745`)
carries WebSocket, the current policy engine, and the `emptyResultAccept`
resolver together, with green tests. Build the chain patterns there.

The reconcile also settles one thing this map assumed. The policy engine —
which the whole config-driven design rests on — exists only on the main-tracking
line. The shipping WS line carried an older engine (25 files vs 31). Without the
reconcile, the pattern work would have been built against the older engine or
without WebSocket.

See [erpc-fork-reconcile.md](erpc-fork-reconcile.md) for the triage, the
resolutions, and one open decision (the per-request fallback escape).
