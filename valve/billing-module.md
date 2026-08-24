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
the in-Lua sufficiency comparison rounds. The worst case is an account whose
effective balance sits within one ULP of zero at that magnitude overspending by
about $1.6e-8.

A second correction, in the other direction. The originating brief gives the
overdraft bound as roughly 6x `FULL_CREDITS_PER_SEC`, about 30,000 credits, and
an earlier version of this document repeated it. It is unreachable. The tier is
chosen from the effective balance, and any account close enough to zero to
overdraft is far below the $5 SLOW threshold, so it is always on the SLOW tier
by the time overdraft is possible. The reachable bound is 6x
`SLOW_CREDITS_PER_SEC` = 3,000 credits; a FULL-tier account cannot overdraft at
all at the defaults. Confirmed in the monorepo's own code.

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
