# Conflict minimization for the valve fork

Read-only study, 2026-11-19. Refs: `HEAD` = fork main `78717d23`, `MERGE_HEAD` =
upstream main `b577c289`, merge base `f6e4663c` (last sync, 2026-11-04). All
numbers come from git on those refs. "Fork diff" means `git diff f6e4663c HEAD`
on a file that exists upstream. "Conflict lines" come from re-running
`git merge-file --diff3` on the three blobs, so they do not depend on what the
resolving agents have done since.

## 1. What conflicted, and why

The headline: **one upstream commit caused almost all of this sync.**

- 69 files, 170 hunks. Fork side 4,184 lines, upstream side 2,102 lines.
- Upstream #1335 (`528d990c`, "one chain tracker per network", 408 files,
  +67k/-16k, breaking) touched **68 of the 69** files. Those 68 files hold 93%
  of the conflict lines. The next commit, #1455 (WS newHeads on demand),
  touched 13.
- 5 conflicts are modify/delete: upstream deleted `indexer/indexer.go`,
  `indexer/dedup.go`, `indexer/dedup_test.go`, `erpc/networks_served_tip_refresh.go`
  and `.cursor/rules/erpc.md`. They count 1,240 fork-side lines, but the fork's
  real change in them is only about 200 lines.

So no wrapper scheme would have made this sync clean. The fork's footprint
decides only how much of each upstream rewrite lands on fork lines.

### The footprint

| | files | fork lines |
|---|---|---|
| Fork-added files (no conflict possible) | 375 | +112,780 |
| Upstream files the fork edits | 257 | +11,229 / -3,235 |
| ... of which upstream also changed this sync | 158 | |
| ... of which conflicted | 69 | 6,091 (2,369 in tests) |
| ... upstream did not touch this sync | 99 | 5,693 (next sync's risk) |

The fork already keeps 89% of its code in its own files. The problem is the
other 14,464 lines. Their split by kind:

| kind | files | fork diff | conflict lines this sync |
|---|---|---|---|
| Go, non-test | 101 | +4,682 / -1,413 | 4,169 |
| Go tests | 126 | +6,023 / -1,732 | 4,250 |
| generated TS | 2 | +135 / -19 | 158 |
| docs | 17 | +85 / -41 | 49 |
| other (Makefile, .cursor) | 11 | +304 / -30 | 639 |

Two facts about the non-test Go lines:

- **42% are comments**: 1,900 of 4,504 added lines. Many rewrite upstream's own
  comments, or wrap a one-line upstream change in a paragraph of rationale.
  Every comment line is conflict surface.
- **481 lines are whitespace only** (`diff -w` removes them). The worst case is
  gofmt realignment. In `common/config.go` the fork deleted the blank line
  before `WebSocket` in `ServerConfig`, so gofmt realigned all 28 fields. That
  is conflict hunk 2 of 8 in config.go, for zero behaviour.

### Hot spots, ranked by recurring risk

Risk = fork diff lines x upstream commits that touched the file since
2026-08-01 (289 upstream commits in that window).

| file | fork diff | upstream commits | conflicted now | what the fork carries |
|---|---|---|---|---|
| common/config.go | 776 | 52 | 8 hunks, 122+222 | fork fields, `rejectEmptyListItems` (176), `FailsafeConfigList` refactor (~200 deleted upstream lines), `Copy()` fixes, comment drift, realignment |
| common/json_rpc.go | 438 | 16 | no | canonical-hash rewrite (dbcf226a); 3 open PRs touch it |
| erpc/networks.go | 122 | 47 | 5 hunks, 56+126 | hook calls: F7, requestFormat, `MethodHasNoBlockDependency`, arch normalizer refactor |
| common/defaults.go | 148 | 36 | 1 hunk | F7, requestFormat, WS, #1425 merge fix |
| typescript generated.ts | 109 | 48 | 3 hunks | generated from config.go |
| architecture/evm/evm_state_poller.go | 300 | 17 | 8 hunks, 154+46 | near-tip poll fixes (Nov 6-10) |
| erpc/http_server.go | 190 | 21 | 1 hunk | X-ERPC-Served (55), batch goroutine race fix, marshal-err fix, comments |
| erpc/networks_test.go | 379 | 9 | 21 hunks, 171+70 | removed `time.Sleep`/Bootstrap lines (de-flake), new tests |
| telemetry/metrics.go | 84 | 37 | 1 hunk | WS queue/budget metrics, dedup metric, renamed upstream metric |
| indexer/adapters/wsclient/* | 1,005 | 4 | 18 hunks | WS queues and byte budget, tests |
| health/tracker.go | 114 | 19 | 3 hunks | near-tip poll fixes |
| erpc/ws_server.go + subscription_manager.go | 180 | 10+ | 6 hunks | budget glue, non-eth subscriptions |

## 2. Classification of the fork's changes in upstream files

Upstream PR status: 26 open from valvecitydev, **0 ever merged**, 3 closed.
Upstream-PR is therefore not a reliable way to delete divergence. Per memory,
the owner lets the backlog sit. Plan as if a PR never merges, and shape each
fork change so that it would vanish cleanly if it did.

**(a) Bug fix that belongs upstream.**
- PR'd, open: #1504 (empty list item; the PR puts it in a new
  `common/config_empty_item.go`, but the fork carries it inline in config.go),
  #1425, #1477, #1478/#1482/#1502 (prober), #1490/#1496/#1498 (json_rpc.go),
  #1492/#1508 (state poller), #1506, #1500, #1512, #1514, #1516, #1218, and the
  test de-flakes #1219-#1232.
- Not PR'd, still live upstream: the batch goroutine race in `http_server.go`
  (upstream line 649 still captures `architecture, chainId`), the marshal `err`
  shadowing, `Copy()` aliasing fixes in config.go, the
  `FailsafeConfigList` refactor, the canonical-hash rewrite in json_rpc.go,
  the `time.Sleep` removals in networks_test.go. The bug log lists 107 fixed
  entries against about 20 PRs.

**(b) Superseded by upstream.**
- #1415, #1419, #1420 are merged upstream; take upstream's version.
- The #1335 area: upstream deleted the indexer dedup the fork patched
  (89979d4a, 0669261e, a1d8825d) and replaced it with `indexer/feed.go` and
  `taps.go`. It deleted `networks_served_tip_refresh.go`. It rewrote the state
  poller around one chain tracker. The memory note "fork lacks the chain
  tracker" says near-tip fixes are often moot after it. Candidates to drop
  after their pinning tests run on upstream code: f4bed5f2, da0b0ecd, fc2345a6,
  8ed74617, 7760d43e (`networks_tip_follow.go`), the dedup commits. About 700
  fork diff lines.
- Check, do not assume: upstream's new getLogs empty gate (#1434, #1454, #1456)
  overlaps F7 ("empty eth_getLogs is final only when the upstream has the
  block"). F7 may shrink to `trace_filter` only.

**(c) Fork feature that can leave upstream files.** See section 3.

**(d) Must stay inline.** The hook call sites themselves: one or two lines each
in networks.go, network_executor.go, eth_blockNumber.go, and the redaction call
sites in about 10 files (redaction must wrap every place a URL reaches an error).

## 3. Seams that exist today

| seam | exists? | use for |
|---|---|---|
| New file in the same package | yes, always | every fork function now inlined in a hot file |
| `telemetry.DefineCounter/DefineGauge` registry (`telemetry/manager.go:122`) | yes; any file in any package can define a family at init | all fork metrics |
| `yaml:",inline"` embedded structs | yes; upstream uses it at config.go:744, 1101, 3440 | fork config fields as one embedded struct per extended type |
| `common.RegisterArchitecture` (plain map, overwrite allowed) | yes | a fork decorator around the EVM handler (`HandleUpstreamPreForward`, `HandleNetworkPreForward`) |
| integrity `register(&Check{...})` in `architecture/evm/integrity` | yes, package-private: a new file in that package | a fork integrity check, if F7 can be phrased as one |
| `wsclient.Options` (fork-designed) | yes | budget glue already passes through it |
| valverelay "hosting seam" in `cmd/valve-relay` | yes | billing, relay only; it is not the production eRPC binary |
| config extension keys | **no**; `KnownFields(true)` refuses unknown keys | use env (billing model) or the inline embed |
| link point in `cmd/erpc` | **no clean one** | see below |

**The link-point gap.** A fork package that registers an architecture decorator
or init-time hooks needs an import from the production binary.
`check-fork-isolation.sh` forbids that import outside valve-named directories.
Also, the `Dockerfile` (line 33) builds `./cmd/erpc/main.go` alone, so a second
file such as `cmd/erpc/valve_link.go` would not be compiled. The smallest
upstream hook PR: build `./cmd/erpc` (the package) instead of `main.go`;
upstream's own pprof variant already builds `./cmd/erpc/*.go`. Then allow one
fork file, `cmd/erpc/valve_link.go`, holding only blank imports. Deleting it
plus the valve directories still gives stock eRPC, so the deletability argument
in the isolation script still holds. That is a deliberate rule change, and the
script asks for it to be recorded in `valve/billing-module.md`.

### Concrete moves for (c)

| feature | today | move to | fork lines leaving hot files |
|---|---|---|---|
| `rejectEmptyListItems` | config.go, 176 lines | `common/config_empty_item.go`, the exact shape of PR #1504, plus 3 call lines | ~170 in config.go |
| fork config fields (requestFormat, forwardBlockNumber, emptyResult*, notify*, closeSlowSubscribers, fallbackGasLimit, chainProbeInterval) | spread inline with doc comments, realigning neighbours | one embedded struct per type, e.g. `ValveEvmNetworkFields `yaml:",inline"``, declared with docs, `Copy`, validation and defaults in `common/config_valve.go` | ~150-200 in config.go, ~60 in defaults.go, ~30 in validation.go |
| fork metrics (7 families) | telemetry/metrics.go | `telemetry/metrics_valve.go`, or next to the feature | ~80 |
| X-ERPC-Served batch header | http_server.go, ~55 | `erpc/http_served_header.go`, 1 call line; or PR it, since it extends upstream's own execution headers | ~50 |
| WS budget glue (`notifyBudget`, `queueCap`, Options literal) | subscription_manager.go | `erpc/subscription_budget.go`, 1 call line | ~45 |
| URL redaction helpers | appended 185 lines to upstream `util/redact.go` | `util/redact_urls.go` | 185 (footprint; no conflict this time) |
| requestFormat, forwardBlockNumber | networks.go, eth_blockNumber.go | EVM handler decorator via `RegisterArchitecture`, after the link point exists. **Unverified:** requestFormat stores wire params on the ctx, and the handler API cannot return a ctx, so it would have to store them on the request | ~25; low value until the link point exists |

## 4. Patterns

- **Config fields inline vs fork struct.** Inline fields cost a comment block, a
  gofmt realignment, a `Copy` line, a defaults line and a TS regeneration each.
  An embedded fork struct costs one line per extended type. Verify two things
  before you commit to it: tygo's output for an embedded struct, and that
  #1425's field-by-field `networkDefaults.evm` merge copies the embedded
  struct. If you add an inline field anyway, put it after a blank line so
  gofmt aligns it as its own block.
- **Metrics.** The registry makes `telemetry/metrics.go` optional. Never rename
  an upstream metric in place (the fork replaced
  `websocket_subscription_notifications_dropped_total`); add a new one in a
  fork file.
- **Generated TS.** It is tygo output (`tygo generate`, v0.2.21 per
  release.yml) plus tsc output. Do not merge it by hand. Take upstream's
  version, resolve config.go, then regenerate.
- **Docs.** Small: 17 pages, 49 conflict lines. Fork-only keys belong in a fork
  page under `valve/`, or in the PR that upstreams the key.
- **Tests.** The fork appended 170 new `func Test` to upstream test files
  (`config_analyzer_test.go` +787, `thirdparty/*_test.go` about +1,300,
  `wsclient/adapter_test.go` +574). It also edited upstream tests in place
  (de-flakes). Test files were 1,770 of the 4,184 fork-side conflict lines.
  New tests belong in fork-owned files in the same package, for example
  `erpc/ws_server_valve_test.go`. In-place edits of upstream tests should be
  PR'd or reverted.
- **Comments.** Keep the rationale in the commit message and the bug log. In an
  upstream file, a fork change should carry at most one line of comment that
  points to the bug-log entry.
- **Doc drift.** `check-fork-isolation.sh` and `valve/README.md` say the fork
  rebases. The history shows merges (`3e754f9d`, `39392b73`). The argument is
  the same, but fix the wording.

## 5. Process options, honestly

- **git rerere.** It is already on (`rerere.enabled=true`, 63 entries in
  `rr-cache`). With a merge-based sync it helps little: each sync meets new
  upstream hunks, and rerere replays only identical conflicts. It pays when you
  redo the same merge (an abort and retry, a trial merge, agents re-running a
  file). The agents share one `.git`, so they already share the cache. Cost: a
  wrong recorded resolution replays silently. Review it with
  `git rerere diff` and the test gate.
- **Patch queue** (one branch per feature, rebased on each sync). Gains: each
  conflict belongs to one feature, and a superseded feature is dropped by
  deleting its branch. Costs: history is rewritten every sync, which fights
  the `0.3.0.N-valve` tags and the GitHub main policy. Replaying 140 commits
  re-meets old conflicts that a merge resolved once. It needs a curated,
  squashed queue first (the August squash `8ad1b4cf` + `cdfb10b2` was a step
  that way). I do not recommend it now. The same feature attribution comes
  cheaper from one feature per file.
- **One file per feature in the same package.** This is the cheapest lever with
  the best return. A new file never conflicts textually. Symbol clashes with
  upstream show up as compile errors, which are loud and easy to fix. The
  limit: it removes conflicts only for code that can live behind a call. The
  call line itself stays.
- **Early warning.** A nightly CI job that runs
  `git merge-tree --write-tree --name-only HEAD upstream/main` and lists the
  conflicting files would show a #1335-scale rewrite the day it lands. That
  makes it one focused merge, not a surprise inside a two-week batch. It does
  not reduce the total.

## 6. Recommendations, ranked by conflict lines saved per unit of effort

The savings are estimates from this sync's numbers. A future sync will differ.

1. **Regenerate the TS, never merge it.** Add a sync rule: take upstream's
   `typescript/config/src/generated.ts` and `lib/generated.d.ts`, resolve
   config.go, run tygo and tsc. Saves 157 conflict lines this sync, and these
   files have 48 + 23 upstream commits since August. Effort: minutes.
2. **Take upstream on noise, and stop making more.** In this sync, take
   upstream for every comment-only and whitespace-only hunk (brief rule 6).
   Put new inline fields after a blank line. Keep fork rationale out of
   upstream files. Targets: 481 whitespace-only lines, the stale IndexerConfig
   comment and the ServerConfig realignment (2 of 8 config.go hunks), and the
   comment share of 1,900 added lines. Effort: about an hour, plus the habit.
3. **Drop what #1335 superseded.** Run each pinning test on upstream's new
   code. Delete the fork's dedup, served-tip refresh, tip-follow and near-tip
   poller patches where the test passes. About 700 fork diff lines, and about
   1,200 fork-side conflict lines this sync in evm_state_poller.go,
   health/tracker.go, indexer/* and served_tip*. Effort: this sync's
   resolution already has to do it.
4. **Move fork tests into fork-owned `_test.go` files.** Move the 170 appended
   test functions out of upstream test files. PR or revert the in-place edits
   of upstream tests. This sync had 1,770 fork-side test conflict lines, and
   the footprint is +6,023 in 126 upstream test files. Effort: 1-2 days,
   mechanical.
5. **Empty most of the fork out of `common/config.go`.** It is the top-risk
   file: 52 of 289 upstream commits since August touched it. Carry #1504 in
   its PR shape (`config_empty_item.go`, -170 lines). Revert
   `FailsafeConfigList` to upstream's three shadow unmarshalers, or PR it
   (about 200 lines, 2 of 8 hunks). Move fork fields into embedded
   `yaml:",inline"` structs in `common/config_valve.go` (-150 to -200 lines).
   Target: 776 fork lines down to about 150. Effort: 1-2 days, plus checking
   tygo and the #1425 merge.

After those, in order: move metrics to `telemetry/metrics_valve.go` (84 lines,
37 upstream commits), move the X-ERPC-Served, WS budget and redaction helpers to
same-package files (about 280 lines), then send the one-line Dockerfile hook PR
and add `cmd/erpc/valve_link.go`. That link file opens the architecture
decorator path for requestFormat and forwardBlockNumber. Last, PR the unfiled
live bug fixes (batch race, marshal err, `Copy` aliasing), each in its own file
where it can be. The razor applies throughout: the first four
recommendations delete divergence and add no layer. The only new structure
(embedded config structs, the link file) goes into the one file upstream
churns hardest.
