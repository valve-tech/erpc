# Triage of the 79 open entries

Date: 2026-08-21. Source: `upstream-bug-log.md`, 167 entries — 84 fixed, 79
open, 4 not a bug.

## Progress since this triage was written

**2026-08-21, later still.** Entries **121** and **125** are fixed, and they
went together for the reason the family predicts: both turned a value the
process could not read into a silent number. `durationMs` returned 0 ms, which
switched the sticky cooldown off; `LOG_LEVEL` installed `NoLevel`, which
switched logging off. Both now land where an ABSENT value lands, and both say
so. The log reads **88 fixed, 62 open, 18 not a bug** across 168 entries.

Fixing them added two entries. **173** records that `docs/pages/` pins 622
source line numbers with nothing checking them, and that a hand-check of ten on
one page found four pointing at unrelated code. The fork's rebase makes that
worse: an upstream commit that inserts a line above a citation moves the target
and leaves the citation resolving. **174** is the same family as 125, one file
over — `erpc.Init` promises debug for an unreadable `logLevel` and assigns to a
variable nothing reads. It goes in the "document, do not patch" bucket: the
behaviour is already right, and two lines in the startup path are not worth a
permanent conflict. **175** is fixed rather than opened: upstream's `eslint`
commit hook has never run, because nothing in the tree configures eslint, so
every JavaScript edit forced `--no-verify` — which skips the conflict-marker
check too.

The rule generalised cleanly, which is the argument for taking the rest of the
family together: **an unreadable value must cost what an absent value costs,
and must be reported.** Falling back is half of it. Without the report the two
events stay indistinguishable to the operator, so the test idiom needs both
halves — and in both fixes a mutation that removed only the report still failed.

**2026-08-21, later.** The closures are applied: 14 entries moved to `not a
bug`, and 4 of the proposed 18 were rejected on review because they had live
effects the one-line summaries did not show. The log now reads **86 fixed, 63
open, 18 not a bug**.

**2026-08-21.** Entries **53** and **64** are fixed, and they went together:
both were the same hand-listed legacy struct, and the fix deleted all three
copies rather than completing them. That drops the open count to 77 and empties
the first row of the mechanism table below. Entry 53 carries the account.

The drift was worse than the entry recorded. `UpstreamConfig`'s copy dropped
two fields when the entry was written and **four** by the time it was fixed —
`chain` and `chainProbeInterval` arrived with a rebase and nobody updated the
copy. That is the argument for deleting a parallel structure instead of topping
it up, measured rather than asserted.

## What changed

The fork no longer plans upstream pull requests. It tracks `erpc/erpc` by
rebasing, so **every fix the fork carries is a patch it replays forever**. That
changes the question each open entry has to answer. It is no longer "is this a
bug?" — the entries already settled that, with a `file:line` and a reading of
the source. It is now:

> Does carrying this patch cost less than living with the defect?

For a client-visible wrong answer, yes. For a dead branch that no input
reaches, no — deleting it changes no behaviour and buys a rebase conflict in
that file for as long as the fork exists.

## Method, and what it does not cover

Each entry carries a one-line severity summary written by the session that read
the source. This triage groups the 79 by those summaries and by the shape of
the defect. **It does not re-read all 79 sources.** Two entries were re-read,
because a latent crash deserves a second look, and both moved bucket as a
result. Treat a bucket as a proposal with a stated reason, not as a verdict.

## The buckets

| Bucket | Count | What to do |
|---|---|---|
| **Edge** | 10 | One defect, ten entries. One patch at the config edge. |
| **Fix** | 47 | Carry a fork patch. Ranked below. |
| **Close** | 14 | Dead or unreachable. Change no code. |
| **Document** | 4 | True, and a patch would make it worse. |
| **Watch** | 3 | Real, not actionable yet. |
| **Duplicate** | 1 | Entry 71 restates 24. |

## The headline: ten entries obey one rule, at ten sites

Entries **18, 25, 36, 53, 64, 117, 121, 125, 126, 127** share one property
(53, 64, 121 and 125 are now fixed):

> eRPC collapses three different operator events into one outcome — "did not
> say", "said this", and "said something I cannot read".

Entry 121 states it best in its own text: *"Parse failure is not the same event
as absence, and the code collapses the two."* An operator who writes
`minSwitchInterval: '30 s'` gets a cooldown of zero, while one who omits the key
gets 30 seconds. Writing the knob wrongly buys LESS protection than not writing
it at all. That inversion is the family.

**Correction, and it changes the plan.** The first version of this triage said
one patch at the config edge closes all ten. That was a guess about mechanism,
and it is wrong. The guess was that `KnownFields(true)` fails to reach inside
the nine types in `common/config.go` that define their own `UnmarshalYAML`. A
probe says otherwise: a typo'd key under `SelectionPolicyConfig`,
`UpstreamConfig` and `NetworkDefaults` is rejected, same as the control. Strict
decoding already works.

The ten sit in at least six code paths, so they are ten small patches sharing
one rule, not one patch:

| Mechanism | Entries | Where |
|---|---|---|
| A fallback decode's shadow struct has not grown with the real type, and its error masks the real one | **53, 64** | `common/config.go` `UnmarshalYAML` |
| Defaulting cannot tell "unset" from "set wrong" — a nil check stands in for "the user did not specify" | **18, 25, 36** | `common/defaults.go` |
| ~~A parse failure becomes a silent zero or silence~~ — FIXED | ~~**121, 125**~~ | `internal/policy/stdlib/install.go`, `cmd/erpc/` |
| A value marshals out and will not parse back | **117** | `common/architecture_evm.go` |
| A guard tests the wrong thing (`len(s) > 1`, not `s != ""`) | **127** | `cmd/erpc/main.go` |
| The warning list covers project-level keys only | **126** | `common/legacy/translate.go` |

It is still the item to do first, for a different reason than claimed. The
sites are small, two pairs genuinely share a fix (53 with 64, 121 with 125), and
one test idiom covers all ten: **assert that a wrong value and an absent value
produce different outcomes, and that the wrong one is reported.** Ten patches
under one rule cost far less to carry and to review than ten unrelated ones.

Two cautions, both from the entries themselves. Entry 53 and entry 64 each have
a test that **asserts today's broken behaviour**; those tests change with the
fix, and the change is the point. Entry 18 records that the current behaviour
**may be intended** — settle that before treating it as a defect.

## Closed: 14 entries of dead code — applied 2026-08-21

**12, 40, 51, 55, 60, 68, 75, 101, 106, 107, 116, 137, 162, 164** now carry
`**Status:** not a bug`, which takes the open count from 77 to 63.

Every one is an unreachable branch, an uncalled function, work a later line
overwrites, or an argument nothing reads. Their own summaries say so: "Severity:
none today. Unexercised machinery."

They stay in the log as knowledge — each still reads as an active guard, and the
next reader deserves to know it is not one. They are not worth a patch. Deleting
dead code in a fork costs a permanent conflict in that file and changes no
behaviour.

### Four failed review, and stayed open

This bucket was proposed as **18**. Reading each entry before flipping it —
rather than trusting the one-line summary the bucket was built from — rejected
four. That is a 22% error rate in summary-based triage, and the reason this
document said to treat a bucket as a proposal.

- **8** — not dead. `AnalyseConfig` has no caller in this repo but it is
  **exported**, and the entry carries a pinned test. A healthy upstream on the
  correct chain fails validation with `invalid hex string: 123`. The same file
  already gets it right on its other path, so the fix is one line. Moved to
  rank 3.
- **37** — the code runs. It invents an id like `-8` from a process-global
  counter, and that id reaches a config dump. Cosmetic, not absent. Moved to
  rank 5.
- **54** — the BRANCHES are dead; the effect is not. QuickNode traffic is filed
  under `nodejs` and Edge under `chrome`, so a per-client breakdown names the
  wrong client. Wrong data, with a clear right answer: order the specific tests
  before the general ones. Moved to rank 5.
- **114** — its own status corrects its title. Two of three items are dead
  weight; the first is not. `EvmGetChainId` formats a uint64 as decimal, so a
  node reporting a chain id at or above 2^63 makes `strconv.ParseInt` fail with
  a range error. The entry calls that branch "rare, not dead". Moved to rank 3.

## Document these 4, do not patch them

**33, 93, 94, 112.**

These are metric and status-code semantics that mislead a reader. A patch would
change what a counter means. Downstream code already reads these counters and
codes around exactly these traps — `upstream="n/a"` covering three different
events (94) is a documented trap in the fork's own operational notes. Changing
the meaning under a consumer that has already adapted is a regression, not a
fix. Entry 112 is the mechanism behind 93, not a second defect.

Write the semantics down where the consumer reads them. Leave the counters
alone.

## Watch these 3

- **10** — an upstream test that fails about one run in three under load. Chased
  and not reproduced: with the old deadline restored and all eight cores
  saturated it still passed. The hypothesis is recorded in the entry.
- **H1** — aliased memory returned on one path and copied on the other. Its own
  status says it "is still only a hazard". Nothing observed reaches it.
- **138** — a JSON block number above 2^53 arrives already wrong. Not eRPC's to
  fix alone.

## The 43 to fix, ranked

Ranked by what a client or an operator loses, not by how hard each is.

**1 — a client gets a wrong or broken answer (11).**
7, 24, 29, 39, 48, 74, 76, 77, 86, 87, 136.
A corrupt cache entry answered as HTTP 200 (86), a truncated request body
answered 200 and blamed on the server (29), `"0x"` normalised to the zero hash
(7), a released response that reads exactly like a nil one (76, 77), a listing
that never hands out its next-page token so the caller believes it saw
everything (24).

**2 — a crash or a leak (5).**
9, 26, 45, 58, 109.
Entries 9 and 58 are one-line nil dereferences, and both are cheap: 9's two
sibling constructors already carry the guard it lacks, and 58 only has to check
its error before it uses the client. Entry 26 is a real send-on-closed panic
that **cannot be pinned by a test** — a panic in a background goroutine kills
the test binary. Entry 109's second half is still open.

**3 — a control is silently not applied, or gives a wrong verdict (9).**
8, 31, 44, 49, 59, 73, 97, 114, 128.
The gRPC surface ignores every `queryShim` limit, so a cost control covers half
the traffic (73). The WebSocket read deadline is never armed, so half-dead
clients are never reaped (31).

**4 — the fork's own code (4).**
6, 92, 100, 150.
These carry **no rebase debt at all** — the fork owns the file. Entry 6 is
rising in severity because the fork's own polyglot work surfaced it. Entry 92
is fork code by its own status line. Take these whenever convenient.

**5 — diagnostics, admin surfaces and small defects (18).**
5, 11, 15, 16, 17, 21, 34, 37, 52, 54, 61, 65, 90, 113, 143, 154, 161, 166.
Real, contained, individually small. Entry 34 is five defects in one entry.
Entry 52's first bullet is already fixed and only its second is open. Entry 143
is low alone and high next to 99.

## Suggested order

1. **The config edge (10 entries, ten small patches under one rule).** Best
   value, lowest carry cost. Four done — 53, 64, 121, 125. Six left: 18, 25, 36,
   117, 126, 127, and 18 needs a decision before it needs a patch.
2. **The fork's own code (4 entries).** No rebase debt.
3. **Rank 1 and rank 2 (16 entries).** Wrong answers and crashes.
4. ~~Decide on the closures.~~ **Done** — 14 applied, 4 rejected on review.
5. Ranks 3 to 5 as capacity allows.
