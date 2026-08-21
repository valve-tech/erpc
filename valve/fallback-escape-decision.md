# eRPC fork — does it still need the per-request fallback escape?

Repo read: `/Users/michaelmclaughlin/Documents/valve-tech/github/erpc`, branch
`main` at `e2efc82`. The two dropped commits live only on
`fix/ws-upgrade-behind-gzip` and `origin/valve-ws`; `git branch --contains`
confirms neither is on `main`.

Every `file:line` below points at that repo unless the path starts with
`valve-node-app/`. I ran one test to prove the central claim; everything else I
read. The "Verified vs inferred" section draws the line.

## Recommendation

**Keep the Go code deleted. Solve it in the selection policy instead.** The
policy engine already returns the exact list the request loop sweeps, and the
request loop already tries every upstream in that list before it gives up. So a
policy that ranks fallback upstreams **last** instead of **dropping them**
gives you the per-request escape for free, with no Go code and no fork
divergence.

One caveat, stated up front: the gap is real, not theoretical, and on valve's
own rendered gateway config it is worse than the previous session assumed. Read
"What this costs today" before you decide how urgent it is.

## 1. What the dropped commits did

`5a46e14` ("fix(failover): close tracker-signal gap and add per-request
fallback escape", João Gomes, 2026-05-22) changed 7 files, +1,187 / -68. It did
three separate things. Only the third one is the "escape".

**Fix A — record block-availability skips.** When an upstream fails the
block-availability gate, `handleBlockSkip` bumped a Prometheus counter but
never told the health tracker. The patch added
`RecordUpstreamRequest` + `RecordUpstreamFailure` for retryable skips. Without
it, an upstream that is rejected on every request still shows `errorRate = 0`
to the policy.

**Fix B — record circuit-breaker short-circuits in the state poller.** When the
breaker is open, `PollLatestBlockNumber` returns before `tryForward` runs, so
the tracker stops seeing samples and `errorRate` freezes. The patch recorded a
failure for exactly the `ErrFailsafeCircuitBreakerOpen` case, and no other, to
avoid double counting.

**Fix C — the escape itself.** `Network.Forward` gained an outer
`escalationLoop`. When the inner loop exhausted the request's upstream list
with a retryable error, and `failover.onDefaultsExhausted` was on, the code
replaced the list with the fallback-tier upstreams, cleared their per-request
error state, and re-entered the loop once. Supporting parts: a new
`UpstreamsRegistry.GetFallbackEscapeUpstreams` that filters by the
`tier:fallback` tag and deliberately ignores the tracker cordon; an
`escalatedToFallbacks atomic.Bool` on `NormalizedRequest` to bound it to one
escalation per request; and a counter,
`erpc_network_fallback_escape_total{project,network,category}`.

**In operational terms.** Without the patch, a request whose upstream list is
exhausted returns `ErrUpstreamsExhausted` to the client, even when a healthy
fallback upstream sits idle. With the patch, that same request quietly tries
the fallback and the client gets an answer. The commit message reports an
end-to-end run against four mock servers: 20 of 20 outage requests served by
the fallback, 0 client-facing failures, 20 escape firings.

`454c54d` ("test: adapt failover/selection tests to rebased upstream API",
Jonny, 2026-06-01) is test-only, 4 files. It is a rebase repair, not new
behaviour. Its most interesting line is an admission: it weakened
`NoEscapeWhenFailoverDisabled` from "expect `ErrUpstreamsExhausted`" to "expect
the escape counter stays flat", *because upstream's own tiering may now serve
the request anyway*. Someone had already noticed the overlap.

**A correction.** The brief says the old integration point was
`evmHighestBlockNumber`. That is wrong. `git show 5a46e14 | grep -c
evmHighestBlockNumber` returns **0**, and so does the same grep on `454c54d`.
That function belonged to `51faede`, a different dropped commit, which the
reconcile correctly killed as dead. The escape hatch touches
`erpc/networks.go`, `common/request.go`, `upstream/registry.go`,
`telemetry/metrics.go` and `architecture/evm/evm_state_poller.go` — none of the
served-tip surface. **Porting it is a smaller job than the brief implies.**

## 2. Where `FailoverConfig` stands today

`FailoverConfig` survives, and the previous session was right that it is live.
It is also load-bearing in a place nobody mentioned.

| site | status |
|---|---|
| `common/config.go:742` — the type, with `OnDefaultsExhausted *bool` at `:748` | live, parsed from YAML |
| `common/config.go:752` — `Enabled()` | live |
| `common/config.go:736` — `NetworkDefaults.Failover` | live |
| `common/config.go:2271` — `NetworkConfig.Failover` | live |
| `common/defaults.go:2150-2157` — inheritance from network defaults | live |
| `erpc/subscription_manager.go:387` — `failoverOn` | **load-bearing** |
| `erpc/subscription_manager.go:404` — WebSocket tier split | **load-bearing** |
| `erpc/networks.go` — the whole HTTP forward path | **zero references** |

The WebSocket path is the surprise. `subIngressSelector.Select`
(`erpc/subscription_manager.go:376`) returns two lists. When
`onDefaultsExhausted` is on, a WebSocket upstream tagged `tier:fallback` goes
into the second list; when it is off, it mixes into the first
(`erpc/subscription_manager.go:404-408`). The indexer then subscribes on the
defaults, and **only touches the fallbacks when every default failed**
(`indexer/indexer.go:243-247`).

That is valve's per-request escape, alive and well — on the subscribe path.
The HTTP path lost it.

**This is the misleading-config finding.** One YAML key,
`failover.onDefaultsExhausted`, changes WebSocket filter-subscribe routing and
changes nothing at all for HTTP JSON-RPC. An operator who sets it reasonably
expects it to govern both. The doc comment at `common/config.go:739-741` still
promises "within-request escalation between upstream groups ... Failover
operates per-request only", which is now true for subscribes and false for
requests.

The pinned test at `common/config_test.go:1215-1273` carries an honest
reconcile note and asserts current behaviour, so nothing here is hidden. But
the note lives in a test file, not in the config comment an operator reads.

## 3. What upstream does instead, and the exact gap

Upstream moved the tier split into the policy engine's default program. The
relevant two lines are `internal/policy/default_policy.js:46` and `:49`:

```js
.whenEmpty(() => upstreams)
.preferTag('!tier:fallback', { minHealthy: 1, fallback: 'tier:fallback' })
```

`preferTag` (`internal/policy/stdlib/stdlib.js:942-953`) is a hard filter, not
a sort. If at least `minHealthy` upstreams match the primary pattern, it
**returns only those** and discards everything else. `minHealthy` is 1. So one
surviving primary is enough to remove every fallback from the result.

The engine evaluates that program on a timer, not per request. The slot owns a
ticker (`internal/policy/slot.go:126-158`); the default interval is **15
seconds** (`common/defaults.go:3044`), not the 1 minute the old commit message
assumed. The request path never evaluates anything — `getOrdered`
(`internal/policy/engine.go:372-391`) is a plain cache read of the last tick's
slice, and `materializeOrder` (`internal/policy/slot.go:748-768`) is a pure
ID-to-upstream lookup that preserves the JS order exactly.

The default program applies even when the operator supplies no
`selectionPolicy`. `Network.Bootstrap` (`erpc/networks.go:229-245`) creates an
empty `SelectionPolicyConfig` when the field is nil, and the engine upgrades
the placeholder to `default_policy.js` at register time
(`internal/policy/default_policy.go:25-40`).

**Here is the gap, in one sentence.** Between two ticks the request's upstream
list is frozen, and if the policy dropped the fallback tier at the last tick,
no request in that window can reach a fallback — however hard it fails.

I proved the first half by running an existing upstream test.
`TestStdlib_RichDefaultPolicy` (`internal/policy/stdlib/stdlib_test.go:538`)
builds two upstreams, `rpc1` (primary) and `rpc2` (`tier:fallback`), ticks the
real engine with the real default policy, and asserts:

```go
ordered := engine.GetOrdered("evm:1", "*", "*")
require.Len(t, ordered, 1)
require.Equal(t, "rpc1", ordered[0].Id())
```

It passes. **The fallback is not in the list at all** — not last, not
deprioritised. Absent.

The second half follows from the request loop.
`erpc/networks.go:2100-2261` iterates the list through
`NormalizedRequest.NextUpstream` (`common/request.go:1312-1372`), which is a
plain round robin over `upstreamList` with no health check of its own. What the
tick put in the list is exactly what the request can try. Nothing more.

**A per-tick decision and a per-request decision differ in this:** the per-tick
decision is made from *aggregate history* before your request arrives, and it
cannot see your request fail. The per-request decision is made from *this
request's own failures*, at the moment they happen. Valve's escape read the
second signal. Upstream reads only the first.

### How fast does the tick catch up?

Faster than the old commit message feared, and by a different route.

A dead node's block-head lag now climbs on its own, driven by its healthy
peers. When any upstream reports a new network high,
`Tracker.SetLatestBlockNumber` sets `needsGlobalUpdate`
(`health/tracker.go:1380-1384`) and recomputes `BlockHeadLag` for **every**
upstream on the network (`health/tracker.go:1461-1479`), including the wildcard
bucket the network-scope policy reads (`health/tracker.go:1489`). The default
program excludes on `blockNumberLagAbove(16)` or `blockSecondsLagAbove(30)`
(`internal/policy/default_policy.js:43`).

So a node that stops advancing gets excluded about 30 wall-clock seconds later,
plus up to 15 seconds of tick latency. That happens with no request traffic to
the dead node and **without** either tracker-signal fix from `5a46e14`. Fixes A
and B were written to unfreeze `errorRate`; the lag predicate reaches the same
verdict from a different sensor. Neither fix is in the tree today
(`erpc/networks.go:2736-2783` records no tracker sample;
`architecture/evm/evm_state_poller.go` never calls `RecordUpstreamFailure`),
and I do not think you need them.

That leaves a worst case of roughly **45 seconds** in which the policy still
believes a dead primary is healthy. Every request in that window that the
primary cannot serve fails, with the fallback untouched.

## 4. What this costs today

Valve's own rendered config makes the window worse, for two reasons that
compound.

**Reason one: one primary.** `RenderGatewayConfig`
(`valve-node-app/internal/catalog/gateway.go:417`) marks an upstream
`tier:fallback` when it is not local **and** the chain has a local node. The
normal shape is therefore one local node as the entire primary tier, with the
public endpoints behind it
(`valve-node-app/internal/catalog/gateway.go:240-246`). With one primary,
`preferTag(..., {minHealthy: 1})` gives every request a list of length **one**.
There is no second primary to sweep to.

**Reason two: no retry.** The gateway template
(`valve-node-app/internal/catalog/gateway.go:209-248`) renders no `failsafe`
block and no `networkDefaults`. eRPC adds no implicit network failsafe in that
case — `NetworkConfig.SetDefaults` only fills in retries when a defaults block
exists (`common/defaults.go:2223-2230`). So there is no network retry and no
hedge either.

Put together: **on a valve chain with a local node plus public fallbacks, a
single failed request to the local node is a client-visible error.** No retry,
no second primary, no fallback in the list. The public endpoints the operator
configured for exactly this purpose are not tried. This holds until the tick
notices, which for a stalled node is about 45 seconds and for an intermittently
erroring node needs more than 10 samples and an error rate above 0.7
(`internal/policy/default_policy.js:8`).

The gap is not theoretical. It fires on the first flake.

Two things soften it. A local node on the same machine is a short, reliable
hop, so flakes are rarer than against a remote provider. And a chain with no
local node has no `tier:fallback` at all
(`valve-node-app/internal/catalog/gateway.go:384-390`), so every upstream stays
in the list and sweeps normally.

## 5. One more thing worth your eye

The two policy lines look reversed. `whenEmpty` runs at
`internal/policy/default_policy.js:46`, **before** `preferTag` at `:49`. Reading
the implementations (`stdlib.js:1218` and `stdlib.js:942-953`), a tick in which
*every* upstream fails the health filters restores the raw set, and `preferTag`
then finds the primaries present and returns them — discarding the fallback
tier at the worst possible moment.

The comment on `preferTag` says "fall back to `tier:fallback` if no primary
survives", but after `whenEmpty` no primary can ever be missing. The existing
test `TestNetworkPolicy_FallbackTier_ActivatesWhenPrimaryAllBroken`
(`erpc/networks_selection_policy_test.go:652`) does not catch this: there the
fallback survives the filters, so the list is never empty and `whenEmpty` never
fires. Its own comment claims the safety net "puts them back", which the code
does not do in that scenario.

**I label this an inference.** I read it; I did not run it. It needs both tiers
excluded on the same tick, which is unlikely. If you adopt Option 3 below you
write your own chain and the question disappears. If you keep the upstream
default, it is worth an issue to `erpc/erpc`.

## 6. Your three options

### Option 1 — drop it permanently

**What you lose.** The HTTP per-request escape, permanently. You accept the
window in section 4. If you also raise valve's rendered config to include a
network retry, the loss shrinks a lot: a retry re-runs the sweep against the
same one-item list, which recovers transient flakes on the local node but never
reaches a public endpoint.

**What to delete.** Not a clean cut, because the WebSocket path uses the flag.
The tidy version keeps the behaviour and deletes the knob:

1. Make the tier split unconditional at `erpc/subscription_manager.go:404` —
   drop the `failoverOn &&`. This matches what the HTTP policy already does by
   default, and keeps the escape at `indexer/indexer.go:243-247`.
2. Delete `failoverOn` at `erpc/subscription_manager.go:387` and the stale
   comments at `:362` and `:373`.
3. Delete `FailoverConfig` and `Enabled()` (`common/config.go:739-757`), both
   struct fields (`common/config.go:736` and `:2271`), and the inheritance
   block (`common/defaults.go:2150-2157`).
4. Delete the two pinned tests (`common/config_test.go:1215-1288`) and
   `TestFailoverConfig_Enabled` (`erpc/networks_test.go:12413`).

Cost: about an hour. It removes a YAML key that today does one thing and reads
like it does another.

### Option 2 — restore it, ported

**What you write.** Less than the brief suggests, because the served-tip
rewrite does not touch this code.

- `UpstreamsRegistry.GetFallbackEscapeUpstreams` — about 25 lines, and the
  original already uses the current `cfg.HasTag(common.TagTierFallback)` API,
  so it drops in.
- `escalatedToFallbacks` on `NormalizedRequest` plus two accessors — about 20
  lines, unchanged.
- The counter in `telemetry/metrics.go` — about 15 lines, unchanged.
- The `escalationLoop` in `Network.Forward` — the real work. The old diff wraps
  a loop body that upstream has since moved to `erpc/networks.go:2100-2261` and
  changed: it now sets a sweep-iteration context flag at `:2106`, adds an
  `IsRetryableTowardNetwork` early break at `:2258`, and handles
  `emptyResultAccept` at `:2201-2220`. Re-deriving the wrap against that body is
  a careful half-day, not a copy.

Total: roughly 250 lines of Go plus tests. `454c54d` gives you the test file,
already adapted once.

**Risk.** Three things. The escape mutates `effectiveReq`'s upstream list
mid-execution, which now interacts with hedging and with the sweep-iteration
tagging; the original guarded consensus explicitly and would need re-checking
against the current executor. It bypasses the tracker cordon by design, so a
cordoned fallback can be handed real traffic. And it is fork-local code inside
the single hottest function in the tree, so it collides with every future
upstream pull of `networks.go` — the exact cost the last reconcile paid.

### Option 3 — express it in the policy, not in Go

**This works, and it is the fork's stated direction.** Four verified facts make
it work:

1. The JS return value **is** the request's upstream list, in order
   (`internal/policy/slot.go:290` and `:748-768`).
2. The request loop sweeps that whole list on failure
   (`erpc/networks.go:2100-2261`, `common/request.go:1312-1372`).
3. The stdlib has the parts: `byTag` (`stdlib.js:339`) and `union`
   (`stdlib.js:602-607`), which appends the second list's members after the
   first and de-duplicates by id.
4. A policy may be a block-bodied arrow function — `CompileProgram` wraps the
   source in parentheses and compiles it as an expression
   (`common/compiler.go:69`). The YAML key is `selectionPolicy.evalFunc`
   (`common/config.go:2778`).

So replace the hard `preferTag` split with a soft one:

```js
(upstreams, ctx) => {
  const healthy = upstreams
    .removeCordoned()
    .excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
    .excludeIf(all(samplesAbove(10), throttleRateAbove(0.4)))
    .excludeIf(any(blockNumberLagAbove(16), blockSecondsLagAbove(30)))
    .whenEmpty(() => upstreams);

  // Primary tier first, ranked. Fallback tier appended, never dropped.
  const primary  = healthy.byTag('!tier:fallback').sortByScore(PREFER_FASTEST);
  const fallback = healthy.byTag('tier:fallback').sortByScore(PREFER_FASTEST);

  return primary
    .union(fallback)
    .stickyPrimary({ hysteresis: 0.30, minSwitchInterval: '30s' })
    .probeExcluded({ sampleRate: 0.1, minSamples: 10, minSamplesWindow: '60s' });
}
```

A healthy primary still serves every request, because the loop returns on the
first success and never reaches position two. A failing primary now escalates
**within the same request**, because position two exists. That is precisely
what `5a46e14` bought, in config, with no Go code and no merge surface.

**What it costs.** Two things, both real.

*Hedging.* If you ever add a `hedge` policy, the hedged copy takes the next
upstream in the list — which is now a paid provider. eRPC's own bundled default
sets `hedge.maxCount: 2` at a p70 delay (`common/defaults.go:137-140`). Valve's
rendered config sets no failsafe at all
(`valve-node-app/internal/catalog/gateway.go:209-248`), so today this costs
nothing. Keep it in mind if you add hedging later.

*Score multipliers.* Valve renders `routing.scoreMultipliers.overall: 0.2` on
fallback upstreams (`valve-node-app/internal/catalog/gateway.go:243-245`). The
engine still honours it (`internal/policy/eval.go:405`,
`internal/policy/stdlib/stdlib.js:717`). Under the chain above the multiplier
only orders fallbacks among themselves, since the tiers are sorted separately.
That is harmless, but the config line no longer means what it looks like. Drop
it when you adopt the policy, or keep it and document why.

**Where it lands.** The policy is per network in YAML, so valve's gateway
template grows one `selectionPolicy.evalFunc` block per network — a change in
`valve-node-app/internal/catalog/gateway.go`, not in the fork. The fork ships
nothing. That is the whole point.

## Verified vs inferred

**Verified by running:** `TestStdlib_RichDefaultPolicy` passes, so the default
policy really does return a one-item list when one primary and one fallback are
both healthy (`go test ./internal/policy/stdlib/ -run TestStdlib_RichDefaultPolicy`).

**Verified by reading, with citations above:** every `file:line` claim — the
`FailoverConfig` reference map, the WebSocket tier split and its indexer
consumer, the 15-second tick default, the cache-read `getOrdered`, the
round-robin `NextUpstream`, the lag-recompute path in the tracker, valve's
rendered gateway shape, the absence of an implicit network failsafe, and the
full content of both dropped commits.

**Inferred, not run:**

- The `whenEmpty` ordering problem in section 5. The reasoning is three lines of
  JS, but no test exercises it.
- The end-to-end behaviour of the Option 3 policy. Each ingredient is verified
  separately; the assembled chain is not. I could not test it without writing a
  file into the erpc repo, which this task forbids.
- The claim that a valve chain flake becomes a client error. It follows from
  four verified facts (one primary, no retry, hard `preferTag`, frozen list),
  but I did not boot a gateway and pull the node's plug.

**Opinion, clearly labelled:** that Option 3 is the right answer. The evidence
supports "Option 3 is feasible and cheap". Preferring it over Option 2 is a
judgement about where fork divergence hurts most, and that is yours.

## What I could not determine

**Does the Option 3 chain behave as written, end to end?** Settle it with one
test in the fork, modelled on
`erpc/networks_selection_policy_test.go:652`: three upstreams (one primary, two
`tier:fallback`), the custom policy, all three healthy, one forced tick, then
fail the primary's mock and assert a fallback serves the request on the same
`network.Forward` call. That single test settles the whole recommendation.

**Does the `whenEmpty`/`preferTag` order actually discard fallbacks?** Settle it
by extending `TestNetworkPolicy_SafetyNet_WhenAllBroken`
(`erpc/networks_selection_policy_test.go:699`) with a third upstream tagged
`tier:fallback` that is also degraded, then asserting which upstreams appear in
`GetOrdered`.

**How often does this fire in valve production?** I have no telemetry. The
counters that would answer it are `erpc_network_no_upstreams_available_total`
and `erpc_selection_exclusion_total{reason=block_head_lag_seconds_above}`. If
the first is flat over a month of real traffic, the gap is rare and Option 1
becomes defensible on evidence rather than on hope.

**Whether the WebSocket subscribe path is exercised at all in valve's
deployment.** `subIngressSelector` only matters when the indexer runs and a
client subscribes. If nothing subscribes, `FailoverConfig` is fully vestigial
and Option 1's deletion list gets simpler.
