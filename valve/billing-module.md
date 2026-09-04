# The valve billing module

`valvebilling/` performs the relay's per-request billing path inside this
fork: resolve a cost, authorize it against Redis, let the caller forward, then
capture. It is built to the monorepo's handoff brief,
`deploy/rpc/runbooks/erpc-billing-module-brief.md` (2026-08-24).

## The diff against upstream is zero files

That is the design constraint, not a nice property. The fork carries
`reconcile/ws-plus-main` and `archive/harvest-onto-main`, and this work must
not make reconciliation harder.

Measured on 2026-08-24: `valvebilling/` is a path that exists in neither
reconciliation branch nor in `upstream/main`, so it cannot conflict. Nothing
outside the directory changed — not `go.mod`, not `go.sum`, not one upstream
file.

Two decisions buy that:

**Configuration comes from the environment, not `erpc.yaml`.** eRPC decodes
its config with `KnownFields(true)`, so teaching it the word `billing` needs a
field in `common/config.go` — an upstream-owned file this fork has already
edited +495/-215, and one of its worst recurring conflict sites. One line
there is not worth it when an environment variable does the same job.

**Nothing links the module in.** There is no import of `valvebilling` from
`erpc/`, so no fork-owned import file is needed yet. When a host does wire it
up, the cheapest seam is a `cmd/` binary that builds an `*erpc.ERPC` directly
— `internal/simulator/orchestrator.go:269` already does exactly that — rather
than editing `erpc/http_server.go`'s 590-line request closure.

## What is deliberately not here

**The metering decision.** It lives in `authorize.lua`, which runs inside
Redis. This module calls it. The copy here is byte-identical to the
monorepo's `AUTHORIZE_LUA` as of `a08e9b9`, so go-redis and ioredis hash to the
same SHA1 and share one cached script instead of loading two that could
differ. `TestAuthorizeScript_MatchesTheMonorepoDigest` pins that. If it fails,
re-copy the script — do not update the constant.

Reimplementing that decision in Go would be the single worst change anyone
could make here. Four semantic mutations to the TypeScript `authorize()`
survived a 740-test suite in the monorepo, including one that would have given
every freshly topped-up account an instant `no_credits` with nothing going red.

**A refresh timer.** The module owns no goroutine. What refreshes pricing, and
how often, is the host's decision — and a package that started its own timer
would be doing something the flag could not fully switch off.

## Off means absent

A disabled module is a `nil *Module`. `Enabled()` is nil-safe, so the caller
asks one question and takes the stock path. There is no Redis connection, no
timer, and no state left running. `New` returns `(nil, nil)` when the flag is
clear — a nil module with a nil error is the SUCCESS case.

A disabled module that is used anyway returns an error rather than a neutral
answer. Both neutral answers are silent: "allowed" bills nobody while looking
healthy, "denied" breaks a deployment that never wanted billing.

## What can still break quietly

Cost. It is computed here rather than in Lua, and a wrong cost bills the wrong
amount with nothing going red. `testdata/cost-corpus.json` is the contract —
340 rows and 1,105 cases generated in the monorepo from the live pricing
table. Every case asserts the resolved PATH as well as the value, because a
case meant for tier 2 that silently fell through to tier 3 could land on the
same number by coincidence and pass.

Three mutations were run against the resolver and all three were caught:
folding the method to lowercase, deleting the zero-address fallback, and
setting the default to 20.

That last one is not hypothetical. `DEFAULT_CU` really was 20 until commit
`d36e09c` cut it to 6, and two comments in the monorepo still said 20 on
2026-08-24 — including the header of the very file that documents this as the
only silent failure mode. Two independent readers copied the wrong value from
it before the source was checked. The monorepo fixed both in `d170faa`.

## The overdraft tests measure a two-second window, and a loaded machine misses it

`cpsBucketTTL` is 2 seconds (`valvebilling/overdraft_test.go:68`) — the lifetime
`authorize.lua` arms on the cps bucket. The overdraft tests measure a rate
against that bucket, so the whole burst has to land inside one window or the
numbers mean nothing.

`requireOneOverdraftWindow` (`overdraft_test.go:243`) enforces that. It asserts
`credits == bucket` and fails with "the burst straddled a bucket expiry (took
%v); the single-window numbers below are not valid".

**That guard is doing its job, and it is not a billing defect.** A burst that
crosses a bucket boundary produces a rate measured across two windows, which is
a false number. The test refuses to report it. Read a failure here as "the
machine could not run the burst fast enough", not as "the bound is wrong".

Observed once, on 2026-09-04, during a single `go test` invocation covering 30
packages while the container-backed `data` package was pulling and running
DynamoDB, Postgres and Redis. Between four and seven of these tests failed, a
different subset on each attempt, every one reporting a burst of 2.1 to 3.0
seconds against the 2-second bucket.

It did not reproduce. Same tree, same machine, same day:

| how it was run | result |
|---|---|
| `go test ./valvebilling/` alone | ok, 42.0s |
| the same, under 8 busy-loop processes on 8 CPUs | ok, 44.1s |
| the full 30-package run again, machine otherwise quiet | ok, 49.8s |

So synthetic CPU load does not trigger it. What did was a run where many test
binaries and three containers competed for I/O at once. That is a narrower
condition than "slow machine", and it is worth knowing before anyone widens the
window to make a red run go green.

**Do not widen `cpsBucketTTL` or relax the guard.** Either change makes the
suite report a rate it did not measure, which is the one outcome worse than a
failure. Re-run on a quiet machine, or use `go test -short ./valvebilling/`,
which skips the timing work and passes in about 16 seconds.

The real exposure is CI. A loaded runner fails here and the failure reads like a
billing regression, because the test names are all about credits and bounds. If
that happens, check the reported burst duration first — if it is above 2
seconds, the machine is the finding.

## A precision limit in the shared script, recorded not fixed

`authorize.lua` does `tonumber(ARGV[1])`, and Lua numbers are doubles, so
integers above 2^53 round. Measured: below 2^53 a one-unit shortfall is
caught; at 2^53 it is invisible and two units are caught; at 2^54 the
granularity is four.

This IS reached in production. Read raw from Redis on 2026-08-24, the largest
live `ceiling` is `99999680453646021` — 11.1x 2^53, where the double's
granularity is 16 credits. That account is also the source of essentially all
current traffic.

An earlier draft of this section hedged that number as unsettled, on a
suggestion that the only account matching the description was a system account
seeded at 10^6. The suggestion was wrong and the hedge was worse than the
original claim: it made a measured fact read as a doubt. The number was
re-measured directly rather than argued about.

It is nonetheless harmless, and the reason is the important part: the STORED
values are exact, because `spend` accumulates by Redis `INCRBY` on int64. Only
the in-Lua sufficiency comparison rounds.

**Measured rather than derived, and smaller than the generic bound.** 16 is the
ULP at that magnitude, but this ceiling is not a worst case for it: it reads
into Lua as `99999680453646016`, a drift of −5 credits, and the largest
overspend it permits is **3 credits** — about $0.000000003 at the ledger's peg.
An earlier draft of this section quoted the generic 16 and $1.6e-8. See
`valvebilling/limits_test.go`, which measures each boundary against the real
script.

**And the failure needs a specific shape, which the same tests establish.** An
account whose balance is exactly exhausted — `ceiling == spend` — refuses a
1-credit charge correctly at EVERY magnitude tested, up to 2^62. Equal operands
round to the same double, so their difference is exactly zero however large
they are. The error requires the operands to round in OPPOSITE directions, and
there are two shapes that do it:

- a 1-credit **balance** disappearing, from **2^54** ($18.0M), where the
  customer is wrongly REFUSED their last credit — the error runs against the
  customer, not the house;
- a 1-credit **charge** wrongly allowed, from **2^53** ($9.0M), which needs a
  live `pending` in the sum.

Draining `pending` on settle removes the second shape entirely.

A second correction, in the other direction. The originating brief gives the
overdraft bound as roughly 6x `FULL_CREDITS_PER_SEC`, about 30,000 credits, and
an earlier version of this document repeated it. It is unreachable. The tier is
chosen from the effective balance, and any account close enough to zero to
overdraft is far below the $5 SLOW threshold, so it is always on the SLOW tier
by the time overdraft is possible. The reachable bound was derived here as 6x
`SLOW_CREDITS_PER_SEC` = 3,000 credits. That derivation is wrong twice, and
`valvebilling/overdraft_test.go` now measures **3,486** against real Redis: one
window yields `floor(500/6) x 6 = 498` because the last approval must fit
whole, and the window is anchored at the first charge rather than a clock, so a
12-second span at the worst phase touches seven windows. A FULL-tier account
cannot overdraft at the DEFAULT threshold — but the threshold has no floor, and
a small enough one puts a near-empty account on the FULL branch at 34,986.

The TypeScript relay passes the same decimal string to the same script and has
always behaved this way. Do NOT "fix" this in Go: byte-identical outcomes are
the requirement, and diverging would make the two implementations disagree,
which is the failure this module exists to avoid. If it is ever worth
changing, the script changes for both callers or neither.

`TestAuthorize_CarriesTheFullDecimalAndSharesTheScriptsPrecisionLimit` pins the
measured boundary, so a change in Redis or in the script shows up as a test to
read rather than a silent behaviour change.

## One correction to the brief

The brief's hazard 3 says `amount_wei` "is Postgres numeric and exceeds 2^53".
The type can; no live row does. The monorepo confirmed on 2026-08-24 that the
whole table maxes out at 50, and that the column is misnamed — every row is
credits-per-request, not token wei. Token-wei amounts belong to the x402 path,
which never calls the credits meter.

The strict decoding here is still right, for a different reason than the brief
gave: the column is `numeric`, nothing constrains a future row, and a rounded
read would be silent. A JSON number is refused rather than accepted.

The monorepo has one live instance of the pattern this guards against, at
`beacon-handler.ts:276` (`Number(amountWei)`). Harmless at 50. Do not mirror
it if beacon pricing is ever ported.

## The block-span tariff, and the one hazard it introduces

`valvebilling/rangecost.go` prices a request by the block range it names. It
exists because the method table does not: `eth_getLogs` bills 18 credits for one
block and 18 for five million, and every other range method is flat the same
way. `trace_filter` is 12, `debug_traceBlockByNumber` is 12, `eth_getFilterLogs`
is 18. None of them read the range. That is the same price for six orders of
magnitude more work, and it is the largest mispricing in the table.

This is not a precision fix. The window model below removes the precision limit
on its own. The tariff stands or falls on the mispricing alone.

It reads no method name. It looks for a params object carrying `fromBlock` or
`toBlock`, by position or by name, so the next range method to appear is priced
correctly with no edit. The razor argues for that: an 83-entry method table is
an enumerated case list, and a charge proportional to the work a request names
demotes it to a per-request floor.

### Two defects were shipped and caught by the tests, not by review

**The widest span billed nothing.** `Span.Blocks()` computes `To - From + 1`.
For `From: 0, To: MaxInt64` that is 2^63, which wraps to `MinInt64`, and the
charge path read the negative result as "no blocks". The single most expensive
request a client can name — `{"fromBlock":"0x0","toBlock":"0x7fffffffffffffff"}`
— was free at every tariff. `Credits` now measures the GAP, which is always
representable: `units = gap/BlocksPerUnit + 1` carries the round-up rule and the
overflow fix in one expression, because `gap = qB + r` with `1 <= r+1 <= B`
proves exactly one more unit is always due.

`Blocks()` still wraps on that one span and is still exported. The charge path
no longer reads it, so no bill is affected, but anything that logs it or
compares it against a limit gets `-9223372036854775808` with nothing going red.
Pinned as a characterization test. The honest repair changes the signature.

**The overflow comment was false.** It claimed a 2^63-block span overflows "any
tariff". At 1,000 blocks per unit and 5 credits per unit the exact charge is
46,116,860,184,273,880, which fits an int64 with room. Erroring there would have
refused a request the tariff can price. The code now returns the exact charge
wherever it fits, and the credit gate refuses 46 quadrillion credits on its own
merits.

### The hazard: a cold start refuses every open-ended range

`Credits` refuses to price a range it cannot resolve, rather than returning
zero. It has to: eRPC forwards `safe` and `pending` to the upstream untouched
(`architecture/evm/json_rpc.go:55-80`), so `{"fromBlock":"earliest",
"toBlock":"safe"}` returns whole-chain data, and answering zero would let one
word buy the tariff off.

The cost of that is a cold start. `Heads{}` is what the process holds until the
head poller answers, and in that window every range with a tag or a defaulted
end is unresolvable — **including plain `{"fromBlock":"0x1"}`, which is the
ordinary open-ended `eth_getLogs`**. So at boot, on every chain, `Credits`
errors for the most common range request there is.

**That forces the caller's policy, and the forcing is the useful part.** The
fallback cannot be "refuse", because a cold start would then refuse all
`eth_getLogs` — an outage caused by a pricing function. It has to be **bill the
flat price and COUNT it**. The counter is what keeps the bypass visible, and the
two cases have different signatures: a cold start is a burst that ends when the
poller answers, and a client leaning on `safe` is a counter that never returns
to zero. Neither is silent, which was the brief's one requirement.

### Open, and deliberately not decided here

- A backwards range is swapped, so `{"toBlock":"0x5"}` at head 1,000,000 bills
  999,996 blocks for a query that returns nothing. Swapping is right for eRPC's
  `eth_query` shim, where `order: desc` makes `from > to` intentional, and wrong
  for `eth_getLogs`, where it means an empty result. The two cannot be told
  apart without reading the method name, which this file will not do.
- A filter nested under a key is not found and pays flat.
- `eth_getLogs` by `blockHash` pays flat. Correct today — one block.

`rangecost_test.go` is 132 subtests and kills 13 of 13 mutants, including one
the reviewer added for a bug in my own first patch: reading `Found` before
`Resolved` priced a hand-built `Span` as free.

## Draining the counters, if that is the direction

The operator proposed keeping credits as integers and periodically draining the
Redis counters as they settle to Postgres or on-chain, so Redis holds an
unsettled delta rather than a lifetime total. `valvebilling/limits_test.go`
measures what that buys and what it requires.

**It works, but only if the drain shrinks the CEILING, not just the spend.**
The rounding error scales with the larger operand, and that is `ceiling`. Three
shapes at the live magnitude, all measured:

| shape | verdict |
|---|---|
| large ceiling, drained spend, small cost | right — but because the balance is nowhere near zero, not because the arithmetic is exact |
| `spend ≈ ceiling` | **wrong**, by up to 3 credits at the live ceiling |
| small ceiling AND small spend | exact, at every cost tested |

So draining `spend` alone leaves the arithmetic wrong and merely moves the
balance away from the point where it shows. The precision limit disappears only
when both operands are small — which means `ceiling` has to be a REMAINING
balance that the settle refreshes, not a lifetime grant.

### The safe drain

Settle amount `V`, then:

1. `DECRBY ceiling V`
2. `DECRBY spend V`

**Never `SET 0`.** Measured: with a concurrent `Capture` of 250 credits landing
between the read and the write, `SET 0` discarded all 250 and `DECRBY V` lost
none.

**`DECRBY` is not the final answer either — see "How to zero it" below.** It
survives the concurrent capture, which is the only failure this section tested.
It does not survive a crash between the durable write and the `DECRBY`, which
bills the window twice. The settle wants `RENAME` to a named staging key plus
an idempotent write. Read both sections before building one. `SET 0` is only safe for an authoritative recompute, which is what
`REFRESH_LUA` does for `ceiling` today — the same move on `spend` loses money.

**Ceiling first.** A crash between the two steps leaves the account temporarily
short, so it refuses — safe. Spend first would hand out `V` free credits. Do
both in one `MULTI` or one script if the settle path can.

Draining `pending` on settle also removes the wrongly-allowed shape entirely,
since that shape needs a live `pending` in the sum.

### The window is what removes the limit, not the unit size

The operator's later framing is sharper and it is the one to build: **Redis
accumulates, Postgres resolves, the counter goes back to zero.** Redis holds a
window's worth of activity; Postgres holds the truth.

That makes the precision limit a function of how long a counter is allowed to
run, not of how fine a credit is.

The volume is now measured: **33,065,653 requests in the 24 hours to
2026-08-25**, from `relay.relay_request` on valve-ingress — about 383 a second.
An earlier version of this section said 21,000,000 a day and flagged it as
unsourced. It was, and it was wrong.

Carry the caveat that came with it: migration 0019 measured valve's own Admin
Operations project at **99.993% of all rows**, so this is a LOAD figure, not
customer demand.

The cost per request also moved. Monorepo commit `ea1e12b` multiplied
`METHOD_CU` by 100, so `DEFAULT_CU` is 600 rather than 6. Together those two
corrections change the table below by 157x — 229,623 credits a second, not
1,458.

At that rate the largest value any counter ever reaches is:

| settle window | max counter value | headroom to 2^53 |
|---|---|---|
| 1 minute | 13,777,355 | 653,768,374× |
| 1 hour | 826,641,325 | 10,896,140× |
| 1 day | 19,839,391,800 | 454,006× |
| 1 year | 7,241,378,007,000 | 1,244× |

Even if the settler never ran for a **year**, three orders of magnitude of
headroom remain. The 157x correction moved every number and changed no
conclusion, which is the useful thing to know about it. So the peg keeps its resolution: 1 credit stays $10⁻⁹ and the
choice stops being a precision question.

The requirement above does not go away. Zeroing `spend` alone leaves `ceiling`
at 10¹⁷ inside `effective = ceiling + pending - spend`, and the error scales
with the larger operand. **Both numbers have to be windowed.** Postgres grants a
window's allowance into Redis, Redis meters against it, the settle returns the
spend and re-grants.

That turns the Redis ceiling into a **lease**, which is not machinery added on
top — it falls out of "resolve into Postgres and set to zero" if the model is
applied to both counters. It is also what bounds overspend once more than one
eRPC process runs, exactly, and with no dependence on a clock.

### How to zero it, measured against real Redis

"Set to zero" is the right model and the wrong instruction. The settle has two
independent failure points — a concurrent capture, and a crash mid-settle — and
each of the obvious primitives survives only one of them.

A window of 1000 credits settles. A concurrent `Capture` of 250 lands during
it. Postgres must end up holding 1250. Measured on `redis-server` 7.2.4,
2026-08-24:

| strategy | crash point | Postgres ends at |
|---|---|---|
| read → Postgres → `DECRBY V` | after the Postgres write | **2250** — billed twice |
| `GETSET spend 0` → Postgres | after the `GETSET` | **250** — window lost |
| `RENAME spend spend:settling` → Postgres → `DEL` | after the `RENAME` | 1250 — exact |
| `RENAME` → Postgres → `DEL`, plain append write | after the Postgres write | **2250** — billed twice |
| `RENAME` → Postgres → `DEL`, upsert keyed on (account, window) | after the Postgres write | 1250 — exact |

`GETSET` closes the concurrency race, and that is why it looked right. It opens
a worse hole: it clears the counter BEFORE the amount is durable, so the value
exists only in process memory and a crash loses the window with nothing going
red. Losing money quietly is worse than billing twice, which at least shows up
in a reconciliation.

**The settle needs both halves.** An atomic `RENAME` to a NAMED staging key, so
the amount survives a crash under a name a recovery pass can find. AND a
durable write that is idempotent on that name, so finishing an orphan twice
posts the amount once. Either half alone fails at one of the two crash points.

This corrects an earlier recommendation in this document that named `GETSET`
alone. `valvebilling/settle_test.go` reproduces every row of the table.

### The limits underneath, measured

| limit | boundary | at the 10⁹ peg |
|---|---|---|
| a 1-credit balance disappears | 2^54 | $18,014,398.51 |
| a 1-credit charge wrongly allowed (needs `pending`) | 2^53 | $9,007,199.25 |
| `INCRBY` refuses to cross int64 max | 9223372036854775807 | $9,223,372,036.85 |
| `Capture` refuses a cost past int64 before Redis is touched | 2^63 | — |

`INCRBY` errors rather than wrapping, and the counter is unchanged. A cost at
ERC-20 wei scale (10^24) never bills on this rail.

**One test-environment caveat.** miniredis and real Redis diverge on a cost
above int64: real Redis converts it to `1e+24` and answers `no_credits`, while
miniredis fails to compile the script and `Authorize` returns an ERROR. Neither
bills, but a caller that fails open on an authorize error would behave
differently under test than in production. The fixtures were cross-checked
against a real `redis-server` 7.2.4 for this reason.
