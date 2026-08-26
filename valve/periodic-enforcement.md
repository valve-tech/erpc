# Serving billing from periodically refreshed state

eRPC serves the request. The relay stops being a hop and becomes a reporting
system. Enforcement moves from a synchronous credit gate to state that eRPC
refreshes on a timer and reads from its own cache.

This document works out what that costs, what it can and cannot enforce, and
what is left to build.

Written 2026-08-24 against `main` at `6d9924ed`.

**Two things about how this was written.** It began as a plan for hosting a
blocking billing gate inside eRPC, and the operator changed the architecture
mid-draft. The rebase-cost analysis, the "what eRPC already has" survey and
the migration plan all survive; the hosting ranking mostly does not, for the
reason section 7 gives. Separately, another agent was building `valverelay/`
and `cmd/valve-relay/` while this was written. Neither existed in the tree
when these measurements were taken, so everything said about them is a plan.

**The filename no longer matches the content.** This is not about host
wiring any more.

**Line citations are a snapshot.** The tree was being edited by another agent
while this was written — `auth/strategy_database.go` changed under section 1
mid-draft, and the document says so where it matters. Re-derive a citation
before relying on it.

---

## The re-frame in one paragraph

The old question was "where does the blocking authorize call live". The new
question is "what must eRPC decide in-process, and what can be decided out of
band and delivered as refreshed state". The answer is better than expected,
because eRPC already has the machinery: an API-key strategy that reads a
projection through a cache, a `User` that carries a rate-limit budget, and
four rate-limit enforcement points that honour it. Enforcement needs no new
hot-path code. It needs one number chosen by a human — the cache TTL — and
one thing eRPC genuinely cannot do, which section 1 names.

---

## 0. What was measured, and when

Every number was taken on 2026-08-24 in this working tree unless it says
otherwise. Estimates say "estimate". Numbers from the monorepo brief say so.

| Measurement | Value | Source |
|---|---|---|
| Auth strategies that populate `User.RateLimitBudget` | 5 of 5 | `auth/strategy_{database,jwt,secret,siwe,network}.go` |
| Rate-limit enforcement points | 4 (`auth`, `project`, `network`, `upstream`) | `auth/authorizer.go:137`, `erpc/projects.go:390`, `erpc/networks.go:3382`, `upstream/upstream.go:620` |
| Database strategy positive-cache TTL, default | **1 hour** | `common/defaults.go:3270-3272` |
| Database strategy negative-cache TTL | **5 s**, hardcoded | `auth/strategy_database.go:76` |
| Database strategy cache max entries, default | 10,000 | `common/defaults.go:3276-3279` |
| Database strategy lookup timeout, default | 1 s | `common/defaults.go:3297` |
| Database strategy retry, default | 3 attempts, 100 ms base backoff | `common/defaults.go:3305-3310` |
| Metric families carrying a `user` label | 38 | `grep -c '"user"' telemetry/metrics.go` |
| Admin methods for API-key CRUD | 4 | `erpc/admin.go:45-51` |
| Upstream commits since 2026-02-24 | 191 | `git rev-list --count --since=2026-02-24 upstream/main` |
| …touching `common/config.go` | 46 | same, `-- <path>` |
| …touching `erpc/http_server.go` | 22 | same |
| …touching `cmd/erpc/main.go` | 2 | same |
| Fork diff in `erpc/http_server.go` | +177/−64 in 23 hunks | `git diff --numstat upstream/main HEAD` |
| `git merge-tree HEAD origin/reconcile/ws-plus-main` | 20 conflicts | count `^CONFLICT` |
| `git merge-tree HEAD origin/archive/harvest-onto-main` | 15 conflicts | same |
| `valvebilling/` in upstream or either reconciliation branch | absent | `git ls-tree -r --name-only` |
| Importers of `valvebilling` outside itself | 0 | `grep -rn valvebilling --include=*.go` |
| Credits per USD (the ledger peg) | 10⁹ | `packages/api/src/credits/pricing.ts:17-18`, `packages/relay/src/meter.ts:75-76` |
| Credits per USD (the web calculator) | 10⁶ | `packages/web/src/lib/pricing.ts:16` — **confirmed WRONG 2026-08-24; the ledger's 10⁹ governs, see section 2** |
| CPS bucket window | 2 s, fixed | `packages/utils/src/credits-lua.ts:148-164`, `:201-204` |
| Worst-case request lifetime **today** | 6 s = (1+**1**) × 3,000 ms | ring has ONE backend; `proxy-forward.ts:80` caps attempts at `min(maxUpstreamAttempts, backends.length)` |
| Worst-case request lifetime **with 3 eRPC backends** | 12 s = (1+3) × 3,000 ms | what spreading eRPC across the fleet creates |
| **Reachable overdraft bound today** | **1,992 credits = $0.0000019920/account** | 4 windows × 498; derived from the measured per-window 498 |
| **Reachable overdraft bound with 3 backends** | **3,486 credits = $0.0000034860/account** | MEASURED; `valvebilling/overdraft_test.go`; section 2 |
| Deployment artifacts setting the five tier knobs | 0 | searched `config/ deploy/ scripts/ services/ docker-compose.yml .env*` |

---

## 1. What must stay synchronous, and what can go periodic

| Concern | Can eRPC enforce it from cached state? | What staleness costs |
|---|---|---|
| **Identity** — who is this key | **Yes, already does** | A new key does not work until first read. A miss is a live lookup, so this is bounded by the negative cache: 5 s. |
| **Revocation** — this key must stop | **Partly. This is the real gap.** | Up to the positive TTL, per process. See below. |
| **Rate limiting** — requests and credits per second | **Yes, already does** | The *budget name* is stale, not the counting. A tier change takes up to the TTL to apply; the limit itself is enforced exactly. |
| **Credit sufficiency** — does the account still have money | **No. Not from cached state.** | Unbounded without a rate limit; `TTL × RPS × cost` with one. Today's reachable bound is 1,992 credits; 3,486 once eRPC is spread across three backends. Either way any practical TTL is orders of magnitude larger. Section 2. |
| **Per-method policy** — per-second caps on named methods | **Yes, natively and better** | eRPC's budgets are per-method rules already. No staleness beyond the budget name. |

### Identity — already solved, and the shape is right

`auth.DatabaseStrategy` is exactly the pattern the re-frame describes. It
reads an API-key record through a `data.Connector`, in front of a ristretto
positive cache and a negative cache, deduplicated by singleflight, with a
fail-open circuit that latches on connector failure and probes for recovery
once per second (`auth/strategy_database.go:29-53`, `:123-281`). The struct
comment records the 2026-05-13 incident that hardened it: every failed
request went through the full singleflight-plus-Get-plus-Error-log path even
after the database was known unreachable, and the log fan-out blocked on the
stdout write lock, growing goroutines from ~4k to ~96k.

The record shape is `{userId, enabled, rateLimitBudget}`
(`auth/strategy_database.go:216-220`, the `userData` struct). That is a
projection. It is
`{accountId, revoked?, tier}` under different names.

`common.User` carries `Id` and `RateLimitBudget` (`common/user.go:8-16`), and
all five strategies populate the budget. The JWT strategy derives it from a
token claim (`auth/strategy_jwt.go:124-131`), so there is precedent for a
strategy computing the budget from the credential rather than reading it from
static config. The database strategy reads it from the record
(`auth/strategy_database.go:251-253`). The authorizer then prefers the user's
budget over the strategy's (`auth/authorizer.go:119-124`). Per-user
enforcement works end to end today.

The coordinator's reading of all of this is correct.

### Revocation — the gap, and it is smaller than it looks

The default positive TTL is **one hour** (`common/defaults.go:3270-3272`). A
key revoked in Postgres keeps working for up to an hour on every eRPC
instance that has already seen it.

eRPC does have an invalidation path, and it is better than expected. There
are four admin JSON-RPC methods — `erpc_addApiKey`, `erpc_listApiKeys`,
`erpc_updateApiKey`, `erpc_deleteApiKey` (`erpc/admin.go:45-51`) — and each
mutating one writes through the connector and then drops the local cache
entry (`erpc/admin.go:208`, `:410`, `:489`). Setting `enabled: false` refuses
the key and caches the refusal for 5 s
(`auth/strategy_database.go:245-248`).

**The limit is that `InvalidateCache` is per-process.** It calls
`s.cache.Del(apiKey)` on a ristretto cache (`auth/strategy_database.go:446`,
`InvalidateCache`).
There is no pub/sub, no shared-state hook, no fan-out. An admin call reaches
one instance; every other instance in the fleet keeps serving the revoked key
until its own TTL expires. The relay solves this today with Redis pub/sub on
`valve:key:invalidate` (`packages/keystore/src/index.ts:210-218`).

So revocation across a fleet is: set the TTL short enough to be the bound you
accept, **or** fan the admin call out to every instance. The second is a
loop over instances in the reporting system, not new eRPC code. I have not
verified that the admin transport is reachable per-instance rather than
through a load balancer; that is section 9's.

### The blocker nobody has named yet: the raw key becomes a store key

This is the most important finding of the re-frame and it is a hard one.

`DatabaseStrategy` looks the record up by the **raw API key**:
`getWithRetries(lookupCtx, data.ConnectorMainIndex, apiKey, rangeKey)`
(`auth/strategy_database.go:182`), where `rangeKey` is the constant `"*"`. The `data.Connector` contract addresses a
record by `(partitionKey, rangeKey)` and compares both as literals
(`data/connector.go:59-70`).

The Redis connector composes the Redis key as
`fmt.Sprintf("%s:%s", partitionKey, rangeKey)` — `data/redis.go:433`, and
identically at `:410` and `:333`. So putting the projection in Redis means
**every customer's raw API key becomes a Redis key name**, visible through
`SCAN`, `MONITOR` and any RDB backup. That is precisely the exposure of
2026-08-02, and the entire `valvebilling/hashkey.go` design exists to prevent
it (`valvebilling/hashkey.go:19-36`).

Three ways out, none free:

1. **Use the PostgreSQL connector instead.** The key lands in a
   `partition_key TEXT` column (`data/postgresql.go:319-324`). A column is
   not a keyspace, so `SCAN` and `MONITOR` do not reach it — but the raw key
   is then stored in plaintext at rest, which the monorepo deliberately does
   not do anywhere.
2. **Add a fork-owned connector driver that hashes the partition key.**
   `data.NewConnector` is a hard-coded switch (`data/connector.go:119-136`),
   not a registry, so this costs an edit there plus a driver constant and a
   config struct in `common/config.go` — the file upstream touched 46 of 191
   commits. It is the clean answer and the expensive one.
3. **Keep a thin authenticating layer in front** that swaps the raw key for
   an opaque token before eRPC sees it. That is the hop the re-frame is
   trying to delete.

I checked whether the gRPC connector could serve this, since it is read-only
and answers from request metadata. It cannot: it is a Blockchain Data
Standards read-through cache with a method allowlist for `eth_*` and Solana
calls (`data/grpc.go:25-33`, `:58-71`). It cannot serve an API-key record.

There *was* a second, smaller version of the same problem, and it was fixed
while this was being written. `DatabaseStrategy` logged the raw API key at
ten sites, three of them at `Warn` or `Error` and so live at default log
levels. Another agent replaced all ten with `util.RedactSecret`, which emits
`redacted=` plus five hex characters of a SHA-256 digest — enough to
correlate two lines in one incident, not enough to identify the key
(`util/redact_secret.go:31-38`, `auth/strategy_database.go:138`, `:229`,
`:237`, `:246`). **This closes the log half of the exposure and does nothing
about the store half**, which is the blocker above: a redacted log line does
not help when the credential is the Redis key name.

**I am not certain there is no fourth option**, and this is the single point
where I would most want a second opinion before the design is committed.

### Rate limiting — eRPC is better at this than the relay

Four enforcement points, distinguished by an `origin` label: `auth`
(`auth/authorizer.go:137`), `project` (`erpc/projects.go:390`), `network`
(`erpc/networks.go:3382`) and `upstream` (`upstream/upstream.go:620`). A
user's budget overrides the strategy's. Budgets carry per-method rules, so
the relay's per-method per-second buckets — today a hardcoded map in
`packages/relay/src/method-rps.ts:19-24` — become configuration.

What goes stale is which budget a user is assigned to, not the counting. If
an account is downgraded, it keeps the old budget for up to the TTL and is
rate-limited exactly against it the whole time.

### Credit sufficiency — the one thing that cannot go periodic

A balance is a moving quantity that the request itself moves. No cache can
represent it. This is the whole of section 2.

---

## 2. The central trade-off, quantified

### What today's system actually guarantees

The current system already tolerates bounded overdraft, and the mechanism is
documented in the relay's own source. `packages/relay/src/meter.ts:227-267`
says sufficiency is **read, not reserved**, so N in-flight requests each see
the same balance and collectively debit past it. `spend` does not move until
`capture()` (`meter.ts:388-391`).

The bound is the per-**account** credits-per-second bucket at
`valve:credits:<accountId>:cps`. Its key carries no time component and the
script arms `EXPIRE 2 NX` on both the reject and the commit path
(`packages/utils/src/credits-lua.ts:148-164`, `:201-204`), so it is a fixed
**2-second** window, not a 1-second one. The comment's formula:

```
overdraft  ≈  cpsLimit × ceil(maxInFlightSeconds / 2)
```

A request lifetime is at worst `(1 pinned attempt + min(maxUpstreamAttempts,
backends)) × upstreamTimeoutMs`.

**`backends` is the size of the hash ring, and the ring has ONE member.**
`proxy-forward.ts:80` reads `Math.min(config.args.maxUpstreamAttempts,
backends.length)`, and `backends` is the eRPC backend list — not sequential
tries against one server. `maxUpstreamAttempts` is 3, but `min(3, 1)` is 1:

```
today          (1 + min(3, 1)) × 3,000 ms  =   6 s
3 backends     (1 + min(3, 3)) × 3,000 ms  =  12 s
```

An earlier revision of this document wrote (1 + 3) × 3,000 = 12 s for today,
citing this exact file, and did not notice the `Math.min`. So did the comment
at `meter.ts:261-267` it was checking. The 12 s figure is not wrong — it is
**premature**. It describes the fleet after eRPC is spread, which is a change
already on the roadmap, and it was measured against a wall no request can
currently reach.

This matters beyond arithmetic: **spreading eRPC across the fleet is a
capacity change that silently doubles maximum overdraft exposure**, because
the overdraft window is one request lifetime and that lifetime is bounded by
ring size. Neither change predicts the other in isolation.

Twelve seconds spans six 2-second windows, each granting a fresh `cpsLimit`.
Hence the factor of 6. Six seconds spans three.

### The stated 30,000 is not the reachable bound. 1,992 is today; 3,486 after.

The comment derives ≈ 6 × `FULL_CREDITS_PER_SEC` = 6 × 5,000 = 30,000
credits. That arithmetic is right and the tier is wrong.

Tier is chosen from the effective balance before the gates run
(`packages/utils/src/credits-lua.ts:133-146`): an account with `effective <
thresh` is `SLOW`, and `thresh` is `SLOW_MODE_THRESHOLD_USD × 10^9` = 5×10⁹
credits. An account can only overdraft when its effective balance is within a
few credits of zero — about 10⁹ times below the threshold. **It is therefore
always in `SLOW` at the moment overdraft is possible.** The reachable bound
is:

```
6 × SLOW_CREDITS_PER_SEC  =  6 × 500  =  3,000 credits
```

**Both halves of that arithmetic were wrong, and they were wrong in opposite
directions.** `valvebilling/overdraft_test.go` measures it against a real
redis-server 7.2.4 under real concurrency, and the answer is **3,486**.

*The window yields 498, not 500.* The bucket gate is `cpsCount + cost >
cpsLimit`, so the last approval has to fit WHOLE. At the default cost of 6,
one window yields `floor(500/6) × 6 = 498` and strands 2. Reaching exactly
3,000 needs a cost that divides 500 — 1, 2, 4, 5, 10, 20, 25 or 50. Six
windows at cost 6 is 2,988.

*The span touches seven windows, not six.* `EXPIRE ... 2 NX` is armed only by
the charge that CREATES the key, so the window is a tumbling one anchored at
the first charge — not sliding, and not aligned to a clock. Measured: PTTL
falls 1.998 → 1.977 → 1.955 → 1.932 across four charges and is never
refreshed. A 12-second span that starts mid-window gets that window's tail
PLUS six more.

```
3 backends, worst phase:  7 × floor(500/6) × 6  =  7 × 498  =  3,486 credits
today,       worst phase:  4 × floor(500/6) × 6  =  4 × 498  =  1,992 credits
```

The same tumbling-anchor argument gives today's figure: a 6-second span that
starts mid-window gets that window's tail plus three more, so four windows,
not seven. **3,486 is the post-spreading bound and this document previously
labelled it "today".** The error is in the safe direction — it overstates
today's exposure, so nothing was under-protected — but it is the number that
changes when the ring grows, and mislabelling it hides exactly that.

The structural argument above stands — the overdrawing account is always
`SLOW`, and 30,000 is stated for the wrong tier. The number attached to it
was 16% low.

**A client that paces to the window edge collects two full allowances in 162
ms** (498 + 498 = 996), measured. Neither document recorded that the tumbling
window bursts at its boundary, and it is what makes the seventh window
reachable.

**The bound does not care how fast the client sends.** Measured over one
window at SlowCPS 500 and cost 6:

| goroutines | requests offered | approved | credits |
|---|---|---|---|
| 1 | 2,564 | 83 | 498 |
| 8 | 2,592 | 83 | 498 |
| 64 | 2,816 | 83 | 498 |
| 256 | 3,584 | 83 | 498 |
| 512 | 4,608 | 83 | 498 |

Offering 1.8× the load changes the overdraft by zero credits; a faster client
only collects its rejections sooner. That is exactly the property this whole
document depends on — a CPS bucket caps credits per unit TIME, where a cached
projection's bound scales with request rate. It holds.

**The 3,000-ish bound is a property of the configured threshold, not of the
script.** The tier test is `effective < thresh`, and `LoadTierLimitsFromEnv`
requires the threshold to be positive but sets no floor. So
`SLOW_MODE_THRESHOLD_USD=0.000001` is ACCEPTED and puts a 600-credit account
on the `FULL` branch, whose window is 4,998. Measured overdraft in a single
window: 4,398. Over a 12-second span: 7 × 4,998 = **34,986** — the
"unreachable" 30,000, reached and passed, through a value the config
currently allows. If the bound is meant to be an invariant rather than a
coincidence, `requiredCredits` needs a floor at one `FULL` window. That is a
policy change and it is not made yet.

**A cost above the tier's limit can never be authorized at all.** There is no
first-request exemption in the gate, so `cost > cpsLimit` is a permanent
`cps_throttle` rather than a throttle that clears. At `SLOW_CREDITS_PER_SEC=500`
a low-balance account cannot make any single request over 500 credits — a
JSON-RPC batch of eleven 50-credit methods, for one. The customer gets a
rate-limit code for what is really a hard cap.

A `FULL`-tier account holds at least 5×10⁹ credits and would have to burn
five billion inside one 12-second window to reach zero; the CPS bucket caps
that window at 30,000. **At the defaults the `FULL` branch cannot overdraft
at all.** The 30,000 figure is an unreachable upper bound stated for the
wrong tier.

Two secondary caveats. Six windows assumes a pinned upstream *and* at least
three backends; unpinned it is 3 × 3 s = 9 s → 5 windows → 2,500 credits.
And `FULL_CREDITS_PER_SEC=0` parses as a real zero, skips the bucket at the
`cpsLimit > 0` guard, and makes overdraft **unbounded** — the RPS caps bound
request count, not spend (`packages/relay/src/meter.ts:255-266`,
`meter.test.ts:620-639`). Nothing in the monorepo sets it to 0. It is a
latent trap, not a live bug.

### The five tuning knobs are code fallbacks that no deployment sets

| Variable | Code default | Source |
|---|---|---|
| `FULL_CREDITS_PER_SEC` | 5,000 | `packages/relay/src/meter.ts:83-86` |
| `SLOW_CREDITS_PER_SEC` | 500 | `meter.ts:88-91` |
| `SLOW_MODE_THRESHOLD_USD` | $5 → 5×10⁹ credits | `meter.ts:78-81` |
| `FULL_RATE_RPS` | 1,000 | `meter.ts:97-100` |
| `SLOW_RATE_RPS` | 100 | `meter.ts:102-105` |

**Measured: none of the five appears in any deployment artifact in the
monorepo** — not `config/`, `deploy/`, `docker-compose.yml`, `scripts/`,
`services/`, nor the variable-name lists of `.env` or `.env.example`.
Production runs from `EnvironmentFile=/opt/valve/.env`
(`config/systemd/valve-relay.service:10`), so the values above are what
production uses unless that file sets them, which nothing in the repository
records. Section 9 asks somebody to confirm against the box.

### Units: everything is plain integer credits, and 1 credit = $10⁻⁹

There is no 10¹⁸ scaling factor anywhere. `amount_wei` is `numeric(78, 0)`
with **scale 0** (`packages/db-schema/src/shared/method-pricing.ts:31`),
seeded straight from `METHOD_CU` with no multiplier
(`packages/api/src/db/seeds/007_method_pricing.ts:32`), read as
`BigInt(row.amount_wei)` (`packages/relay/src/method-pricing-cache.ts:99`)
and compared directly against `ceiling + pending − spend`
(`credits-lua.ts:136-137`). The column name is a misnomer, exactly as
`valve/billing-module.md:106-119` records.

The peg is `1 USD = 10⁹ credits`, stated identically in about ten places
including `packages/api/src/credits/pricing.ts:17-18` and
`packages/relay/src/meter.ts:75-76`. It is what converts the USD-denominated
`SLOW_MODE_THRESHOLD_USD` into the credit ledger.

So today's reachable overdraft bound, 3,486 credits measured, is **$0.0000034860
per account**.

### The bound a cached projection gives

The shape of the two bounds differs. The CPS bucket caps **credits per unit
time**, so it does not care how fast a client sends. A cached projection's
bound is `TTL × request-rate × cost-per-request`, which does.

**eRPC's rate limiter is what restores an absolute bound.** If the projection
carries a `rateLimitBudget` and eRPC enforces it — which it does, at four
points — the rate is capped and:

```
overdraft_credits  =  TTL  ×  RPS_budget  ×  cost_per_request
overdraft_usd      =  overdraft_credits  /  CREDITS_PER_USD
```

This is why the two halves are not separable. The cached projection is safe
only **because** the same projection carries the rate limit. Ship identity
without the budget and the overdraft is unbounded.

### The decision table

Cost 6 is `DEFAULT_CU`, read from the source constant at
`packages/utils/src/method-cu.ts:131`. Cost 50 is the largest row in the live
pricing table (`packages/db-schema/drizzle/0021_beacon_method_pricing.sql`).
RPS 1,000 is `FULL_RATE_RPS`'s default; 100 is `SLOW_RATE_RPS`'s.

| TTL | RPS | cost | credits | at 10⁹ peg | at 10⁶ peg |
|---|---|---|---|---|---|
| 5 s | 100 | 6 | 3,000 | $0.000003 | $0.003 |
| 60 s | 100 | 6 | 36,000 | $0.000036 | $0.036 |
| 60 s | 1,000 | 6 | 360,000 | $0.00036 | $0.36 |
| 300 s | 1,000 | 6 | 1,800,000 | $0.0018 | $1.80 |
| **1 h (eRPC default)** | 1,000 | 6 | 21,600,000 | **$0.0216** | **$21.60** |
| 1 h | 1,000 | 50 | 180,000,000 | $0.18 | $180 |

Read the first row as: *a 5-second TTL at a 100 RPS budget reproduces today's
bound exactly.*

### Why there are two dollar columns — SETTLED 2026-08-24, use 10⁹

The ledger peg is 10⁹ credits per USD. But the landing page's cost calculator
uses a different one: `DEFAULT_USD_PER_CREDIT = 0.000_001`, i.e. **10⁶
credits per USD**, with a comment saying "1 credit == 1 CU"
(`packages/web/src/lib/pricing.ts:16`, `:76-85`). The two disagree by 1,000×,
and there is no CU-to-credit multiplier anywhere in the relay path to
reconcile them — verified at `proxy-handle-single.ts:171-173` and
`method-pricing-cache.ts:99`, `:111`.

Under the ledger peg a standard 6-credit read costs $0.000000006, so a
million requests cost **$0.006**. Under the calculator's peg the same million
cost **$6**.

One independent calibration supports the ledger peg for **byte**-priced rows:
`packages/db-schema/drizzle/0014_firehose_pricing_calibration.sql:41-42` sets
0.5 credits per byte against a stated "$0.50/GB target", and
0.5 × 10⁹ ÷ 10⁹ = $0.50/GB exactly. Nothing equivalent calibrates the
**request**-priced rows.

**RESOLVED after this section was written. The ledger peg of 10⁹ governs.**
It appears in five independent places across the money path — the relay meter,
api funding, the facilitator's credits-value and its wire-from-env, and the
trial grant — plus the deposit watcher's "1B credits = 1 stable unit". The
calculator's 10⁶ is a single constant in one file contradicting all five.

So the **web calculator over-quotes by 1,000×**, and every dollar figure in
this document should be read from the 10⁹ column. The 10⁶ column is kept
because it shows what the landing page currently implies, which is now a
pricing question with the operator rather than an engineering one.

The original text follows, because the reasoning that surfaced it is worth
keeping: the whole overdraft exposure swung by three orders of magnitude on a
discrepancy this document could not resolve from source. At the ledger peg, eRPC's one-hour default costs
two cents per account and the question is close to moot. At the calculator's
peg it costs $21.60 per account per hour, times every account that runs to
zero, times every eRPC instance. Section 9 asks for a ruling.

### So: degree, or kind?

Both, honestly, and it depends which comparison you make.

**A change of degree, if you compare the mechanisms.** Today's system already
serves requests it would have refused had it known the balance. A cached
projection does more of the same thing for longer. Neither reserves.

**A change of kind, if you compare the bounds.** Today's 3,486 credits is not
a policy choice — it falls out of the request lifetime and the 2-second
window, and it is $0.000003. Reproducing it needs a TTL of about 5 seconds at
a 100 RPS budget, and about 0.6 seconds at 1,000 RPS with the worst pricing
row. A sub-second TTL is not a cache; it is a synchronous lookup with extra
steps. **Any TTL long enough to be worth calling periodic accepts an
overdraft orders of magnitude larger than today's.**

That is not an argument against the design. It is an argument that the design
must be justified in **dollars per account per window**, not by analogy to
the existing bound. The existing bound is an accident of the retry loop.

Three things the arithmetic ignores, stated so nobody mistakes it for a
guarantee:

- **It assumes the account sustains its full budget for the whole TTL.** It
  is a worst case, not an expectation.
- **It is per eRPC instance.** N instances each hold their own ristretto
  cache. The fleet-wide bound is N times the number in the table. Nothing in
  eRPC shares those caches.
- **It is per account.** Total exposure is the bound times the number of
  accounts that reach zero inside one TTL window.

### What is genuinely lost

A blocking authorize refuses the request that would take an account below
zero. A cached projection cannot, because the process that would refuse it
does not know the balance moved. No TTL recovers that. The question is only
whether the resulting exposure is smaller than the cost of a synchronous hop
— and section 9's first two questions are the only place that can be decided.

### Three corrections this produced for sibling documents

I was told to change no file but this one, so these are recorded rather than
applied.

**1. "The overdraft window" is not a thing.** `valvebilling/doc.go:23-24` and
`valve/billing-module.md:37-38` both list "the overdraft window" among what
the Lua script owns. No such construct exists — there is no key, no TTL and
no grace flag implementing one, in `AUTHORIZE_LUA` or in `meter.ts`. What the
script owns is the **CPS bucket**. The window is emergent: its length is set
by `proxy-forward.ts`'s retry loop and by where `capture()` is called. The
phrase should be "the credits-per-second bucket".

**2. The 10¹⁷ ceiling figure is probably wrong.**
`valve/billing-module.md:88-93` states that "the largest live `ceiling` is
about 1e17, roughly 11x 2^53, where the granularity is 16 credits". Its only
source in the monorepo is prose at `packages/relay/src/meter.ts:31-33`,
describing an account that held "~10^17 credits of ceiling". That is the
**system account**, and `packages/api/src/db/seeds/006_system_keys.ts:163-166`
describes the same account as holding ~$0.001, funded by the seed at 10⁶
credits (`:277`). The two comments disagree by a factor of 10¹¹, and the seed
constant backs the smaller. The largest routine grant anywhere in the code is
the $2 trial, 2×10⁹ credits
(`packages/api/src/services/trial-credits.ts:47`).

If the smaller figure is right, live ceilings are ~10⁹, and 2^53 ≈ 9×10¹⁵ is
never approached — so `billing-module.md`'s precision-limit section describes
a hazard that production does not reach. **The strict decoding in
`cost.go` is still correct**, for the reason that document already gives on
its own terms: the column is Postgres `numeric`, nothing constrains a future
row, and a rounded read would be silent.

**This is not settled.** Nobody queried production Redis. It needs one
`GET valve:credits:<accountId>:ceiling` against the largest live account, and
that is a two-minute job for someone with access.

**3. The brief's 30,000 is unreachable at the DEFAULT threshold.** See above.
The measured reachable bound is 3,486. It is not an invariant: a
`SLOW_MODE_THRESHOLD_USD` small enough to put a near-empty account on the FULL
tier reaches 34,986, and the config currently accepts one.

---

## 3. The usage feed

If eRPC serves directly, the reporting system has to learn what was consumed.
**eRPC emits usage today and persists none of it.** `telemetry/` is
Prometheus counters and histograms; `data/postgresql.go` is a key-value
connector for caching and shared state, not a usage sink; there is no
ClickHouse and no per-request insert anywhere in the tree.

Three candidates.

### (a) A post-answer Redis `INCRBY` — this already exists

`valvebilling.Capture` is exactly this: one `INCRBY` on
`valve:credits:<accountId>:spend` after the upstream answers, refusing
negative and out-of-range values, and returning early on zero
(`valvebilling/capture.go:25-49`). Its doc comment already says a capture
error must not withhold the answer — log it, count it, record zero weight
(`:21-24`).

**Preserves:** exact per-request accounting in the same ledger the api
service already reads. The `spend` key accumulates by `INCRBY` on int64, so
the stored values are exact even though the in-Lua comparison rounds above
2^53 (`valve/billing-module.md:81-99`). It is also the input the projection
writer needs, so the loop closes: capture feeds spend, spend feeds the
projection, the projection feeds eRPC's cache.

**Loses:** it is one Redis round trip per request in eRPC's process. That is
one syscall, not one hop — the brief measured the *whole* relay-to-eRPC hop
at 0.15 cores against 0.39 for gzip, and a single `INCRBY` is a fraction of
that. It also means eRPC's process holds a Redis connection and a piece of
valve-specific code, which is the thing the re-frame was trying to avoid.

### (b) eRPC's Prometheus counters — free, and lossy in a specific way

`erpc_network_request_received_total{project, network, category, finality,
user, agent_name}` is incremented once per routed sub-call
(`telemetry/metrics.go:595-599`, called from `erpc/projects.go:161-163`).
The `category` position carries the **method**, and `user` carries
`User.Id`. 38 metric families carry a `user` label.

So eRPC already emits per-(user, network, method) request counts. Cost is a
pure function of (chainId, method) — that is what `PriceTable.Resolve` is —
so an aggregator can scrape the counters and apply the pricing table itself.
No eRPC code changes at all.

**Preserves:** attribution by user, network and method, at zero cost and zero
new code.

**Loses, and these are disqualifying for a ledger:**

- **Prometheus is a sampled time series, not a log.** A scrape gap loses
  counts. A process restart resets the counter — `rate()` handles that,
  exact cumulative totals do not.
- **`user` has a sentinel.** `NormalizedRequest.UserId()` returns the literal
  string `"n/a"` when no user resolved (`common/request.go:1175-1186`). An
  aggregator that groups by `user` gets a phantom account holding all
  unauthenticated traffic.
- **The `user` label is droppable by configuration.**
  `metrics.counterDropLabels` (`common/config.go:3186-3197`) removes labels
  from every counter. The wrapper's own comment says: "Dropping a label
  collapses every series that differed only in that label into one. The
  counters remain correct — their sums are preserved — but the dropped
  dimension is no longer queryable. Check downstream consumers before
  dropping a label that a billing or attribution pipeline groups by"
  (`telemetry/labeled_counter.go:99-102`). One YAML line silently destroys
  the feed.
- **Cardinality.** user × network × method is 106 methods × 4 chains × N
  accounts. This grows without a bound anyone has set.

Use it for **observability and reconciliation** — a cheap independent check
that the ledger and the traffic agree. Do not use it as the ledger.

### (c) The `X-ERPC-*` cost headers — an attribution signal, not a meter

Upstream ships `server.costHeaders`, default false
(`common/config.go:311-316`, `common/defaults.go:894`). When on, every
response carries `X-ERPC-Calls`, `X-ERPC-Billable`, `X-ERPC-Methods`,
`X-ERPC-Credits`, `X-ERPC-Credits-Total`, `X-ERPC-Network-Id` and
`X-ERPC-Network-Alias` (`erpc/http_server.go:1415-1447`). The header block's
own comment says it exists "so a proxy in front can attribute usage per
network without parsing bodies or re-deriving the network from the URL".

**Preserves:** the sub-call count, the method set and the resolved network,
per response, without body parsing.

**Loses:** it needs somebody in front to read it — which the re-frame
removes. And the credits are **vendor** credit units, what eRPC's upstreams
charge, not valve's pricing table; `isBillableItem` is eRPC's billability
rule, which counts execution reverts as billable and protocol failures as
not (`erpc/http_server.go:1405-1414`). The relay's rule differs: it bills
JSON-RPC errors deliberately, and on the WebSocket path it bills at
admission.

### Recommendation

**(a) for the ledger, (b) for reconciliation, (c) not at all** in the new
architecture, because it needs the hop that is being deleted.

The honest counter to (a): it puts valve code in eRPC's process, which is
what the re-frame set out to avoid. The defence is that it is one
non-blocking `INCRBY` after the answer is already on the wire, it cannot
refuse a request, and it is code that already exists and is already tested.
Nothing else produces an exact ledger.

---

## 4. What this makes of `valvebilling`

Plainly, because the razor in `CLAUDE.md` says unexercised machinery is
itself a commitment.

| Component | Verdict |
|---|---|
| `capture.go` (50 lines) | **Survives unchanged.** It is the usage feed. |
| `cost.go` + `testdata/cost-corpus.json` | **Survives, and moves.** The aggregator needs it; eRPC does not. |
| `hashkey.go` + vectors | **Survives.** Any projection keyed by hashed key needs it, and section 1's blocker is about exactly this. |
| `authorize.go` + `authorize.lua` | **Probably dead on the hot path.** Say so. |
| `module.go` | **Shrinks to almost nothing.** |
| `config.go` | **Survives, smaller.** |

### Authorize

`Authorize` exists to make a blocking sufficiency decision before forwarding.
If eRPC serves directly and enforces from a cached projection, nothing on the
hot path calls it. Keeping it "in case we go back" is the commitment the
razor warns about.

Two honest qualifications, neither of which rescues it as hot-path code:

- **The Lua script must stay, and stay byte-identical**, because whatever
  computes the projection still needs the metering decision, and the api
  service still calls it. `TestAuthorizeScript_MatchesTheMonorepoDigest`
  keeps both callers on one cached script. Deleting `authorize.lua` would
  fork that (`valvebilling/authorize.go:14-34`).
- **The relay still meters WebSockets synchronously today**, and it does it
  with `checkAndConsume` — authorize followed by an unconditional capture,
  billing at admission (`packages/relay/src/meter.ts:421-446`). If WebSockets
  stay on the relay, `Authorize` stays alive there. That is a reason for the
  *function* to exist somewhere, not a reason for it to sit on eRPC's HTTP
  path.

So: keep `authorize.go` if and only if a caller is named. If the answer after
section 9's decisions is "nothing calls it", delete it and keep the `.lua`
and its digest test.

### `Module`

`Module` bundles Redis, the price table, hash, authorize and capture behind
one nil-safe handle (`valvebilling/module.go:19-107`). With `Authorize` and
`ResolveCost` moved to the aggregator, what is left inside eRPC's process is
a Redis client and `Capture`. That does not need a module type. The
nil-is-disabled design was good and it is worth keeping in whatever replaces
it — "off" as the absence of an object, not a branch
(`valvebilling/module.go:11-18`).

---

## 5. What genuinely remains to build

Enforcement is now configuration. These four are not.

### The aggregator

Reads the usage feed, prices it, and decides each account's state. It owns
`cost.go`, the pricing-table refresh against `shared.method_pricing`, and the
`METHOD_CU` table. It is a service with a timer, which is exactly what
`valvebilling` deliberately refused to be
(`valvebilling/module.go:98-107`).

One thing it needs that has no home today: **`METHOD_CU` has no production
source in this fork.** `NewPriceTable(methodCU, defaultCU)` takes them as
parameters on purpose, because hard-coding them is how implementations drift
(`valvebilling/cost.go:116-129`). The only copy here is
`valvebilling/testdata/method-cu.json` — 83 entries, `defaultCu: 6` — read
solely by `cost_test.go:59`.

### The projection writer

Turns account state into `{userId, enabled, rateLimitBudget}` records eRPC
can read, and pushes them. Two ways in: write the connector's store directly,
or call the admin API (`erpc/admin.go:45-51`), which also drops the local
cache. The admin API is better for revocation and worse for bulk, because it
is per-instance.

This is where section 1's raw-key blocker has to be resolved. It is the
gating unknown for the whole design.

### The ledger

Exact per-request spend, per account, durable. Today that is Redis `spend`
plus the api service's reconciliation. If Prometheus is the feed instead of
`Capture`, there is no ledger — see section 3.

### The billing itself

Invoicing, credit purchase, refunds, the `ceiling`/`pending`/`closing` keys.
Already the api service's job and unaffected by any of this. Listed so nobody
thinks it moved.

---

## 6. What eRPC already has, and what it genuinely lacks

Unchanged from the earlier draft except where the re-frame moved something.

**Already has, do not duplicate:**

- Five auth strategies, including a hardened database strategy with positive
  and negative caches, singleflight and a fail-open circuit.
- Four rate-limit enforcement points honouring a per-user budget.
- API-key CRUD over admin JSON-RPC, with cache invalidation
  (`erpc/admin.go:45-51`).
- Per-user telemetry across 38 metric families.
- Opt-in cost/attribution response headers (`server.costHeaders`).
- A trusted-identity inbound header, `projects[].trustUserIdHeader` plus
  `X-ERPC-User-Id` (`common/config.go:763-772`, `common/request.go:99`,
  `:344`) — **upstream, not fork**. Less useful now that the fronting proxy
  is going away, but it is the escape hatch if a thin authenticating layer
  has to stay for section 1's blocker.
- A complete WebSocket server and subscription manager — but these are
  **fork-owned files upstream does not have**: `erpc/ws_server.go` +790/−0
  and `erpc/subscription_manager.go` +606/−0. Editing them costs nothing on
  the rebase axis.
- Both dependencies the billing path needs, as direct requires:
  `github.com/redis/go-redis/v9 v9.22.0` and `github.com/jackc/pgx/v4
  v4.18.3` (`go.mod:27`, `:33`). A Postgres loader adds no `go.mod` line.

**Genuinely lacks:**

- Any usage persistence. It emits and never stores.
- Any billing, balance, or account model.
- Any quota over a window longer than a rate-limit period.
- Any distributed cache invalidation.
- Any way to look a key up by anything other than the literal credential.

---

## 7. Rebase cost, and why the hosting ranking mostly evaporated

The fork tracks upstream by rebasing, so a line changed in an upstream-owned
file is replayed forever. Cost is lines × how often upstream touches that
file.

| File | Upstream commits since 2026-02-24 | Fork's existing diff |
|---|---|---|
| `common/config.go` | 46 | +495/−230 |
| `erpc/networks.go` | 26 | — |
| `erpc/http_server.go` | 22 | +177/−64, 23 hunks |
| `common/validation.go` | 21 | +53/−9 |
| `go.mod` | 16 | — |
| `erpc/init.go` | 7 | +31/−10 |
| `erpc/erpc.go` | 3 | +6/−0 |
| `cmd/erpc/main.go` | 2 | +30/−5 |
| a new `cmd/valve-relay/` | 0 | n/a |
| `valvebilling/` | 0 | n/a |

**The finding the coordinator asked me to confirm or deny: if enforcement is
configuration of an existing strategy, there is nothing to host in eRPC's
process.** That is true for identity, revocation, rate limiting and
per-method policy. All four are `erpc.yaml`, at zero fork lines.

It is **not** true for two things:

1. **The usage feed, if it is `Capture`.** One `INCRBY` after the answer.
   That needs a call site inside eRPC. The cheapest is the same seam as
   before — a `cmd/` binary that builds an `*erpc.ERPC` directly, as
   `internal/simulator/orchestrator.go:269` already does — at zero upstream
   lines. If it must sit on the real HTTP path, the call site is
   `erpc/http_server.go:748`, immediately after `project.Forward` returns, in
   the file upstream touched 22 times in six months. That is an estimated
   25-35 lines, and it is an estimate because nothing is written.
2. **Section 1's raw-key blocker,** if it is solved by a fork-owned connector
   driver. `data.NewConnector` is a hard-coded switch
   (`data/connector.go:119-136`), so that costs an edit there plus a driver
   constant and config struct in `common/config.go` — 46 of 191 commits.
   This is now the single most expensive item in the design, and it exists
   only because of a security constraint, not a functional one.

One thing worth recording because it makes a config section cheaper than
`billing-module.md` assumed: a fork-added `erpc.yaml` section does **not**
need a `typescript/config/src/generated.ts` change. Both the YAML path
(`common/config.go:120-121`) and the TypeScript path
(`common/config.go:3430-3431`) strict-decode into the same Go `Config`
struct, so a Go field serves both at runtime. The fork's `Indexer` section
proves it — absent from `generated.ts` and working. A `.ts` config author
gets no editor type, which is a papercut. It does not change the conclusion,
because `common/config.go` is still the most-churned file in the tree.

---

## 8. Migration

### The monorepo rejected a mirrored shadow, and the re-frame strengthens that

`docs/superpowers/specs/2026-08-23-relay-go-port-design.md:80-110` rejects a
mirrored shadow with a diff engine. The argument is not that shadows are
hard. It is that commit `a08e9b9` moved the billing decision out of both
languages into `AUTHORIZE_LUA`, so **the thing a shadow would prove is no
longer written in the language being ported**. Its table finds exactly one
genuinely silent failure mode — cost computation — and a live shadow is an
expensive way to test a pure function.

Three further strikes it lists: rollback is already a Caddy reload, so a
shadow spends weeks avoiding a risk undoable in one command; a shadow reports
nothing until the diff engine exists, whereas a strangled route reports on
day one; and the tee costs capacity on a box measured at 58.3% CPU stall
after `a08e9b9`.

**The re-frame makes this stronger, not weaker.** Under the new architecture
there is no second implementation of the decision at all — eRPC does not
decide sufficiency, it reads a projection. There is nothing for a shadow to
diff.

### The staged cutover

The stages change, because what moves is enforcement rather than a route.

| Stage | What changes | How to tell it worked | Rollback |
|---|---|---|---|
| 0 | Aggregator runs and writes the projection. Nothing reads it. Cost corpus green; `hashApiKey` vectors green. | Projection matches the relay's key configs, row for row. | Stop the writer |
| 1 | eRPC reads the projection for **identity only**, for one internal key. Relay still gates. | 401 rate flat; `user` label populated in eRPC metrics. | Config revert |
| 2 | eRPC enforces the **rate-limit budget** from the projection. Relay still gates credits. | `erpc_rate_limits_total{budget, origin}` matches the relay's per-key limits. | Config revert |
| 3 | **The decision.** Relay stops gating credits; eRPC serves directly; TTL set from section 2's table. | Overdraft per account stays inside the chosen dollar bound. Watch it directly — this instrument does not exist yet. | Config revert, then Caddy reload to reinstate the hop |
| 4 | Usage feed switches to `Capture` in eRPC's process. | `valve_credits_drift_bps` flat across the switch. | Revert the binary |
| 5 | beacon, then x402 Mode-1, then WebSockets. | Per the port design. | Caddy reload |

Stage 3 is the only irreversible-feeling one, and it is not actually
irreversible — but it is the first stage where a customer can overdraw in a
way the old system would have refused. It should not be crossed until the peg
is settled and the dollar bound is chosen — section 9's questions 1 and 2.

### What runs in parallel

Both systems, for stages 1 to 4. Two things are shared and must be watched:

- **Redis.** The api service, the relay and the aggregator all touch the same
  buckets and the same script. This is the design working — one cached
  script, one ledger — but a bug in one corrupts state the others read.
- **The `cps` bucket has no time component in its key**, so a lost TTL wedges
  an account until someone notices. The Lua arms `EXPIRE … NX` on the reject
  path for exactly this reason, after a 44-hour outage on 2026-08-07
  (`packages/utils/src/credits-lua.ts:83-86`). Every caller must run the
  identical script.

### How anyone would know it still bills correctly

1. **The cost corpus, before anything ships.**
   `valvebilling/testdata/cost-corpus.json` — 340 rows, 1,105 cases from the
   live pricing table, each asserting the resolved **path** as well as the
   value, so a tier-2 case that fell through to tier 3 cannot pass by
   coincidence. Three mutations were run and all three were caught. This is
   the only instrument covering the one silent failure mode, and the re-frame
   does not change that.
2. **`valve_credits_drift_bps`, continuously.** Named by the port design as
   the capture-correctness alarm. It is the instrument for stage 4.
3. **A new instrument stage 3 needs and nobody has built: an overdraft
   monitor.** Per account, how far below zero did the effective balance go,
   in credits and in dollars, and for how long. Today's system bounds this
   structurally at 3,486 credits, and `valvebilling/overdraft_test.go` now
   measures it. Under a
   cached projection it is the primary safety signal, and it must exist
   **before** stage 3, not after.
4. **`valve_meter_outcomes_total{code, billing_class}`** across each stage —
   `packages/relay/src/metrics.ts:178`. Distribution by `code` should not
   move.

---

## 9. Open questions

### These need a human decision

1. ~~**Which credits-per-USD peg governs request pricing?**~~ **ANSWERED
   2026-08-24: the ledger's 10⁹ governs.** It is stated in five independent
   places across the money path; the calculator's 10⁶ is one constant
   contradicting all five. What remains is not an engineering question — the
   landing page over-quotes by 1,000× and someone has to decide which number
   the product means. That is with the operator. Question 2 is no longer
   blocked.

2. **How many dollars of overdraft, per account, per window, are
   acceptable?** This number sets the cache TTL and therefore the whole
   design — section 2's table converts between them. Ask it in dollars, not
   by analogy to today's 3,486-credit bound: that bound is $0.0000034860 and
   is an accident of the retry loop and a tumbling window's phase, not a
   policy anyone chose. eRPC's one-hour
   default is almost certainly not the answer, but at the ledger peg it costs
   about two cents per account, which may well be acceptable.

3. **How is the raw-key blocker resolved?** Section 1. PostgreSQL connector
   (raw key at rest in a column), a fork-owned hashing connector driver (an
   edit in the most-churned file), or keep a thin authenticating layer (the
   hop the re-frame deletes). This is a security decision with an
   architecture consequence, and it is the gating unknown.

4. **Is the ledger `Capture` or Prometheus?** Section 3. `Capture` is exact
   and puts valve code in eRPC's process. Prometheus is free and is not a
   ledger. Choosing Prometheus means accepting that billing is reconstructed
   from a sampled time series.

5. **Does `authorize.go` keep a caller?** If WebSockets stay on the relay it
   lives there. If nothing calls it, delete it and keep only `authorize.lua`
   and its digest test. Someone has to say which.

6. **How does fleet-wide revocation work?** Short TTL, or fan the admin call
   out to every instance. The second needs per-instance addressability that I
   have not verified exists.

7. **Where does `METHOD_CU` come from in production?** A generated Go file, a
   Postgres table read by the same refresher as pricing, or an embedded
   asset. The brief's preference order says "generate from one source" beats
   a second copy. Cross-repo ownership decision.

8. **Does the gas oracle move with the relay?** Inherited unanswered from
   `docs/superpowers/specs/2026-08-23-relay-go-port-design.md`, "Open
   question". Listed so it is not lost.

9. **Confirm the five tier values against the live box.** None of
   `FULL_CREDITS_PER_SEC`, `SLOW_CREDITS_PER_SEC`, `SLOW_MODE_THRESHOLD_USD`,
   `FULL_RATE_RPS` or `SLOW_RATE_RPS` appears in any deployment artifact in
   the monorepo, so section 2 assumes the code fallbacks. Production reads
   `EnvironmentFile=/opt/valve/.env`
   (`config/systemd/valve-relay.service:10`), which is not in the repository.
   Somebody with access should read it. If it sets
   `FULL_CREDITS_PER_SEC=0`, overdraft is unbounded **today** and that is an
   incident, not a design question.

10. **Read the largest live `ceiling`.** One `GET` settles whether
    `valve/billing-module.md`'s 10¹⁷ figure or the seed's 10⁶ is right, and
    with it whether that document's precision-limit section describes a live
    hazard or a theoretical one. Section 2's corrections.

### These can be settled without a human

1. **Can eRPC enforce a per-user rate limit from cached state today?** Yes.
   Five strategies populate `User.RateLimitBudget`; the authorizer prefers it
   over the strategy's; four enforcement points honour it. No new code.
2. **Does a fork-added config section need a `generated.ts` change?** No.
   Section 7.
3. **Does a Postgres pricing loader need a `go.mod` line?** No. `pgx/v4` is a
   direct require at `go.mod:27`.
4. **Can the gRPC connector serve the projection?** No. It is a BDS
   read-through cache with a method allowlist (`data/grpc.go:25-33`,
   `:58-71`).
5. **Can `server.costHeaders` replace `ResolveCost`?** No. Vendor credit
   units under eRPC's own billability rule, not valve's pricing table.
6. **Is cache invalidation distributed?** No. `s.cache.Del` on a ristretto
   cache, per process, with no fan-out (`auth/strategy_database.go:446`,
   `InvalidateCache`).

### Where I am uncertain, stated plainly

- **The 1,000× peg discrepancy is unresolved and I could not resolve it from
  source.** Section 2 gives both columns rather than picking one. This is the
  largest single uncertainty in the document.
- **The five tier values are read from code fallbacks, not from the box.**
  Verified absent from every deployment artifact in the monorepo; not
  verified against `/opt/valve/.env`.
- **The largest live `ceiling` is unresolved.** Two comments in the monorepo
  disagree by 10¹¹ and nobody queried Redis.
- **The overdraft bound is now MEASURED, and the derived figure was wrong.**
  `valvebilling/overdraft_test.go` runs it against a real redis-server 7.2.4
  under concurrency: **3,486 credits**, not the 3,000 this document derived.
  Two independent errors in the same direction pair: the window yields
  `floor(500/6) x 6 = 498` rather than 500, and the tumbling window is
  anchored at the first charge rather than a clock, so a 12-second span at the
  worst phase touches SEVEN windows. The 6-window worst case still assumes a
  pinned upstream and at least three backends; unpinned it is 5 windows.
- **I did not verify that eRPC's admin transport is addressable per
  instance.** Fleet-wide revocation by admin fan-out depends on it.
- **I did not verify who writes `valve:credits:<account>:ceiling`,
  `:pending` and `:closing`, or on what schedule.** It is in
  `packages/api/src/credits/`, outside the relay's request path. Every
  version of this design depends on it continuing to run.
- **I am not certain there is no fourth way around the raw-key blocker.** It
  is the point in this document where I would most want a second reader.
- **The 25-35 line estimate for a `Capture` call site is an estimate.**
  Nothing is written.
- **`valverelay/` and `cmd/valve-relay/` were in flight.** Under the
  re-frame, most of what a relay binary would have done is now eRPC
  configuration. What that work should become is worth re-deciding rather
  than finishing on the old premise.
