# eRPC fork reconcile — WebSocket line onto current upstream

Done 2026-08-13 in `valve-tech/erpc`. Result branch: **`reconcile/ws-plus-main`**,
merge commit `b684745`. Build clean, vet clean, tests green. **Not pushed.**

> **Superseded in part.** A second merge (`151c29c`) then brought the fork up to
> upstream `erpc/erpc` — 39 commits, including Solana support and an
> architecture registry. The branch is now 0 behind upstream. Read
> [erpc-merge-review.md](erpc-merge-review.md) for that stage and the combined
> code review; this document remains the record of stage one.

## Why this was needed

`valve-tech/erpc` had two fork lines that could not see each other.

| line | tip | has WebSocket | has current policy engine | ships |
|---|---|---|---|---|
| `origin/valve-ws` | `e909aac`, tag `valve-ws-v1` | yes | no (25 files vs 31) | **yes** |
| `origin/main` → `fix/empty-result-rotation` | `b2a8302` | **no** | yes | no |

`origin/main:clients/registry.go:101` still reads `"websocket client not
implemented yet"`. The WS code — `clients/ws_json_rpc_client.go` (724 lines) and
`erpc/ws_server.go` (741 lines) — lived only on `valve-ws`.

`git merge-base origin/main origin/valve-ws` returned nothing. That was an
artifact: `.git/shallow` truncated `main` at 50 commits, below the fork point.
`git fetch --unshallow` restored full history (1,158 commits) and the real fork
point, **`3135a1b`** (2026-05-29, erpc#903). True divergence: `valve-ws` 15
ahead / 68 behind, `fix/ws-upgrade-behind-gzip` 18 ahead / 68 behind.

`git cherry` confirmed none of the 17 valve commits had landed upstream.

## What the straight merge showed

Merging `fix/ws-upgrade-behind-gzip` into the main line conflicted in **15
files, ~1,818 lines**, with `erpc/networks.go` alone at 6 hunks / 726 lines.

Reading the first conflict answered why. Upstream had replaced
`EvmHighestLatestBlockNumber` wholesale with the stateless served-tip majority
pick (`evm.PickServedTip`, `servedTipAnchor`, per-lane partitions). Valve's
commit `51faede` patched a monotonic guard inside a function that no longer
exists. The big-bang merge was asking me to hand-reconcile dead code.

So: triage every commit first, drop what upstream superseded, then merge.

## Commit triage

**Carried forward (12).**

| commit | why it is still needed |
|---|---|
| `5627db1` | WebSocket + event indexer. Absent from main. 52 files, +8,342. |
| `3bbc290` `faf6648` `1886390` | WS self-heal, its e2e regression, the slim-down. |
| `111dd72` | WS upgrade past the gzip and timeout wrappers. |
| `94e412e` | sharedState keep-latest pub/sub. Main's `redis_pubsub_manager.go` has plain `select` blocks, no keep-latest. |
| `cddeb01` | Four upstream-instance leak paths. Main still uses a bare `once`, not `clientCreations` keyed by `UniqueUpstreamKey`. |
| `bf9f50b` | `pooledGzipReadCloser` closes its source. Main's `WrapGzipReader` takes no source, so the HTTP/2 stream stays pinned. |
| `ccfbfda` | Breaker half-open permit leak. **Still live on main.** |
| `71cc292` | Per-goroutine batch network resolution. **Still live on main.** |
| `9372a10` | `tx_replay_attack` → nonce "already known". Main's `error_normalizer.go` has no replay handling. |
| `6fb6b51` | Preserve `latest`/`finalized` tags. Main covers 1 of the 5 state-read methods. |

**Dropped (6).**

- `51faede` — **dead.** Patched a function upstream deleted.
- `5a46e14` + `454c54d` — **deferred, needs your call.** See below.
- `e909aac` — fork identity + CI, already on `feat/valve-release-workflow`.
- `a7a53ec` — gofmt; re-run once the reconcile settles.
- `a88e68a` — a merge commit.

Replaying the 12 keepers onto the fork point applied **cleanly, all 12**. The
re-merge then dropped to **12 files / ~1,132 lines**, and `erpc/networks.go`
from 726 conflicted lines to 256. `erpc/http_server.go`'s 204-line conflict
disappeared entirely.

## How the conflicts were resolved

- **`erpc/networks.go` → upstream.** Every valve change here was the cross-pod
  monotonic block mechanism (`latestBlockShared`, `finalizedBlockShared`,
  `evmHighestBlockNumber`, `tierUpstreamsByGroup`), which the served-tip design
  supersedes. Checked explicitly: **none of it was WebSocket code.**
- **`clients/registry.go` → valve's structure, upstream's argument.** Valve's
  `clientCreations` map and `ws`/`wss` client, with upstream's per-upstream gRPC
  pool size threaded into the gRPC branch.
- **`common/config.go` → upstream's field set plus valve's `WebSocket` field.**
  Upstream had added `GrpcReflection`, which valve lacked.
- **`telemetry/metrics.go` → both.** The new metrics are disjoint.
- **`upstream/upstream.go` → upstream.** Both sides fixed the same state-poller
  leak; upstream's `sync.Once` is equivalent to valve's mutex.
- **`docs/pages/**` → upstream.** The fork's WS page stands alone at
  `docs/pages/operation/websocket.mdx`, so shared upstream pages stay
  conflict-free on future pulls. This drops valve's WS annotations from four
  upstream pages — say the word if you want them back.
- **Test files → both**, where the two sides added disjoint tests.

Then four rounds of fallout the auto-merge could not see: a duplicated
`SharedStateRegistry` accessor, an orphaned `evmStatePollerMu.Unlock()`, the
network-level shared-counter wiring in `networks_registry.go`, and valve tests
still on the pre-`Tags` `Group` API or older `NewUpstreamsRegistry` /
`NewNetwork` signatures.

## Verification

- `go build ./...` — clean.
- `go vet ./...` — clean. One `copylocks` warning at
  `erpc/query_executor_test.go:341` pre-dates the merge (present on the base
  branch).
- Tests green: `common`, `upstream`, `clients`, `failsafe`, `health`,
  `internal/policy`, `internal/policy/stdlib`, `architecture/evm`, `util`,
  `consensus`, `auth`, `thirdparty`, `erpc`.
- **Every WebSocket suite passes** on the merged tree: upgrade-behind-gzip,
  upgrade-without-gzip, ungraceful-death self-heal, basic RPC, batching, error
  handling, multi-connection, subscriptions.
- Only failures are container-backed (DynamoDB, Redis, Postgres). They fail
  identically on the base branch with no Docker running.

## Two live bugs still on main

Valve already fixed both. They are worth upstreaming to erpc.

1. **Breaker wedges open until restart.** `failsafe/breaker.go:181` returns
   early on `OutcomeIgnore` without releasing the half-open trial permit, so
   `halfOpenInflight` leaks. Once it saturates trial capacity, every
   `TryAcquirePermit` in HalfOpen is denied — real traffic and recovery probes
   both fail, permanently. Ignorable outcomes (timeouts, cancellations) are
   exactly what a transient blip produces during a trial.
2. **A batch entry can be forwarded to the wrong chain.** `erpc/http_server.go:456`
   captures `architecture` and `chainId` by reference in the per-entry
   goroutine, and lines 652–653 assign to them. The first entry to resolve
   publishes its network to every sibling goroutine, which then take the
   "already resolved" branch.

## One decision for you

**Does the fork still need the per-request fallback escape (`5a46e14`)?**

Valve's version adds `GetFallbackEscapeUpstreams`, which ignores the cordon and
escalates to `tier:fallback` upstreams **within a single request** once the
primary set is exhausted.

Upstream now covers adjacent ground in the policy engine's default program
(`internal/policy/default_policy.js`): `preferTag('!tier:fallback', {minHealthy:
1, fallback: 'tier:fallback'})` for the tier split, and `whenEmpty(() =>
upstreams)` as an outage net. But those run **per policy tick**, not per
request. A request that exhausts its primaries mid-tick still fails.

Related fallout worth knowing: `FailoverConfig` survived the merge and is live —
`erpc/subscription_manager.go:387` reads `Failover.Enabled()` — but its
`onDefaultsExhausted` suppression path now has nothing to suppress, because
`NewDefaultNetworkConfig` no longer carries a `SelectionPolicy`. I pinned that
in `common/config_test.go` with a reconcile note rather than quietly deleting
the test.

If you want the escape back, `5a46e14` and `454c54d` need porting onto the
served-tip world — their old integration point (`evmHighestBlockNumber`) is gone.

## State on disk

- Worktree: `<scratchpad>/reconcile`, branch `reconcile/ws-plus-main`.
- Helper branch `valve-ws-cleaned` holds the 12 replayed keepers on the fork point.
- Your `fix/empty-result-rotation` checkout was never touched.
- Nothing pushed.

## Next

Once you accept this branch, it is the one to build the polyglot chain patterns
on — it is the only branch with WebSocket, the current policy engine (the
config-driven seam the whole design rests on), and the `emptyResultAccept`
resolver together. See [erpc-polyglot-feasibility.md](erpc-polyglot-feasibility.md).
