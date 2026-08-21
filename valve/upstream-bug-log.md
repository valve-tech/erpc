# eRPC upstream bug log

Bugs that live in code the fork inherits from upstream `erpc/erpc`. Each one is
a candidate to report or to send back as a patch.

The fork tracks upstream by rebasing on it. It does NOT send these back as
pull requests — that was considered and ruled out. So every fix here is a patch
that must survive each rebase, or be retired when upstream fixes the same
defect its own way. The 73 entries reading "FIXED in the fork" are therefore a
rebase risk register, not a pull-request backlog.

**Status key.** Every entry carries exactly one of four statuses.

- **`**Status: FIXED in the fork.**`** — the fork carries a fix that upstream
  still needs. The status names the test that pins the fix, or states that no
  test pins it.
- **`**Status:** open.`** — the defect is still in the code. A test may assert
  today's broken behaviour so that a fix breaks the test rather than passing
  unnoticed; the status names that test.
- **`**Status:** not a bug — <reason>.`** — recorded so nobody "fixes" it.
- **`**Status:** unverifiable — <reason>.`** — the claim cannot be decided from
  this tree.

Every entry below was checked against the code on 2026-08-19, not inferred. The
audit corrected the `file:line` citations that had drifted.

**Reading this file.** Four things about its shape are deliberate, and each
one has misled a reader at least once.

- **Entry ids have gaps, and one gap is real work.** 78, 79, 84, 89, 102, 103,
  104, 119, 124, 129, 139, 148 and 149 do not exist and never did — parallel
  sessions reserved numbers and did not all use them. `git log --all -S` finds
  no heading for any of those.
  **150 to 154 were different: they existed only on an unmerged branch**,
  `worktree-agent-a47de169d3033a216` (commit `8761576`, 2026-08-19), together
  with a fuller fix for entry 46 than `main` had. They are merged in now. An
  earlier version of this note said all of 148-154 never existed; that was
  checked with `git log -S` against `main` only, which walks HEAD and cannot
  see an unmerged branch. Use `--all` when asking whether something ever
  existed.
- **Ids are not in order.** Sessions appended wherever they were working. The
  file is searched, not scrolled, so the order was left alone rather than
  renumbering every entry for tidiness.
- **Section headers are weaker evidence than Status lines.** Headers drifted as
  later sessions appended under whichever one was last. Two of them said things
  that had stopped being true. The per-entry Status line is gated by
  `valve/check-bug-log.sh`; the headers are not. Trust the Status line.
- **A test named here may be one this tree no longer has, on purpose.** Several
  entries name a deleted or renamed test in order to say what replaced it —
  read the sentence, not the name. `valve/check-test-citations.sh` lists every
  name the tree cannot resolve, with the line it sits on, so those read as what
  they are. Run it after a rebase, which is when a rename happens. It is a
  report and not a commit hook: on its first run, 2026-08-21, it checked 173
  names, found 167 live and 6 absent, and every one of the 6 was correct prose.
  A hook with that record gets bypassed, and the bypass would take the
  conflict-marker check with it. The count is not pinned here on purpose — a
  correct rename ADDS a name, because the entry then names both the old test
  and the new one.

---

## 1. API keys cannot be revoked or used on a memory or Redis store

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
highest.** Security-adjacent. Pinned by
`TestAdmin_AddedKeyAuthenticatesAndRevokedKeyStopsWorking`.

`erpc/admin.go:370` and `:466` look up a record with
`connector.Get(ctx, data.ConnectorMainIndex, apiKey, "*", nil)`. The range key
is the literal string `"*"`, not a wildcard the connector expands.

The memory connector (`data/memory.go:169`) and the Redis connector
(`data/redis.go:434`) both search for the literal key `"<apiKey>:*"` and miss.
The consumer auth strategy reads with the same wildcard
(`auth/strategy_database.go:181`).

So on an in-memory or Redis auth store, `erpc_addApiKey` writes a record that
update, delete **and authentication** all fail to find. An operator can issue a
key and then neither use it nor revoke it.

**The defect is connector-dependent, which is why it survived.** PostgreSQL
expands the wildcard — `data/postgresql.go:1196-1197` rewrites `*` into a SQL
`LIKE '%'` — so the whole feature works there. Memory and Redis build the
literal key and miss. Anyone testing on Postgres sees nothing wrong.

The record is written at `erpc/admin.go:200` as `Set(ctx, apiKey, userId, …)`,
so the range key is the user id, and the reader does not know it.

Pinned by `TestAdmin_UpdateAndDeleteReachTheRecordOnAStoreWithoutWildcards` and
`TestDatabaseStrategy_AuthenticateReadsTheRecordAtItsCanonicalAddress`, with
`TestDatabaseStrategy_AuthenticateSucceedsWhenRangeKeyMatches` as the control.

---

## 2. `CopyResponseForRequest` deadlocks on an unparsed response

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high.** A hung request never returns. Pinned by
`TestCopyResponseForRequest_UnparsedOriginalCompletes`.

`common/response.go:635` took `resp.RLockWithTrace(ctx)`, a read lock on the
response's `sync.RWMutex`. If the response was not parsed yet, it then called
`resp.JsonRpcResponse(ctx)`, which takes `r.Lock()` on the same non-reentrant
mutex at `common/response.go:331`.

The goroutine blocked forever while holding the read lock.

The comment at `:638` says this path is "common with multiplexed requests", so
the intended case was the one that hung.

**The fix** parses before taking the read lock, and re-reads the parsed pointer
under the lock so `Release()` still cannot free it mid-clone. The critical
section shrank; it never grew. Taking the write lock for the whole operation
would also break the self-deadlock, but it would queue the multiplexed waiters
behind each other and behind every other reader. The fan-out test named below
rejects that variant.

Covered by `TestCopyResponseForRequest_UnparsedOriginalCompletes` and
`TestCopyResponseForRequest_DoesNotSerialiseTheFanOut`, both bounded to 5
seconds: a regression fails fast instead of wedging the run.

---

## 3. No error type satisfies `common.ResponseMetadata`

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** Operators lose diagnostics exactly when they need them. Pinned by
`TestLookupResponseMetadata_FindsTheUpstreamErrorTypes`.

`common/response.go:64` declares `IsObjectNull(ctx ...context.Context) bool`.

`ErrUpstreamRequest.IsObjectNull()` (`common/errors.go:842`) and
`ErrUpstreamsExhausted.IsObjectNull()` (`:1075`) took **no argument**. Neither
type satisfied the interface, although both implemented every other method.

`common.LookupResponseMetadata` therefore returned nil for every error, so
`writeResponseMetadataHeaders` (`erpc/http_server.go:1294`) wrote no
`X-ERPC-Cache` and no `X-ERPC-Upstream` header on an error response. Checked:
`X-ERPC-Attempts` and the retry/hedge counters come from `ExecState`
(`:1262`), not from this interface, so they were never affected.

**The fix** makes both methods variadic and adds the two static assertions the
package lacked, so the signature cannot drift again without a compile error.

One consequence, deliberate: in a batch response the failed items now count
toward `withMeta` (`erpc/http_server.go:1571`) with `FromCache()` false, so a
batch mixing a cache hit with an upstream failure reports `X-ERPC-Cache:
PARTIAL:1` instead of `HIT`. That is the honest answer — the failed item was
not served from cache.

---

## 4. `WithRetryableTowardNetwork` throws the concrete type away

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** The client reads the wrapper instead of the real error. Pinned by
`TestWithRetryableTowardNetwork_ReturnsTheReceiver`.

`common/errors.go:220` returned the embedded `*BaseError`, not the receiver's
concrete type. Twenty-five call sites chain it: twenty-one in production code,
four in tests.

So a gRPC `InvalidArgument`, or a BDS `INVALID_PARAMETER`, reached the HTTP
layer as a bare `*BaseError`. That value implements neither `ErrorStatusCode()`
nor its own identity, so `errors.As` for `*ErrEndpointClientSideException` did
not find it.

The live consumer is `buildErrorResponseBody` (`erpc/http_server.go:1747`): it
calls `errors.As(err, &exe)` to lift an `*ErrEndpointExecutionException` out of
a failed bundle so the client reads the revert rather than the wrapper. The
four EVM paths that mark a revert retryable
(`architecture/evm/error_normalizer.go:268, 289, 375, 406`) all returned a
`*BaseError`, so that lift never fired for them.

Correction to the original report: `ErrorStatusCode()` has **no** production
consumer in this tree — `determineResponseStatusCode`
(`erpc/http_server.go:1629`) keys off `HasErrorCode`, and the `Code` field
survives inside the `*BaseError`. So the status did not flip to 500; the body
selection was the damage. Only tests read `ErrorStatusCode()`.

**The fix** keeps `*BaseError.WithRetryableTowardNetwork` — every error type
stays a `RetryableError`, so the four production `err.(RetryableError)`
assertions still set the flag — and overrides the method on the five concrete
types that are chained with it today, each returning its own receiver. A type added later
still needs its own override; the alternative, unexporting the base method to
force a compile error, would silently turn those dynamic assertions into
no-ops, which is a worse failure than a wrong body.

Classification always worked, because it reads the code rather than the type.

---

## 5. `erpc_listCordoned` hides method-scoped cordons

**Status:** open. **Severity: medium.** The listing reports a false all-clear.

`erpc/admin.go:771` asks each upstream for `CordonedReason("*")` only. An
upstream cordoned for a single method never appears, although the handler's own
comment promises "every upstream currently cordoned in a project".

An operator who cordons `eth_getLogs` during an incident, then runs the listing
to check what is still marked, sees an empty list and concludes the fleet is
clean. The narrow cordon stays in place with nothing pointing at it.

Pinned by `TestAdmin_ListCordonedShowsOnlyWholeUpstreamCordons`.

---

## 6. One provider types its non-EVM upstreams two different ways

**Status:** open. **Severity: medium**, and rising — this is breakage our own
polyglot work surfaced.

`thirdparty/provider.go` builds a base upstream config along two paths. The
fresh path calls `UpstreamConfig.SetDefaults`, which forces `Type = evm` when
the type is empty (`common/defaults.go:1915`). The override path does
`baseCfg = override.Copy()` (line 99) and skips `SetDefaults` entirely.

So `btc:mainnet` comes out `Type=evm`, while `btc:testnet` with a matching
override comes out `Type=""`.

It did not matter while only EVM and SVM existed. With the btc family live, a
provider-generated btc upstream is typed EVM today.

Pinned by
`TestProvider_BuildBaseUpstreamConfig_TypeDefaultingDivergesBetweenItsTwoPaths`.

---

## 7. `"0x"` normalises to the zero hash instead of erroring

**Status:** open. **Severity: medium.** Silent bad cache key.

`util/json_rpc.go` checks `s == ""` **before** stripping the `0x` prefix. So
`"0x"` survives the guard, becomes an empty digit string, and left-pads to 64
zeros.

A caller that passes a client-supplied `blockHash` straight through gets a
valid-looking cache key for a block that does not exist, with no error.
Afterwards `"0x"` and `"0x0"` are indistinguishable.

Pinned by `TestNormalizeBlockHashHexString_BareZeroPrefixBecomesTheZeroHash`.

---

## 8. The config analyzer misparses the chain ID it just fetched

**Status:** open. **Severity: low today, latent.** No caller in this repo.

`erpc/config_analyzer.go:1093` hands the result of `Upstream.EvmGetChainId` to
`common.HexToInt64`. But `EvmGetChainId` returns a **decimal** string —
`upstream/evm_upstream_ops.go:51` formats it with `strconv.FormatUint(dec, 10)`
— and `HexToInt64` demands a `0x` prefix.

A healthy upstream on the **correct** chain therefore fails validation with
`invalid hex string: 123`, and the mismatch check at `:1107` is unreachable.

The same file gets it right on its other path: `GenerateValidationReport` uses
`strconv.ParseInt(chainStr, 0, 0)` at `:398`. Two readers of one value
disagreeing is the tell.

`AnalyseConfig` has no caller in this repo, but it is exported, so a fork or a
future CLI path that calls it refuses to start against a good fleet.

Pinned by `TestValidateUpstreamEndpoints_MisparsesTheChainIdItJustFetched`.

---

## 9. `NewErrUpstreamMalformedResponse` panics on a nil upstream

**Status:** open. **Severity: low.** Latent.

`common/errors.go:895` calls `upstream.Id()` with no nil guard. Its siblings
`NewErrEndpointMissingData` (`:2323`) and `NewErrEndpointContentValidation`
(`:3094`) both guard.

The single call site (`clients/http_json_rpc_client.go:626`) passes a live
upstream today.

---

## 10. `TestIntegrity_Network_ConfigLevelDrivesCorroboration` is flaky

**Status:** open. Upstream's own test, from `178a8f1` (erpc#948).

Fails roughly one run in three when `make test-fast` runs its six shards
concurrently. Passes 20 of 20 alone, and its whole shard passes alone.

Reproduced on the pre-merge tree, so it is not caused by any fork change. Not
yet diagnosed; the setup uses `LockMaxWait: 200ms` and `UpdateMaxWait: 200ms`
over an in-memory connector, so a cross-process lock is not the cause.

**Hypothesis, recorded on 2026-08-21, NOT confirmed.** "In-memory" rules out
lock CONTENTION, not the deadline. A goroutine starved of CPU can miss a 200 ms
wait on an uncontended in-memory lock just as easily. If a shared-state update
expires, one upstream drops out, corroboration has nothing to corroborate
against, and the test is served rpc1's `0x5` instead of the corroborated `0x0`
— which is exactly the assertion that fails.

**Deliberately not acted on.** The fix would be the one applied to 168: delete
the deadline the test is not testing. It was not applied here, for two reasons.
The `200 ms` pair is a copied fixture idiom across eight `erpc` test files, so
changing one is inconsistent and changing all eight is a wide edit made on a
guess. And an attempt to reproduce 168's much better-understood flake failed
under eight CPU burners, so a reproduction of this one is not cheap either.
Confirm the hypothesis first; the entry stays open until someone does.

---

## 11. `guessVendorName`'s multi-level-TLD guard is off by one

**Status:** open. **Severity: medium.** Silently merges per-vendor metrics.

`upstream/upstream.go:1442` reads `if len(rooDomain) < 5`. The comment on the
next line says the guard exists for "multi-level TLDs like co.uk".

`"co.uk"` is exactly 5 characters, so `5 < 5` is false. The guard never fires
for the case it names. `https://rpc.example.co.uk` yields `unknown-co.uk`, and
`*.com.au` (6 characters) yields `unknown-com.au`.

Every distinct vendor under one multi-level TLD therefore collapses into a
single `vendor` label on `erpc_upstream_request_total`,
`erpc_upstream_error_total` and every other per-vendor series. Per-provider
error rates and credit accounting merge with no warning.

The fix is `<= 5`, or dropping the heuristic for a public-suffix list.

---

## 12. Three unreachable defensive branches in `svmFetchGenesisHash`

**Status:** not a bug. **Severity: none today.** Unexercised machinery.
Closed as not a bug on 2026-08-21: the code cannot change what an operator
or a client sees, so there is nothing to repair. It stays here because it
still reads as live code. Deleting it in the fork would cost a permanent
rebase conflict and change nothing — see `valve/open-entry-triage.md`.

`upstream/svm_upstream_ops.go:84-93` has three branches that cannot run: the
`resp.JsonRpcResponse()` error path, `if jrr == nil`, and `if jrr.Error != nil`.
The `Unmarshal` failure below them is now covered by
`TestSvmGenesisGate_RejectsAResultThatIsNotAHash`. `u.Forward` already converts
a JSON-RPC error answer into a returned error, so control never gets past the
earlier `if err != nil`.

Confirmed by coverage: those three blocks stay at 0 while every other line in
the function reaches 1, across all six genesis-gate tests.

Worth reporting because it reads as a safety net and is not one.

---

## 13. Data race in `JsonRpcResponse.WriteTo`

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
highest of the read-path bugs.** Verified under `-race`. Pinned by
`TestJsonRpcResponse_WriteTo_ServesConcurrentClientsWithoutRacing`.

`common/json_rpc.go` `WriteTo` takes `r.errMu.RLock()` — a **read** lock. In the
`else if r.Error != nil` branch it then WRITES the shared field:

```go
r.errBytes, err = SonicCfg.Marshal(r.Error)
```

Two concurrent `WriteTo` calls on a response that carries a typed `Error` and no
`errBytes` race on that field. The race detector reports the write at `:678`
against the read of `len(r.errBytes)` at `:660`.

Multiplexing shares one response across many waiting clients, so this is
reachable in production rather than theoretical.

It was left unpinned at first, because a race test would have failed the
baseline. The fork fixed it, so the test now passes under `-race`.

The fork marshals the error into a LOCAL variable and leaves `r.errBytes`
alone, so `WriteTo` writes no shared field under its read locks.

---

## 14. A prefix in `ignoreFields` makes consensus agree on divergent data

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high.** Silent wrong answer. Pinned by
`TestRemoveFieldsByPaths_TheBroaderPathSubsumesItsOwnExtension`. A mutation
test on 2026-08-19 restored the old builder and reproduced both symptoms.

`common/json_rpc.go` builds a path tree from the ignore list. When one path is
a prefix of another and comes first — `["logs", "logs.*.blockTimestamp"]` — the
builder finds `pathTree["logs"]` already set to `true`, fails the map type
assertion, and keeps writing the remaining segments onto the **root**.

The root then carries a `"*"` entry, and `removeFieldsRecursive` applies it to
every other top-level member.

Two genuinely different responses therefore hash equal, so consensus reports
agreement on data that diverges. Reversing the order of the same two paths gives
a different answer.

The shipped defaults do not hit it. Any operator-written list can.

The consensus-level statement of the same rule is pinned by
`TestCanonicalHashWithIgnoredFields_APathAndItsPrefixHashTheSameEitherWay`.

**The fix.** The builder now treats the broader path as the winner. A leaf
write ends the path, and a path whose ancestor is already removed whole stops
where the ancestor sits instead of walking on. Removing `logs` already removes
everything under it, so `logs.*.blockTimestamp` adds nothing, in either order.

**The tests.** `TestRemoveFieldsByPaths_TheBroaderPathSubsumesItsOwnExtension`
runs both orders and checks that a sibling no path names survives.
`TestCanonicalHashWithIgnoredFields_APathAndItsPrefixHashTheSameEitherWay` says
the same thing at the consensus level: one answer whatever the order, and a
field outside the list still separates two bodies. The old pinning test,
`TestRemoveFieldsByPaths_APrefixPathPoisonsTheRootTree`, is gone — the same
commit replaced it.

**Mutation result (2026-08-19).** With the old builder restored, both tests
fail: the subsumption test loses `receipt.blockTimestamp`, which no ignore path
names, and the consensus test gets two different hashes from the same two paths
in opposite orders. With the fix restored, both pass.

---

## 15. `IsSemiValidJson` corrupts whole log records

**Status:** open. **Severity: medium.** Loses the diagnostics around the error.

`common/utils.go:42` tests only the first byte (`{ [ " n t f`). Callers at
`common/request.go:928` and `common/json_rpc.go:547,558` then splice the value in
with `e.RawJSON`.

A client body of `not json`, or an upstream error body of `failed to connect`,
passes the check. The **entire zerolog record** becomes unparseable, so a log
pipeline drops it — losing `lastUpstream`, `networkId` and the error itself
alongside the bad field.

Pinned by
`TestNormalizedRequest_MarshalZerologObject_QuotesABodyThatIsPlainlyNotJson`.

---

## 16. `TranslateToJsonRpcException` throws away the method-ignored detail

**Status:** open. **Severity: low.** Unhelpful client error.

`common/json_rpc.go:1654-1668`. `NewErrUpstreamMethodIgnored` stores `method`
and `upstreamId` in `Details`, but the translation passes `nil` details and
prefixes the deepest message with a phrase the message already contains.

The client reads `"method ignored by upstream: method ignored by upstream
configuration"` and never learns which method, or which upstream.

---

## 17. Two marshallers drop fields their YAML twins keep

**Status:** open. **Severity: low.** Misleading admin output.

- `ProviderConfig.MarshalJSON` (`common/config.go:1005-1014`) omits
  `ignoreNetworks`; `MarshalYAML` at `:1016` includes it. An operator comparing
  the JSON and YAML admin dumps sees a network exclusion in one and not the
  other.
- `SecretStrategyConfig.MarshalJSON` (`:3082-3086`) emits only
  `{"value":"REDACTED"}`, dropping `id` and `rateLimitBudget`. In a JSON dump
  every secret strategy is indistinguishable from every other one. `MarshalYAML`
  keeps both.

---

## 18. Auth strategy type inference is last-block-wins, silently

**Status:** open. **Severity: low, and uncertain — may be intended.**

`common/defaults.go:3135-3185`. `AuthStrategyConfig.SetDefaults` overwrites a
declared `type` when a sub-block for another type is also present, with the last
block in source order winning.

A leftover `secret:` block above a new `jwt:` block changes which strategy
authenticates, with no warning.

This may be deliberate inference. If so, validation is the natural place to
reject the ambiguous shape rather than silently pick one.

Pinned by `TestAuthStrategyConfig_SetDefaults_TheLastBlockWinsWhenSeveralArePresent`.

---

## 19. A wrapped nonce exception disables idempotency and re-broadcasts a transaction

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high.** Pinned by
`TestUpstreamPostForward_ethSendRawTransaction/AWrappedNonceExceptionReachesTheIdempotencyPath`.

Fixed together with 35, which is the same root cause. See "Fixes the fork
already carries" below. The pinning test is now
`TestUpstreamPostForward_ethSendRawTransaction/AWrappedNonceExceptionReachesTheIdempotencyPath`.

`architecture/evm/eth_sendRawTransaction.go:80` gates the idempotency path with
`common.HasErrorCode(re, ErrCodeEndpointNonceException)`.

`HasErrorCode` (`common/errors.go:2597`) type-asserts `StandardError` and
`*BaseError`, and walks `Unwrap() []error`. It does **not** walk a plain
`fmt.Errorf("%w", …)` chain.

The `errors.As` seven lines below it, at `:87`, **does** walk that chain.

So any layer that wraps an `ErrEndpointNonceException` with `%w` fails the first
gate, returns early, and the caller re-broadcasts a transaction that is already
in the mempool. The two checks disagree about what counts as the same error.

Reordering the two checks would also fix it, but the fork repaired
`HasErrorCode` instead, so every consumer of that walk gains the same fix. See
E in "Fixes the fork already carries".

---

## 20. `binarySearchEarliest` reports the tip as the earliest retained block

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high.** Two defects that compound. Pinned by
`TestBinarySearchEarliest/NothingAvailableIsAnError` and
`TestBinarySearchEarliest/UnsupportedProbeIsReportedAsUnsupported`.

`architecture/evm/evm_state_poller.go:1770-1847`.

**20a.** The loop narrows `[l, r]` and returns `l` without ever probing the
final value. If no block in the range answers, `l` converges on `high`, and the
current tip is recorded as the earliest block the node retains. That is the most
restrictive possible bound, not fail-open.

**20b.** `checkProbe` returns `(ok, unsupported, err)` and the search discards
`unsupported` entirely (now handled at `:1783`). A node with no tracing engine answers `-32601`
to every trace method at every height, so `probe: traceData` walks about log2(N)
requests and then declares earliest = latest — indistinguishable from a node
that pruned its whole history.

An operator who configures `blockAvailability.lower` therefore sees a node that
answers nothing recorded as retaining history from the tip upward.

**20c.** None of the four probe implementations ever returns a non-nil error —
each folds transport and JSON-RPC failures into `(false, …, nil)`. The
`checkProbe` pass-through error therefore still cannot fire. The error logging
in `PollEarliestBlockNumber` (`:940`) does fire now, because the search itself
returns `errEarliestProbeUnsupported` and `errEarliestBlockUnavailable`.

`TestBinarySearchEarliest/EarliestEqualToHighIsStillFound` pins the one case
the post-loop probe rescues: a node that retains exactly the tip.

---

## 21. `GetDiagnostics` reports a deliberate opt-out as a failure

**Status:** open. **Severity: low.** False alarm on the admin surface.

`architecture/evm/evm_state_poller.go:1266` sets `diag.SkipSyncingCheck` from
the operator's own config. Line `:1270` then folds that flag into
`skipSyncingCheck`, and `:1279` emits *"syncing check disabled after consecutive
failures (method may not be supported)"*.

An operator who sets `skipSyncingCheck: true` on purpose reads a diagnostic
telling them their node may not support `eth_syncing`.

The latest and finalized equivalents are correct: they read the poller's own
`skipXCheck`, not a configured one.

---

## 22. Dead branches in the EVM block-number path

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
none.** Both dead branches are gone. `BuildGetBlockByNumberRequest` now converts
a `float64` block height to hex itself, and rejects a fraction, a negative
height and a value past 2^53. The unreachable `low < 0` clamp in
`binarySearchEarliest` was deleted. Pinned by
`TestBuildGetBlockByNumberRequest/NormalizesAJsonDecodedNumberToHex`.

- `architecture/evm/eth_getBlockByNumber.go:32` — `BuildGetBlockByNumberRequest`
  lists `float64` in its type switch, but `common.NormalizeHex` does not handle
  `float64`, so that branch can only produce "invalid block number or tag".
  `float64` is exactly the type a JSON-decoded number arrives as, so the case
  looks deliberate.
- `evm_state_poller.go:1715` — `if low < 0 { low = 0 }` has no observable
  effect. Every caller passes `low = 0`, and with a negative `low` the midpoints
  stay non-negative, so the search converges identically and sends identical
  requests.

---

## 23. A second load-triggered flaky test, in `architecture/evm`

**Status: FIXED in the fork.** Upstream still carries it. The cause was in the
poller, not the test. `SuggestFinalizedBlock` no longer takes
`finalizedUpdateInProgress` on its common path — the mutex is deleted — so a
suggestion that arrives while a background update runs is no longer dropped.
Only a MAJOR forward jump goes to the background chain-identity check, and that
path logs the one drop it can still make. Commit 47d863f made the change.
`go test ./architecture/evm/ -run TestSuggestFinalizedBlock -count=15 -race`
passes. Pinned by
`TestSuggestFinalizedBlock_SmallAdvanceSurvivesAMajorJumpVerification`.

`architecture/evm/evm_state_poller_suggest_gate_test.go:226` and `:240` each
wait 2s on `SuggestFinalizedBlock`.

The mechanism is `evm_state_poller.go:825`: `SuggestFinalizedBlock` does
`if !e.finalizedUpdateInProgress.TryLock() { return }` and **silently drops**
the suggestion while an update is in flight. The value becomes visible inside
the goroutine BEFORE its `defer Unlock()` runs, so the first
`require.Eventually` can return while the mutex is still held. The second
suggestion is then dropped and never retried.

Reproduced on a clean tree with
`go test ./architecture/evm/ -count=1 -parallel 8 -race`, failing
`TestSuggestFinalizedBlock_MajorJumpChainIdMismatchDroppedAndCordoned`.

**That last sentence used to read** "the drop-on-contention is deliberate in
production — the next poll re-observes — so the fix belongs in the test, not the
poller." It survived the fix and contradicted this entry's own status, which
says the cause was in the poller. The status is right: `47d863f` changed the
poller, not the test. Left here as the correction rather than deleted, because
the reasoning was wrong in an instructive way — "the next poll re-observes"
assumed a next poll always comes.

Compare bug 10: that is another flaky test whose cause is a test racing a
background poller, and it is still open.

---

## 24. `PostgreSQLConnector.List` never issues a pagination token

**Status:** open. **Severity: medium.** A caller believes it enumerated
everything.

`data/postgresql.go:1321`. The query at `:1368` asks for `limit+1` rows so it
can detect a next page. The scan loop at `:1379` calls `rows.Next()` a
`limit+1`-th time and breaks at `:1381` **without recording that row** — so the
extra row is already consumed. The probe at `:1411` then calls `rows.Next()`
again, which would need a `limit+2`-th row the query never fetched. It always
returns false, and `nextToken` stays empty.

Measured against a live container: 30 rows, `limit 1` returns 1 row and an empty
token; `limit 5` returns 5 rows and an empty token.

`erpc_listApiKeys` (`erpc/admin.go:270`) on PostgreSQL therefore returns
at most one page and always reports "no more pages". A caller looping until the
token is empty stops after one page believing it saw everything.

Pinned by `TestPostgreSQLConnector_CRUD/List never issues a pagination token
(defect pinned)`.

---

## 25. A negative `statePollInterval` panics eRPC at startup

**Status:** open. **Severity: medium.** Crash from a config typo, with no
validation error.

`data/dynamodb.go:723` calls `time.NewTicker(d.statePollInterval)` with no
guard, and `time.NewTicker` panics on any non-positive duration.

`common/defaults.go:1332` substitutes a default only when the value is exactly
`0`, so a negative duration in the operator's YAML survives config load
untouched.

`statePollInterval: -1s` under a DynamoDB shared-state connector therefore
panics the process — "non-positive interval for NewTicker" — the first time any
shared counter is watched, which is during startup. No validation error names
the field.

Pinned by `TestDynamoDBConnector_WatchCounterNonPositivePollInterval`.

---

## 26. Watch cleanup can panic with send-on-closed-channel

**Status:** open. **Severity: medium.** The PostgreSQL goroutine leak is now
pinned. The panic itself is not, and cannot be: a panic in a background
goroutine kills the test binary, so a test that triggers it fails the whole
package instead of recording anything. The PostgreSQL watch teardown has since
been restructured — `cleanup` now calls `releaseWatcher`, which closes the
channel under `listener.mu` — but the fallback poller still sends outside that
lock, so both the race and the leak survive.

- **DynamoDB** (`data/dynamodb.go:757-759`): `cleanup` runs `close(done)` then
  `close(updates)`. The polling goroutine can be inside `getSimpleValue` — a
  network round trip — when that happens. On return it executes
  `select { case updates <- st: default: }` at `:743-746`. **A send on a closed
  channel panics even inside a select with a default.** The agent confirmed
  that with a standalone program.
- **PostgreSQL** (`data/postgresql.go:672-690`, `:692-704`): the same race, plus
  a goroutine leak. The 30-second fallback poller exits only on `ctx.Done()`.
  `cleanup` calls `ticker.Stop()` then `releaseWatcher`, which closes `updates`
  at `:920`; `Stop`
  neither stops the goroutine nor drains a tick already delivered, so the poller
  can wake after the close and send at `:682`. One goroutine leaks per watch
  stopped without cancelling its context.

Cancelling a shared-state watch — an upstream removed, a config reload, a
network torn down — can therefore crash the process.

The LISTEN broadcast path at `:1016-1023` is safe: `cleanup` removes the watcher
under `listener.mu` before closing.

The PostgreSQL goroutine leak is pinned by
`TestPostgreSQLConnector_WatchCleanupLeaksItsFallbackPoller`: it starts 25
watches on one key, stops all 25, and asserts the goroutine count stays up by
25. Fixing the leak makes the test fail, which is the point.

---

## 27. Redis reverse-index TTL comparison is dead code

**Status: FIXED in the fork.** Upstream still carries it. **Severity was: none
today.** The two sentinels are now named constants in the units go-redis really
returns: `redisTTLKeyMissing = time.Duration(-2)` and `redisTTLNoExpiry =
time.Duration(-1)` (`data/redis.go:31-32`). Both branches now run. Pinned by
`TestRedisReverseIndexTTL_DetectsAnExpiredTarget`.

`data/redis.go:417` and `:423` compare a TTL against `-2` and `-1` seconds.

go-redis v9.22.0's `DurationCmd.readReply` (`command.go:1630-1642`) returns
`time.Duration(n)` for those sentinels — that is **−2 nanoseconds** and −1
nanosecond, not −2s and −1s. Both branches are unreachable.

The cost today is one wasted `GET`, because the fallthrough returns the same
`ErrRecordNotFound`. But the comment claims the branch handles an expired
reverse-index target, and a future change relying on it would silently not run.

---

## 28. An inverted condition drops the caller's request id over HTTP

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high.** One character: `err != nil` became `err == nil` at
`erpc/http_server.go:580`. Pinned by
`TestHttpServer_ABlockedMethodEchoesTheCallersIdOverHttp` and a batch case
proving each entry gets its own id back.

`erpc/http_server.go:580`:

```go
if jrr, err := nq.JsonRpcRequest(); err != nil {
    jsonrpcVersion = jrr.JSONRPC
    reqId = jrr.ID
}
```

The id is copied only when the lookup **failed** — and on failure
`JsonRpcRequest` returns `(nil, err)`, so that body would nil-dereference if it
ever ran. It never runs, because `Validate()` at `:518` already cached the
parse. So `reqId` silently stays nil.

This is the **only** site in the repository with the inverted test.
`ws_server.go:362`, `ws_server.go:501` and `common/tracing_util.go:178` all use
`err == nil`, and the adjacent admin block at `:653` uses `if jrr != nil`.

Any method blocked by `ignoreMethods` or `allowMethods` therefore answers
`{"jsonrpc":"2.0","id":null,"error":{…}}` over HTTP. Every JSON-RPC client pairs
answers to calls by id, so the caller cannot match the refusal and waits out its
own timeout. In a batch, nothing matches at all.

The same project config behaves differently over HTTP and over WebSocket.

---

## 29. A truncated request body answers HTTP 200 and blames the server

**Status:** open. **Severity: medium.**

`erpc/http_server.go:412` with `:1881`. `util.ReadAll` fails on a truncated body
— a gzip stream that ends early, for instance. The raw error reaches
`handleErrorResponse`, matches none of its error-code cases, and falls through
to the default `http.StatusOK` with a `-32603` server-fault body.

The sibling case one branch above (`:390-405`), a body whose gzip **header**
does not parse, is wrapped in `common.NewErrInvalidRequest` and correctly
answers 400.

So two malformed uploads get two different verdicts. The truncated one reads as
success to every proxy, CDN and dashboard in front of eRPC, and a client that
retries on server faults retries an upload that can never succeed.

---

## 30. RFC 7239 `Forwarded` is unsupported, and fails silently

**Status: FIXED in the fork.** Upstream still carries it. **Severity: medium.** Every client collapsed into one
bucket. The fix, the parser it replaced both with, and the tests are recorded
under 133.

`erpc/http_server.go:2391` defines `parseForwardedFor`, which parses the
standardised `Forwarded` header. **Nothing calls it.**

`resolveRealClientIP` at `:2329` parses every configured `trustedIPHeaders`
entry with `parseXForwardedFor`, which expects a bare comma-separated IP list.

An operator who configures `trustedIPHeaders: ["Forwarded"]` gets nothing out of
it: `for=203.0.113.7` fails `net.ParseIP`, the chain comes back empty, and the
request falls back to the proxy's own address. Every client behind that edge
then shares one rate-limit bucket, one IP in metrics, and one IP in logs — with
no error anywhere.

Fix: wire it up, or delete it and document that only XFF-style headers work.

---

## 31. The WebSocket read deadline is never armed

**Status:** open. **Severity: medium.** Half-dead clients are never reaped.

`erpc/ws_server.go:104-106`. The pong handler sets
`SetReadDeadline(now + pingInterval*2)`. That is the **only** `SetReadDeadline`
call in the file, and nothing arms a deadline when the connection opens.

A client that never sends a single pong therefore never receives a deadline.

A half-dead client — socket open, no longer reading, no longer answering pings —
holds its connection and its subscription fan-out membership indefinitely. The
ping `WriteControl` cannot rescue it either: at the default 30-second interval
it writes a 2-byte frame, so filling the socket buffer to trigger a write
failure takes effectively forever. Connection counts grow and
`"websocket read ended"` never logs for those connections.

Fix: call `SetReadDeadline(now + pingInterval*2)` once before the read loop.

---

## 32. The gRPC surface silently ignores three declared wire fields

**Status: FIXED in the fork.** Upstream still carries it. **Severity: high for the first.** Silent wrong
answers.

- **`GetLogs` ignored `blockHash`** (`erpc/grpc_server.go`).
  `evm.GetLogsRequest` defines `blockHash` as the alternative to
  `fromBlock`/`toBlock`. The handler never read it, so a client filtering by
  block hash sent an empty `{}` filter and the upstream applied its own default
  range — the latest block on most clients. The operator saw a 200 and a
  well-formed log list **for the wrong block**.
- **Every handler ignored `chainId` and `chainGenesisHash`**
  (`erpc/grpc_server.go`). Those fields exist on every BDS request so a
  client can pin the chain it expects. eRPC took the chain only from the
  `x-erpc-chain-id` metadata, so a client sending `chainId: 1` in the body with
  `x-erpc-chain-id: 137` in the metadata received Polygon data without
  complaint.

**One answer per field, and no third state.**

- `blockHash` is **honoured**. `GetLogs` puts it in the eth_getLogs filter. A
  request that sets `blockHash` AND a `fromBlock`/`toBlock` range is a
  contradiction the wire itself forbids, so the handler returns
  `InvalidArgument` instead of serving one half of it.
- `chainId` is **honoured**. It names the chain when the metadata omits it, and
  a value that contradicts `x-erpc-chain-id` returns `InvalidArgument` naming
  both numbers. The comparison uses the decimal string the router builds the
  network id from, so the check asks exactly the question that matters: will
  eRPC route to the chain you pinned?
- `chainGenesisHash` is **refused**. eRPC selects a network by chain id alone
  and holds no genesis hash for an EVM network, so it cannot honour the pin.
  The handler returns `Unimplemented` naming the field. An explicit refusal is
  the honest answer: the client learns the pin did nothing.

The check is one function, `pinnedChain`, reading the two fields off the
message descriptor by name. It is not a table of request types. Every BDS
request message that declares `chainId` or `chainGenesisHash` is covered,
including the request type nobody has written yet, and `extractRequestInput`
runs it for all thirteen handlers — seven unary RPCs, five query streams and
the block stream.

Pinned by `erpc/grpc_server_chain_pin_test.go`:
`TestGrpcGetLogs_SendsTheBlockHashFilter`,
`TestGrpcGetLogs_RejectsABlockHashTogetherWithARange`,
`TestGrpcChainPin_RejectsAChainIdTheMetadataContradicts`,
`TestGrpcChainPin_TakesTheChainFromTheRequestWhenTheMetadataOmitsIt`,
`TestGrpcChainPin_AcceptsAChainIdThatAgreesWithTheMetadata` and
`TestGrpcChainPin_RefusesAChainGenesisHashItCannotHonour`. The two chainId
tables each run over GetLogs, GetBlockByNumber, QueryBlocks and StreamBlocks,
and the genesis table runs over the three of those whose message declares the
field. They prove the check sits on the shared path and not on one handler.

Mutation: deleting the `blockHash` block fails the two GetLogs tests; deleting
the genesis refusal fails all three subtests of the refusal table; deleting the
chainId branch fails all eight subtests of the two chainId tables.

No test pinned the silent-ignore behaviour for these three fields. One test did
pin the same defect on a fourth field — see entry 165.

---

## 33. The two streaming services label the same failure differently

**Status:** open. **Severity: low.** Clients cannot retry on status code.

`erpc/grpc_server.go:432-480` return the processor's error raw, so gRPC labels
it `codes.Unknown`. `erpc/grpc_server.go:487` wraps it with `mapToGRPCStatus`,
so the same error reaches a `StreamBlocks` client as `codes.Internal`.

One line per handler fixes it.

---

## 34. Small, low-severity, easy

**Status:** open. All five parts are still present, and they agree. The
refusal paths still pass the nil outer `err`, the marshal call still shadows
it, the typo still ships, the guard still contradicts the next line, and
`writeMessage` still has no production caller. Only the last part moved: a
test now calls it, so it is dead in production but no longer unexercised.

- `erpc/http_server.go:689` and `:724` — refusal paths call
  `common.EndRequestSpan(requestCtx, nil, err)` where the outer `err` is nil, so
  "admin is not enabled" and "architecture and chain must be provided" record as
  **successful** spans. Span-error-rate dashboards under-report them.
- `erpc/http_server.go:871-873` — `msg, err := common.SonicCfg.Marshal(err.Error())`
  shadows the error parameter, so the retry on the next line marshals the
  *marshal* error rather than the original failure. Theoretical, since
  marshalling a Go string essentially cannot fail, but the shadowing hides it.
- `erpc/http_server.go:722` — user-facing typo, `"configureed via domain aliasing"`.
- `erpc/ws_server.go:581` — `writeMessage` is dead code.
- `erpc/grpc_server.go:72` guards `if cfg != nil`, then `:103-104` and `:148`
  dereference the same config unconditionally. Unreachable after `SetDefaults`,
  but the guard states an invariant the next line breaks.

---

## 35. `HasErrorCode` does not follow a single `Unwrap() error`

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** Found independently by two agents, and the fix repaired a third
consumer nobody was looking at — see below. **Severity was understated.** Pinned
by `TestHasErrorCode_FollowsAPlainWrapChain`.

`common/errors.go:2597-2633` handles `StandardError`, `*BaseError` and
`Unwrap() []error` — but not the `Unwrap() error` that `fmt.Errorf("%w", …)`
produces.

Any eRPC error wrapped that way loses its code. Two separate consequences were
found from opposite ends of the codebase:

- The nonce-exception gate at `architecture/evm/eth_sendRawTransaction.go:80`
  fails, so idempotency is skipped and a transaction is re-broadcast (see 19).
- `mapToGRPCStatus` degrades the error to `codes.Internal`.

That two agents reached the same root cause from unrelated directions is the
strongest signal in this log. Fixing `HasErrorCode` closes both.

---

## 36. An unrelated `directiveDefaults` key cancels the legacy integrity migration

**Status:** open. **Severity: medium.** An upgrade-path regression that turns
data-integrity enforcement back on without saying so.

The root cause is a `nil` check standing in for "the user did not specify this",
defeated by an earlier defaulting pass.

Order of events inside `ProjectConfig.SetDefaults`:

1. `NetworkDefaults.SetDefaults` runs first (`common/defaults.go:1341`).
2. It calls `DirectiveDefaultsConfig.SetDefaults` (`:1714`), which fills
   `EnforceHighestBlock`, `EnforceGetLogsBlockRange` and
   `EnforceNonNullTaggedBlocks` with `true` when they are nil (`:1728`).
3. Each network copies that whole block (`:2142`).
4. The legacy migration at `:2291` writes only into a field that is **still
   nil** — and finds `true` already there. It does nothing.

Its own comment says *"Only migrate if the user hasn't explicitly set the new
directive"*. But the guard cannot tell *the user set it* from *the defaults set
it*.

An operator writes `networkDefaults.evm.integrity.enforceHighestBlock: false`
and it works. Then they add any unrelated key under
`networkDefaults.directiveDefaults` — `retryEmpty: false`, say — and
enforcement silently stays ON. The only visible symptom is requests rejected for
a highest-block violation.

The same config at the **per-network** level works correctly, because there the
migration runs before the defaults pass. So the two levels disagree.

Pinned by
`TestNetworkDefaults_ADirectiveDefaultsBlockCancelsTheLegacyIntegrityMigration`.

---

## 37. A converted shorthand upstream gets an invented override id

**Status:** open. **Severity: low.** Cosmetic today.

`convertUpstreamToProvider` clears the override's id (`common/defaults.go:1456`)
and then calls `cfg.SetDefaults(upstream)` (`:1475`), which reaches
`UpstreamConfig.SetDefaults` on that override. With an empty endpoint,
`url.Parse("")` gives an empty scheme, so `:1907` synthesises `"" + "-" +
<counter>` — an id like `-8`, dependent on a process-global counter.

The id shows up in a config dump. Routing is unaffected, because
`thirdparty/provider.go:120` overwrites `baseCfg.Id` from `UpstreamIdTemplate`
before use, and nothing resolves overrides by id.

`SetDefaults` should not invent an id when there is no endpoint to derive one
from.

---

## 38. Inverted numeric ranges make error classification dead code

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high for Alchemy.** Both bound pairs now run the right way round, so the
branches match integers again. The body below describes the pre-fix
expressions. Pinned by
`TestAlchemyVendor_ApplicationDefinedBands_AreClientSideAndStopAtTheNetwork`
and
`TestConduitVendor_GetVendorSpecificErrorIfAny_TheServerErrorBandIsServerSide`.

Two vendors compare an error code against a range whose bounds are the wrong way
round, so **no integer can satisfy the condition**. Verified by evaluating the
predicate over the whole code space.

- **`thirdparty/alchemy.go:504`** — three empty ranges chained in one `else if`:
  `code >= -32099 && code <= -32599 || code >= -32603 && code <= -32699 ||
  code >= -32701 && code <= -32768`. The whole branch is dead, so Alchemy
  client-side faults — invalid params, method not found, parse errors — never
  receive `.WithRetryableTowardNetwork(false)`.

  An operator observes eRPC retrying a deterministic failure against every
  remaining upstream in the network. Quota and latency spent on a request that
  cannot succeed anywhere. The comment directly below cites Alchemy's own
  error-code reference, so the intent is unambiguous.

- **`thirdparty/conduit.go:194`** — `code >= -32000 && code <= -32099`. Every
  Conduit error in the standard server-error band falls through to the generic
  normaliser instead of `ErrEndpointServerSideException`.

Both are one-line fixes: swap the bounds.

---

## 39. A filter that matches nothing becomes a permanent "not yet populated"

**Status:** open. **Severity: medium.** A stuck state reported as transient.

`thirdparty/remote_cache.go:99`. `chainstack.fetchNodes` and
`quicknode.fetchEndpoints` both declare `var allX []*T` and return it, so a
filter matching nothing returns `nil`.

The cache stores that `nil` and `Lookup` reports it **fresh** — correctly, the
fetch succeeded. But every vendor's `resolve*` helper branches on `value == nil`
to mean *cold start*.

So an operator whose Chainstack project or QuickNode filter matches no nodes
sees `vendor remote-data cache not yet populated; retry shortly` for the life of
the process. The message says transient; the state is permanent. It never says
"your filter matched nothing".

The map-returning vendors escape this only because they allocate with `make`.

---

## 40. `EnsureFresh` has no callers, and disagrees with the pattern it documents

**Status:** not a bug. **Severity: low.** Unexercised machinery. Closed as
not a bug on 2026-08-21: the code cannot change what an operator or a client
sees, so there is nothing to repair. It stays here because it still reads as
live code. Deleting it in the fork would cost a permanent rebase conflict
and change nothing — see `valve/open-entry-triage.md`.

`thirdparty/remote_cache.go:252`. `EnsureFresh` is documented as "the canonical
hot-path call", but all eight vendors open-code `Lookup` +
`TriggerAsyncRefresh` + a `== nil` check instead.

It also diverges from that convention: it keys on `Has`, so a published `nil`
reads as usable where the open-coded version treats it as a cold start. And it
races — a refresh can publish between its `Lookup` and its `Has`, so it returns
the pre-refresh zero value with `usable == true`.

Either adopt it everywhere or delete it. A helper that contradicts the pattern
it claims to standardise is worse than no helper.

---

## 41. Two vendors panic on a config the others reject politely

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** Bootstrap crash instead of a config error. Pinned by
`TestInfuraAndLlama_GenerateConfigs_AMissingEvmBlockIsAConfigError`.

`thirdparty/infura.go:84` and `thirdparty/llama.go:57` read
`upstream.Evm.ChainId` with no nil check.

Ankr, BlastAPI, BlockPi and Conduit all guard first and return
`"...requires upstream.evm to be defined"`.

So an operator who configures an Infura or Llama provider without an `evm` block
gets a panic at bootstrap rather than a message naming the missing field.

`TestInfuraAndLlama_GenerateConfigs_AMissingEvmBlockIsAConfigError` requires
each vendor to name the missing field instead of panicking.

---

## 42. Three smaller `thirdparty` defects

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
low.** All three defects are repaired. `fetchChainIDs` returns nothing, so
no caller guards a dead error and every probe failure reaches the logger.
`fetchNodes` closes each page body before it requests the next page. The
QuickNode normaliser keeps the caller's `details`. Pinned by
`TestFetchChainIDs_ReportsEveryFailureOnItsOwnLogger`,
`TestChainstackFetchNodes_ReleasesEachPageBeforeTheNextRequest` and
`TestQuicknodeVendor_GetVendorSpecificErrorIfAny_CarriesTheCallersDetails`.

- **`thirdparty/chainstack.go:331` and `thirdparty/quicknode.go:474`** —
  `fetchChainIDs` collects per-node errors, logs them, then always returns
  `nil`. The callers at `chainstack.go:121` and `quicknode.go:380` guard with
  `if err != nil { logger.Warn()… }`, so that warning is dead. Nodes silently
  keep chain ID 0 and drop out of routing with no signal at the caller.
- **`thirdparty/chainstack.go:281`** — `defer resp.Body.Close()` sits inside the
  pagination loop, so every page's body stays open until `fetchNodes` returns.
  On a large account that holds one connection per page for the whole walk.
- **`thirdparty/quicknode.go:566`** — the normaliser shadows its own `details`
  parameter with `var details map[string]interface{} = make(...)`. The caller at
  `architecture/evm/error_normalizer.go:22` fills `details` with `statusCode`
  and the response headers, and none of it reaches a QuickNode error. QuickNode
  is the only vendor that does this.

---

## 43. The missing-`evm` panic is six more vendors, not two

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** Bootstrap crash instead of a config error. Pinned by
`TestSixVendors_GenerateConfigs_AMissingEvmBlockIsAConfigError`.

Entry 41 named Infura and Llama. A sweep of every `upstream.Evm` dereference in
`thirdparty/` finds six more `GenerateConfigs` that read `upstream.Evm.ChainId`
with no nil check, against eighteen that guard first:

- `thirdparty/envio.go:223`
- `thirdparty/erpc.go:114` and `thirdparty/erpc.go:139`
- `thirdparty/etherspot.go:97`
- `thirdparty/pimlico.go:176`
- `thirdparty/routemesh.go:116`
- `thirdparty/thirdweb.go:100`

An operator who configures any of these six without an `evm` block crashes the
process at bootstrap instead of reading which field is missing. eRPC's own
vendor is the worst of the six: `erpc.go:114` sits on the preset-endpoint path,
which is the normal way to configure it.

**The fix does not copy the same nil check into six more vendors.** Every
`upstream.Evm` read in `thirdparty/` asks for the same field: `ChainId`. So
the fix deletes the nil case instead of branching on it.

- `UpstreamConfig.EvmChainId()` (`common/vendors.go`) reports the configured
  chain id, and zero when there is no `evm` block. The six vendors read the
  chain id through it. Four of them (envio, etherspot, pimlico, routemesh)
  already return "requires upstream.evm.chainId to be defined" for a zero, so
  they gained no branch at all. Two (erpc, thirdweb) fed the chain id straight
  into a URL builder, which would have produced `.../0`; they now return the
  same message.
- `common.GenerateVendorConfigs` wraps the call and converts a panic into an
  error naming the vendor. Both call sites use it —
  `thirdparty/provider.go:59` and `upstream/upstream.go:285` — so the ninth
  vendor, the one nobody has written yet, reports a config error instead of
  killing the process at bootstrap. It reads `Vendor.Name()` inside the
  guarded scope, so a vendor that panics there is covered too.

Pinned by `TestSixVendors_GenerateConfigs_AMissingEvmBlockIsAConfigError`
(`thirdparty/vendor_configs_test.go`),
`TestGenerateVendorConfigs_AVendorPanicBecomesAConfigError` and
`TestUpstreamConfig_EvmChainId_ReportsZeroWhenThereIsNoEvmBlock`
(`common/vendors_test.go`), and
`TestProvider_GenerateUpstreamConfigs_AVendorPanicBecomesAnError`
(`thirdparty/provider_vendor_panic_test.go`), which drives the real provider
bootstrap path. Mutation: reverting the six vendors fails all seven subtests
with a nil dereference; removing the `recover` fails the boundary test;
reverting `provider.go` fails the provider test.

See entry 82 for what it takes to reach the panic from a config file.

---

## 44. Etherspot builds an endpoint with no host for an unknown chain

**Status:** open. **Severity: medium.** A silent bad upstream, not a config
error.

`thirdparty/etherspot.go:126`. `generateUrl` declares `var etherspotURL string`
and fills it only inside two `if` arms — one for a mainnet, one for a testnet. A
chain in neither table leaves it empty. Nothing checks that, so the function
formats `"?apikey=<key>"` and `url.Parse` accepts it without complaint.

`GenerateConfigs` then returns success with `Endpoint: "?apikey=<key>"`. Every
other vendor reports `"unsupported network chain ID for <vendor>: <id>"` here.

An operator who names a chain Etherspot does not serve gets a registered
upstream that can never connect, with no message saying why. Worse, the key is
still in the string, so it reaches the logs of whatever tries to dial it.

Pinned by
`TestEtherspotVendor_GenerateConfigs_TheChainPicksTheHostAndAnUnknownChainPicksNone`.

---

## 45. QuickNode's endpoint walk holds one connection per page

**Status:** open. **Severity: low.** The chainstack half of entry 42, unfixed.

`thirdparty/quicknode.go:435`. `defer resp.Body.Close()` sits inside
`fetchEndpoints`'s pagination loop, so every page's body stays open until the
whole walk returns. This is the same defect entry 42 recorded for
`chainstack.go:232`, which has since been repaired; QuickNode was not.

An account with many endpoints holds one connection per 100-endpoint page for
the length of the walk.
## 46. The policy eval timeout races its own result, then throws it away

**Corroborated independently — see 144**, which adds the proof that the discard
is guaranteed rather than likely.

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high.** Verified under `-race`. An operator lost the "my policy is too slow"
signal completely, and the `internal/policy/stdlib` suite flaked because of it.

`internal/policy/slot.go:224-244`. `tickOnce` runs the eval on its own
goroutine and shares two plain variables with it:

```go
var (
    evalRes *EvalResult
    evalErr error
)
done := make(chan struct{})
go func() {
    defer close(done)
    evalRes, evalErr = runEval(...)          // :232 — writes evalErr
}()

if timeout > 0 {
    select {
    case <-done:
    case <-time.After(timeout):
        evalErr = fmt.Errorf("%w after %s", ErrEvalTimeout, timeout)  // :239
        <-done                                // waits for the goroutine
    }
}
```

Two defects, one root cause.

**The race.** On the timeout branch the parent writes `evalErr` at `:239`
while the eval goroutine may write the same variable at `:232`. Nothing
orders the two writes. The race detector reports exactly this pair.

**The swallowed timeout.** The parent then blocks on `<-done`, so the
goroutine's assignment lands **after** the parent's. A slow-but-successful
eval overwrites `ErrEvalTimeout` with `nil`, and the slot proceeds to
publish the late result as if it had arrived on time. A slow-and-failing
eval reports the eval's own error instead. Either way `ErrEvalTimeout`
never reaches the `Decision`, so `emitMetrics` cannot classify
`kind="timeout"` and `selection_eval_errors_total{kind="timeout"}` is a
counter that can never increment. `ErrEvalTimeout` is dead.

An operator whose policy has grown past its `evalTimeout` therefore sees no
error, no metric and no log — only a routing verdict computed from a stale
snapshot, published later than the config allows.

**How to see it.** Run `go test ./internal/policy/stdlib/ -race -count=2`.
Those tests set `evalTimeout` to 50 ms; the stdlib primer alone costs about
20 ms per fresh runtime under `-race`, so on a loaded machine the timeout
fires and the race with it. The failures move around between runs — 8
tests on one run, 1 on the next — which is the tell. `-count=1` usually
passes.

**The fix.** The goroutine publishes an `evalOutcome{res, err}` on a buffered
channel instead of writing the parent's variables. The parent picks the winner,
and when the deadline fires the deadline wins. It still waits for the goroutine
— sobek cannot be interrupted mid-call — but it now DISCARDS the late result
rather than letting it overwrite `ErrEvalTimeout`.

Pinned by six tests in `internal/policy/slot_eval_timeout_test.go`:
`TestSlot_EvalTimeout_ReachesTheDecision`, `…_KeepsThePreviousOrder`,
`…_CountsAndLogs`, `…_HasNoRaceOnTheOutcome`,
`TestSlot_EvalWithinTimeout_PublishesItsResult` and
`TestSlot_UserThrowAboutATimeout_IsNotAPolicyTimeout`. None bets on machine
load: every eval busy-waits well past its deadline, so the timeout always fires
first and the result always arrives late. What they assert is that the late
result LOSES. Reverting the concurrency fix fails them; reverting the
classification narrowing fails the last one. `go test ./internal/policy/...
-race -count=2` is clean.

Those six came from commit `8761576` on `worktree-agent-a47de169d3033a216`,
which fixed this defect on 2026-08-19 and never merged. `main` had one
weaker test of its own until the branch was found while pruning worktrees.

`selection_eval_errors_total{kind="timeout"}` can now increment. `emitMetrics`
classifies on `strings.Contains(d.Error, "timed out")`, and the error text
reaches `Decision.Error` for the first time.

The classifier matched the bare words "timed out", so a user eval that THREW
about its own timeout was counted as a policy timeout. It now matches
`ErrEvalTimeout.Error()`, pinned by
`TestSlot_UserThrowAboutATimeout_IsNotAPolicyTimeout`. **Entry 150** carries
the residual: matching a rendered string is the wrong shape whatever string it
matches.
## 47. A PostgreSQL listener connection is never released

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high.** Shared-state watches stop working once the process has watched
`maxConns` distinct counter keys. Pinned by
`TestPostgreSQLConnector_WatchCounterInt64_ReleasesTheListenerConnection`.

`getOrCreateListener` (`data/postgresql.go:872`) takes one connection out of
the listener pool per watched key — `connectListener` at `:880`, which calls
`listenerPool.Acquire` at `:975` — and stores the listener in `p.listeners`
(`:923`). Nothing ever gives that connection back.

- `WatchCounterInt64`'s cleanup (`:692-706`) removes the caller's channel from
  `listener.watchers` and closes it. It does not touch the listener, its
  goroutine, or its connection.
- The listener's reconnect branch (`:896-899`) overwrites `conn` with a fresh
  one and never calls `Release()` on the old one, so every reconnect leaks
  another pooled connection.
- `p.listeners` (`:84`) has no `Delete` anywhere in the file.

The listener pool is built with `MaxConns = p.maxConns` (`:957`). Once the
connector has seen `maxConns` distinct keys, `Acquire` blocks. `connectListener`
then retries every 5 seconds forever (`:976-979`), and `WatchCounterInt64`
returns only when the caller's context ends — with
`failed to create listener: context deadline exceeded`.

eRPC watches one counter per tracked value per network, so a fleet with more
networks than `maxConns` reaches the limit during normal startup. An operator
sees shared-state watches fail one by one, with nothing reported at boot.

Measured against a live container with `maxConns` 3: three watches succeed,
all three cleanups run, and the fourth watch times out.

**The fix ties the listener's life to its watchers.** A listener is shared by
every watcher of one key, so it lives exactly as long as it has one:

- `subscribe` joins a watcher to the listener under the same lock that guards
  the watcher list, and builds a fresh listener when it finds one already
  marked closed.
- `releaseWatcher` removes the watcher and, when it was the last one, marks
  the listener closed, deletes it from `p.listeners` and cancels it.
- `runListener` owns the pooled connection for its whole life: it acquires
  every replacement, and it runs `UNLISTEN` and `Release()` on the way out.
  Single ownership is what makes the teardown safe — the teardown only
  cancels a context, so it never releases a connection another goroutine is
  reading from. The reconnect branch releases the broken connection before it
  acquires the next one.
- `getOrCreateListener` stores with `LoadOrStore`. Two watchers racing on the
  same key used to build two listeners, and the second `Store` orphaned the
  first — connection, goroutine and all.

`pgxListener.conn` is no longer dead: it is now the `*pgxpool.Conn` the
goroutine holds, and the reconnect branch keeps it current.

Pinned by `TestPostgreSQLConnector_WatchCounterInt64_ReleasesTheListenerConnection`
(`data/postgresql_listener_leak_test.go`), which counts
`listenerPool.Stat().AcquiredConns()` against a real container: three watched
keys take three connections, and all three come back when the callers leave.
Mutation: with `data/postgresql.go` reverted the test fails because the count
never returns to zero.

`TestPostgreSQLConnector_ListenerPoolIsExhaustedByWatchedKeys` is deleted —
it asserted the leak, and its own comment said to remove it once the leak was
fixed.
## 48. A WebSocket client is sent an empty frame when a response cannot be written

**Status:** open. **Severity: medium.** The client's call never resolves.

`erpc/ws_server.go:592` — `writeNormalizedResponse` opens a frame with
`NextWriter`, streams the response into it, and closes the frame. When
`resp.WriteTo(w)` fails, eRPC logs at Debug and closes the frame anyway, so
gorilla ships a complete text message with a zero-length payload.

The client reads a message that is not JSON and carries no `id`. It cannot
match it to a call and it cannot report an error, so the call waits for the
client's own timeout. The HTTP path answers the same failure with a JSON-RPC
error envelope (`erpc/http_server.go:1921` hands over to `writeFatalError`).

Fix: on error, abandon the frame instead of closing it, or replace it with a
JSON-RPC error carrying the request id.

Pinned by `TestWsWriteNormalizedResponse_SendsAnEmptyFrameWhenItCannotSerialise`,
which records the empty payload as the current behaviour.

---

## 49. The WebSocket batch writer discards every error it can produce

**Status:** open. **Severity: medium.** No client signal, no server signal.

`erpc/ws_server.go:651-653`:

```go
bw := NewBatchResponseWriter(responses)
_, _ = bw.WriteTo(w)
_ = w.Close()
```

`BatchResponseWriter.WriteTo` reports three distinct failures — a dead socket,
an entry it cannot marshal, and a response with nothing to write. All three
land here and all three are dropped. The client receives a truncated JSON array
inside a complete text frame, which every JSON parser rejects, and the operator
gets no log line at all. `writeNormalizedResponse` at least logs at Debug.

Fix: log the error, and close the connection rather than shipping a frame the
client cannot parse.

Pinned by `TestWsWriteBatchResponse_AbandonsTheBatchWhenTheSocketDiesMidFrame`,
which confirms eRPC stops writing but records nothing.

---

## 50. The "request premature context error" branch cannot run

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
low.** The unreachable inner block is gone: the handler reads
`context.Cause` unconditionally and logs the reason the request ended.
Pinned by `TestRequestHandler_ReportsWhyTheRequestContextEnded`.

`erpc/http_server.go:770-791`:

```go
if err := httpCtx.Err(); err != nil {
    if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
        cause := context.Cause(httpCtx)
        ...
        s.logger.Trace().Err(err).Msg("request premature context error")
        writeFatalError(httpCtx, http.StatusInternalServerError, err)
    }
    ...
}
```

`context.Context.Err()` returns exactly `context.Canceled` or
`context.DeadlineExceeded` — that is the interface contract, and the otel
wrappers between here and `r.Context()` delegate it unchanged. The inner block
is therefore unreachable.

Three consequences: `context.Cause` is never read, so a cancellation reason the
code deliberately attaches is never surfaced; the trace line never prints, so an
operator debugging "the client received nothing" has nothing to find; and
`writeFatalError` never fires on this path. The response-release loop around it
does run and is correct.

The fix reads the cause unconditionally and logs it at Debug, and the
unreachable inner block is gone.

Pinned by `TestRequestHandler_WritesNothingWhenTheRequestContextIsAlreadyDone`,
which covers the reachable half — eRPC releases the responses and writes
nothing.

---

## 51. Three unreachable defensive branches in the WebSocket write path

**Status:** not a bug. **Severity: none.** Unexercised machinery. Closed as
not a bug on 2026-08-21: the code cannot change what an operator or a client
sees, so there is nothing to repair. It stays here because it still reads as
live code. Deleting it in the fork would cost a permanent rebase conflict
and change nothing — see `valve/open-entry-triage.md`.

`erpc/ws_server.go:573`, `:607` and `:640` each guard
`wsc.conn.SetWriteDeadline(...)` and give up on an error. gorilla's
`Conn.SetWriteDeadline` (v1.5.3, `conn.go`) stores the deadline and returns
`nil` unconditionally; it never touches the socket. All three branches are dead.

The deadline itself is load-bearing and correct — only the error check is dead.

---

## 52. Two smaller write-path defects

**Status:** open. **Severity: low.** The first bullet is FIXED in the fork,
pinned by `TestBatchResponseWriter_ReportsAnEntryThatWroteNothing`. The
second bullet is still open.

- `erpc/http_batch_resp.go:74` — FIXED. The message read
  `fmt.Errorf("no bytes written for response %d error: %w", i, err)` and was
  reached only when `err == nil`, so every message ended in `%!w(<nil>)`. The
  fork drops the `%w` and names the entry type instead. Pinned by
  `TestBatchResponseWriter_ReportsAnEntryThatWroteNothing`.
- `erpc/http_server.go:1911` and `:798`, both with `:869` — when a body fails
  part-way through, `writeFatalError` calls `w.WriteHeader` a second time
  (net/http logs "superfluous response.WriteHeader call") and appends a second
  JSON document after the partial first one. With a dead socket nothing
  arrives, so this is harmless; with a live socket and a non-transport failure
  — an entry the batch writer cannot marshal, or a response with nothing to
  write — the client receives one unparseable body.

## 53. The legacy upstream `failsafe:` object drops every key added since

**Status: FIXED in the fork.** Upstream still carries it. **Severity: medium.**
Fixed together with entry 64, which had the same cause. Pinned by
`common/config_backcompat_unmarshal_test.go:TestUpstreamConfig_UnmarshalYAML_TheLegacyFailsafeObjectKeepsEveryOtherKey`,
which replaces `TestUpstreamConfig_UnmarshalYAML_LegacyFailsafeObjectDropsNewerKeys`.
That test asserted the defect.

`UpstreamConfig.UnmarshalYAML` decoded into a shadow struct. When that decode
failed — which a legacy single-object `failsafe:` always caused — it fell back
to a hand-listed `oldShadow` and copied field by field.

`oldShadow` did not grow with `UpstreamConfig`. **The drift was measured twice,
and it had grown between the two readings.** When this entry was first written
it dropped `rateLimitCountMode` and `creditUnits`. By the time it was fixed on
2026-08-21 it dropped **four** fields: `chain` and `chainProbeInterval` had
arrived with a rebase, and nobody updated the copy. That is the argument
against topping the list up.

An operator who configured credit-based rate limiting got flat per-request
counting instead. No warning, no error — their budget drained at the wrong
rate.

**The fix deletes the structure rather than completing it.** `FailsafeConfig`
is the only field whose SHAPE differs between the two schemas, so the decision
belongs there. `FailsafeConfigList` (`common/config.go`) is a
`[]*FailsafeConfig` with its own `UnmarshalYAML` that accepts a list or a
single mapping, and the three types that carry a `failsafe:` key now use it.
With the legacy shape handled where it varies, the fallback decode has no
remaining purpose, so all three hand-listed copies are gone —
`UpstreamConfig`'s `oldShadow`, `NetworkDefaults`'s `oldNetworkDefaults` and
`NetworkConfig`'s `oldNetworkConfig`, 187 lines in total. There is one decode
path now, and no parallel struct that can drift again.

The test asserts a PROPERTY, not a field list: the two forms must produce equal
configs apart from `failsafe` itself. A field list in the test would rot the
same way `oldShadow` did.

**Two details worth keeping.**

The list type decides on the value's SHAPE, not on which decode failed. The
first attempt chose by failure, and it broke a test that already existed: a
list holding one bad field fails as a list, gets retried as a single policy,
and reports "cannot unmarshal !!seq into common.FailsafeConfig" — which says
nothing about the field the operator got wrong.

The shape comes from a decode into `interface{}`, not a `yaml.Node`. A Node
stays empty here, because this is yaml.v3's obsolete unmarshaler signature and
it cannot fill one; that was verified by probe, not assumed. The modern
signature does fill it, but its `Node.Decode` builds a decoder with
`KnownFields` off, which would stop an unknown key inside a failsafe policy
being reported at all. Losing that would re-open this entry's own class of
defect.

**Mutation result (2026-08-21).** With the pre-fix `common/config.go` restored
and the new tests kept, all three fail: the upstream property test reports the
configs unequal, and both message tests still see the shadow type's complaint.
With the fix restored, all three pass and the whole `common` package is green.

## 54. Two agent-name branches are shadowed and can never run

**Status:** open. **Severity: low.** A test asserts today's broken behaviour:
`TestSimplifyAgentName_EarlierBranchesShadowQuicknodeAndEdge`
(`common/request_http_context_test.go`).

`simplifyAgentName` (`common/request.go:1221`) is an ordered chain of substring
tests. Two later branches are unreachable:

- `"quicknode"` contains `"node"`, so the `node` test at
  `common/request.go:1243` claims it first. The `quicknode` branch at
  `common/request.go:1288` cannot run for any real QuickNode user agent.
- Every Chromium browser sends `Chrome` in its user agent, so the `chrome`
  test at `common/request.go:1261` claims Edge before the `edge` branch at
  `common/request.go:1270`. Released Edge also spells itself `Edg/`, which
  `"edge"` does not match at all.

The operator sees no error. They see QuickNode SDK traffic filed under
`nodejs` and Edge traffic filed under `chrome`, so a per-client breakdown
names the wrong client. Order the specific tests before the general ones.

## 55. Every failsafe defaulting error path in `common/defaults.go` is dead

**Status:** not a bug. **Severity: lowest.** No test, because no input
reaches it. Closed as not a bug on 2026-08-21: the code cannot change what
an operator or a client sees, so there is nothing to repair. It stays here
because it still reads as live code. Deleting it in the fork would cost a
permanent rebase conflict and change nothing — see `valve/open-entry-
triage.md`.

`FailsafeConfig.SetDefaults` (`common/defaults.go:2613`) can never return a
non-nil error. Every leaf it calls — `TimeoutPolicyConfig`,
`RetryPolicyConfig`, `HedgePolicyConfig`, `CircuitBreakerPolicyConfig`,
`MisbehaviorsDestinationConfig` — returns a literal `nil` on every path, and
`ConsensusPolicyConfig`'s only error comes from `MisbehaviorsDestinationConfig`.

So every caller's wrapper is unreachable: `common/defaults.go:1061` and `:1074`
(`policy #%d in failsafeForGets` / `ForSets`), `:1695`, `:1944`, `:1948`,
`:1956`, `:1966`, `:2125`, `:2134`, `:2227` (`failsafe[%d]`). Either give the
leaf validators something to reject, or drop the `error` return.

---

## 57. The BDS pool leaks a connection when Shutdown races its maintainer

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** One leaked socket per race. Pinned by
`TestPoolShutdown_JoinsTheMaintainer`.

`clients/grpc_bds_resilience.go:510` closed `stopCh` and then walked the
connection slots. It never waited for the maintainer goroutine it started at
`:210`.

The maintainer wakes every `bdsMaintainInterval`, and a tick already in flight
dials a replacement (`recycleConn`, `:297`) and swaps it into a slot
(`swapInReplacement`, `:337`). Both steps run after `Shutdown` has read the
slot it is about to overwrite. So a replacement installed after the walk is
never closed, and its socket lives for the process lifetime.

The window is one tick wide per pool, so it needs an unlucky shutdown. eRPC
rebuilds clients on config reload, not only at process exit, so the leak
accumulates across reloads on a long-lived process.

The fix is a `sync.WaitGroup` on `bdsPool`: `Add(1)` before
`go p.maintainLoop()`, `defer Done()` inside the loop, `Wait()` at the top of
`Shutdown` — outside `poolMu`, because the maintainer takes that lock itself.
The wait is bounded by `bdsVerifyTimeout`, the maintainer's only blocking step.

The same missing join is why the maintainer could not be tested: without a
happens-before edge, a test that compressed `bdsMaintainInterval` raced the
maintainer's read of it, and `-race` said so. Pinned by
`TestPoolShutdown_JoinsTheMaintainer`.

## 58. `getHttpClient` uses the proxy client before checking the error

**Status:** open. **Severity: low.** Latent; not reachable through today's
config path.

`clients/http_json_rpc_client.go:213-221`:

```go
client, err := c.proxyPool.GetClient()
if c.isLogLevelTrace {
    proxy, _ := client.Transport.(*http.Transport).Proxy(nil)   // :215
    c.logger.Trace()....Msgf("using client from proxy pool")
}
if err != nil {                                                  // :218
    ...
    return c.httpClient
}
```

`GetClient` returns `(nil, err)` when the pool holds no clients
(`clients/proxy_pool_registry.go:24`). At trace level, line 215 dereferences
that nil `*http.Client` before line 218 ever looks at `err`, and the request
goroutine panics.

Line 215 also asserts the transport type without the `, ok` form and calls
`Proxy` without checking it is set, so a pool built with a plain
`http.Client` panics there too.

Today `createProxyPool` (`:70`) refuses a pool with no URLs, and
`NewProxyPoolRegistry` propagates that error, so no empty pool ever reaches
`GetClient`. The defect is the ordering, not the reachability: any future
caller that builds a `ProxyPool` directly, or any pool that can empty at
runtime, turns a logged error into a panic. Move the trace block below the
error check.

`TestProxyPool_GetClientOnAnEmptyPoolErrorsAndNamesThePool` pins the
`GetClient` contract this depends on.

---

## 59. Three vendors probe the wrong endpoint when one vendor serves two providers

**Status:** open. **Severity: medium.** A wrong supported/unsupported verdict
at bootstrap, from config alone.

Six vendors answer `SupportsNetwork` by sending `eth_chainId` to the endpoint
and comparing the answer. Each caches the probe client in a `sync.Map`. Three
key that map on the URL and the chain; three key it on the chain alone:

| vendor | cache key | site |
| --- | --- | --- |
| erpc | url + chain | `thirdparty/erpc.go:261` |
| goldsky | url + chain | `thirdparty/goldsky.go:257` |
| routemesh | url + chain | `thirdparty/routemesh.go:153` |
| **envio** | **chain only** | `thirdparty/envio.go:265`, `:277` |
| **pimlico** | **chain only** | `thirdparty/pimlico.go:239`, `:251` |
| **thirdweb** | **chain only** | `thirdparty/thirdweb.go:140`, `:152` |

`ProvidersRegistry` hands every provider the SAME vendor instance
(`thirdparty/providers_registry.go:23` calls `vendorReg.LookupByName`), so two
providers of one vendor share one cache. And all three of the chain-keyed
vendors build their URL from per-provider settings: envio from `rootDomain`,
pimlico from `apiKey`, thirdweb from `clientId`.

So an operator who configures two providers of the same vendor gets this: the
first provider probed for chain N caches its client, and every later provider
asking about chain N is handed that client and probes the FIRST provider's URL.
The second provider's own endpoint is never contacted.

What the operator sees:

- Two envio providers, one public and one self-hosted. The self-hosted one is
  reported supported or unsupported according to what the public endpoint says,
  and its own host is never reached.
- Two pimlico providers on different keys. If the first key is rate-limited,
  the second is reported unsupported even though it is fine.

`GenerateConfigs` builds each provider's URL correctly, so the upstream that
lands is right. The damage is the support verdict, and a network wrongly judged
unsupported never gets an upstream at all.

The entry is also never evicted, so a rotated credential does not take effect
until the process restarts.

The fix is the one line the other three already carry: key on
`parsedURL.String()` plus the chain.

Pinned by
`TestProbeVendors_getOrCreateClient_TheCacheKeyDecidesWhetherASecondUrlIsHonoured`,
which asserts today's split and fails once the three converge.

---

## 60. Seven vendor normalisers do work the caller overwrites two lines later

**Status:** not a bug. **Severity: low.** Dead work that reads as a feature.
Closed as not a bug on 2026-08-21: the code cannot change what an operator
or a client sees, so there is nothing to repair. It stays here because it
still reads as live code. Deleting it in the fork would cost a permanent
rebase conflict and change nothing — see `valve/open-entry-triage.md`.

`thirdparty/ankr.go:125`, `blastapi.go:160`, `chainstack.go:423`,
`erpc.go:155`, `goldsky.go:165`, `onfinality.go:102` and `tenderly.go:140`
share one `GetVendorSpecificErrorIfAny` body: copy `jrr.Error.Data` into
`details["data"]`, then return `nil`.

Their only caller is `architecture/evm/error_normalizer.go:29`. Returning `nil`
means control falls through to `:43-64`, which writes `details["data"]` from the
same `err.Data` on every branch of its type switch — the string branch after
stripping a `"Reverted "` prefix, the default branch verbatim. So the vendor's
write is always overwritten with the same value or a better one.

The effect is that seven vendors appear to contribute error detail and
contribute none. A maintainer adding an eighth vendor copies the body and gets
nothing for it. Either delete the seven bodies or move the useful part — the
prefix strip — into them.

Recorded, not pinned: pinning "this line has no effect" would fail the moment
the line is deleted, which is the outcome we want.

Not a bug, recorded so it is not "fixed" by mistake: the same seven bodies read
`bodyMap.Error` with no nil check. The caller guards `jr != nil && jr.Error !=
nil` at `error_normalizer.go:27` before it calls, so the dereference cannot
fire from the only path that reaches it.

---

## 61. Two smaller `thirdparty` divergences

**Status:** open. **Severity: low.**

- **`thirdparty/dwellir.go:111-115`** — `SupportsNetwork` catches the chain-ID
  parse error and returns `(false, nil)`. Eleven sibling vendors — ankr,
  blastapi, blockpi, envio, etherspot, infura, llama, onfinality, pimlico,
  routemesh and thirdweb — return it. An operator who typos a network ID reads
  "dwellir does not serve this network" instead of the parse error naming the
  bad ID. Pinned by
  `TestDwellirVendor_SupportsNetwork_AMalformedChainIdIsASilentNoNotAnError`.
- **`thirdparty/dwellir.go:177`** — `OwnsUpstream` claims `dwellir://` but not
  `evm+dwellir://`. Every one of the other twenty-one built-in vendors claims
  both, and `common/defaults.go:1541` accepts `evm+dwellir://` as a shorthand.
  Nothing observes the gap today, because `convertUpstreamToProvider`
  (`common/defaults.go:1430`) turns every non-http endpoint into a provider and
  clears `Endpoint` before `LookupByUpstream` (`upstream/upstream.go:254`) ever
  runs — which makes the scheme branch of all twenty-two `OwnsUpstream` methods
  unreachable in the current wiring. Pinned by
  `TestEveryVendor_OwnsUpstream_ClaimsItsOwnSchemeAndNobodyElses`, which records
  dwellir as the exception.

---

## 56. A task started after shutdown wedges its whole Initializer

**Status: FIXED in the fork** (`util/initializer.go`). Upstream still carries
it. **Severity was: high.** A hung shutdown and a hung request path.

`attemptRemainingTasks` ran every schedulable task under one `sync.WaitGroup`
and waited for all of them to *start*. It called `wg.Add(1)` for every task the
walk looked at and `wg.Done()` in each branch that skipped one, plus one more
inside the launched goroutine. That goroutine returned early when the app
context was already cancelled — **without calling `wg.Done()`**. `wg.Wait()`
then blocked forever.

The blast radius was the whole Initializer, not one task.
`attemptRemainingTasks` takes `i.tasksMu` and releases it through a `defer`, so
the mutex was never released either. Every later `ExecuteTasks`,
`attemptRemainingTasks` and `Stop` on that Initializer blocked on the same
mutex.

One Initializer serves many resources (one bootstrap task per network and per
upstream), and `NetworksRegistry.GetNetwork` calls `ExecuteTasks` on the
request path. So any request that raced process shutdown — or any task
scheduled after the app context was cancelled — stranded every later caller AND
the shutdown sequence itself. The task's own state was recorded correctly
(`TaskFailed`, with the context error); only the callers hung.

**The fix makes two changes, and neither adds a branch.**

1. The cancelled-app-context case no longer launches a goroutine. The walk
   records the failure itself — store the error, log it, publish `TaskFailed`,
   close the done channel — and moves on. A task that never starts cannot fail
   to report that it started.
2. `wg` now counts launched goroutines only: one `Add` next to the `go`, one
   `Done` on that goroutine's only path, immediately after it builds the task
   context. The four bookkeeping `Done` calls in the skip branches are gone, so
   there is no count left to get wrong. Nothing between the `Add` and the
   `Done` can fail or return early.

The barrier itself stays. `attemptRemainingTasks` still waits for every
launched goroutine to start before it returns, because callers read `State()`
and `ctxCancel` straight after `ExecuteTasks`. Deleting the barrier made
`TestInitializer_MultipleTasksMixedResultsNoRetry` and
`TestInitializer_ErrorsJoinsEveryTaskFailure` fail.

The ordering that entry 122 fixed is untouched: `Stop` still cancels the
auto-retry loop and waits for it BEFORE it takes `tasksMu`.

The walk also collected a `tasksToRun` slice that nothing ever read. Deleted.

Pinned by
`TestInitializer_ATaskScheduledAfterShutdownDoesNotWedgeTheInitializer`, which
asserts that `ExecuteTasks`, a second `ExecuteTasks` and `Stop` all return.
Every assertion races its call against a five-second deadline, so a regression
FAILS the test instead of hanging the package — which is how entry 122 stayed
hidden for so long. Against the previous code the first assertion fails in five
seconds. `go test -race ./util/ -count=4` is green.

Adjacent, same function, **severity: low, still open** — when a task returns
`context.Canceled`, the handler tries
`bt.lastErr.CompareAndSwap(nil, wrappedError{err: err})`. The CAS can never
succeed: the walk stored `wrappedError{err: nil}` into that `atomic.Value`
before the attempt, so the current value is a `wrappedError`, never `nil`. The
cancellation reason is dropped, and `Wait` substitutes `"task failed without
specific error"`. The task is still counted as failed; only the reason is lost.
Pinned by `TestInitializer_CancelledTaskIsReportedWithoutItsReason`.

---

## 62. `SuggestFinalizedBlock` drops a suggestion under contention; the latest twin does not

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** Raised from "low in production" — see the second reproduction
below. Pinned by `TestSuggestFinalizedBlock_SmallAdvanceAppliesInline` and
`TestSuggestFinalizedBlock_SmallAdvanceSurvivesAMajorJumpVerification`.

`architecture/evm/evm_state_poller.go:824` — `SuggestFinalizedBlock` takes
`finalizedUpdateInProgress` with `TryLock` and RETURNS when the lock is held.
The suggestion is discarded, not queued. Nothing re-issues it, so the finalized
head stays where it was until the next successful poll — which the debounce can
push a full interval away.

`SuggestLatestBlock` at `:532` does not behave this way. It applies a small
advance inline and synchronously, and defers to a goroutine only for a MAJOR
forward jump. Two counters written from the same request path therefore have
different delivery guarantees, and only one of them is documented as
best-effort.

In production the loss self-heals: the next response carrying a finalized
number suggests again. The cost is a stale finalization lag for one poll
interval, on an upstream busy enough to have suggestions overlap — which is
exactly the upstream whose lag matters.

In tests it does not self-heal, because a test usually suggests once. Found via
`TestNetwork_FallbackTierDoesNotDefineTheServedTip`, which timed out after
20.26s under `make test-fast` waiting for a value that was never going to
arrive. The fix there was to stop routing test seeds through `Suggest*` at all
(see `seedEvmHeads` in `erpc/networks_selection_policy_test.go`); a wider
`Eventually` ceiling cannot help, since the write was dropped rather than
delayed.

**Second reproduction, and why the severity rose.**
`TestSuggestFinalizedBlock_MajorJumpMatchingApplies`
(`architecture/evm/evm_state_poller_suggest_gate_test.go:218`) fails under
`go test -race` with "Condition never satisfied". The race detector reports no
data race — it only slows the process down, which widens the window until the
drop is reliable. On this machine the unfixed code failed 2 of 20 `-race` runs
scoped to that one test, and every failure cost the full 2s `Eventually`
ceiling.

The mechanism is not contention. The goroutine published the new value and only
then ran its deferred `Unlock`, so the value was visible while the lock was
still held. Any caller that reacts to the value it just observed lands in that
window, with no concurrency of its own:

    p.SuggestFinalizedBlock(1000)
    require.Eventually(... p.FinalizedBlock() == 1000 ...)   // returns at publish
    p.SuggestFinalizedBlock(major)                            // TryLock fails, discarded

**Fix.** `SuggestFinalizedBlock` now has the same shape as
`SuggestLatestBlock`. It applies the suggestion inline, and hands only a MAJOR
forward jump to `verifyThenSuggestFinalizedBlock`, a background goroutine that
runs the chain-identity check under its own `finalizedMajorVerifyInProgress`
lock. The common path takes no lock at all, so nothing can drop it.

This deletes structure rather than adding it: the bespoke
"every suggestion behind one TryLock" arrangement is gone, and the two counters
now have one delivery guarantee instead of two. Coalescing the pending
suggestion was the alternative; it keeps the goroutine and adds a slot plus a
re-read loop to protect a path that no longer needs protecting.

One drop remains, on both counters: a MAJOR jump arriving while another major
jump is verifying. That one is forced — the check makes a live `eth_chainId`
call, so it must be asynchronous and serialized. It is now logged at Debug on
both paths (the latest twin used to return silently), and it is re-observed on
the next suggestion or verified poll.

Pinned by `TestSuggestFinalizedBlock_SmallAdvanceAppliesInline` and
`TestSuggestFinalizedBlock_SmallAdvanceSurvivesAMajorJumpVerification`
(`architecture/evm/evm_state_poller_suggest_drop_test.go`). The second test
parks the major jump inside `eth_chainId` on a gate it controls, so the drop is
deterministic and needs no race detector.

---

## 63. A TypeScript config with no exports panics instead of explaining itself

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** Pinned by `TestLoadConfig_TypeScriptWithNoExportsExplainsItself`
(`common/config_ts_exports_test.go`).

`loadConfigFromTypescript` (`common/config.go:3355`) calls
`runtime.Exports().Get("default")`. `Runtime.Exports` (`common/runtime.go:58`)
is `r.vm.GlobalObject().Get("exports").ToObject(r.vm)`. When the compiled
module declares no exports at all, that global is JS `null`, and sobek's
`ToObject` raises a TypeError that unwinds out of the Go call as a panic.

The very next statement (`common/config.go:3390`) exists to catch exactly this
mistake: it returns "config object must be default exported from TypeScript
code AND must be the last statement in the file". It never runs for the
no-export case, so the operator who forgot `export default` gets a Go stack
trace with `TypeError: Cannot convert undefined or null to object` instead of
the sentence that tells them what to type.

`export default undefined` and `export default null` DO reach the friendly
error, because those leave a real `exports` object behind. The difference is
invisible from the config file, which is what makes the panic surprising.

One guard in `Runtime.Exports` — return nil when the value is null or
undefined — fixes it, and the existing check at `:3390` then handles the rest.

## 64. A mistyped current-schema key is reported against the legacy shadow

**Status: FIXED in the fork.** Upstream still carries it. **Severity: medium.**
Fixed together with entry 53, which had the same cause. Pinned by
`TestNetworkDefaults_UnmarshalYAML_ReportsTheRealTypeMismatchForACurrentOnlyKey`
and `TestNetworkConfig_UnmarshalYAML_ReportsTheRealTypeMismatchForACurrentOnlyKey`
(`common/config_unmarshal_gaps_test.go`), which replace the two
`…_CurrentOnlyKeyReportsTheLegacyShadow` tests. Those asserted the defect.

`NetworkDefaults.UnmarshalYAML` (`common/config.go:900`) and
`NetworkConfig.UnmarshalYAML` (`common/config.go:2501`) decode the node into
the current schema, and on failure decode the SAME node into a hand-listed
legacy shadow struct. The shadow is a subset: it has no `multiplexing` and no
`failover`.

So a plain type mistake in one of those keys produces this:

```yaml
networkDefaults:
  multiplexing: yes-please      # a string where a bool belongs
```

```
yaml: unmarshal errors:
  line 1: field multiplexing not found in type common.oldNetworkDefaults
```

Two things are wrong for the operator. The message says a key they wrote does
not exist, when it does and is valid. And it names `common.oldNetworkDefaults`,
an unexported struct declared inside the function body, which appears in no
documentation and in no config they can edit.

The real error — `cannot unmarshal !!str into bool` — is what
`return originalErr` (`common/config.go:936` and `:2541`) intends to deliver.
It does not arrive: the legacy attempt's unknown-field complaint is recorded
against the decoder and surfaces from the outer `Decode` instead. A key that
BOTH shapes declare (`rateLimitBudget`) reports correctly, which is why this
was never noticed.

**The fix removes the second decode entirely.** The legacy single-object
`failsafe:` was the only reason either type kept a shadow struct, so once
`FailsafeConfigList` accepts both shapes itself there is nothing left to fall
back to. `NetworkDefaults.UnmarshalYAML` and `NetworkConfig.UnmarshalYAML` are
deleted, and both types now take the plain decode. With no second attempt to
record a complaint against the decoder, the real error is the one that reaches
the operator. Entry 53 carries the full account of the change.

The tests now assert three things for each mistyped key: the operator sees the
real type mismatch, no internal shadow type name appears, and the error does
not say "not found in type" — a key that exists must never be reported as
missing, because that sends the operator hunting for a typo they did not make.

**Mutation result (2026-08-21).** With the pre-fix `common/config.go` restored,
both tests fail and report exactly the old message,
`field multiplexing not found in type common.oldNetworkDefaults`.

## 65. A rate-limit rule logs nanoseconds under a key that says milliseconds

**Status:** open. **Severity: low.**
`TestRateLimitRuleConfig_MarshalZerologObject`
(`common/config_unmarshal_gaps_test.go`).

`RateLimitRuleConfig.MarshalZerologObject` (`common/config.go:3230`) writes
`Str("waitTimeMs", fmt.Sprintf("%d", c.WaitTime))`. `WaitTime` is a
`common.Duration` (`common/duration.go:8`), which is `time.Duration` — a
nanosecond count. `%d` prints that count verbatim.

An operator who writes `waitTime: 1s` sees `"waitTimeMs":"1000000000"` and
reads it as a wait of eleven and a half days. The other three fields on the
same line are correct, which makes the wrong one easy to trust.

`Dur`/`Int64` with `.Milliseconds()` fixes it, or renaming the key to
`waitTime` and using the type's own `String()` — the sibling `period` field on
the same line already takes the `String()` route.

---

## 85. One stray dash in a YAML config kills eRPC at boot

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high.** Bootstrap crash instead of a config error. Pinned by
`TestLoadConfig_AnEmptyListItemNamesItselfInsteadOfPanicking`.

An operator writes a list item and leaves it empty:

```yaml
projects:
  -
```

YAML decodes that item as a nil pointer. `SetDefaults` then dereferences it and
the process dies with a nil-pointer panic before the logger says anything
useful. The operator sees a stack trace, not the line to fix.

The whole eleven-byte input is `projects:\n-`.

Every list of config objects carries the same defect, and so does one map. Each
site below is a distinct nil receiver, all reached from `Config.SetDefaults`
(`common/defaults.go:49`):

| container | panic site |
| --- | --- |
| `projects` | `common/defaults.go:1340` |
| `projects[].upstreams` | `common/defaults.go:1894` |
| `projects[].networks` | `common/defaults.go:2223` |
| `projects[].providers` | `common/defaults.go:1743` |
| `projects[].providers[].overrides` (map) | `common/defaults.go:1751` |
| `projects[].auth.strategies` | `common/defaults.go:3136` |
| `rateLimiters.budgets` | `common/defaults.go:3312` |
| `database.evmJsonRpcCache.connectors` | `common/defaults.go:1049` |
| `database.evmJsonRpcCache.policies` | `common/defaults.go:772` |

The map case needs a key with an empty value:

```yaml
projects:
  - id: a
    providers:
      - vendor: alchemy
        overrides:
          evm:1:
```

`FuzzLoadConfigYaml` (`common/config_fuzz_test.go`) found the first one in
under two seconds. The rest came from walking the same shape across the config
tree.

The fork rejects an empty entry before `SetDefaults` runs and names it:

```
config: projects[0].upstreams[0] has no content, remove the entry or fill it in
config: projects[0].providers[0].overrides[evm:1] has no content, remove the entry or fill it in
```

`rejectEmptyListItems` walks the decoded config once and errors on any nil
pointer inside a list or a map. It is generic rather than a guard per
container, so a list added later is covered too. A `null` inside a `params`
list stays legal — that is a JSON-RPC value, not an empty entry.

One behaviour changes beyond the crash: an empty
`networks[].methods.definitions` key used to decode to nil and get ignored in
silence. It is now an error. An empty definition carries no fields, so the only
thing it can express is a typo.

Pinned by `TestLoadConfig_AnEmptyListItemNamesItselfInsteadOfPanicking`,
`TestLoadConfig_AnEmptyMapValueNamesItselfInsteadOfPanicking` and
`TestLoadConfig_ANullParamSurvivesTheEmptyItemCheck`.

---

## 86. A corrupt cache entry becomes an HTTP 200 with a broken body

**Status:** open. **Severity: medium.** Silent protocol violation toward the
client.

The cache read path builds a response straight out of the stored bytes and
never checks that they are JSON:

- `architecture/evm/json_rpc_cache.go:1158`
- `architecture/svm/json_rpc_cache.go:435`

Both call `common.NewJsonRpcResponseFromBytes(nil, resultBytes, nil)`, which
stores the bytes as the result verbatim. `WriteTo` then copies them onto the
wire.

Store the value `{"number":"0x1b4` — a value truncated mid-string — under the
key for `eth_getBlockByNumber("0x1b4")`, and the next cache hit answers:

```
HTTP 200
{"jsonrpc":"2.0","id":11,"result":{"number":"0x1b4}
```

No layer reports an error. `cache.Get` returns nil error, `WriteTo` returns nil
error, and the log stays quiet. The client raises a JSON syntax error and the
operator has nothing to correlate it with.

The cache is a SHARED store — Redis, PostgreSQL, S3 — so its bytes are not
eRPC's to trust. A truncated value, a partial object, or another writer on the
same key all produce this.

The fix costs a scan of every cache hit, so the trade-off belongs to the
maintainer. `TestEvmJsonRpcCache_ACorruptStoredValueReachesTheClientVerbatim`
pins the current behaviour and fails the moment the read path starts
validating.

---

## 87. eRPC launders a non-conforming upstream body into a broken client response

**Status:** open. **Severity: low.** Interop defect, no crash.

RFC 8259 forbids an unescaped control character inside a JSON string. sonic
accepts one, and `JsonRpcResponse.WriteTo` (`common/json_rpc.go:631`) forwards
the result and error bytes verbatim, so eRPC hands the client a body its own
parser rejects.

An upstream error message with a literal newline is enough:

```
{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom
next line"}}
```

`ParseFromStream` accepts it. `WriteTo` reproduces it byte for byte. Go's
`encoding/json`, `JSON.parse` and Python's `json` all reject the result. eRPC
reports success at every layer.

`FuzzJsonRpcResponseParseFromStream` found it as `{"result":{"":"\x1e"}}`
(`common/testdata/fuzz/FuzzJsonRpcResponseParseFromStream/46ff799e75a85057`).

Verbatim pass-through of the result is a deliberate design fact (see the
`NormalizedResponse.MarshalJSON` note at the end of this file), so the fix is
either a validity check at parse time or escaping at write time — both cost a
scan of every response. `TestJsonRpcResponse_AnUnescapedControlCharacterPassesThrough`
pins the current behaviour.

A related gap in the same function IS fixed in the fork: an upstream reply with
no `id` member made `WriteTo` emit `{"jsonrpc":"2.0","id":,"result":…}`, which
no client can parse. JSON-RPC 2.0 names `null` as the id of a response whose id
cannot be determined, so that is what goes on the wire now. Pinned by
`TestJsonRpcResponse_WriteTo_AMissingIdBecomesNull`.

---

## 88. `go vet ./...` fails on a copied `sync.Map` in a test helper

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
low.** `go vet ./...` now exits 0. No test pins this fix.

```
$ go vet ./...
erpc/query_executor_test.go:341:60: call of setUnexportedField copies lock value: sync.Map contains sync.noCopy
```

The helper builds an atomic snapshot, stores into it, then passes it BY VALUE:

```go
atomicMap := &sync.Map{}
atomicMap.Store(networkID, upstreams)
setUnexportedField(t, registry, "networkUpstreamsAtomic", *atomicMap)
```

A `sync.Map` holds a mutex and pointers to its own internal maps. Copying one
after a `Store` duplicates the mutex state and shares the internal `dirty` map
with the original. Here the original is dropped immediately, so the test passes
— but the pattern is exactly what `sync.noCopy` marks as forbidden.

This predates the fuzzing work (it arrived with `70a1f17`, the unified
selection policy commit) and is unrelated to it. It is recorded because it
makes `go vet ./...` non-zero for every contributor, which trains people to
ignore vet output.

`setUnexportedField` already takes an `interface{}`, so passing `atomicMap`
instead of `*atomicMap` — and reading the field as a pointer — clears it.

---

## 72. A static method is refused as a missing historical block

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high.** Every `eth_chainId` and `net_version` failed on any upstream that
declared a lower availability bound.

`architecture/evm/block_ref.go:356` gives block-agnostic, cache-forever methods
the block reference `*` and the block number **1** — the comment says so: "We
use block number 1 as a signal to indicate data is finalized on first ever
block". It is a cache sentinel, not a block anybody asked for.

`erpc/networks.go:2867-2894` (`checkUpstreamBlockAvailability`) reads that
sentinel as a real block. It takes `EvmBlockNumber()`, finds 1, compares it
against the upstream's resolved lower bound and returns
`ErrUpstreamBlockUnavailable{blockNumber: 1}` with `retryable=false`. The lower
bound arrives from `evm.blockAvailability.lower` **or** from the much more
common `maxAvailableRecentBlocks`, which resolves to `latest - N`
(`upstream/upstream.go:1029-1038`) — so any non-archive node in the pool is
affected.

`erpc/networks.go:2940-2969` (`eligibleUpstreamIDsForBoundary`) reads the same
sentinel and returns an empty boundary lane for those methods when
`selectionPolicy.evalPerBoundary` is on.

What an operator sees: on a fleet of pruned nodes, `eth_chainId` and
`net_version` fail on every upstream and come back as upstreams-exhausted,
while `eth_getBalance` against `latest` on the same nodes succeeds — because a
tip-bound read resolves to block number 0 and skips the gate. Chain-id probes
are also how eRPC validates an upstream, so the same sentinel can put a healthy
node out of service.

The gate needs to distinguish "no block dependency" from "block one". Reading
the block *reference* (`*`) rather than the number is enough.

**The fix.** `evm.MethodHasNoBlockDependency` (`architecture/evm/block_ref.go`)
answers from the same `CacheMethodConfig` that produced the sentinel: a method
marked `finalized` or `realtime` has no block dependency, so no upstream can be
missing its block. Both gates call it and return early.

Asking the config, rather than decoding the number 1, is the point. A check for
`blockNumber == 1` would re-encode the sentinel in a second place, and a check
for `blockRef == "*"` would be wrong — `*` also means "several differing block
params" for `eth_getLogs`, where the block dependency is real.

`TestBlockAvailability_StaticMethodIsNotGatedByTheCacheSentinel`
(`erpc/networks_boundary_test.go`) pins both halves, and asserts that a genuine
historical read below the bound is STILL refused, so the fix cannot pass by
disabling the gate. Proven by reverting the fix: the test fails on both
methods. Full `make test-fast` green on 2026-08-21.

## 73. The gRPC query surface ignores every `queryShim` limit

**Status:** open. **Severity: medium.** A cost and blast-radius control that
covers only half the traffic.

`upstreams[].evm.queryShim` carries `enabled`, `allowedMethods`,
`maxBlockRange`, `maxLimit`, `defaultLimit` and `concurrency`. The JSON-RPC
surface honours all of them: `architecture/evm/eth_query.go:74-91` gates on
`enabled` and `allowedMethods`, and `eth_query_helpers.go:361` reads the rest.

The BDS gRPC `QueryService` runs a second, independent shim in `erpc/` and
reads none of it. `erpc/query_shim.go:458` (`queryLimit`) hard-codes a default
of 100 and enforces no maximum; `erpc/query_executor.go:276`
(`resolveQueryBounds`) checks only `from <= to` and enforces no range cap.

What an operator sees: `maxBlockRange: 2` on an upstream, and a single gRPC
`QueryBlocks` for a 17-block range still issues 17 `eth_getBlockByNumber`
calls to that upstream — all billed, all against its rate budget. Setting
`enabled: false` does not turn the gRPC path off either.

`TestGrpcQueryBlocks_WalksAWiderRangeThanTheUpstreamsQueryShimAllows` and
`TestGrpcQueryBlocks_ServesTheQueryEvenWhenTheShimIsTurnedOff`
(`erpc/query_shim_limits_test.go`) pin both.

## 74. `shimQueryTraces` drops the cursor when a page ends on genesis

**Status:** open. **Severity: medium.** Silent short answer.

`erpc/query_shim.go:246`:

```go
var cursor *evm.CursorBlock
if hasMore && lastIncluded > 0 {
    cursor = cursorFromNumber(lastIncluded)
}
```

`lastIncluded` is a plain block number, and 0 is a real block. A query that
starts at `earliest` and fills its limit inside genesis therefore returns a
full page with **no cursor**. A client reads a missing cursor as "the range is
exhausted" and stops, so every block after genesis is dropped from the answer
with nothing said.

`shimQueryTransactions` (`:99`) does not have this problem — it guards on a
`*evm.BlockHeader` being non-nil, which separates "block 0" from "no block".
The same fix applies here.

`shimQueryTransfers` inherits it: it copies the trace page's cursor through
untouched (`:291`).

`TestShimQueryTraces_DropsTheCursorWhenItStopsOnBlockZero` and
`TestShimQueryTransfers_InheritsTheSameLostCursor`
(`erpc/query_shim_cursor_test.go`) pin it, and
`TestShimQueryTraces_CarriesACursorWhenItStopsOnABlockAboveGenesis` is the
positive control that isolates the block number rather than the pagination
rule.

## 75. Four dead methods on `networkExecutor`

**Status:** not a bug. **Severity: low.** Dead code, one of it a bypassed
wrapper. Closed as not a bug on 2026-08-21: the code cannot change what an
operator or a client sees, so there is nothing to repair. It stays here
because it still reads as live code. Deleting it in the fork would cost a
permanent rebase conflict and change nothing — see `valve/open-entry-
triage.md`.

`erpc/network_executor.go` declares `Timeout()` (`:112`), `HasHedge()`
(`:134`), `HasRetry()` (`:142`) and `shouldRetry()` (`:374`). None of them has
a single caller anywhere in the module. `networkExecutor` is unexported, so
nothing outside the package can reach them either.

`shouldRetry` is the interesting one. Its doc comment says "Returning true
causes the caller to emit a `network_retry_attempt_total{reason}` metric", but
`runRetry` (`:281`) calls `shouldRetryWithReason` directly and never goes
through the wrapper. The upstream-scope twin does use both — `upstreamExecutor`
calls its own `Timeout()` (`upstream/upstream.go:990`) and `shouldRetry()`
(`upstream/upstream_executor.go:201`) — so these four look like a copied
surface that was never wired.

Delete them, or wire `runRetry` through `shouldRetry` so the comment is true.

---

---

## 80. A TypeScript config with no exports panics instead of explaining itself

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** Bootstrap crash instead of a config error. (Another branch logs the
same defect as entry 63.) Pinned by
`TestLoadConfig_TypeScriptWithNoExportsExplainsItself`.

`loadConfigFromTypescript` (`common/config.go:3355`) called
`runtime.Exports().Get("default")`. `Runtime.Exports` (`common/runtime.go:58`)
was `r.vm.GlobalObject().Get("exports").ToObject(r.vm)`. A compiled module that
declares no exports leaves that global as JS `null`, and sobek's `ToObject`
raises a `TypeError` that unwinds out of the Go call as a panic.

The very next statement existed to catch exactly this mistake: it returns
"config object must be default exported from TypeScript code AND must be the
last statement in the file". It never ran for the no-export case, so an
operator who forgot `export default` read a Go stack trace with
`TypeError: Cannot convert undefined or null to object` instead of the sentence
that names the fix.

`export default undefined` and `export default null` DID reach the friendly
error, because those leave a real `exports` object behind. The difference is
invisible from the config file, which is what made the panic surprising.

The fix: `Runtime.Exports` returns nil for an absent, null or undefined
`exports`, and the caller reports the missing default export. Pinned by
`TestLoadConfig_TypeScriptWithNoExportsExplainsItself`,
`TestLoadConfig_TypeScriptWithAnEmptyDefaultExportExplainsItself` and the two
`TestRuntime_Exports_*` tests (`common/config_ts_exports_test.go`). Mutation:
reverting `common/runtime.go` fails `TestLoadConfig_TypeScriptWithNoExportsExplainsItself`
and all three subtests of `TestRuntime_Exports_ReturnsNilWhenThereAreNoExports`,
each with the original `TypeError` panic. The empty-default-export test keeps
passing, which is the point of entry 80: those two spellings always reached the
friendly error.

---

## 81. A shared PostgreSQL listener dies with its first watcher

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high.** Silent: later watchers of the same key receive nothing and report
nothing. Pinned by
`TestPostgreSQLConnector_WatchCounterInt64_AListenerSurvivesItsFirstWatcher`.

Found while fixing entry 47, and measured against a live container.

`getOrCreateListener` (`data/postgresql.go:936`) ran the listener goroutine on
the context of whichever watcher created it. That listener is shared by every
watcher of the key. So the first watcher cancelling its context stopped the
goroutine for all of them, and nothing removed the dead listener from
`p.listeners` — every later watcher of that key joined a listener that never
delivers a notification again.

A network torn down, an upstream removed, or a request-scoped watch ending is
enough to trigger it, and the fallback poller's 30-second tick hides it: the
value still moves, just late and only on the poll.

The listener now runs on `p.appCtx` and stops only when its last watcher
leaves. Pinned by
`TestPostgreSQLConnector_WatchCounterInt64_AListenerSurvivesItsFirstWatcher`
(`data/postgresql_listener_leak_test.go`): two watchers on one key, the first
one leaves with its context, and the second must still see a published value.
Mutation: with `data/postgresql.go` reverted the second watcher receives
nothing for 20 seconds and the test fails.

---

## 82. The missing-`evm` panic needs more than an omitted `evm` block

**Status:** not a bug — this entry corrects entries 41 and 43 rather than
reporting a defect of its own, and the correction still holds.
`UpstreamConfig.SetDefaults` still defaults an empty `type` to `evm` and still
creates an empty `Evm` block for any evm-prefixed type, so a plain YAML config
never reaches the panic.

Entries 41 and 43 say an operator who omits the `evm` block crashes eRPC at
bootstrap. The panic is real, but a plain YAML config does not reach it.
`UpstreamConfig.SetDefaults` (`common/defaults.go:1893`) defaults an empty
`type` to `evm` and then creates an empty `Evm` block for any evm-prefixed
type. Every config path runs it: project upstreams, provider overrides
(`ProviderConfig.SetDefaults`), and shorthand upstreams converted to providers.
`Provider.buildBaseUpstreamConfig` fills the block in again for any `evm:`
network id.

Two inputs still reach a nil `Evm` at a vendor:

- an override whose `type` is not evm-prefixed (`type: svm`), on a network id
  that is not `evm:`-prefixed — `TestProvider_GenerateUpstreamConfigs_AVendorPanicBecomesAnError`
  drives exactly this through the real provider path;
- any caller that builds an `UpstreamConfig` without `SetDefaults` — every
  vendor unit test in `thirdparty/`, and any embedder using eRPC as a library.

So the severity is right for a library caller and overstated for a YAML
operator. The fix in entry 43 covers both, and does not depend on which one
an operator hits.

---

## 83. `go vet ./...` fails at HEAD on a test helper

**Status: FIXED in the fork.** Upstream still carries it. The fix is in the
fork's own test helper (`erpc/query_executor_test.go:342`), and `go vet ./...`
now exits 0 with no findings. No test pins this fix.

`newTestUpstreamsRegistry` built a `*sync.Map`, stored one entry in it, then
passed `*atomicMap` to `setUnexportedField`. Dereferencing the pointer copies
the map's internal lock, which `go vet`'s copylocks check rejects:
`call of setUnexportedField copies lock value: sync.Map contains sync.noCopy`.
It is the only vet finding in the repo, and it predates this work.

The helper now takes the address of the target field and calls `Store` through
it, so nothing copies the lock. `TestQueryBlocks_*` and `TestQueryLogs_*` still
pass, and `go vet ./...` is clean.

---

# Bugs in the FORK's own code — ours to fix, not upstream's

## F1. The chain-family cordon latch is one-shot in both directions

**Status: FIXED in the fork.** This was the fork's own code, so upstream never
carried it. Cordon is now level-triggered against the upstream's live state:
`apply` asks `u.CordonedReason("*")` at
`upstream/chain_family_bootstrap.go:285` instead of testing a latch. Uncordon
stays edge-triggered on ownership through
`cordonedByProbe.CompareAndSwap(true, false)` (`:272`), so a recovery lifts
only a cordon the poller placed. Pinned by
`TestChainProbePoller_ReCordonsAFailingNodeAfterSomethingUncordonsIt`.

`upstream/chain_family_bootstrap.go:226` guarded the cordon with
`cordonedByProbe.CompareAndSwap(false, true)`. That call is gone. The cordon now
reads the upstream's live state at `:285`, and only the uncordon keeps a CAS
(`:272`).

The intent was to make the UNCORDON side idempotent, so the poller does not
fight an operator. The cordon side inherited the same latch.

Once the poller has cordoned a node, an operator who manually uncordons it
leaves the latch set. Every later probe then fails the CAS and returns without
cordoning, however far behind the node falls.

So a chain-family node uncordoned once during an incident stays in rotation
serving stale reads indefinitely. It regains protection only after it becomes
healthy — which clears the latch — and then degrades again.

This defeats the brief's "do not build a silent balancer" requirement, and it
is our code, so it does not go upstream. Fix it in the fork.

---

# Fixes the fork already carries that upstream still needs

These are working patches, not just reports. Each is a candidate pull request.

## E. `HasErrorCode` now follows a plain `%w` chain

**Status: FIXED in the fork.** Upstream still carries it. `HasErrorCode`
(`common/errors.go:2597`) now walks a plain `Unwrap() error` link (`:2628`), and
the `StandardError` branch no longer returns early, so the walk continues past a
plain link inside an eRPC cause chain. Pinned by
`TestHasErrorCode_FollowsAPlainWrapChain`.

Two hunks, twelve lines. The new branch handles a plain `fmt.Errorf("%w", …)`
wrapper at the top of the chain. Dropping an early `return` handles the same
hole one level down, because `BaseError.HasCode` stops at a plain link inside
its own cause chain — without it, the defect just moves deeper.

The change is monotone: it can only turn `false` into `true`, and only when a
matching code is genuinely in the chain. All 157 non-test call sites were
traced.

It closes three consumers, not two:

- The nonce-exception gate (log entry 19) — no more re-broadcast.
- `mapToGRPCStatus` (log entry 35) — a wrapped eRPC error maps to its real
  `codes.*` instead of `codes.Internal`.
- **`classifyProbeErr` (`internal/policy/prober.go:456`)** — nobody was looking
  at this one. It tests four codes against errors the chain families build with
  `fmt.Errorf("evm probe: %w", err)`, so before the fix it could never match any
  of them and labelled **every** probe failure `"error"`. It now labels timeout,
  auth, throttled and skipped correctly. That function has no test, so nothing
  pinned the wrong labels.

`BaseError.HasCode` itself is deliberately untouched. `DeepSearch`, `CodeChain`
and `DeepestMessage` share the same stop-at-a-plain-link shape, so fixing the
method is one coherent follow-up rather than a piece to break off here.

## F. A blocked method echoes the caller's request id over HTTP

**Status: FIXED in the fork.** Upstream still carries it. One character:
`erpc/http_server.go:580` now reads
`if jrr, err := nq.JsonRpcRequest(); err == nil`. Pinned by
`TestHttpServer_ABlockedMethodEchoesTheCallersIdOverHttp`.

See log entry 28.

## A. Transport failures lose their cause identity

**Status: FIXED in the fork.** Upstream still carries it.
`NewErrEndpointTransportFailure` (`common/errors.go:2187`) wraps a
non-`StandardError` cause in `redactedCauseError` (`:2177`). That type renders
the redacted message from `Error()` and returns the untouched original from
`Unwrap()`, so `errors.Is` reaches the sentinel again. Pinned by
`TestErrEndpointTransportFailure_PreservesSentinelIdentity`.

`NewErrEndpointTransportFailure` stripped the endpoint URL by building a new
error with `errors.New`. That discarded the original object, so
`errors.Is(err, context.DeadlineExceeded)` was false — and the same for
`io.EOF`, `net.ErrClosed` and every other sentinel from the HTTP client. Code
that tried to tell a timeout from a closed connection could not.

The fork's fix renders a redacted message from `Error()` and returns the
untouched original from `Unwrap()`. It deliberately does not implement
`StandardError`, because `BaseError` already renders a non-`StandardError`
cause through `Error()` on every surface — so redaction is inherited on all
nine surfaces with no change to `BaseError` and no change to any caller.

Redaction got stronger, not weaker: it now writes a visible
`<redacted-endpoint>` placeholder instead of an empty string, so a test can
assert redaction happened rather than assert a secret is absent.

## B. A `null` error member is read as a failure

**Status: FIXED in the fork.** Upstream still carries it. Both parse sites
(`common/json_rpc.go:167` and `:376`) now skip a `null` error member through
`IsJsonNull` (`common/json_rpc_null_error.go:27`), and `ParseError` stays
untouched. Pinned by `TestJsonRpcResponse_NullErrorMemberIsNotAnError`.

JSON-RPC 1.0 requires both members on every response, so a success carries
`"error": null`. eRPC's parser only asked whether the member was **present**.
Four bytes of `null` went into `ParseError`, matched none of its shapes, and
fell through to "treat the raw data as the message".

Every successful bitcoind response arrived as
`ErrEndpointServerSideException` with the message `"null"`, and every btc
request exhausted its whole upstream pool. **EVM providers that send the member
were hitting this too.** No fixture in the repo carried it, so nothing caught
it.

The guard sits at the two parse sites that know they are reading an `error`
**member**, not inside `ParseError` — which also receives whole response bodies,
where a bare `null` is malformed rather than a success.

## C. The circuit breaker wedges open on an ignored outcome

**Status: FIXED in the fork.** Upstream still carries it. `Breaker.Record`
(`failsafe/breaker.go:187`) now decrements `halfOpenInflight` before it returns
on `OutcomeIgnore`, so the trial permit is released without counting the outcome
as a success or a failure. Pinned by
`TestBreaker_HalfOpenPermitReleasedOnIgnoredOutcome`.

The breaker returns early on `OutcomeIgnore` **without releasing the half-open
trial permit**. The breaker then stays open until the process restarts.

## D. A batch entry can be forwarded to the wrong chain

**Status: FIXED in the fork.** Upstream still carries it. The per-entry
goroutine at `erpc/http_server.go:488` takes `architecture` and `chainId` as
parameters instead of capturing them, so each batch entry resolves its own
network. Pinned by `TestBatchRoutesEachEntryToItsOwnNetwork`.

The per-batch-entry goroutine captured `architecture` and `chainId` by
reference, so an entry could be routed to another entry's chain.

## 66. The state poller dereferences a nil response it just tested for

**Status: FIXED in the fork.** Upstream still carries it. The split guards now
stand at `architecture/evm/evm_state_poller.go:1337` (`fetchBlock`, which
returns `(0, 0, nil)`) and `:1408` (`fetchSyncingState`, which returns an
`ErrEvmStatePoller` error). Pinned by
`TestFetchBlock_ANilAnswerReportsNoBlockInsteadOfPanicking` and
`TestFetchSyncingState_ANilAnswerIsAnErrorNotANotSyncingClaim`.

Both `fetchBlock` and `fetchSyncingState` do this:

```go
jrr, err := resp.JsonRpcResponse()
if err != nil { return 0, 0, err }
if jrr == nil || jrr.Error != nil {
    return 0, 0, jrr.Error        // <- nil pointer dereference when jrr == nil
}
```

`NormalizedResponse.JsonRpcResponse` returns `(nil, nil)` for a nil receiver
(`common/response.go:315`), so `jrr` is nil exactly when `Forward` answered
`(nil, nil)`. That pair is reachable: `Upstream.Forward` logs it by name —
"upstream request ended with nil response and nil error"
(`upstream/upstream.go:824`) — and then returns `nrs, nil` with `nrs` nil,
which `failsafeExecutor.Run` passes straight through.

The guard's own author meant to return the error member. Instead the poller
panics inside the `Poll` fan-out goroutine, which has no recover, so the
process dies. An operator sees eRPC exit with a SIGSEGV stack in
`(*EvmStatePoller).fetchBlock` and no upstream named in the log.

The same shape in the three availability probes
(`evm_state_poller.go:1614`, `:1662`, `:1712`) is written safely — it returns
`(false, false, nil)` — so the two poll helpers are the odd ones out.
`TestCheckProbe_ANilAnswerReadsAsNotAvailableAndNotAsUnsupported` pins the
safe form.

**Fix.** Each guard now splits the nil case from the error case, and each
returns what its own signature can express honestly.

`fetchBlock` returns `(0, 0, nil)` — the same triple a `null` result produces.
The two callers already handle that pair exactly: `err != nil || blockNum == 0`
counts a failed poll and latches `skipLatestBlockCheck` /
`skipFinalizedCheck` after ten of them. So the fix adds no new path.

`fetchSyncingState` returns an `ErrEvmStatePoller` error. It has no neutral
value: `false` claims the node is fully synced, and the poller learned nothing.
Its caller counts the failure and logs it.

Pinned by `TestFetchBlock_ANilAnswerReportsNoBlockInsteadOfPanicking` and
`TestFetchSyncingState_ANilAnswerIsAnErrorNotANotSyncingClaim`
(`architecture/evm/evm_state_poller_nil_response_test.go`). Both drive the real
helpers through a `forwardingUpstream` that answers `(nil, nil)`, and both
produce a real SIGSEGV against the unfixed code.

## 67. `Cache.Set` dereferences a response that `shouldCacheResponse` handles as nil

**Status: FIXED in the fork.** Upstream still carries it. `GetResultBytes`
and `ResultLength` now decide the nil receiver on the type, so both call sites
and every future one are safe, and the guard inside `shouldCacheResponse` is
deleted. The fix and its tests are recorded under 134, which also corrects the
assumption below: the caller's `if resp != nil` does NOT prove `rpcResp != nil`,
because a released response also reads as `(nil, nil)` (entry 76).

`shouldCacheResponse` documents and guards the nil case explicitly
(`json_rpc_cache.go:1181-1189`: "both arguments can arrive nil together").
Its caller does not. With `resp == nil` and a policy whose empty behaviour is
`only` or `allow`, `shouldCacheResponse` returns `true`, and the very next
line runs `rpcResp.GetResultBytes()` on a nil `*JsonRpcResponse`.

The only production caller guards with `if resp != nil`
(`erpc/networks.go:2395`), so it cannot fire today — but that caller also
wraps the goroutine in a recover, which is what would keep the process alive
if it ever did. Either the guard at 818 is missing or the guard inside
`shouldCacheResponse` is unnecessary; the two disagree.

## 68. Four dead branches found while raising `architecture/evm` coverage

**Status:** not a bug. All four branches are still dead, and each still
reads as an active guard. They stay upstream candidates: today's behaviour
is correct, so the only cost is the false impression that a guard is live.
Closed as not a bug on 2026-08-21: the code cannot change what an operator
or a client sees, so there is nothing to repair. It stays here because it
still reads as live code. Deleting it in the fork would cost a permanent
rebase conflict and change nothing — see `valve/open-entry-triage.md`.

1. **`json_rpc_cache.go:565-586`** — the `CacheEmptyBehaviorIgnore` arm of the
   post-fan-out emptiness switch cannot run. Every winner is filtered by the
   identical condition inside the fan-out goroutine at `:375`
   (`jrr.IsResultEmptyish() && policy.EmptyState() == Ignore` reports a miss
   and returns), so no emptyish-under-ignore result ever reaches `:563`.
   Five statements of miss telemetry that never fire.
2. **`evm_state_poller.go:364-366`** — the `else` arm of the syncing-failure
   handler. It runs only when the local `skip` is true, but `skip` is read at
   `:323` and the function returns at `:335` whenever it is true. The
   `if !skip` at `:359` is therefore always taken.
3. **`eth_getLogs.go:280-282`** — capping the split threshold by
   `getLogsMaxAllowedRange` can never change the outcome. The cap only applies
   when `threshold > maxRange`, and a split needs `requestRange > threshold`,
   which implies `requestRange > maxRange` — already rejected with an error at
   `:254`.
4. **`eth_getLogs.go:225`** — `topicCount = 1` for a scalar `topics[0]` is
   unobservable. The only reader is `topicCount > maxTopics` with
   `maxTopics > 0`, so a count of one can never reject.

Also confirmed unreachable, and correctly documented as such in the source:
`evm_state_poller.go:1793`, `:1799`, `:1815` and `:1839` (`binarySearchEarliest`'s pass-through
of a `checkProbe` error no probe implementation produces).

---

---

# Redundant guards — not defects, recorded so they are not re-derived

Each of these is shadowed by another check, so a single-line mutation of it is
unobservable. They are not bugs, and a test cannot pin them.

- `consensus/rules.go:932` — the unassailable-lead short-circuit declines while
  `preferNonEmpty` is set and the leading group is empty. The very next check at
  `:936` already declines for any leader that is not non-empty, and both read the
  same `best`. Unobservable.
- `consensus/rules.go:103-105` — `prefer-highest-value-for` floors the agreement
  threshold at 1. A response group always holds at least one response, so
  `count < threshold` is already false for every threshold at or below 1. The
  floor changes nothing.
- `consensus/executor.go:1019` — empty-round guard, shadowed by `:1063`.
- `consensus/executor.go:1200` — `continue`, unreachable given the guard at `:1205`.
- `auth/authorizer.go:126` — empty-budget guard, shadowed by
  `upstream/ratelimiter_registry.go:225`.
- `upstream/chain_family_bootstrap.go:210` — `probe.Tip > 0`, shadowed by
  `health/tracker.go:1367`.
- `architecture/evm` — `enforceHighestBlock`'s tag guard, shadowed by the
  switch's `default` branch.
- `clients/http_json_rpc_client.go:233` and `:341` — duplicate cancelled-request
  checks; `architecture/svm/handler.go:63` and `hooks.go:703` likewise. Both
  pairs are deliberate defence in depth.
- `data/grpc.go:190-193` — the duplicate-server guard in the bootstrap task.
  Dropping it lets the second server overwrite the first in
  `clientByNetwork`, and both outcomes leave exactly one client that a caller
  cannot tell apart. `TestNewGrpcConnector_TwoServersForOneChainLeaveOneClient`
  covers the branch; no assertion can kill a mutation of it.
- `data/redis_pubsub_manager.go:193` — the ping health check in
  `reconnectPubSub`. Removing it does not install a subscription on a dead
  redis, because the `Receive` confirmation at `:203` fails and the cleanup at
  `:207-211` clears `m.pubsub` anyway. The check saves a round trip; it does
  not decide the outcome.
- `common/config.go:1117` and `common/config.go:2389` — the unknown-field guard
  before each legacy-shape fallback. The legacy struct is a subset of the
  strict one, so an unknown key fails both decodes and `return originalErr`
  after the fallback produces the same error. Deleting either guard alone
  changes nothing.
- `common/defaults.go:1462` and `:1402` — the "failed to convert upstream
  (id: %s) to provider" wrap, written twice on the same error path: once inside
  `convertUpstreamToProvider` and once around the call. Removing either alone
  leaves the operator with the same message; removing both drops the upstream
  id, which
  `TestConvertUpstreamToProvider_UnknownVendorAbortsTheLoad` catches.
- `common/request.go:279` and `:289` — the nil-receiver guards in
  `NormalizedRequest.CreditUnitsTotal` and `CreditUnitsByVendor`. `ExecState()`
  already returns nil for a nil request, and `ExecState.CreditUnitsTotal`
  (`common/exec_state.go:176`) and `CreditUnitsByVendor` guard their own nil
  receiver, so removing either outer guard changes nothing an assertion can see.
- `common/json_rpc.go:551-553` — the `else` arm that logs `errBytes` as a plain
  string when it is not semi-valid JSON. `ParseError` (`common/json_rpc.go:421`)
  is the only writer of `errBytes` and writes only after the payload already
  parsed into a JSON-RPC error object, so those bytes always start with `{`.
  The arm is unreachable, not untested.
- `common/defaults.go:1057` and `:1070` — `f.MatchMethod = "*"` for connector
  failsafe policies, shadowed by the same default inside
  `FailsafeConfig.SetDefaults` (`common/defaults.go:2619`).
- `common/request.go:1225`, `:1228`, `:1234`, `:1237`, `:1249`, `:1255`,
  `:1270`, `:1273`, `:1276`, `:1282` — the `curl`, `wget`, `insomnia`,
  `httpie`, `java`, `ruby`, `edge`, `viem`, `ethers` and `alchemy` branches of
  `simplifyAgentName`. For each client's canonical user agent the generic
  first-word fallthrough already yields the same label, so the branch only
  matters for the non-canonical spellings (`libcurl-agent/1.0`,
  `GNU Wget/…`). Under the repo's design razor the fallthrough is the primary
  path and these are optimisations, correctly.

---

# Not a bug — recorded so it is not "fixed" by mistake

- **`eth_sendRawTransaction` is deliberately absent** from
  `IsNonRetryableWriteMethod` (`architecture/evm/util.go:7`). Idempotency
  handling exists: `architecture/evm/eth_sendRawTransaction.go`, the
  `idempotentTransactionBroadcast` config flag (`common/config.go:2588`), and a
  purpose-built "already in mempool or on-chain" error (`common/errors.go:3022`).
- **`simulateTransaction` is deliberately absent** from SVM's non-retryable
  write list (`architecture/svm/util.go`).
- **`NormalizedResponse.MarshalJSON` always fails once parsed**
  (`common/response.go:468`). Raw result bytes must reach the client verbatim
  via `WriteTo`. This is a design fact.
- **`MemoryConnector.WatchCounterInt64` is an inert no-op** (`data/memory.go:222`).
  Single process, no pub/sub needed. Every current caller selects with a timeout.
- **The BDS maintainer recycles the FIRST over-age conn each tick**
  (`clients/grpc_bds_resilience.go:277`). If a freshly dialled conn were
  already over-age, slot 0 would be recycled every tick and later slots never.
  It cannot happen: `bdsConnMaxAge` is 5 minutes and `bdsMaintainInterval` is
  60 seconds, so a replacement always survives several ticks. Recorded because
  the one-per-tick cap looks like starvation until those two constants are read
  together. `TestMaintainLoop_AgeRecyclesAtMostOneConnPerTick` pins the cap.

---

# Found while raising coverage — real defects, not "not a bug" items

## 69. A consensus round can return neither a response nor an error

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** The caller got `(nil, nil)`. Pinned by
`TestConsensus_PayloadFreeParticipantsDoNotProduceANilNilAnswer` and
`TestRule_LowParticipantsAcceptMostCommonServesTheRealError`.

`consensus/rules.go:740-742` — the "low participants + accept-most-common"
rule returns the best error group's `FirstError`:

```go
if bestError := a.getBestError(); bestError != nil {
    return &slotResult{Error: bestError.FirstError}
}
```

`getBestError` (`consensus/analysis.go:324-344`) ranks **infrastructure-error**
groups alongside consensus-valid ones. An infrastructure-error group does not
always carry an error. `classifyAndHashResponse` (`analysis.go:499-504`) files a
participant whose `inner` returned `(nil, nil)` — no payload, no failure — as
`ResponseTypeInfrastructureError` with the hash `"error:generic"`, and the
grouping loop at `analysis.go:119-133` only sets `FirstError` from a member with
`r.Err != nil`. A group made only of such responses has `FirstError == nil`.

When that group outranks every consensus-valid group, the rule returns
`&slotResult{Error: nil}` with no `Result`. `enforceWinnerComposition` passes it
through (`groupOf` finds no backing group), and `(*executor).Run`
(`consensus/executor.go:167`) returns `out.Result, out.Error` — `(nil, nil)` — to
the network layer.

An operator sees a request that produced no response body and no error, with no
consensus dispute or low-participants error to explain it. The same round with
one fewer result-less participant answers normally, so it looks intermittent.

**Correction to the trigger.** `inner` returning `(nil, nil)` cannot reach the
analysis: `executeParticipant` (`consensus/executor.go:792`) filters that pair
and sends a bare `nil` down the response channel. The reachable trigger is the
other half of the same branch — a participant that returns a NON-nil
`*NormalizedResponse` whose `JsonRpcResponse` is `(nil, nil)`.
`classifyAndHashResponse` has an explicit arm for it (`analysis.go`, the
"Successful response" block, `jr == nil`), and `NormalizedResponse.Release`
produces exactly that shape: it frees the parsed payload and stores `nil` over
the cached pointer, after `parseOnce` has already run. The consensus executor
releases responses itself.

**Fix.** `getBestError` now skips any group whose `FirstError` is nil. The
function's single caller returns `FirstError` straight to the client, so a group
with no error is never a useful answer. Skipping it lets the rule serve a real
error from a smaller group, and leaves the low-participants branch at
`rules.go:743-745` reachable when no group holds an error at all. The fix adds
no branch — it makes one that was already written reachable.

Rejected alternative: making the rule fall through when `FirstError` is nil. It
answers "not enough participants" even when the round really saw an execution
revert, so it discards an error the operator can act on.

Pinned by `TestConsensus_PayloadFreeParticipantsDoNotProduceANilNilAnswer`
(`consensus/executor_nil_winner_test.go`), which drives the whole executor and
asserts `Run` does not answer `(nil, nil)`, and by
`TestRule_LowParticipantsAcceptMostCommonServesTheRealError`
(`consensus/rules_infra_group_test.go`), which was the old pin recording the
defect and now records the fix.
`TestRule_AllParticipantsAnsweredWithNothingReportsLowParticipants` covers the
neighbouring rule (`rules.go:808-816`), which handles the same group shape
correctly.

---

## 70. `BootstrapTask.Wait` busy-spins on a full core and ignores its context

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
low.** Wasted CPU, and an uncancellable wait. Fixed with entry 157 — same
function, and 157's fix needs this one to be safe.

`util/initializer.go:111-117`:

```go
ch := t.doneVal.Load()
if ch == nil {
    if state != TaskPending {
        continue          // no context check, no sleep, no yield
    }
    select {
    case <-ctx.Done():
        return ctx.Err()
    case <-time.After(10 * time.Millisecond):
        continue
    }
}
```

`attemptRemainingTasks` swaps a task to `TaskRunning` at `:324` and publishes
the attempt's done channel at `:329`. A `Wait` that reads the state between the
two finds `state == TaskRunning` with `doneVal == nil` and takes the bare
`continue`. That path never checks `ctx.Done()` and never yields, so the
goroutine spins at 100% of one core until another goroutine publishes the
channel or moves the task out of `TaskRunning`. A cancelled context does not
end it; the sibling `TaskPending` branch two lines below does check.

In production the window is nanoseconds, so the cost is a few wasted
iterations. It is unbounded only in principle — nothing in the type enforces
that the two writes stay adjacent, and `Wait` is called from the request path
via `ExecuteTasks`.

**The fix.** Both no-progress paths now use the one ctx-aware sleep. There are
two of them since 157: the branch this entry describes, and a second where
`Wait` loops past an attempt's end and finds `doneVal` still holding the closed
channel it just consumed. Selecting on an already-closed channel returns
instantly, so that route spins exactly the same way. `Wait` remembers the
channel it consumed and sleeps until a new one is published.

Pinned by `TestBootstrapTask_WaitInTheNoChannelWindowHonoursACancelledContext`,
which replaces the old `…WaitBusySpinsAndIgnoresItsContextBeforeTheDoneChannelExists`.
That test was written to be flipped and said so. Its comments cited "bug 60";
the entry is 70, and the citation is corrected.

---

## 71. `PostgreSQLConnector.List` can never hand out a next-page token (DUPLICATE of 24 — independently rediscovered, kept as corroboration)

**Status:** open. **Severity: medium.** Silent truncation. The duplicate claim
holds: entry 24 reports the same unreachable `hasMore` probe in the same
function.

`data/postgresql.go:1368-1426`. The query asks for `limit+1` rows so the code
can tell whether another page exists. The scan loop then consumes that probe row
itself:

```go
for rows.Next() {
    if count >= limit {
        break            // the (limit+1)-th row has already been consumed
    }
    ...
    count++
}
...
if count == limit {
    hasMore := false
    for rows.Next() {    // no rows are left — the probe row is gone
        hasMore = true
        break
    }
    if hasMore { ... nextToken = ... }
}
```

`rows.Next()` advanced past the probe row before the `break`, so the second loop
asks for row `limit+2`, which the `LIMIT` never fetched. `hasMore` is therefore
always false and `nextToken` is always `""`. Statements `:1411-1413` and
`:1416-1422` are unreachable.

A caller paging a cache index reads the first `limit` entries and is told the
listing is complete. Everything past the first page is invisible, with no error
and no log line. The offset token itself works — `List` decodes and applies it
correctly — so a caller that builds its own token can page; nothing in eRPC
does, because nothing is ever handed one.

Fix: record the probe row when the scan loop breaks (`hasMore = true` at the
break) instead of re-reading the cursor afterwards.

Pinned by
`TestPostgreSQLConnector_DatabasePaths/List_truncates_at_the_limit_and_never_offers_a_next_page`.

---

# Hazards — safe today, only because every caller happens to cooperate

## H1. `GetNetworkUpstreams` returns aliased memory on one path and a copy on the other

**Status:** open. The hazard is unchanged, and it is still only a hazard.

`upstream/registry.go:412-439`. The lock-free fast path returns the stored
slice itself:

    if v, ok := u.networkUpstreamsAtomic.Load(networkId); ok {
        if arr, ok2 := v.([]*Upstream); ok2 {
            return arr          // the registry's own backing array
        }
    }

The fallback path returns a fresh copy (`cp`). So the same function hands back
either shared or owned memory, and which one you get depends on whether the
snapshot is warm — that is, on timing.

All 19 production callers only range over the result or copy it out, so
nothing is broken today. But a future caller that sorts the result in place
(`sort.Slice(ups, ...)`, the obvious way to rank upstreams) would reorder the
registry's shared snapshot for every other reader, concurrently and without a
lock. Nothing in the signature or the doc comment warns about it.

Weaken it by making the contract single-valued: copy on the fast path too, or
return a read-only type. Do not rely on callers staying polite.

Found while fixing a `go vet` failure in `erpc/query_executor_test.go`, which
copied a populated `sync.Map` over the registry's field. That fixture bug is
fixed; this one is upstream's.

# Polyglot live run, and the work that followed it

Entries **90 to 94** came from a live run on 2026-08-17, with one eRPC process
serving Ethereum mainnet, Solana mainnet-beta and Bitcoin mainnet at once. Full
run: [polyglot-live-run.md](polyglot-live-run.md). Config:
[polyglot-live-pool.yaml](polyglot-live-pool.yaml).

The header used to read "entries 90–94" and the section now holds 36 entries.
Later sessions appended here rather than starting a section of their own, so
everything after 94 came from ordinary code reading, NOT from the live run. Do
not cite this header as evidence that an entry was reproduced against a live
chain — check the entry itself.

## 90. `erpc/chain_families.go` says btc cannot serve, and btc serves

**Status:** open. The comment is still in the file, and it is now wrong twice
over: entry 91 fixed `IsValidNetwork`, so the whole paragraph must go, not just
its first two sentences (`erpc/chain_families.go:22-27`).

The file's SCOPE note reads:

> WHAT IS STILL NOT REGISTRY-DRIVEN — a btc UPSTREAM does not bootstrap.
> `Upstream.detectFeatures` (`upstream/upstream.go`) recognises only evm and
> svm and rejects everything else, so a btc upstream never reaches the pool and
> no btc request is ever forwarded.

That is no longer true. `detectFeatures` now ends in an `else` branch that calls
`detectChainFamilyFeatures` (`upstream/upstream.go:1410`), which is the
registry-driven path. In the live run five btc upstreams bootstrapped and
answered `getblockchaininfo`, `getblockhash` and `getblock` from real Bitcoin
nodes.

The comment matters because it tells the next reader not to bother. Anyone
adding a fourth family reads it and concludes the seam is unfinished.

The second claim in the same paragraph is now false too. It says
`common.IsValidNetwork` (`common/validation.go`) still knows only evm and svm.
Entry 91 fixed that function, and the function does not live in
`common/validation.go` — it is `common/network.go:114`, and it asks the family
registry. A reader concludes that a btc network cannot be named in a provider's
`onlyNetworks` list, which is no longer true.

**Fix:** delete the whole paragraph. Both of its claims are stale.

## 91. `IsValidNetwork` enumerates two architectures, so a provider cannot name a third

**Status: FIXED in the fork.** Upstream still carries it. Inherited from
upstream, where the function sits at `common/network.go:95`; the fork's fixed
version starts at `common/network.go:114`. Pinned by
`TestIsValidNetwork_AsksTheRegistryNotAHardCodedList`,
`TestIsValidNetwork_KeepsRejectingANonPositiveChainId` and
`TestIsValidNetwork_AcceptsTheBuiltinFamilies`.

The fix separates the two questions the function was conflating. The chain
family owns the ID shape, so `IsValidNetwork` now asks the registry through
`util.IsValidNetworkId` — the same property `IsValidArchitecture` twelve lines
above already had. Config policy stays behind: EVM still requires a positive
chain id, because `util.IsEvmNetworkIdBody` accepts a negative integer on
purpose (see its comment) and delegating outright would start loading
`evm:0` and `evm:-1`.

Pinned by three tests in `common/network_validation_test.go`. The registry test
registers a FAKE family rather than naming btc: `common` cannot import
`architecture/btc` without a cycle, and pinning one real family would test the
wrong thing — what matters is that any registered family is accepted, including
the next one nobody has written. Three mutations staged, three detected:
restoring the enumerated prefix match, dropping the chain-id policy, and
loosening it to `>= 0`.

`IsValidNetwork` matches the `evm:` prefix, then the `svm:` prefix, then
returns false:

```go
func IsValidNetwork(network string) bool {
	if strings.HasPrefix(network, "evm:") { ... }
	if strings.HasPrefix(network, "svm:") { ... }
	return false
}
```

Its two callers gate `providers[].onlyNetworks` and
`providers[].ignoreNetworks` (`common/validation.go:973`, `:980`). So a
registered, probeable, routable family is refused at config load the moment an
operator names it in a provider filter. Reproduced:

```yaml
providers:
  - vendor: drpc
    onlyNetworks:
      - btc:mainnet
```

```
failed to load configuration: project.*.providers.*.onlyNetworks.*
'btc:mainnet' is invalid must be like evm:1
```

This is the ONLY thing in the whole polyglot exercise that needs Go rather than
configuration. Everything else — `IsValidArchitecture`, `IsValidNetworkId`,
`Network.prepareRequest`, the client factory, `detectFeatures` — is already a
registry lookup.

It is also an unforced commitment in the razor's sense. The registry already
knows every served architecture, and `ChainFamily.ValidateNetworkId` already
owns the per-family id shape. The function re-derives from a hand-written list
what the registry can answer.

**Fix:** split the network id on its first `:`, look the prefix up with
`LookupChainFamily`, and ask the family to validate the body — the same two
steps `util.IsValidNetworkId` already takes. The evm and svm branches then
become the families' own methods, and no architecture name stays in this file.

## 92. A prober mirror is indistinguishable from client traffic in the upstream counters

**Status:** open. Fork code (`internal/policy/prober.go:414`). The counter path
still cannot carry the label: `Tracker.RecordUpstreamRequest`
(`health/tracker.go:943`) takes no composite argument at all.

One client `getblockhash` produced two upstream calls. The second went to
`btc-onfinality`, which the selection policy had EXCLUDED for being in the
`tier:fallback` tier. It was a prober mirror — `Prober` re-samples excluded
upstreams so they can heal — not routing. But it lands in the counters looking
exactly like routing:

```
erpc_upstream_request_total{agent_name="curl",attempt="2",composite="none",
  upstream="btc-onfinality"} +1
erpc_upstream_selection_total{reason="primary",upstream="btc-onfinality"} +1
erpc_upstream_attempt_outcome_total{outcome="success",
  upstream="btc-onfinality"} +1
```

Three problems in those three lines. The probe carries the CLIENT's
`agent_name`, so it cannot be filtered out by agent. It carries
`composite="none"`, so it cannot be filtered out by composite either. And
`reason="primary"` says the excluded upstream was chosen first, which is the
opposite of what happened.

Only `erpc_upstream_request_duration_seconds` gets the honest label:
`RecordUpstreamDuration(u, method, duration, isSuccess, "probe", ...)` passes
`"probe"` as the composite type. The counters take a different path and never
see it.

The consequence is not cosmetic. An operator watching `selection_total` reads
"my fallback tier is taking primary traffic" and removes the tier, which is
exactly wrong — the tier was working.

**Fix:** carry the same `"probe"` composite value into `RecordUpstreamRequest`
and the selection counter, or give `erpc_upstream_selection_total` a
`reason="probe"`. One label value makes the row filterable everywhere instead
of in one histogram.

## 93. In-request failover never increments `erpc_network_retry_attempt_total`

**Status:** open. Inherited (`telemetry/metrics.go:681`). The counter still has
one increment site, `erpc/network_executor.go:312`, inside the network-scope
retry loop, and no transport-driven rotation reaches it.

I killed the upstream eRPC had chosen and drove 8 more requests. All 8 failed
over to the second upstream inside the same client request:

```
erpc_upstream_request_errors_total{error="ErrEndpointTransportFailure",
  upstream="evm-shim-b"} 8
erpc_network_successful_request_total{attempt="2",upstream="evm-shim-a"} 8
```

`erpc_network_retry_attempt_total` has **no samples at all** after that run.
Prometheus only publishes a counter once it is incremented, so an absent series
means it never fired. `erpc_upstream_attempt_outcome_total` agrees: the
successful second attempt carries `is_retry="false"`.

The counter's own `reason` vocabulary says it should have fired. Its declared
reasons are `empty_result / pending_tx / retryable_error / block_unavailable /
missing_data`. A dead socket is a retryable error, eRPC classified it as
`ErrEndpointTransportFailure` with `severity="critical"`, and it did advance to
the next upstream. Nothing was counted.

So the response-driven retry paths feed this counter and the transport-driven
rotation does not, while the name promises both. An alert written as "page me
when retries spike" stays silent through a total upstream outage. The signals
that DID move are `erpc_upstream_request_errors_total` and the `attempt` label
on `erpc_network_successful_request_total`.

**Fix:** increment it with `reason="retryable_error"` when the loop advances
after a transport failure. Failing that, say in the Help text that transport
failover is excluded — today nothing warns a reader that a rotation is not a
retry.

## 94. `upstream="n/a"` is three different events, and only one is a cache hit

**Status:** open. Inherited. Semantics already pinned in
`telemetry/metrics_semantics_test.go:53`. No `served_by` label exists yet, and
`getSlot` still has no pre-forward short-circuit, so the open question stands.

`upstream="n/a"` is widely read as "served from cache". In this run no cache
existed — the config has no `database:` block — and three separate methods
still recorded it:

```
erpc_network_successful_request_total{attempt="0",category="eth_chainId",
  network="evm:1",upstream="n/a"} 16
erpc_network_successful_request_total{attempt="0",category="getGenesisHash",
  network="svm:mainnet-beta",upstream="n/a"} 1
erpc_network_successful_request_total{attempt="0",category="getSlot",
  network="svm:mainnet-beta",upstream="n/a"} 1
```

Two have a confirmed cause: architecture pre-forward hooks answer them inside
the process and never call an upstream —
`projectPreForward_eth_chainId` (`architecture/evm/hooks.go:38`) and
`projectPreForward_getGenesisHash` (`architecture/svm/hooks.go:143`).

The third, `getSlot` with `commitment: finalized`, has no pre-forward
short-circuit in `architecture/svm/handler.go`. The likely cause is
`networkPostForward_getSlot` (`architecture/svm/hooks.go:684`), which enforces
the network's highest known slot and can replace the response. **I did not
confirm that**, and I record it as an open question rather than a conclusion.

The existing test already states the rule correctly: `"n/a"` means "the
upstream was not resolved for this event". The gap is that a hook answer, a
cache hit and an unbootstrapped upstream all collapse into that one value, so
no consumer can tell them apart. A cache hit-rate computed from this label
overcounts by however many hook short-circuits the traffic contains — for
`eth_chainId`, that is 100%.

**Fix:** distinguish the local-answer case. A `served_by="hook"` /
`"cache"` / `"upstream"` label, or a separate counter for hook
short-circuits, would make the three readable apart. Until then, no dashboard
should derive cache hit-rate from `upstream="n/a"`.

---

## 76. A released `NormalizedResponse` reads as `(nil, nil)`, exactly like a nil one

**Status:** open. **Severity: medium.** `common/response.go:309` and `:488`.

`NormalizedResponse.JsonRpcResponse` returns `(nil, nil)` for a nil receiver.
It returns the same pair for a NON-nil receiver that has been released:

    r.releaseOnce.Do(func() {
        ...
        r.jsonRpcResponse.Store(nil)      // response.go:503
        ...
    })

`Release` stores nil over the cached pointer. A later read finds nothing
cached, and `parseOnce` has already run, so `parseOnce.Do` is a no-op and the
function returns `r.jsonRpcResponse.Load(), nil` — nil, and no error saying
why.

Two callers therefore cannot tell "no response" from "the response was freed
under me". `classifyAndHashResponse` (`consensus/analysis.go`) files it as an
infrastructure error with no error attached, which is the shape that produced
bug 69. The state poller dereferenced it, which was bug 66.

The ordering matters: releasing BEFORE the first parse leaves `parseOnce`
armed, so the next read parses a nil body and gets a real "no body available to
parse" JSON-RPC error. Releasing AFTER the first parse produces the silent nil.
The same object answers differently depending on when it was released.

Weaken it by making the released state say so: return a typed
"response already released" error instead of `(nil, nil)`. Then every existing
`if err != nil` guard catches it, and no caller has to add a nil check it
currently forgets.

Found while fixing bugs 66 and 69, which are both consequences of this.

## 77. `(*executor).Run` still has one silent `(nil, nil)` route

**Status:** open. **Severity: low.** `consensus/executor.go:164-166`.

    out := e.executeConsensus(...)
    if out == nil {
        return nil, nil
    }

`executeConsensus` returns `outcome.winner`, and `runAnalyzer` assigns
`winner` on the same two lines that assign `analysis`
(`executor.go:545-546` and `:578-579`). `determineWinner` never returns nil —
every rule action builds a `slotResult`, and the unmatched fallthrough builds a
dispute error. So the branch cannot fire today.

It is worth deleting anyway. It is the last route by which `Run` can hand the
network layer an empty body with no explanation, and it converts a future
regression — one new rule action that returns nil — into exactly the silent
outcome bug 69 was. Every other no-winner case in this package produces a named
error. An `ErrConsensusLowParticipants` here would cost nothing and would fail
loud instead.

Found while fixing bug 69.

---

## 95. `NewUpstream` hands a vendor its logger, then overwrites it

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** A data race in the bootstrap path. No test pins this fix.

`upstream/upstream.go:275` passed `&lg` into `common.GenerateVendorConfigs`.
A vendor may keep that pointer past the call: `AlchemyVendor.GenerateConfigs`
starts an async credit-unit refresh (`thirdparty/alchemy.go:297` ->
`thirdparty/remote_cache.go:191`) whose goroutine logs from it later.

Line 298 then rewrote `lg` IN PLACE to add the `vendorName` field:

    lg = pup.logger.With().Str("vendorName", pup.VendorName()).Logger()

So the refresh goroutine reads the `zerolog.Logger` struct while bootstrap
writes it. `go test -race ./erpc/` reports four races on adjacent fields of
that struct. It needs only a config with an Alchemy upstream.

The rewrite is a retro-fit: it adds a field to a logger whose pointer four
earlier lines (237, 251, 259, 275) already handed out. That works only while
no holder logs concurrently, and the async refresh broke that assumption.

Fix: the vendor call gets its own copy (`vlg := lg`), so nothing mutates a
value another goroutine holds. The retro-fit itself stays — see 97.

## 96. Concurrent network bootstrap corrupts the shared selection policy

**Status: FIXED in the fork**, and superseded by entry 131 (2026-08-19), which
stops `Network.Bootstrap` writing the config at all. Upstream still carries it.
**Severity was: high.** The mutex stays because `RegisterNetwork` is exported;
no production registration reaches it with a shared pointer any more.

**One premise below is unconfirmed:** "every network that leaves
`selectionPolicy` at the default arrives with the SAME
`*SelectionPolicyConfig`". I traced every config-load path and could not
reproduce it — see entry 164. The fix under 131 makes the question moot.

`internal/policy/default_policy.go` — `upgradeDefaultPolicy` rewrites three
fields on the config it is given:

    cfg.EvalFunc = DefaultPolicySource()
    cfg.EvalFuncOriginal = cfg.EvalFunc
    cfg.CompiledProgram = program

`Engine.RegisterNetwork` calls it BEFORE taking `e.mu`, so the write is
unguarded. Networks bootstrap concurrently (`erpc/networks_registry.go:300`
-> `erpc/networks.go:259`), and every network that leaves `selectionPolicy`
at the default arrives with the SAME `*SelectionPolicyConfig`. Two goroutines
therefore write `CompiledProgram` at once. `go test -race` reports it as a
write/write race.

A torn `CompiledProgram` does not fail at bootstrap. It surfaces later as a
selection policy that will not evaluate, on a config the operator never
edited.

Fix: serialise the rewrite. The function's own early return already makes it
idempotent — the second caller sees the replaced `EvalFunc` and stops — so a
mutex is all it needs.

## 97. The policy engine edits a config it does not own

**Status:** open. Partly fixed. The selection-policy half is fixed under
entry 131: `Network.Bootstrap` registers a per-network COPY, so the policy
engine no longer receives a config anyone else holds. The `NewUpstream` logger
half (entry 95) is still a retro-fit. Kept open until both are done.

Both races come from the same habit: a component mutates an object the caller
owns and may share. `upgradeDefaultPolicy` rewrites the caller's
`SelectionPolicyConfig`; `NewUpstream` rewrites a logger it already lent out.
Each fix above stops one race. Neither stops the next one.

The weakening in both cases is to stop writing shared input. The engine can
keep the upgraded source and compiled program in its own per-network
`networkRegistration`, which it already builds under `e.mu` and already owns.
`NewUpstream` can build the vendor-labelled logger once, before it hands any
pointer out, rather than retro-fitting a field afterwards.

Neither is done here, because both are wider than a race fix and each needs
its own pin.

---

## 98. The documented exit codes are not the codes the shell sees

**Status: FIXED. `util/exit.go` now uses 11 and 12, and every exit-code
reference in `docs/` moved with them. Pinned by
`cmd/erpc/cli_test.go:TestExitCodes_FitInOneByte`, which fails for any code
that does not survive `& 0xFF`, collides with the config-error code 1, or
reaches the shell's signal range. **Severity was medium.**

`util/exit.go`:

    ExitCodeERPCStartFailed  = 1001
    ExitCodeHttpServerFailed = 1002

A Unix exit status is eight bits. The kernel reports `code & 0xFF`, so:

- 1001 becomes **233**
- 1002 becomes **234**

`docs/pages/config/example.mdx:1127` documents `1001` by name, and four more
lines in that file tell the operator to expect `exit 1001` for a missing
config file, a validation error, or an invalid `--endpoint` URL. No shell,
systemd unit, Kubernetes probe or CI gate ever sees 1001.

Both values are confirmed by running eRPC, not by reading the code:

    $ printf 'projects:\n-\n' > boom.yaml && erpc boom.yaml ; echo $?
    233
    $ go test ./test/          # the HTTP server fails to bind
    exit status 234

The two codes do not collide today, which is why this has gone unnoticed:
233 and 234 are still distinct. That is luck. Any future code whose low byte
matches an existing one collides silently, and codes 1-255 are already
reachable from the same table.

Fix: use values that fit in a byte. Exit codes are a one-byte channel, so a
number that does not fit is not a smaller commitment than one that does — it
is a wrong one.

## 99. A library package terminates the operator's process

**Status: FIXED in the fork.** Upstream still carries it. **Severity: medium
for an embedder, high for a test run.** Pinned by
`cmd/erpc/cli_test.go:TestStart_HttpServerCannotBind_InitReturnsTheErrorAndTheBinaryExits`,
which occupies the port, then asserts the BINARY exits with
`ExitCodeHttpServerFailed` — the library returns the error rather than
ending the process itself.

`erpc/init.go` called `util.OsExit(...)` at three sites from inside the `erpc`
package. That package is a library: `cmd/erpc` is the binary, and `erpc.Init`
is what an embedder calls.

So a Go program that embeds eRPC and fails to bind its HTTP port did not get
an error it could handle. Its whole process ended, inside a goroutine the
caller never started.

This is not theoretical. `go test ./test/` died with exit 234 and printed NO
assertion, no panic and no reason — the HTTP server could not bind, the
goroutine called `OsExit`, and the test binary was gone before it could report
anything. The package is excluded from `make test-fast`, so nobody saw it.

**The fix returns the error, and the binary keeps the exit code.**

`Init` starts three transports in goroutines: the HTTP server, the gRPC server
and the metrics server. A failure in any of them is asynchronous, so it has to
travel back somehow. `Init` already blocked on `<-appCtx.Done()` at the end, so
the report needs no new machinery beyond a value on that wait:

    serverFailed := make(chan error, 3)   // one slot per transport
    ...
    select {
    case <-appCtx.Done():
        // graceful shutdown, as before
    case serverErr = <-serverFailed:
        // return it to the caller
    }

The buffer holds one slot per transport, so a goroutine that fails after `Init`
has already returned writes its error and exits instead of leaking. No
callback, no error group, no new type. The wait that was already there simply
became a wait on two things instead of one.

The three goroutines wrap their error with `erpc.ErrServerFailed`. `cmd/erpc`
is the one place that owns the process, so it does the mapping:

    code := util.ExitCodeERPCStartFailed
    if errors.Is(err, erpc.ErrServerFailed) {
        code = util.ExitCodeHttpServerFailed
    }
    util.OsExit(code)

An operator watching for the HTTP-server exit code still sees it. An embedder
gets an error value and decides for itself. (Entry 98 is still open: the two
exit-code CONSTANTS do not fit in a byte. That is a separate defect and this
fix does not change either value — it only moves where the code is chosen.)

Pinned by `erpc/init_test.go`:
`TestInit_ReturnsWhenTheHttpPortIsAlreadyBound` is the embedder's test — it
holds a port, calls `Init`, and asserts `Init` returns an error naming that
port. `TestInit_InvalidHttpPort` was rewritten: it asserted the old exit code,
which pinned the defect, and now asserts the returned error. Both tests replace
`util.OsExit` with a function that fails the test, so a return to the old
behaviour reports itself with the exit code it tried to use.
`cmd/erpc/main_test.go:TestMain_Start_HttpPortAlreadyBound` pins the other
half: the binary still exits with `ExitCodeHttpServerFailed`.

Mutation: restoring `util.OsExit` in the HTTP goroutine fails both `Init`
tests ("Init must not end the process, but it called util.OsExit(1002)");
removing the `errors.Is` mapping in `cmd/erpc` fails the binary test with
"expected exit code 1002, got 1001".

**The test-level workaround:** the brief for this work said an agent had
replaced `util.OsExit` with a recorder inside `test/`. No such workaround
exists at this branch's base — `grep -rn OsExit test/` finds nothing, so there
was nothing to remove. What `test/` had instead was a goroutine that logged the
`Init` error and dropped it, over a data race; see entry 167. That is now gone:
the harness returns the failure with its reason.

Update: the `test/` package now takes that indirection. Its test binary
replaces `util.OsExit` with a recorder that stops only the calling goroutine,
so a start failure is reported instead of ending the run. That is a workaround
in one package, not the fix; this entry stays open. See 140 for the config gap
that made the exit fire, and 143 for the logger that hid the reason.

Related: 43 and 63 are the same shape. A library turns an operator's mistake
into a fatal event instead of a value the caller can act on.


## 105. A negative or fractional JSON block number becomes a real block number

**Status: FIXED in the fork.** Upstream still carries it. Found while covering `util/`.

`util/json_rpc.go:99` converts a JSON number to a block number with a bare
cast:

    case float64:
        blockNumber = fmt.Sprintf("0x%x", uint64(v))

Every JSON number arrives as a `float64`, so the cast decides what an
out-of-range or non-integral value means. Go leaves that result
implementation-defined: the spec says a float-to-integer conversion whose value
the target type cannot represent "succeeds but the result value is
implementation-dependent". The parser returns no error either way.

Measured on this machine (darwin/arm64, Go's saturating conversion):

    -1      -> "0x0"                  (genesis)
    -1e18   -> "0x0"                  (genesis)
    NaN     -> "0x0"                  (genesis)
    1e30    -> "0xffffffffffffffff"
    +Inf    -> "0xffffffffffffffff"
    1.5     -> "0x1"                  (truncated, no error)

An architecture whose conversion traps to the minimum instead of saturating
produces a different block number for the same request. The same eRPC build on
two CPUs disagrees on what block a request refers to.

`architecture/evm/block_ref.go:483` feeds the result into
`parseCompositeBlockParam`, which is the cache block reference and the input to
block-availability upstream selection. So `eth_getBlockByNumber` with `-1`
routes and caches as genesis — a block every pruned node claims to have —
while the upstream itself rejects the parameter.

The weakening is to stop guessing. The switch already has an exact rule for
the well-formed cases; the out-of-range and non-integral cases are not in the
observed data as valid input, so they belong in the error return that the
`default` branch already provides:

    case float64:
        if v < 0 || v > math.MaxUint64 || v != math.Trunc(v) {
            return "", nil, fmt.Errorf("invalid block number: %v", v)
        }
        blockNumber = fmt.Sprintf("0x%x", uint64(v))

**The fix.** `checkBlockNumberFloat` in `util/json_rpc.go` now names the five
ways a number fails — NaN, infinite, negative, at or above 2^64, and not whole
— and `ParseBlockParameter` returns that error instead of casting. The `int64`
branch rejects a negative value the same way; it had the identical bare cast,
which turned `-1` into the largest possible block rather than genesis. The
`uint64` branch needs no check.

The bound is `v >= 2^64`, not `v > math.MaxUint64` as the sketch above says.
`math.MaxUint64` rounds UP to 2^64 in float64, so the sketch lets 2^64 itself
through and the cast still overflows. `2^64` is exact in float64, so the
comparison against it is the precise one.

**The callers.** Every caller already handles an error from this function,
because the `default` branch has always returned one for an unsupported type.
`architecture/evm/block_ref.go` propagates it through `parseCompositeBlockParam`
into `ExtractBlockReferenceFromRequest`; `architecture/evm/json_rpc.go` and
`architecture/evm/safe_block_routing.go` fall through to their no-reference
path; `clients/grpc_bds_client.go` wraps it at both call sites. No caller
needed a change.

**The tests.** `TestParseBlockParameter_RejectsANumberThatNamesNoBlock`
(`util/json_rpc_test.go`) covers -1, -1e18, NaN, both infinities, 1e30, exactly
2^64, 1.5 and a negative `int64`.
`TestParseBlockParameter_AcceptsEveryNumberAUint64Represents` keeps the
rejection from swallowing 0, 1, 21000000, 2^63 and `MaxUint64`.

**Mutation result (2026-08-19).** With the two checks removed, all nine
rejection cases fail. With the checks restored, the whole `util` package
passes.

**One limit remains, and it is not eRPC's to fix.** A JSON number above 2^53
loses precision in the parser before `ParseBlockParameter` ever sees it, so the
accepted range from 2^53 to 2^64 is nominal. A client that needs a block number
that large must send it as a hex string. See entry 138.

## 106. `ParseBlockHashHexToBytes` guards against its own guarantee

**Status:** not a bug. Minor. Dead code, not a live defect. Closed as not a
bug on 2026-08-21: the code cannot change what an operator or a client sees,
so there is nothing to repair. It stays here because it still reads as live
code. Deleting it in the fork would cost a permanent rebase conflict and
change nothing — see `valve/open-entry-triage.md`.

`util/json_rpc.go:72-78` checks two conditions that the line above rules out:

    b, err := evm.HexToBytes(norm)
    if err != nil { ... }
    if len(b) != 32 { ... }

`norm` comes from `NormalizeBlockHashHexString`, which returns either an error
or a string of `0x` plus exactly 64 lowercase hex digits. Given that string,
`HexToBytes` cannot fail and cannot return other than 32 bytes. Both branches
are unreachable, and the coverage profile confirms no test in the tree reaches
them.

The cost is not the two statements. It is that the pair reads as "this
function handles hashes of any length", so a later change to the normalizer
looks safe when it is not. Either delete the branches and state the
normalizer's guarantee in a comment, or move the length check into the
normalizer where the invariant is actually established.

## 107. The quantile tracker's NaN guard cannot fire, and would not help if it did

**Status:** not a bug. Minor. Dead code, verified by probe. Closed as not a
bug on 2026-08-21: the code cannot change what an operator or a client sees,
so there is nothing to repair. It stays here because it still reads as live
code. Deleting it in the fork would cost a permanent rebase conflict and
change nothing — see `valve/open-entry-triage.md`.

`health/quantile.go:159-167` guards the value coming out of the sketch:

    if math.IsNaN(seconds) || math.IsInf(seconds, 0) { ... return 0 }

DDSketch rejects NaN and both infinities at `Add`, which
`QuantileTracker.Add` (`health/quantile.go:47`) logs and drops. A sketch
therefore never holds a value that could produce a NaN or Inf quantile. I
probed all three inputs: each one is refused at the input, and a real sample
added next to them still reads back correctly.

The second half matters more. `GetQuantile` converts the result with
`time.Duration(seconds * float64(time.Second))`, and a float-to-integer
conversion of NaN is implementation-defined — on this machine it yields 0. So
even if the sketch did return NaN, the caller would already see 0. The guard
adds a log line, not a behaviour.

Keeping it is cheap; the entry exists so nobody reads it as evidence that the
sketch can emit NaN, and so a future reader knows the real defence is at
`Add`.

## 108. `GetUpstreamMetrics` scopes results twice, and the guard that reads like the scope is the optimisation

**Status:** not a bug — the `ups: ups` rebuild carries the scope, so no
observable defect exists. The double scoping still stands in
`GetUpstreamMetrics`, and the hazard for the next edit stands with it. No
comment names `aggKey` as the guard yet.

`health/tracker.go:1174-1183` filters the per-network key list by upstream id,
then rebuilds the lookup key from the caller's own upstream:

    if k.ups == nil || k.ups.Id() != targetID { continue }
    aggKey := upstreamKey{ups: ups, method: k.method, finality: ...All}

Two mutations prove which one enforces the scope. Deleting the id filter
changes nothing observable — `aggKey` carries the caller's upstream, so a
peer's method produces a key that is not in the map and the load misses.
Changing `ups: ups` to `ups: k.ups` also changes nothing on its own, because
the filter has already dropped the foreign keys. Only both together leak one
upstream's metrics into another's map.

That is fine as defence in depth. The hazard is the comment: the filter looks
like the correctness guard, so an optimisation pass that keeps the filter and
switches `aggKey` to the already-loaded `k.ups` reads as a safe change. It is
not. `upstreamKey` compares the interface value, and the tree already has a
test (`TestGetUpstreamMetrics_DistinctUpstreamsSameID_Buckets`) asserting that
two DISTINCT upstream instances can share an id. The filter passes for such a
peer; only the `ups: ups` rebuild keeps it out.

A comment on `aggKey` naming it as the scope guard costs one line and removes
the trap.

## 122. `Initializer.Stop` deadlocks against its own auto-retry loop

**Status: FIXED in the fork.** Upstream still carries it.
**Severity: high.** Shutdown hangs forever, with a mutex held.

`Stop` took `tasksMu` and then, still holding it, waited for the auto-retry
goroutine:

    i.tasksMu.Lock()
    defer i.tasksMu.Unlock()
    if cancel := i.cancelAutoRetry.Load(); cancel != nil { cancel.(context.CancelFunc)() }
    i.autoRetryWg.Wait()          // <- blocks here, holding tasksMu

The loop it waits for calls `attemptRemainingTasks`, which takes `tasksMu`
itself (`initializer.go:299`). If the loop passed its context check just
before `Stop` cancelled, it goes on to that `Lock` and blocks. It then never
returns, so `autoRetryWg` never drains, so `Stop` never returns and never
releases the mutex. Nothing breaks the cycle — that `Wait` has no timeout and
takes no context.

Found by `go test -race ./util/ -count=6`, which wedged for the full 40-minute
test timeout. The dump is unambiguous:

    goroutine 14668 [sync.WaitGroup.Wait, 39 minutes]:
      util.(*Initializer).Stop  .../util/initializer.go:503
    goroutine 10698 [sync.Mutex.Lock, 39 minutes]:
      util.(*Initializer).Stop  .../util/initializer.go:495

In production this hangs shutdown until the orchestrator's grace period
expires and it sends SIGKILL.

Fix: cancel the loop and wait for it BEFORE taking `tasksMu`. The wait no
longer needs the lock, and the loop can finish.

Pinned by `TestInitializer_StopDoesNotDeadlockAgainstTheAutoRetryLoop`. The
window is between the loop's context check and its `Lock`, so one `Stop` almost
always misses it — the test drives 400 short-lived initializers and fails on
the first `Stop` that does not return. Against the old ordering it fails at
round 71; with the fix it passes at `-count=3` and under `-race`.

## 123. A test counted attempts with a plain int across two goroutines

**Status: FIXED in the fork.** Fork-owned test defect, not upstream's.

`util/initializer_test.go` incremented `var attempts int` inside a task body,
which runs on the initializer's goroutine, and read it from the test
goroutine. `go test -race ./util/` reported the write and the read.

It stayed hidden because 122 wedged the package first: the run never got far
enough to report anything. With the deadlock fixed the race surfaces in 0.2
seconds. Now an `atomic.Int64`.

## 109. `Initializer.Stop` returns while its task goroutines are still running and still logging

**Status:** open. **Severity: medium.** Found by `go test ./util/ -race
-count=2`. **The FIRST of the two halves below is now fixed; the second is
not, so the entry stays open.**

**Half 1 is fixed.** `Stop` no longer decides for the caller that a timeout is
survivable. It returns `errors.Join(fmt.Errorf("stop abandoned tasks that were
still running: %w", waitCtx.Err()), destroyErr)`. The test is `waitCtx.Err()`,
NOT the wait error: since 157, `WaitForTasks` also reports tasks that FAILED,
and a failed task is not an abandoned stop — the initializer stopped cleanly,
the task simply did not succeed. Only the deadline means goroutines are still
running. Pinned by `TestInitializer_StopReportsThatItAbandonedRunningTasks`,
with `TestInitializer_StopReportsNoErrorWhenTasksActuallyFinished` as the
control so the pin cannot pass by making every `Stop` fail. Mutation-proven.

Reaching that branch needs a task whose `Fn` IGNORES its context, because a
well-behaved one always ends inside `TaskTimeout` and `Stop` waits
`TaskTimeout+100ms`. That is not a contrived test: it is precisely the
misbehaving task this entry is about.

**Half 2 is not fixed.** A task goroutine still outlives the logger's owner.
Writing the pin proved it rather than fixing it — the first version used
`zerolog.NewTestWriter(t)` and the leaked goroutine wrote to it after
`tRunner` finished, exactly the race quoted below. The test now uses
`io.Discard`, so pinning half 1 does not import half 2 into every run of the
package. That is a containment, not a fix.

**Update (entry 56's fix).** The fix for entry 56 does NOT fix this one, and
the entry stays open. It does two things to the reproduction, and neither
touches the defect:

- Each task goroutine now publishes its terminal state LAST, after the log line
  the state summarises (entry 158). A caller that waits for a terminal state
  therefore has a happens-before edge on that log write, so the specific stack
  quoted below no longer races for such a caller.
- The two `util` tests that reproduced it were themselves at fault and are
  fixed (entries 155 and 156). `go test -race ./util/ -count=4` is green.

Both halves named below survive. `Stop` still reports a timeout as a log line
and returns `destroyFn`'s error, so a caller still cannot tell a clean stop
from an abandoned one. And a task goroutine whose `Fn` is still running when
`Stop` gives up still holds the caller's logger, so it can still write to a
component the operator has torn down. A green race run means the tests no
longer trip it, not that `Stop` now stops.

`util/initializer.go:506-512`:

    waitCtx, waitCancel := context.WithTimeout(i.appCtx, i.conf.TaskTimeout+100*time.Millisecond)
    defer waitCancel()
    if err := i.WaitForTasks(waitCtx); err != nil {
        i.logger.Warn().Err(err).Msg("failed waiting for tasks to finish within the stop sequence")
    }

`Stop` gives the in-flight tasks one task-timeout to end. When they do not, it
logs a warning and returns anyway. The goroutines that
`attemptRemainingTasks` launched at `:332` keep running, and they keep writing
to `i.logger` — `:366`, `:379` and `:384` all log after the task body
returns.

So `Stop` returning does not mean the initializer stopped. It means the
initializer gave up waiting. The caller frees whatever the initializer was
guarding, while task goroutines still hold the logger it lent them and still
emit "initialization task failed" lines for a component the operator has
already torn down.

The race detector makes it visible. `util/initializer_test.go:19` gives each
test a `zerolog.NewTestWriter(t)`, so a leaked goroutine's log call lands in
`t.Log` after `tRunner` has finished the test:

    WARNING: DATA RACE
    Read at ... by goroutine 6083:
      testing.(*common).destination()
      zerolog.TestWriter.Write()
      util.(*Initializer).attemptRemainingTasks.func1.1()
          util/initializer.go:379
    Previous write at ... by goroutine 6082:
      testing.tRunner.func1()

`go test ./util/ -race -count=1` passes; `-count=2` fails in
`TestInitializer_MultipleRapidFailures`, which is the one test that drives
`Stop` into the timeout branch. I reproduced it on a clean tree with my new
test files removed, so it is not introduced by this work.

Two separate things are wrong, and they need separate fixes:

1. **`Stop` has no way to say "I could not stop".** It logs the timeout and
   returns `destroyFn`'s error, so the caller cannot tell a clean stop from an
   abandoned one. Returning the wait error (joined with `destroyFn`'s) is the
   weaker design — it reports what happened instead of deciding for the caller
   that a timeout is survivable.

2. **A task goroutine outlives the logger's owner.** The task context is
   already cancelled by then; the goroutine simply has more work to do after
   the task body returns. Either it stops logging once the initializer is
   stopped, or `Stop` blocks until the launch WaitGroup drains.

The same test also reads `attempts` (`util/initializer_test.go:452, 469`)
without synchronisation while retry goroutines increment it. That one is a
test defect, not a product defect, and it is not what the detector flagged.

## 110. `/healthcheck` answers HTTP 200 when eRPC cannot serve at all

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high.** A load balancer keeps a dead instance in rotation. `erpc/healthcheck.go`
now writes 503 before both error bodies. Pinned by
`TestHealthCheck_ReportsWhenNoProjectIsLoaded` and
`TestHealthCheck_ReportsWhenErpcNeverInitialized`.

Two branches of `handleHealthCheck` report failure through a plain
`errors.New`:

    if s.erpc == nil { ... errors.New("eRPC is not initialized") ... }
    if len(projects) == 0 { ... errors.New("no projects found") ... }

`handleErrorResponse` picks the HTTP status from the error CODE. A plain error
carries none, so it falls through the whole switch and keeps the default,
`http.StatusOK`. The body is a JSON-RPC error object, and the status line says
200.

I confirmed it before the fix:

    code=200 body={"jsonrpc":"2.0","id":null,
      "error":{"code":-32603,"message":"no projects found","data":{}}}

Every other failure in the same handler answers with a failing code: draining
answers 503, an unhealthy fleet answers 502, an unknown project answers 404.
Only the two worst cases — the process holds no eRPC, or it loaded no project
— answer 200. A load balancer or a Kubernetes probe reads the status code, so
it keeps sending traffic to an instance that can serve nothing.

Fix: write `503 Service Unavailable` before the error body, which is the idiom
the simple-mode failure in the same function already uses. Tests:
`TestHealthCheck_ReportsWhenNoProjectIsLoaded` and
`TestHealthCheck_ReportsWhenErpcNeverInitialized`.

## 111. The genesis fork check silently drops a node that answers with no hash

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** `erpc validate` reports a fork check that never ran.
`erpc/config_analyzer.go` now warns in both loops when an upstream tried and
produced no hash, and it names the reason. Pinned by
`TestValidate_NamesTheOneUpstreamThatHidesGenesis`.

`GenerateValidationReport` compares genesis hashes across a project's
upstreams. It reported an upstream that produced no hash only when the fetch
also produced an error:

    if it.genesisTried && it.genesisErr != nil {
        appendWarn("... could not fetch genesis block: %s", ...)
    }

A node that answers `eth_getBlockByNumber("0x0")` with a block whose `hash`
field is empty returns `("", nil)` from `fetchBlockHashByNumber`. There is no
error, so `genesisErr` stays nil, and the upstream matches neither the "has a
hash" branch nor the warning. It vanishes.

With one upstream the group still warns, through the separate "no upstream
returned genesis block" path. With two or more, where one node answers and one
does not, the report is clean. The operator reads a passing fork check that
compared a single node against itself.

Fix: warn whenever an upstream tried and produced no hash, and name the reason
— an error fingerprint when there is one, "returned no block hash" otherwise.
The same silence existed in the unknown-chain variant of the loop, so both are
fixed. Test: `TestValidate_NamesTheOneUpstreamThatHidesGenesis`.

## 112. `network_retry_attempt_total` cannot fire without a network retry policy

**Status:** open. Inherited. This is the mechanism behind 93, not a second bug.
The emit site is still the only one, and the Help text in `telemetry/metrics.go`
still does not say what the counter excludes.

The counter is wired at exactly one site, `erpc/network_executor.go:312`,
inside `runRetry`. Two conditions gate it, and each one silences it
on its own.

First, the loop bound:

    maxAttempts := 1
    if e.cfg != nil && e.cfg.Retry != nil && e.cfg.Retry.MaxAttempts > 0 {
        maxAttempts = e.cfg.Retry.MaxAttempts
    }

A project whose network failsafe block declares no `retry` key leaves
`e.cfg.Retry` nil, so `maxAttempts` is 1. The emit site sits behind
`attempt+1 < maxAttempts`, which is false on the only pass. The counter is
then unreachable by construction, whatever the upstreams do.

Second, even with `maxAttempts > 1` the counter only fires when the NETWORK
executor decides to retry. Failover between upstreams inside one network
attempt happens below this loop and never reaches the increment. That is the
case entry 93 measured: eight real failovers, zero samples.

So the metric counts network-scope retry decisions, not failovers. The name
says otherwise. An operator who alerts on it sees nothing through an outage,
and sees nothing at all if their config omits the retry key.

**Fix:** as 93 — count transport-driven rotation, or say in the Help text what
the counter excludes. Add the `maxAttempts == 1` case to that text: today a
config with no `retry` key publishes a permanently empty series.

## 113. Chain-id verification counts upstreams it never checked

**Status:** open. Inherited (`erpc/healthcheck.go`). **Severity: low.** It
misreads as failures on a mixed-architecture project. `checkEvmChainId` still
skips non-EVM upstreams and still divides by `len(upstreams)` in all three
messages. No test covers a project with a skipped upstream.

`checkEvmChainId` skips any upstream that is not EVM:

    if upsConfig.Type != common.UpstreamTypeEvm || upsConfig.Evm == nil {
        continue
    }

Its result messages then divide by `len(upstreams)`, which still counts the
skipped ones:

    "all %d / %d upstreams verified"          successTotal, len(upstreams)
    "%d / %d upstreams passed (%d failed)"    successTotal, len(upstreams), ...

A project with one EVM upstream and two SVM upstreams, polled with
`eval=all:evm:eth_chainId`, reports "all 1 / 3 upstreams verified" and status
OK. The operator reads two silent failures where there are none. The verdict
is right; only the count is wrong.

**Fix:** count the upstreams the probe actually ran against. The eligible set
is already known at the top of the loop.

## 114. Two chain-id branches cannot run, and one map contract is unstated

**Status:** open. Inherited. Recorded as dead weight, not as a live fault. Two
of the three items hold. The first does not: `EvmGetChainId` returns
`strconv.FormatUint(dec, 10)` of a uint64, so a node that reports a chain id at
or above 2^63 makes `strconv.ParseInt(upChainId, 0, 64)` fail with a range
error. That branch is rare, not dead.

While covering the chain-identity paths I found three commitments that today's
data does not support.

First, `checkEvmChainId` guards against an unparsable chain id:

    actualChainId, err := strconv.ParseInt(upChainId, 0, 64)
    if err != nil { ... "invalid chain id format" ... }

`EvmGetChainId` already normalises the wire value and returns
`strconv.FormatUint(dec, 10)`. Its output is a decimal string, which `ParseInt`
accepts for every chain id below 2^63. A node that reports a chain id at or
above 2^63 makes `ParseInt(upChainId, 0, 64)` fail with a range error, so the
branch is rare, not dead. `GenerateValidationReport` carries the same guard and
fails at the same bound on a 64-bit build.

Second, `GenerateValidationReport` warns on an empty chain-id answer:

    } else if chainStr == "" { ... "returned an empty chain ID" ... }

`EvmGetChainId` never returns an empty string with a nil error. A node that
answers `""` normalises to `"0"`, and the report says "returned chain ID 0"
instead. The empty-string branch is reachable only when the fetch already
failed, where the preceding error branch takes it first.

Third, `checkEvmChainId` writes into the caller's map without checking it:

    upstreamResult := upstreamsDetails[ups.Id()]
    upstreamResult["expectedChainId"] = expectedChainId

If the caller passes a map with no entry for an upstream id, `upstreamResult`
is nil and the assignment panics. The panic happens in a goroutine with no
recover, so it kills the process rather than failing the health check. Both
call sites build the map from the same slice they pass, so nothing violates it
today — but the function is package-level and the precondition is written
nowhere.

**Fix:** delete the unreachable empty-string branch rather than test it, and either
build the detail map inside `checkEvmChainId` or skip an upstream that has no
entry. All three are unforced commitments the caller's shape is currently
paying for.

## 100. A shadow test asserts an absolute value on a process-global counter

**Status:** open. Found while adding `TestProjectForward_*` in
`erpc/projects_shadow_forward_test.go`. Re-confirmed by probe, not by reading:
running the two shadow tests with `-shuffle=2` puts the mismatch test first,
and `erpc/shadow_test.go:215` then fails. The same pair with `-shuffle=1`
passes, so the order alone decides it.

`erpc/shadow_test.go:215` reads:

    require.Zero(t, shadowMismatch(t, shadows[0], network, "eth_blockNumber"),
        "an agreeing candidate must not also be counted as a mismatch")

`shadowMismatch` sums a Prometheus counter vector. That vector lives for the
life of the test binary, and its labels are the project id, the vendor, the
network label, the upstream id and the method — all of which
`startShadowErpc` hard-codes to the same values for every test in the file.

So the assertion is not "this test counted no mismatch". It is "no test in
this binary has ever counted an eth_blockNumber mismatch on project `prod`,
upstream `candidate`". A new test that mirrors one disagreement on that
method fails a test it never touched, and the failure names the old test.

The other assertions in the same file already read a `before` value and
compare a delta. This one does not, so it is the only one that couples.

Fix: read the counter before the call and assert the delta, the way every
other assertion in the file does. The new tests work around it by mirroring
`eth_getBlockByNumber` instead, which leaves the coupling in place for the
next author to trip over.

## 101. `resolveBlockTag` takes an `upper` argument it never reads

**Status:** not a bug. Found while covering the tag branches in
`erpc/query_executor.go`. `resolveBlockTag` still takes `upper` at
`erpc/query_executor.go:318`, and its body still never reads it. Closed as
not a bug on 2026-08-21: the code cannot change what an operator or a client
sees, so there is nothing to repair. It stays here because it still reads as
live code. Deleting it in the fork would cost a permanent rebase conflict
and change nothing — see `valve/open-entry-triage.md`.

    func (qe *EvmQueryExecutor) resolveBlockTag(ctx context.Context, block string, upper bool) (uint64, error)

The body switches on `block` and never mentions `upper`. `resolveQueryBounds`
passes `false` for the range start and `true` for the range end, so the call
sites read as though the two ends resolve differently. They do not.

Either the parameter once carried a rule that got deleted, or it was added
for one that never arrived. Both leave the same trap: a reader who needs the
upper bound to resolve differently — `latest` on the `to` side of an open
range, say — will believe the hook is already there.

Go does not report an unused parameter, so nothing catches this.

Fix: delete the parameter. If an upper bound really needs its own rule later,
add it then, with a test.

## 115. `UpstreamConfig.Copy` is a partial deep copy

**Status: FIXED in the fork** (`common/config.go`). Upstream still carries it.
`Copy()` now shares no memory with the original. Pinned by
`TestUpstreamConfigCopy_SharesNoMemoryWithTheOriginal`, which has no allowlist
any more, and by
`TestUpstreamConfigCopy_FormerlyAliasedFieldsAreIndependent`, one subtest per
field.

`upstream/registry.go:509` copies an upstream config before it bootstraps the
upstream, and its comment says why: "Deep copy to avoid race conditions when
detectFeatures modifies the config". Line 547 copies again per attempt, because
"NewUpstream mutates it (vendor detection)". Both rely on `Copy()` producing an
object that shares nothing with the operator's config or with the sibling
copies. Four vendors clone a template config the same way —
`thirdparty/chainstack.go:165`, `conduit.go:148`, `quicknode.go:350`,
`superchain.go:223` — one clone per discovered endpoint.

`Copy()` did not produce that object. It started with `*copied = *c`, then
deep-copied some fields and left the rest aliased.

The worst case was `JsonRpcUpstreamConfig.Copy`:

```go
copied := &JsonRpcUpstreamConfig{}
*copied = *c
if c.Headers != nil {
    maps.Copy(copied.Headers, c.Headers)   // dst IS src
}
```

`*copied = *c` gives `copied.Headers` the same map header as `c.Headers`, so
`maps.Copy` copied the map into itself and did nothing. Every copy of an
upstream shared one header map. Headers carry credentials — `thirdparty/
blockdaemon.go:116` writes `Authorization: Bearer <key>`, `thirdparty/
satelink.go:132` writes `X-API-Key` — and two bootstrap attempts writing that
map concurrently is a Go fatal concurrent map write, not a recoverable error.
`thirdparty/chainstack.go:178` writes an auth header into exactly that map, once
per node it discovers. The sibling `GrpcUpstreamConfig.Copy` allocates first
and gets it right, which is what shows the intent.

`common/config.go` allocates the destination map before copying into it. Pinned
by `TestJsonRpcUpstreamConfigCopy_HeadersAreIndependent`.

**The seven fields that stayed aliased are copied now.** Each was decided on
its own, and the answer was the same each time: copy it. `Copy()` runs at
config load and at bootstrap, never per request, so the cost is a few small
allocations per upstream. Against that, every one of these fields is read on a
live path while a sibling bootstrap attempt could be writing it.

- **`UpstreamConfig.Tags`** — a slice. `append` on a copy with spare capacity
  writes into the original's backing array. `common/config.go:1329` appends to
  `Tags` (the legacy `group:`/`cohort:` rewrite) and `common/defaults.go:1778`
  reassigns it; both run before any `Copy`, so nothing crossed the boundary
  today. The selection policy reads tags on the request path.
- **`UpstreamConfig.CreditUnits`** — a map. No writer indexes it today. It is
  the exact shape that turns a race into an unrecoverable fatal, and the
  rate-limit path reads it.
- **`UpstreamConfig.Svm`** — a pointer to three scalars. `ApplyDefaults`
  (`common/defaults.go:1832-1843`) writes `Chain`, `Cluster` and
  `CheckGenesisHash` in place. It runs before `Copy` today, so the write does
  not cross a copy boundary — but this is a live in-place writer of exactly
  this struct, which puts the alias one call-order change away from one
  upstream editing another's identity. Now has a `Copy()`.
- **`UpstreamConfig.Shadow`** — a pointer to a struct holding
  `IgnoreFields map[string][]string`, so a struct-level copy alone still shares
  the map AND each slice inside it. `erpc/shadow.go:169` reads `IgnoreFields`
  per shadow comparison, concurrently with bootstrap. Now has a `Copy()` that
  clones the map and every slice.
- **`UpstreamConfig.Routing`** — a pointer to a struct holding
  `ScoreMultipliers []*ScoreMultiplierConfig`. `internal/policy/prober.go:254`
  reads `Routing.Probe` per probe tick. `ApplyDefaults` already clones this
  struct and its entry list, which shows the code treats a shared `Routing`
  block as a hazard; it stops at the entries by declaring them immutable. That
  is an unforced commitment, so `Copy()` deletes it and copies the entries too,
  including each `Finality` slice. The `*float64` weights stay shared: every
  writer in this tree replaces such a pointer instead of writing through it,
  which is how the rest of the `Copy` family treats `*bool` and `*float64`.
- **`RetryPolicyConfig.EmptyResultAccept` and `.EmptyResultIgnore`** — slices,
  and they escape the config. `common/empty_result.go:18` hands
  `EmptyResultAccept` straight to the request path, and
  `common/defaults.go:2745` assigns `EmptyResultIgnore` to `EmptyResultAccept`,
  so one backing array could reach two fields of every copy of every config.

**The allowlist is gone, not shortened.**
`TestUpstreamConfigCopy_SharesNoMemoryWithTheOriginal` walks the whole config
tree by reflection and now fails on ANY aliased path, including one in a field
added tomorrow. `TestUpstreamConfigCopy_FormerlyAliasedFieldsAreIndependent`
adds the same claim as plain writes, one subtest per field, so a field that
becomes aliased again names itself. Against the previous `Copy()` the
reflection test lists all seven paths and all seven subtests fail.

## 116. `ErrUpstreamsExhausted.DeepestMessage` can never reach its single-cause branch

**Status:** not a bug. The single-cause branch is still unreachable.
`TestUpstreamsExhaustedDeepestMessage`, sub-test "a single joined cause
still reports the bucket summary, not the upstream's own message", asserts
today's behaviour and passes. Closed as not a bug on 2026-08-21: the code
cannot change what an operator or a client sees, so there is nothing to
repair. It stays here because it still reads as live code. Deleting it in
the fork would cost a permanent rebase conflict and change nothing — see
`valve/open-entry-triage.md`.

`common/errors.go:1255` reads:

```go
s := e.SummarizeCauses()
if s != "" {
    return s
}
if joinedErr, ok := e.Cause.(interface{ Unwrap() []error }); ok {
    children := joinedErr.Unwrap()
    if len(children) == 1 && children[0] != nil {
        ...return the child's own message...
    }
```

`SummarizeCauses` uses the same type assertion and classifies every child into
some bucket — an unrecognised error falls through to `other++`. So for any
joined cause with at least one child it returns a non-empty string, and
`DeepestMessage` returns before the branch below. For a joined cause with zero
children `len(children) == 1` is false. The branch is unreachable on every
input.

What an operator sees: on a network with one upstream, a failure reports
`1 upstream server errors` instead of the node's own text
(`execution reverted at 0x...`). The code that would have surfaced the text is
there and never runs.

Pinned by `TestUpstreamsExhaustedDeepestMessage`, sub-test "a single joined
cause still reports the bucket summary, not the upstream's own message".

## 117. `AvailbilityConfidence` does not survive a YAML round trip

**Status:** open. `UnmarshalYAML` still accepts only `blockhead`, `1`,
`finalizedblock` and `2`, while `String()` still emits `stateProven`.
`TestAvailbilityConfidenceUnmarshalYAML`, sub-test "does not round-trip
stateProven", asserts today's behaviour and passes.

`common/architecture_evm.go` gives the type three values. `String()` and
`MarshalYAML()` emit `blockHead`, `finalizedBlock` and `stateProven`.
`UnmarshalYAML` accepts `blockhead`, `1`, `finalizedblock` and `2`, and
rejects everything else.

So `stateProven` marshals out and fails to parse back, and so does the
`unknown(0)` an unset value produces. An operator who dumps the effective
config and feeds it back gets `invalid availability confidence: stateProven`.

The reachable damage is small today: the only YAML-configurable field of this
type is `EvmNetworkConfig.EmptyResultConfidence`, and its two readers
(`architecture/evm/common.go:79`, `erpc/networks.go:871`) only test for
`Finalized`, so `stateProven` would be inert there even if it parsed. That is
the reason to record the asymmetry rather than close it by adding a value the
readers ignore — the parser and the printer should agree on one set, whichever
set that is.

Pinned by `TestAvailbilityConfidenceUnmarshalYAML`, sub-test "does not
round-trip stateProven".

## 118. Two different requests can share one cache key

**Status: FIXED in the fork.** Upstream still carries it. **Severity: highest.** A client could
receive another request's data. **Confirmed independently by direct probe**, not
by reading the code, and re-confirmed in a later audit: an out-of-tree
recomputation of the double SHA-256 that `CacheHash` performs reproduces the
recorded digest for both parameter lists. Both requests below produced a
byte-identical key:

    A = eth_getStorageAt:5153a6f084c403121fd652f1b9d01eab89d6fac7c28b5106fd459fa00bfd1b08
    B = eth_getStorageAt:5153a6f084c403121fd652f1b9d01eab89d6fac7c28b5106fd459fa00bfd1b08

The worked case below uses `eth_getStorageAt`, where the colliding twin is
nonsense a node rejects. The entry first named `eth_getLogs` topics
`["0xa","0xb"]` against `["0xab"]` as the reachable case. Those two do NOT
collide: the old hasher wrote `0xa` then `0xb`, which spells `0xa0xb`, not
`0xab`. The reachable `eth_getLogs` case is topic NESTING, and it is worse,
because both sides are well-formed filters a node answers:

    topics: [[A, B], C]   — topic0 is A or B, and topic1 is C
    topics: [A, [B, C]]   — topic0 is A,      and topic1 is B or C

Both wrote A, B, C in that order and nothing else, so they shared one key.
The two filters ask different questions of the chain, and whichever landed
first served the other its logs.

`JsonRpcRequest.CacheHash` (`common/json_rpc.go`) hashes the parameters by
feeding each one to `hashValue` in turn:

```go
hasher := sha256.New()
for _, p := range r.Params {
    err := hashValue(hasher, p)
    ...
}
```

`hashValue` writes each value's bytes and nothing else. No separator goes
between adjacent parameters, and none goes between a map key and its value or
between array elements. Two parameter lists whose concatenations match
therefore produce the same key for the same method.

A worked case, both `eth_getStorageAt`:

- `["0xabc", "0xdef", "latest"]` hashes `0xabc` + `0xdef` + `latest`
- `["0xabc0xdef", "", "latest"]` hashes the same bytes

The second request is nonsense to a node, but the cache answers it before any
node sees it — and a write under the second key serves the first. The same
collision class exists between an array and a flattened string, and between an
object's keys and its values.

**It is not only the cache.** `Network.multiplexKey` (`erpc/networks.go`) hands
`CacheHash` straight back for every EVM network, and multiplexing defaults to
ON. Two colliding in-flight requests therefore shared one multiplexer, and the
follower received the leader's response verbatim. That path needs no cache
configured at all, so every EVM deployment carried the wrong-answer risk.

**The fix.** `hashValue` now writes a self-delimiting encoding. A scalar writes
a type tag, its payload length in decimal, a `:` and the payload. A container
writes a type tag, its member count and a `:` before its members. An object
frames its keys the same way, so the boundary between a key and its value
cannot move either.

The choice of a length prefix over a separator byte is the whole point.
Parameters carry arbitrary strings, so any byte picked as a separator is a byte
a string may itself contain — that moves the collision rather than removing it.
A length prefix has no such hole, whatever bytes the payload holds. The `:`
after the digits is unambiguous for the same reason a netstring is: a digit is
never a `:`, so a reader always knows where the count ends. The encoding is
decodable, and a decodable encoding cannot map two values onto one byte string.

**The tests.** `TestCacheHash_SeparatesParamListsThatConcatenateToTheSameBytes`
(`common/json_rpc_hashing_test.go`) holds six pairs that collided before:
adjacent params, the `eth_getLogs` topic nesting above, an array against the
string its members spell, an object key against its value, two arrays against
one, and a number against its own digits.
`TestHashValue_EncodesEveryStructureDistinctly` states the rule the six pairs
sample — it encodes 26 values, including `nil` against the string `"null"` and
a string that spells a whole frame, and fails if any two produce the same
bytes. The old pinning test is gone; this pair replaces it.

**Mutation result (2026-08-19).** With the frame header stubbed out to write
nothing, all six pairs and the distinctness test fail. With the framing
restored, both tests pass and so do `common`, `util` and `architecture`.

**Cache invalidation — state this to operators before the upgrade.** The
framing changes every cache key eRPC computes. There is no version marker in
the key format to bump: the key is `{method}:{sha256(params)}`, and the method
prefix is the only structure it carries. I did not add one, because a version
tag would not change what happens — a format change already produces keys
disjoint from the old ones, and the hash itself is the generation break. So on
upgrade **every existing entry in every cache connector becomes unreachable at
once.** Expect a full miss storm: the first request for each key goes upstream,
and upstream load returns to cold-start levels until the cache refills. Old
entries are never read again and expire on their own TTL, so a shared store
holds two generations for one TTL. Plan capacity for both. A wrong answer is
worse than a cold cache, but an operator must not meet the miss storm by
surprise.

The same collision class existed in the CONSENSUS response hash. Entry 135
fixes it with the same framing, and states why that hash costs nothing to
change on upgrade.

## 120. The query shim advertises an uppercase hex prefix it always rejects

**Status: FIXED in the fork.** Upstream still carries it. **Severity: low.** It cost one client a clear error message, not a
wrong answer. The `0X` test is deleted, which is the weaker of the two options
this entry names. Pinned by
`architecture/evm/eth_query_parse_test.go:TestParseUint64Value_ReportsAnUppercaseHexPrefixAgainstTheInput`,
which replaces
`TestParseUint64Value_RejectsTheUppercaseHexPrefixItClaimsToAccept`.

`architecture/evm/eth_query_helpers.go` — `parseUint64Value` reads every
quantity the query shim takes: `limit`, and the `number` of a pagination
cursor. Its string case tests for both hex prefixes:

    if strings.HasPrefix(v, "0x") || strings.HasPrefix(v, "0X") {
        return common.HexToUint64(v)
    }

`common.HexToUint64` then rejects the uppercase form outright:

    if len(hex) < 2 || hex[:2] != "0x" {
        return 0, fmt.Errorf("invalid hex string: %s", hex)
    }

So `"0X2a"` reaches the branch written for it and comes back as an error. The
caller sees `invalid limit: invalid hex string: 0X2a`, or the same wording for
a cursor number, and has no way to tell that only the case of one character is
wrong. The decimal fallthrough below never runs for these values either,
because the prefix test already claimed them.

The `0X` test is an unforced commitment: it promises a shape the converter does
not accept. Either delete it, so an uppercase value falls to the decimal parser
and fails with a plain message, or lowercase the prefix before the conversion.
Deleting is the weaker change and the one the observed data supports — nothing
here shows a client sending `0X`.

## 121. A typo in `minSwitchInterval` silently disables the anti-flap cooldown

**Status:** open. **Severity: medium.** It turns a one-character config mistake
into fleet-wide primary flapping, with no log line and no metric. Pinned by
`internal/policy/stdlib/duration_knob_test.go:TestStickyPrimary_MinSwitchInterval_DecidesTheHandover`,
whose `UnparseableString` and `WrongType` cases assert today's handover.

`internal/policy/stdlib/install.go` — the `durationMs` global returns 0 for
every value it cannot read: a string `time.ParseDuration` refuses, an empty
string, a boolean, an object. `internal/policy/stdlib/stdlib.js` feeds it the operator's
`minSwitchInterval` and uses the result as the sticky cooldown:

    const minSwitchMs = (opts.minSwitchInterval != null) ? durationMs(opts.minSwitchInterval) : 30_000;

A cooldown of 0 makes the elapsed-time guard `(ctx.now - lastSwitchAt) <
minSwitchMs` always false, so every tick re-decides the primary on the score
gap alone. During an incident that gap is large, which is exactly the case the
cooldown exists for. `stickyPrimary({ minSwitchInterval: '30 s' })` and
`stickyPrimary({ minSwitchInterval: '30sec' })` therefore behave as if sticky
were switched off.

The absent-value default is 30 seconds, so an operator who writes the knob
wrongly gets LESS protection than one who omits it. That is the part worth
fixing. Parse failure is not the same event as absence, and the code collapses
the two. Report the failure — the engine already has a logger on the eval path
— or fall back to the same 30 seconds absence gets. Silent zero is the one
answer that cannot be right.

## 125. An unreadable `LOG_LEVEL` silences the process instead of defaulting to debug

**Status:** open. Pinned by
`cmd/erpc/cli_test.go:TestLogLevelEnv_InvalidValueSilencesLogging`.
**Severity: medium.** The operator loses every log line at the moment the
config is wrong.

`cmd/erpc/main.go:354` reads the `LOG_LEVEL` environment variable on every
command:

    level, err := zerolog.ParseLevel(lvl)
    if err != nil {
        logger.Warn().Msgf("invalid log level '%s', defaulting to 'debug': %s", lvl, err)
    } else {
        cfg.LogLevel = level.String()
    }
    zerolog.SetGlobalLevel(level)

`zerolog.ParseLevel` returns `NoLevel` with the error. The `SetGlobalLevel`
call sits OUTSIDE the else branch, so the failure path installs `NoLevel` as
the global floor. `NoLevel` is 6. Debug, info, warn and error are all below
it, so eRPC drops every one of them. The message promises debug and the code
delivers silence.

The same file gets this right at `main.go:59`, where the identical parse only
calls `SetGlobalLevel` in the else branch. One `LOG_LEVEL=warining` typo
therefore blinds the operator, and the warning that explains it is itself
suppressed on any writer that respects the global level.

Fix: move `SetGlobalLevel(level)` into the else branch, or set
`zerolog.DebugLevel` on the error path so the code does what the message says.

## 126. The legacy translator rewrites network-level keys with no warning

**Status:** open. Pinned by
`common/legacy/translate_config_test.go:TestTranslateFromConfig_NetworkLegacyFieldsWarnNothing`.
**Severity: low.** Nothing breaks today, but the operator never learns the
config is deprecated.

`common/legacy/translate.go:39` builds the warning list from project-level
fields only — `routingStrategy`, `scoreMetricsMode`, and the four inert score
knobs. The network-level legacy keys translate silently:

- `selectionPolicy.evalFunction` is wrapped into the modern `eval`;
- `selectionPolicy.resampleExcluded` appends `.probeExcluded(...)`.

`common/legacy/warnings.go` holds the exact messages for both cases —
`WarnLegacySelectionPolicy` and `WarnResampleExcluded` — plus
`WarnRoutingPolicyEnvVars`. All three are exported, and a repo-wide grep finds
no caller. They are dead code that documents a behaviour the package does not
have.

The effect is a silent rewrite. An operator whose `evalFunction` becomes a
wrapped legacy call gets different selection behaviour with no line in the log
that says so, and no signal that the key is on its way out.

Fix: call the three functions from `Translate` where each condition already
tests true, or delete them. The current state claims a contract the code does
not keep.

## 127. `--config` silently ignores a one-character path

**Status:** open. Pinned by
`cmd/erpc/cli_test.go:TestConfig_OneCharacterPathIsIgnored`.
**Severity: low.** The blast radius is small, but the failure mode is the bad
kind: eRPC runs a config the operator did not ask for.

`cmd/erpc/main.go:300`:

    if configFile := cmd.String("config"); len(configFile) > 1 {

The guard tests for length, not for emptiness. `erpc --config a dump` drops
the flag, falls through to auto-discovery, and loads `./erpc.yaml` instead.
No error, no warning — the operator reads a dump of the wrong file.

The same path treats every other missing config as fatal, which is what makes
this inconsistent rather than merely odd: `--config aa` on a missing file
exits, `--config a` starts.

Fix: test `configFile != ""`. The length bound commits to a rule about file
names that nothing in the data supports.

## 128. The legacy translator's golden fixtures are wired to nothing

**Status:** open. **Severity: low**, and it hides a larger claim.

`common/legacy/testdata/` holds two fixture pairs
(`01-routing-strategy-round-robin`, `08-routing-policy-env-vars`) and a README
that describes ten scenarios and an acceptance test. No Go file in the repo
reads the directory — a grep for `testdata` across `common/legacy/` finds only
the fixtures themselves.

Scenario 08 is the part worth reading. Its `.expected.yaml` says the
translator detects an implicit fallback-group policy and synthesizes an eval
that reads `ROUTING_POLICY_MAX_ERROR_RATE`, `ROUTING_POLICY_MAX_BLOCK_HEAD_LAG`
and `ROUTING_POLICY_MIN_HEALTHY_THRESHOLD`. `common/legacy/translate.go` does
no such thing: it never inspects `upstream.group`, and the env vars appear
nowhere in the package. `WarnRoutingPolicyEnvVars` (see 126) is the dead
warning for this same missing feature.

So an operator who upgrades from a config that relied on the `ROUTING_POLICY_*`
fallback policy gets the canonical default policy instead, with no warning and
no translation. The fixture that would have caught it runs in no test.

Fix: either wire the fixtures to a golden test and implement scenario 08, or
delete the fixtures and the README claims. A fixture nothing runs is a record
of an intention, not a test.

---

# Found by the 2026-08-19 status audit, and the work that followed it

The audit that rewrote every Status line above found **five** new defects while
reading the code each entry cites. The section now holds 32 entries, because
later sessions appended here too. "Recorded, not fixed" is no longer true of
the section as a whole either — several of these are fixed. The Status line on
each entry is the authority; this header is not.

## 130. `writeFatalError` shadows the error it was given, so every fatal POST closes its span as OK

**Status: FIXED in the fork.** Upstream still carries it. **Severity: medium.** The operator lost
the fatal error. Pinned by
`erpc/http_server_fatal_span_test.go:TestRequestHandler_AFatalPostClosesItsSpanAsAnError`
and `…_AFatalPostKeepsTheOriginalMessageInTheBody`.

**The entry was accurate.** A panic on a JSON-RPC POST closed its
`Http.ReceivedRequest` span with status `Ok` and no recorded error. The body was
already correct, so the span assertion is the one that catches it.

**Fix.** `erpc/http_server.go` names the marshal result `marshalErr`, so `err`
still holds the fault when `EnrichHTTPServerSpan` reads it. The fallback no
longer retries the same call with the same input — that retry could never
succeed — and writes a JSON literal instead. `SonicCfg` has `ValidateString:
false`, so marshalling a Go string cannot fail today; the literal is there so
the body stays valid JSON if that ever changes, not because a case is known.

`erpc/http_server.go:850` declares `writeFatalError := func(httpCtx
context.Context, statusCode int, err error)`. Inside the `finalErrorOnce.Do`
closure, `:871` runs:

```go
msg, err := common.SonicCfg.Marshal(err.Error())
```

`msg` is new, so `:=` is legal, and `err` becomes a **new** variable scoped to
the closure. It holds the marshal result, not the fatal error. Two things then
read the wrong value:

- `:873` — the fallback `msg, _ = common.SonicCfg.Marshal(err.Error())` now
  marshals the marshal error's own text, so a client that hits the fallback
  reads "invalid character …" instead of the server fault.
- `:878` — `common.EnrichHTTPServerSpan(httpCtx, statusCode, err)` gets the
  marshal error, which is nil in practice.

`:866-868` forces `statusCode = http.StatusOK` for every POST, per the JSON-RPC
transport rule. `EnrichHTTPServerSpan` (`common/tracing_util.go`) takes its
`err == nil` branch and calls `span.SetStatus(codes.Ok, "")`. So every panic and
every fatal server error on a JSON-RPC POST closes its HTTP server span as OK
with status 200. Tracing shows a clean request where the server died.

Renaming the marshal result repairs both consumers. Found while auditing 34.

No test covers the span status on this path.

---

## 131. `Network.Bootstrap` writes the shared selection policy before the lock that entry 96 added

**Status: FIXED in the fork.** Upstream still carries it.
**Severity: medium.** Pinned by
`erpc/networks_bootstrap_policy_ownership_test.go:TestNetworkBootstrap_DoesNotWriteTheConfigItWasGiven`
and `…_CompilesADefaultPolicyWithoutTouchingTheSource`.

**The entry was accurate about the code.** Both writes run outside the lock,
`Bootstrap` writes a struct it does not own, and on a shared struct
`FailoverOnDefaultsExhausted` is last-writer-wins.

**The consequence needs sharing, and I could not confirm sharing** — see entry
164. I traced the config-load paths and found no way for two networks to arrive
holding one `*SelectionPolicyConfig`, so entry 96's premise looks like a
test-fixture condition rather than a production one. That changes the severity,
not the fix: writing an object you do not own is wrong whether or not today's
callers happen to share it.

**Fix.** `Network.Bootstrap` now registers a shallow COPY of the network's
`SelectionPolicyConfig`. That is entry 97's weakening: the component stops
writing an object it does not own, so no lock is needed and no two networks can
reach the engine with one struct. The copy carries one pointer,
`CompiledProgram`, which nothing mutates after compilation. `upgradeDefaultPolicy`
keeps its mutex — `RegisterNetwork` is exported, and the mutex bounds what any
other caller can do — but no production registration reaches it with a shared
pointer any more.

The tests pin the ownership rule directly rather than chasing a race: they hand
two networks ONE config, then assert that the operator's struct is untouched and
that each network's registered config keeps its own failover flag.

Entry 96 records that concurrent network bootstrap corrupts a shared
`SelectionPolicyConfig`, and the fork now serialises the rewrite inside
`upgradeDefaultPolicy`. `Network.Bootstrap` still writes the same shared struct
one frame earlier with no lock:

- `erpc/networks.go:242` — `cfg.FailoverOnDefaultsExhausted =
  n.cfg.Failover.Enabled()`, unconditional.
- `erpc/networks.go:246-250` — `cfg.SetDefaults()`, which writes `EvalFunc`,
  `EvalFuncOriginal` and `CompiledProgram` (`common/defaults.go`). Those are the
  same three fields entry 96 protects.

If entry 96's premise holds — two networks reach `RegisterNetwork` with one
config pointer — both sites still race. The `FailoverOnDefaultsExhausted` write
is worse than a race: two networks with different `failover.enabled` values
write different booleans to one field, and the last writer wins. One network
then runs with the other network's failover flag, with no log line.

Found while auditing 96.

---

## 132. `parseUint64Value` reports a missing quantity and twelve call sites discard the error

**Status: FIXED in the fork.** Upstream still carries it. **Severity: medium.** A silent wrong
page, not an error. Pinned by
`architecture/evm/eth_query_unreadable_quantity_test.go` (eight tests, two of
them counterweights that must keep passing).

**The entry was accurate**, with one number off: thirteen call sites discard the
error, not twelve, and two more sites drop it with an `err == nil` guard.

**The rule I applied at every site: eRPC never turns a value it could not read
into a number.** An ABSENT field may keep a zero where the shape has no way to
say "absent". A field that is PRESENT and unreadable is a different event, and
it is reported. The sites do not all want the same thing, so here is each one.

**Reject — the value decides what the client reads.**

- `sortLogs` (4 sites) now returns `([]uint64, error)`. It reads `blockNumber`
  and `logIndex` once per log, up front, and reports the first log it cannot
  read. The shim always builds its `eth_getLogs` filter with concrete
  `fromBlock`/`toBlock`, so a pending log — the only conforming log with a null
  block number — cannot appear. Nothing forces tolerance here.
- The paging loop (2 sites) no longer re-reads the maps. It consumes the block
  numbers `sortLogs` returned, so the group boundary and the cursor come from
  the same read that was already validated. This deletes two call sites rather
  than guarding them.
- `fetchTracesForBlock` (1 site) reports a block with no readable `number`. The
  fallback below it would have asked for `trace_block("0x0")` and stamped every
  trace with block 0 — the wrong block's traces, served as this block's.
- `fetchTracesForBlock`'s timestamp (1 site, previously `err == nil`) reports a
  timestamp that is present and unreadable. Dropping it to nil reads downstream
  as a chain that does not report block times.
- `protoTraceFromJSON`'s `traceAddress` entries (1 site). Every entry is present
  by construction, so an unreadable one is always garbage, and a zero moves the
  trace to a different position in the call tree.
- `protoTraceFromJSON`'s `blockTimestamp` (1 site, previously `err == nil`).

**Absent keeps the zero, present-but-unreadable is rejected.** The five
remaining `protoTraceFromJSON` sites — `gas`, `gasUsed`, `subtraces`,
`transactionIndex`, `blockNumber` — go through a new `parseOptionalQuantity`.
Parity omits `gas` and `transactionIndex` on a reward trace, and the proto these
values feed types them as plain `uint64`/`uint32` with no absent state, so
rejecting an absent field would make eRPC refuse valid chain data. Today's data
forces the zero for absence and forces nothing for garbage.

**Entry 120's dead branch, fixed here too.** `parseUint64Value` no longer tests
for a `0X` prefix. Entry 120 named deletion as the weaker of the two options and
said nothing observed shows a client sending `0X`. An uppercase value now falls
to the decimal parser and fails with a message that quotes the input.
`TestParseUint64Value_RejectsTheUppercaseHexPrefixItClaimsToAccept` is replaced
by `TestParseUint64Value_ReportsAnUppercaseHexPrefixAgainstTheInput`.

`parseUint64Value` (`architecture/evm/eth_query_helpers.go:396`) returns
`(0, error)` for a nil, negative or unparseable quantity. Twelve call sites
write `_` for the error and use the zero. Two of them change what the client
reads:

- `sortLogs` (`architecture/evm/eth_query_helpers.go:586-591`) keys the sort on
  `blockNumber` and `logIndex`. A log whose `blockNumber` is absent sorts as
  block 0 and lands at the head of an ascending page.
- The paging loop (`architecture/evm/eth_query_shim.go:185-190`) reads the same
  field twice: once to group logs into blocks, once to build `lastScanned`. One
  unreadable log breaks the grouping and can write a cursor of 0, so the client
  pages from genesis.

Neither path logs anything. Entry 120 gives a confusing message on the same
helper; this gives no message at all. Found while auditing 120.

---

## 133. The gRPC surface has the same RFC 7239 defect as entry 30, and no entry recorded it

**Status: FIXED in the fork.** Upstream still carries it. **Severity:
medium.** Pinned by `erpc/forwarded_header_test.go` (five tests, both surfaces).

**The entry was accurate.** Both surfaces handed every configured header to a
parser that reads bare addresses only, so `for=203.0.113.7` produced an empty
chain and the request fell back to the peer.

**Fix — one parser, not two.** `parseXForwardedFor` and the unreachable
`parseForwardedFor` are replaced by `parseForwardedChain`, which reads whichever
syntax the element carries: an element with `for=` yields that parameter, any
other element is the address itself. HTTP and gRPC both call it. The parser
reads the VALUE rather than deciding from the header's NAME, so an operator can
name any header and eRPC does not need a table of which name means which syntax.
An element that names no address — RFC 7239 also allows obfuscated identifiers
such as `for=_hidden` — is dropped, so the request falls back to the peer rather
than eRPC inventing a client. The trusted-forwarder check is untouched, and a
test pins that reading a standard header did not become a way around it.

`erpc/http_server_client_ip_test.go:TestResolveRealClientIP_DoesNotUnderstandTheRfc7239ForwardedHeader`
recorded the old behaviour and is replaced by
`…_ReadsAForwardedHeaderWhateverSyntaxItCarries`.

Entry 30 records that `Forwarded` is unsupported on the HTTP path and fails
silently. `GrpcServer.grpcClientIP` (`erpc/grpc_server.go:553`) repeats it. The
loop at `:561` walks `gs.trustedIPHeaders` and hands every one of them to
`parseXForwardedFor` at `:566`. That parser understands the
`client, proxy1, proxy2` form of `X-Forwarded-For`. RFC 7239 writes
`for=192.0.2.60;proto=http`, which the parser reads as no address at all.

An operator who configures `Forwarded` on a gRPC server therefore gets an empty
chain and a fallback to the peer address. Every BDS client behind that edge
shares one rate-limit bucket, and metrics and logs show one IP for the whole
fleet. No gRPC test uses an RFC 7239 header. Found while auditing 30.

---

## 134. A second unguarded nil dereference in `Cache.Set`, earlier and broader than entry 67

**Status: FIXED in the fork.** Upstream still carries it. **Severity: low
today, and it fired exactly when an operator debugged a cache problem.** Pinned
by
`architecture/evm/json_rpc_cache_nil_result_test.go:TestCacheSet_ANilResponseAtTraceLevelIsNotAPanic`
and `TestJsonRpcResponse_NilReceiverReadsAsNoResult`.

**The entry was accurate about the panic and its position.** One claim is too
strong: it does NOT fire for every policy. A nil response is emptyish, so
`MatchesForSet` drops any policy whose empty behaviour is `ignore`, and
`len(policies) == 0` returns before the trace block. The trace site needs the
same policy set entry 67 names — `allow` or `only`. What is genuinely different
is the position: it runs before the fan-out, so it panics before
`shouldCacheResponse` gets to decide anything.

**Also worth recording:** the caller guard at `erpc/networks.go:2395` is weaker
than both entries assume. `NormalizedResponse.JsonRpcResponse` answers
`(nil, nil)` for a RELEASED response too (entry 76), so `if resp != nil` does not
prove `rpcResp != nil`.

**Fix — extended, not duplicated, exactly as this entry suggests.**
`GetResultBytes` and `ResultLength` now decide the nil receiver themselves and
return nil and 0. `IsResultEmptyish` on the same type already did, so this makes
the type consistent and fixes 67, 134 and every future reader at once. The
per-call-site guard inside `shouldCacheResponse` is deleted — the type answers
now, so no caller needs its own.

The trace line needed one more change; see entry 160.

Entry 67 records that `Cache.Set` dereferences a response that
`shouldCacheResponse` handles as nil. `architecture/evm/json_rpc_cache.go:740`
is a second dereference of the same `rpcResp`, in the same function, and entry
67 does not name it:

```go
RawJSON("result", rpcResp.GetResultBytes()).
```

It sits inside the `lg.GetLevel() <= zerolog.TraceLevel` block, so it runs
before the policy fan-out and before `shouldCacheResponse`. It therefore panics
earlier than the site entry 67 names, and for every policy rather than only an
`only` or `allow` one.

The caller guard at `erpc/networks.go:2395` keeps it latent, which is the same
reason entry 67 stays latent. The trigger is raising the log level to trace —
what an operator does to debug the cache. Found while auditing 67.


---


## 135. An upstream can forge structure in the consensus response hash

**Status: FIXED in the fork.** Upstream still carries it. **Severity: highest.** A hostile
upstream defeats the check consensus exists to make. **Confirmed by direct
probe**, not by reading the code.

This is bug 118's defect class, in the other hash. `canonicalizeTo`
(`common/json_rpc.go`) writes the canonical form of a response, and consensus
compares upstreams by the SHA-256 of that byte stream. It wrote the JSON
punctuation — `{`, `}`, `[`, `]`, `,`, `:` — around its members, so the
structure looked delimited. It was not: a string value went out RAW, with no
quotes and no escaping. A string that carries the punctuation therefore forged
structure that was never in the response.

Two probes, both re-confirmed on 2026-08-19:

    A = {"a":"1,\"b\":2"}     B = {"a":"1","b":"2"}
    C = {"a":["x","y"]}       D = {"a":["x,y"]}

A and B canonicalized to the same bytes, and so did C and D. Consensus
reported agreement between them. A whole receipt forges the same way: the
one-field body `{"blockHash":"aa,\"status\":1"}` wrote the same bytes as the
honest two-field `{"blockHash":"0xaa","status":"0x1"}`.

**The worse pair, and the one that needs no punctuation at all.** The old form
dropped the `0x` prefix along with the padding zeroes, and wrote a JSON number
as its own digits. So a number agreed with the quantity that spells it:

    {"result":291}    and    {"result":"0x291"}

Both are well-formed answers to the SAME call, neither carries a `,` or a `"`,
and they state different values — 291 against 657. Chains whose results carry
JSON numbers rather than hex strings make this an everyday shape, so a forger
needs no malformed field at all. The same prefix stripping made the quantity
`"0xab"` agree with the plain string `"ab"`.

The values in a response come from the upstream, so a misbehaving or hostile
node reaches this, not a client. An upstream that wants to pass consensus while
returning different data pads one string field until its canonical form matches
the honest answer. Consensus exists to catch exactly that upstream.

**The fix.** `canonicalizeTo` now writes the self-delimiting encoding that the
cache key uses. A leaf writes a type tag, its byte length in decimal, a `:` and
then its payload. An object writes its tag, the count of the members that
survive the emptyish filter, and a `:`; each member name is framed as a leaf of
its own. An array opens with one tag and closes with another. No JSON
punctuation is written at all, because the frames already say where every piece
stops.

An array streams rather than counting, and that is deliberate. It cannot know
how many elements survive the filter without holding every one of them at once,
and a top-level array — a block trace, a page of logs — is the largest value
eRPC hashes. Counting it would double the peak memory of every consensus hash.

**Why the encoding is injective.** A leaf header is a tag byte, decimal digits
and a `:`. A digit is never a `:`, so a reader always finds the end of the
count, and the count then says exactly how many bytes follow. A reader
therefore consumes a payload by length and never scans it, which is why the
array's closing tag is safe: it is only ever read where a tag is expected, and
no payload byte can be mistaken for it. The whole stream decodes into one tree
of tagged values, and a decodable stream cannot map two different values onto
one byte string.

The tags carry the second half of the argument: a string, a quantity, a JSON
literal, a member name, an array and an object each take their own tag, so a
string can never be read as the structure or the number its bytes spell.

A separator byte would not do the job here, and the reason is sharper than it
was for the cache key. A response body IS upstream-controlled data, so any byte
picked as a separator is a byte the attacker puts inside a string.

**What still agrees, on purpose.** Framing removes the collisions; it does not
touch the deliberate identifications the canonical form is for. A padded
quantity still agrees with a bare one — `"0x0005208"` and `"0x5208"` reach the
hash as the hex tag over the digits `5208` — because vendors disagree on
padding and a fleet split on that would dispute every block. An emptyish member
is still dropped, so an upstream that omits a zero field still agrees with one
that sends it. Member order still does not count, and array order still does.

`removeLeadingZeroes` is gone, replaced by `hexQuantityDigits`, which answers
two questions at once: is this a `0x` quantity, and what are its digits without
the padding. The caller tags a quantity differently from a plain string, so the
normalization no longer erases the difference between them. The old function's
second branch — the one that trimmed a quoted `"0x0…` and left a dangling
closing quote — is deleted with it. Nothing reached that branch: `SonicCfg`
unmarshals into `nil`, `bool`, `float64`, `string`, `[]interface{}` and
`map[string]interface{}`, and none of those marshal to a quoted quantity.

**One behaviour changed beyond the framing**, and entry 145 records it on its
own: an array whose every element is emptyish now writes nothing at every
depth, exactly like an object whose every member is emptyish.

**The tests.** `TestCanonicalHash_SeparatesResponsesAnUpstreamCanForge`
(`common/json_rpc_hashing_test.go`) holds eight pairs that hashed the same
before: the two recorded probes, the one-field receipt forge, the number
against the quantity that spells it, the quantity against the plain string of
its digits, a log entry and a nested object against the strings that spell
them, and a boolean against its own spelling.
`TestCanonicalizeTo_EncodesEveryStructureDistinctly` states the rule those
pairs sample — it encodes 25 values, including nested arrays against flat ones
and strings that spell a whole frame, and fails if any two produce the same
bytes.
`TestCanonicalHashWithIgnoredFields_FramesTheBodyItHashes` puts a forged pair
through the ignore-list path, which removes fields and then hashes what is
left; without it that path could stay forgeable after the plain one was fixed.
`TestHexQuantityDigits` replaces `TestRemoveLeadingZeroes` and pins the
normalizer directly, including the inputs it must refuse.
`TestCanonicalHash_AnArrayOfNothingHashesLikeNothingAtEveryDepth` pins the
depth rule that entry 145 records.

**Mutation result (2026-08-19).** Eight mutations, each reverted after its run.
With `canonicalizeTo` put back to raw strings and JSON punctuation, all eight
pairs fail, and so do the distinctness test, the ignore-list test and the three
golden digests in `TestJsonRpcResponse_Hash`. Seven narrower mutations each
fail exactly the case that names them:

- a quantity tagged as a string — the `"0xab"`/`"ab"` pair, and distinctness;
- a JSON literal tagged as a quantity — the `291`/`"0x291"` pair, and distinctness;
- leaves written unframed — the three scalar pairs, and distinctness;
- the array's open and close tags removed — distinctness;
- the array opened before the first surviving element —
  `TestCanonicalHash_AnArrayOfNothingHashesLikeNothingAtEveryDepth` and
  `TestCanonicalizeTo_ReportsThatItWroteNothing`;
- the padding zeroes left in place — `TestHexQuantityDigits` and
  `TestCanonicalHash_NormalizesLeadingZeroesInHex`;
- the uppercase `0X` prefix no longer recognised — `TestHexQuantityDigits`.

With the fix restored, `common` and `consensus` pass, and so does
`make test-fast`.

**Upgrade cost: none.** Unlike the cache key, nothing reads this hash back. It
decides one consensus round and then goes: `consensus/analysis.go` hashes each
participant's response to group the participants, and `consensus/executor.go`
puts the winner's digest into the misbehaviour snapshot. `JsonRpcResponse`
memoizes it in a `sync.Map` that `Free` clears. No connector holds it, no cache
key derives from it, and no two processes ever compare one, so a rolling
upgrade needs no migration and produces no miss storm.

Two visible things change, and both are diagnostic. The digest in the consensus
analysis log is a new value for the same response, so a dashboard that groups
by that string sees its old values stop. The misbehaviour export
(`consensus/export.go`), where an operator has configured a destination, writes
the new digest into its JSONL records — records written before the upgrade
carry the old digest for the same body, and nothing joins the two.

**`ignoreFields` is unaffected** (entry 14). Field removal runs before
canonicalization and is untouched, so a broader path still subsumes its own
extension. The framing sits under the removal, which is why the ignore-list
path gets its own forgery test rather than trusting the plain one.

## 136. `ParseBlockParameter` lets `blockTag` overwrite `blockNumber` in silence

**Status:** open. **Severity: low.** Found while fixing entry 105.

The object form of the block parameter reads three members in order:
`blockHash` returns immediately, then `blockNumber` is assigned, then
`blockTag` is assigned over the top of it. An object naming both gets the tag,
and nothing says so:

    {"blockNumber": "0x10", "blockTag": "latest"}  ->  "latest", no error

`0x10` and `latest` are different blocks. The request is contradictory, so
either member is a guess; refusing it is the answer that guesses nothing.

The same branch reads `blockNumber` only when it is a string. A numeric
`blockNumber` inside the object is dropped, and the caller then gets the wrong
error:

    {"blockNumber": 16}  ->  "block parameter object must contain
                              blockHash, blockNumber, or blockTag"

The object DOES contain `blockNumber`. The message sends an operator looking
for a missing member instead of a rejected type. Feeding a numeric member back
through the same range check entry 105 added would handle it exactly.

---

## 137. The EVM cache key prefixes the method without hashing it

**Status:** not a bug, closed 2026-08-21. Checked while fixing entry 118,
and recorded so the argument below does not have to be rebuilt.

`CacheHash` returns `{method}:{sha256(params)}`. The method sits in the key as
a plain prefix and never enters the hash, so in principle a method name
containing `:` could spell another method's key. It cannot today: the hash is
always 64 hex characters, and hex holds no `:`, so `"a:b" + ":" + h'` can never
equal `"a" + ":" + h`. The prefix boundary is decidable from the right.

The guarantee rests on the hash staying fixed-width and hex. The SVM key
(`architecture/svm/json_rpc_cache.go`) does not rely on that — it writes the
method into the hash as well as the prefix, and says why in a comment. If the
EVM key format ever changes width or alphabet, this entry becomes a real
defect. Hashing the method costs one write.

---

## 138. A JSON block number above 2^53 is already wrong when eRPC receives it

**Status:** open, and not eRPC's to fix alone. **Severity: low today.**
Recorded while fixing entry 105.

Entry 105 now rejects every JSON number a `uint64` cannot hold. The accepted
range still overstates what eRPC can honour. A JSON number arrives as a
`float64`, which represents every integer exactly only up to 2^53. Between 2^53
and 2^64 the parser rounds before `ParseBlockParameter` sees the value, so
block 9007199254740993 arrives as 9007199254740992 and the range check passes
it as a whole number.

No chain is near 2^53 blocks, so nothing observed forces a narrower bound, and
narrowing it on speculation would reject values that are exact and legitimate.
The honest statement is the one to keep: a client that needs a block number
above 2^53 must send it as a hex string, which JSON-RPC has always allowed and
which loses nothing. The same limit applies to any JSON number eRPC reads,
not just this one.


---

## 140. The stress harness builds a server config eRPC refuses, and the refusal kills the test binary

**Status: FIXED in the fork.** Upstream still carries it. **Severity: high for the test suite.** It hid the whole
`test/` package.

`test/fake_erpc.go` — `prepareERPCConfig` built a `common.ServerConfig` with
`HttpHostV4`, `HttpHostV6` and `HttpPortV4`, and set neither `ListenV4` nor
`ListenV6`. `HttpServer.Start` rejects that config outright:

    if s.serverV4 == nil && s.serverV6 == nil {
        return fmt.Errorf("you must configure at least one of server.listenV4 or server.listenV6")
    }

`erpc.Init` runs `httpServer.Start` in a goroutine and answers any error with
`util.OsExit` (bug 99), so `go test ./test/` ended in 0.7 s with no assertion,
no panic and no reason. Measured before the fix:

    $ go test -c -o /tmp/testpkg.test ./test/ && (cd test && /tmp/testpkg.test)
    === RUN   TestStress_EvmJsonRpc_SimpleVariedFailures
    $ echo $?
    234

234 is `1002 & 0xFF` — bug 98, still open on this branch despite a report that
it had landed. `util/exit.go` still reads `ExitCodeHttpServerFailed = 1002`.

The reason stayed invisible because `util.ConfigureTestLogger` sets the global
zerolog level to `Disabled`. eRPC does log the cause; nothing prints it:

    $ LOG_LEVEL=trace /tmp/testpkg.test
    ERR failed to start http server: you must configure at least one of
        server.listenV4 or server.listenV6

Three separate defects had to line up: a config the library refuses, a library
that exits instead of returning, and a logger that swallows the one line that
explains it. Only the first is fixed here — `ListenV4` is now set. The other
two are 99 and 98.

## 141. The stress harness raced on `err` and threw away eRPC's start result

**Status: FIXED in the fork.** Upstream still carries it. **Severity: medium.** A dead eRPC looked exactly like a
live one.

`test/fake_erpc.go` — `executeStressTest` started eRPC like this:

    erpcConfig, localBaseUrl, err := prepareERPCConfig(config)
    ...
    go func() {
        err = initializeERPC(erpcConfig)
        ...
    }()
    time.Sleep(1 * time.Second)
    err = runK6StressTest(fs, localBaseUrl, config)

Two defects in six lines. The goroutine writes the outer `err` while the test
goroutine writes and reads it — an unsynchronised write to a shared variable.
And whatever the goroutine writes is overwritten by the next line, so a failed
`erpc.Init` never reached the caller.

The one-second sleep stood in for readiness. It is not a readiness check: it
passes whether eRPC is serving or gone.

The harness now returns `erpc.Init`'s error on a channel and polls the service
port until it accepts a connection, with the exit-code guard checked on every
pass. A boot failure is reported in about 0.1 s and names the cause.

## 142. The WebSocket mock upstreams write to one connection from two goroutines

**Status: FIXED in the fork.** Upstream still carries it. **Severity: medium for the test suite.** Three
reproducible data races, and the WS tests could not be trusted under `-race`.

`erpc/ws_server_test.go` — `mockWsUpstream` handed each handler the raw
`*websocket.Conn`. gorilla/websocket permits one concurrent writer per
connection; its own documentation says so. Several mock handlers break that
rule: they answer `eth_subscribe` from the read loop and then deliver a
notification from a timer goroutine, both with `conn.WriteJSON`.

    go func() {
        time.Sleep(300 * time.Millisecond)
        deliverLog(conn, "0xblock1", "0xtx1", "0x0")   // writer 1
    }()
    ...
    conn.WriteJSON(...)                                 // writer 2, read loop

`go test -race ./erpc/ -run 'TestWebSocket_'` reported four races on
`Conn.beginMessage` and `messageWriter.flushFrame`, and failed
`TestWebSocket_RegressionFilterFanOutAcrossUpstreams`. The same shape sits in
`TestWebSocket_SubscriptionDedup/TwoClientsShareOneUpstreamSubscription`,
whose 500 ms notification goroutine shares the connection with its read loop.

The counters in these tests were already guarded — the shared plain variable
everyone looks for was not the problem. The connection was.

The fix serialises writes by construction rather than per site: `mockWsUpstream`
now hands handlers a `mockWsConn`, which embeds the connection and holds a
mutex across `WriteJSON`. A handler cannot reach the unguarded write method by
accident. `ws_server_selfheal_test.go` already used this shape by hand, which
is what a per-site fix looks like when someone remembers.

No assertion changed.

## 143. `ConfigureTestLogger` disables logging by default, so a test's only diagnostic is silence

**Status:** open. **Severity: low on its own, high next to 99.**

`util/testing.go` — `ConfigureTestLogger` sets `zerolog.SetGlobalLevel(
zerolog.Disabled)` unless `LOG_LEVEL` is set. Every package that calls it from
`init()` therefore runs its tests with all eRPC logging off.

That is the right default for a passing run. It is the wrong default for a
failing one, because eRPC reports several classes of failure only through the
logger. Bug 140 is the worked example: the library printed the exact reason it
refused to start, the logger dropped it, and the operator saw an empty test
run.

The cost is not the missing lines. It is that a developer has no way to know
the lines exist. Nothing in the failure output mentions `LOG_LEVEL`.

The weakening is to let the failure decide, not the package: keep the default
quiet but write through `t.Log`, so Go prints the buffered output for failing
tests only. Short of that, name the switch in the harness — the `test/` package
now does, in the message it prints when eRPC exits during boot.

## 144. The policy eval timeout races on `evalErr`, and then always loses (DUPLICATE of 46)

**This is entry 46, found again independently.** A different agent reached it
from the WebSocket race sweep rather than from the policy suite, and neither
knew of the other. Two routes to one defect is evidence, so the entry stays.
It also carries the mechanism 46 does not state: the timeout error is not
merely raced, it is GUARANTEED to be discarded. The goroutine assigns
`evalErr` before its deferred `close(done)` runs, so the `<-done` that follows
the timeout write always waits for an assignment that overwrites it.
`ErrEvalTimeout` is written at one line and read at none.

**Status: FIXED in the fork.** Upstream still carries it. Fixed as entry 46,
which now carries the fix and the pin. **Severity was: medium** here and
**high** in 46 — the two entries disagreed, and 46's rating is the right one,
because the operator-visible effect is a timeout that can never be observed.

`internal/policy/slot.go:225-244` — `Slot.tickOnce` runs the JS eval in a
goroutine and guards it with a timeout:

    var (
        evalRes *EvalResult
        evalErr error
    )
    done := make(chan struct{})
    go func() {
        defer close(done)
        evalRes, evalErr = runEval(...)      // line 232
    }()

    if timeout > 0 {
        select {
        case <-done:
        case <-time.After(timeout):
            evalErr = fmt.Errorf("%w after %s", ErrEvalTimeout, timeout)  // line 239
            <-done // let the goroutine finish
        }
    }

Two goroutines write `evalErr` with nothing ordering them. The race detector
caught it during `go test -race ./erpc/ -run 'TestWebSocket_' -count=5`, on
the network bootstrap path:

    WARNING: DATA RACE
    Write at 0x00c0048343e0 by goroutine 45690:
      internal/policy.(*Slot).tickOnce.func1()  slot.go:232
    Previous write at 0x00c0048343e0 by goroutine 45668:
      internal/policy.(*Slot).tickOnce()        slot.go:239

The race is the smaller half. The bigger half is that the timeout branch can
never take effect. The goroutine assigns `evalErr` and only then runs its
deferred `close(done)`, so `<-done` returns after that assignment and with it
visible. Line 239 writes the timeout error, line 240 waits, and the wait
guarantees the goroutine's value has replaced it.

`ErrEvalTimeout` is written at exactly one place in the tree and read at none:

    $ grep -rn ErrEvalTimeout internal/ erpc/
    internal/policy/slot.go:239
    internal/policy/errors.go:14  // comment
    internal/policy/errors.go:16  // declaration

So the "selection policy eval failed; retaining previous cache" warning below
never fires for a slow eval, `consecutiveFails` never increments for one, and
a policy that overruns its timeout is published as if it had finished on time.
The knob reports a bound the code does not hold.

The fix returns the outcome on a channel instead of sharing variables, which
removes the race and makes the timeout verdict stand:

    type evalOutcome struct {
        res *EvalResult
        err error
    }
    outcome := make(chan evalOutcome, 1)
    go func() {
        r, e := runEval(...)
        outcome <- evalOutcome{r, e}
    }()

    select {
    case o := <-outcome:
        evalRes, evalErr = o.res, o.err
    case <-time.After(timeout):
        evalErr = fmt.Errorf("%w after %s", ErrEvalTimeout, timeout)
        <-outcome // drain and discard; the timeout verdict stands
    }

That changes what eRPC does when an eval overruns, so it needs a test that
pins the new behaviour before it lands.


## 145. `canonicalizeTo` wrote bytes it then reported as unwritten

**Status: FIXED in the fork.** Upstream still carries it. **Severity: low.** It changed one hash,
at one depth. Found while fixing entry 135.

The array branch wrote `[` before it knew whether any element would survive the
emptyish filter. When none did, it wrote `]`, returned `false`, and left `[]`
in the writer. Every nested caller renders a child into a borrowed buffer and
throws the buffer away when the child reports `false`, so the stray brackets
vanished — except at the top, where `CanonicalHash` hashes the writer directly
and ignores the flag.

So a result of `[null]` hashed as `[]` while a MEMBER holding `[null]` hashed
as absent. One rule at depth 1 and another at depth 0, from the same branch.

The array branch now writes its opening tag only when the first surviving
element is ready, so an all-emptyish array writes nothing at any depth, exactly
like an all-emptyish object. Pinned by
`TestCanonicalHash_AnArrayOfNothingHashesLikeNothingAtEveryDepth`, which
compares a top-level `[null]` against `[]` and against `{}`, and keeps one
surviving element as the control. `TestCanonicalizeTo_ReportsThatItWroteNothing`
already asserted the flag and still does.

## 146. The bug log carried three unresolved merge-conflict markers

**Status: FIXED in the fork.** Upstream still carries it. This file is the fork's own, so there is
nothing to send upstream. **Severity: low**, and confined to this file. Found
while opening entry 135.

`valve/upstream-bug-log.md` held `<<<<<<< HEAD`, `=======` and `>>>>>>>`
markers from two worktree branches, committed as text. Entry 135 itself sat
inside one of the conflict sides, so a reader could not tell which half of the
file was current.

Two of the three conflicts were nested in each other, and their three sides
held DISJOINT entries: 125-134, then 135-138, then 140-144. All three sides are
correct, so the resolution keeps every entry and renumbers nothing. The
remaining conflict was one paragraph of entry 118 written twice, once before
its fix landed and once after; the "fixed-in-fork" version is the true one and
it stays.

Nothing in the tree reads this file, so the markers cost only a reader's
confidence. They are worth naming because the same accident in a `.go` or
`.yaml` file would not compile, and this one merged in silence.

## 147. A dead branch in `removeLeadingZeroes` produced an unbalanced string

**Status: FIXED in the fork.** Upstream still carries it. **Severity: low.** Unreachable. Found
while fixing entry 135.

`removeLeadingZeroes` had a second branch for a quantity that arrives already
quoted: on `"0x0a"` it trimmed from index 3 and returned `a"` — the closing
quote kept, the opening quote and the prefix gone. Its own test asserted that
output under the name "quoted hex keeps the trailing quote".

The branch could not run. Only `canonicalizeTo` called the function, and its
three call sites pass a Go `string`, a `[]byte`, or the output of
`SonicCfg.Marshal` for a value that is neither. `SonicCfg` unmarshals a
response into `nil`, `bool`, `float64`, `string`, `[]interface{}` or
`map[string]interface{}`, and no `bool` or `float64` marshals to a quoted
quantity.

The replacement, `hexQuantityDigits`, drops the branch: an input that is not a
bare `0x` quantity comes back with `ok` false and reaches the hash whole. That
is the weaker rule — it commits to nothing about what a quoted value means —
and `TestHexQuantityDigits` pins it with the quoted case asserting `false`.

## 165. `GetBlockReceipts` picks one of two mutually exclusive fields, and a test pinned it

**Status: FIXED in the fork.** Upstream still carries it. Found while fixing 32. **Severity: medium.** A
silent wrong block, same shape as the `blockHash` half of 32.

`evm.GetBlockReceiptsRequest` declares `blockNumber` and `blockHash`, and the
proto says of each: "Mutually exclusive with" the other.
`erpc/grpc_server.go` read them as a preference order:

    if len(req.BlockHash) > 0 {
        blockParam = evm.BytesToHex(req.BlockHash)
    } else if req.BlockNumber != nil {
        blockParam = *req.BlockNumber
    }

A client that sets both gets the hash's block and never learns the number was
dropped. The handler now returns `InvalidArgument` when both are set.

**A test pinned the defect.**
`TestGrpcGetBlockReceipts_PrefersTheHashWhenBothAreGiven` asserted the
precedence, with the comment "pins the precedence the handler chose". A wire
contract that says "mutually exclusive" is not a precedence to preserve, so the
test was rewritten rather than worked around. It is now
`TestGrpcGetBlockReceipts_RejectsABlockNumberTogetherWithABlockHash`
(`erpc/grpc_server_rpc_test.go`).

Mutation: deleting the guard fails the rewritten test.

---

## 166. The gRPC surface never fills the chain fields it can answer

**Status:** open. **Severity: low.** The mirror image of 32, on the response
side.

BDS declares chain identity on responses too, and eRPC leaves all of it unset:

- `ChainIdResponse.genesisHash` (`erpc/grpc_server.go`, the `ChainId` handler
  returns `&evm.ChainIdResponse{ChainId: chainID}` only).
- `GetBlockResponse.chainId` and `GetBlockResponse.chainGenesisHash`
  (`GetBlockByNumber` and `GetBlockByHash`).

`chainId` is the cheap one: the handler already knows the network it routed to,
so it can fill `GetBlockResponse.chainId` with no new lookup. That closes the
loop for a client that wants to confirm which chain answered — the same
question entry 32's request-side `chainId` asks. The two genesis-hash fields
need a genesis hash eRPC does not hold, which is the same reason 32 refuses the
request-side field; leaving them unset is correct until eRPC learns one.

A client cannot tell "chain 0" from "not set" through the generated
`GetChainId()` accessor, only through the pointer field, which is why an unset
`chainId` is worth filling rather than leaving to the reader.

---

## 167. The stress harness writes one `err` from two goroutines and reads neither

**Status: FIXED in the fork.** Upstream still carries it. Found while fixing 99. **Severity: medium.** A data
race, plus a swallowed reason.

`test/fake_erpc.go`, in `executeStressTest`:

    go func() {
        err = initializeERPC(erpcConfig)      // writes the outer err
        if err != nil {
            log.Error().Err(err).Msg("Error initializing eRPC")
        }
    }()

    time.Sleep(1 * time.Second)

    err = runK6StressTest(fs, localBaseUrl, config)   // writes it again

`err` is the function-scope variable from `prepareERPCConfig`. The goroutine
and the main path both write it with no synchronisation, so `go test -race`
has a real race to find. Worse, nothing reads the goroutine's value: if eRPC
never came up, the harness logged one line, slept a second, and ran k6 against
nothing.

The fix gives the goroutine its own channel and makes the wait a `select`, so
the harness returns "failed to initialize eRPC: ..." with the reason instead of
sleeping and guessing. This is only reachable now that `Init` returns its
transport failures (entry 99); before, the same failure killed the test binary
outright.

## 168. A third load-triggered flaky test, in `internal/policy/stdlib`

**Status: FIXED in the fork.** Upstream still carries it. **Severity was: low
for correctness, medium for CI trust.** Same shape as entries 10 and 23.

`TestStdlib_DemoteTag_RanksLastNeverDrops`
(`internal/policy/stdlib/stdlib_test.go:1688`) failed once during a
`make test-fast` run that shared the machine with two other full test runs. It
expected the four ranked upstream ids and got an empty list:

    expected: []string{"primA", "primB", "fb1", "fb2"}
    actual  : []string{}

An empty result means the policy never produced an ordering, not that it
produced a wrong one. The test sets `EvalTimeout: 50ms` and then reads
`GetOrdered` straight after `RegisterNetwork`, so a starved CPU can miss the
first evaluation window. That is a 50ms bet on how busy the machine is, which
is exactly what entries 10 and 23 describe.

The same test passes 5 runs in a row on a quiet machine, and the whole
`internal/policy/...` tree passes. Nothing in the failing path was modified by
the work that found this.

**The fix.** The bet is deleted rather than made safer. `testEvalTimeout`
(`internal/policy/stdlib/eval_timeout_testconst_test.go`, and its twin in
`internal/policy`) replaces 77 hand-written per-test deadlines of 10 ms, 50 ms,
100 ms and 200 ms. The initial eval is synchronous inside `RegisterNetwork`
(`internal/policy/engine.go`), so a deadline that cannot plausibly fire makes
these tests deterministic, not merely less flaky.

The constant is 10 s, not larger: `EvalInterval` defaults to 15 s when unset
and `Validate` refuses an `evalTimeout` that is not below the interval. Six
call sites drive a real ticker with a smaller interval and therefore keep their
own small timeout — the constant would be invalid there.

This matters more since 46 was fixed. The timeout now WINS, so an eval that
overruns no longer sneaks a late answer into the cache. Every one of those 77
deadlines became more fragile the moment 46 was fixed.

**What was NOT demonstrated.** I could not reproduce the original failure. With
the 50 ms deadline restored and eight CPU burners saturating all 8 cores,
`./internal/policy/stdlib/ -race -count=3 -parallel 16` still passed. So the
mechanism above is read from the code and is not confirmed by a reproduction.
What IS established: the arbitrary deadline is gone, `-race -count=3
-parallel 16` is clean across the whole policy tree, and the test no longer
depends on how busy the machine is.

## 150. The selection-policy error kind is decided by substring, on a rendered string

**Status:** open. **Severity: low.** A metric lies about which failure
happened. The string match is NARROWED on `main`; the shape is the residual.

`internal/policy/slot.go` — `Slot.emitMetrics` labels
`selection_eval_errors_total{kind}` by searching the decision's error TEXT:

```go
kind := "throw"
if strings.Contains(d.Error, ErrEvalTimeout.Error()) {
    kind = "timeout"
} else if strings.Contains(d.Error, ErrInvalidReturn.Error()) {
    kind = "invalid_return"
}
```

`Decision.Error` is a `string`, so `errors.Is` is not available by the time
the classification runs. The match used to be the bare words `"timed out"`,
which any user eval could produce by throwing about its own timeout. Fixing 46
narrowed the match to this package's own sentinel text, and
`TestSlot_UserThrowAboutATimeout_IsNotAPolicyTimeout` pins it.

The residual is the shape, not the string. A user error that happens to quote
eRPC's own wording still lands in the wrong bucket, and any future rewording
of a sentinel silently reclassifies live traffic. The weak fix is to carry the
typed error to the classifier — `tickOnce` still holds it two lines above the
`emitMetrics` call — and let `errors.Is` decide. That deletes the string
matching instead of tuning it. Left out of the 46 fix on scope: it changes a
function signature the simulator also calls.

## 151. The stdlib test fixtures set an eval timeout the race build cannot meet

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium for the test suite.** Load decided the verdict. Same defect as 168.

Every fixture in `internal/policy/stdlib/*_test.go` set
`EvalTimeout: 50ms` (a few set 100 ms or 200 ms). The stdlib primer costs
about 20 ms per fresh sobek runtime under `-race`, and a loaded machine
pushes a whole tick past 50 ms. None of these tests test the timeout — they
test `excludeIf`, `sortByScore`, sticky routing and the rest.

While the defect in 46 was live this only flaked the suite. With 46 fixed a
missed deadline became a real failure: the tick reports the timeout, keeps the
previous cache, and the test reads an order that never arrived. One run of
`-race -count=5` failed `TestStdlib_Combinators_WhenEmpty_FallbackTo` this
way; the same test passed 40 times in a row when run alone.

**What `main` carries is broader than this entry proposed.** Rather than a 5 s
raise in one package, `testEvalTimeout` replaces all 77 hand-written deadlines
across `internal/policy` AND `internal/policy/stdlib`. It is 10 s, not 5 s, and
the ceiling is not arbitrary: `EvalInterval` defaults to 15 s when unset and
`Validate` refuses an `evalTimeout` that is not below the interval. Six sites
that drive a real ticker keep their own small timeout. See 168.

The general rule: a test that does not test the timeout must not be able to
hit it. Pick a bound the slowest instrumented build cannot reach.

## 152. This log carries three unresolved merge-conflict markers

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium for the log.** THIRD independent record of one defect — see also 159
and 163. 163 carries the repair; this entry and 159 each recorded it first
from a different session, neither knowing of the others.

`valve/upstream-bug-log.md` still holds raw conflict markers:

* `:3868`, `:3873`, `:3877` — entry 118 has two competing status paragraphs.
  The `HEAD` side says `open`; the other side says `fixed-in-fork
  (2026-08-19)`. Entry 118 is the cache-key collision, and commit `1e58054`
  fixed it, so the `fixed-in-fork` side is the true one.
* `:4034`, `:4035`, `:4274`, `:4379`, `:4380`, `:4590` — a nested conflict
  that spans whole entries (125 through 144). Both sides were kept, so the
  content is present but the boundaries are noise.

Left in place on purpose. Repairing it means deciding entry 118's status and
reflowing several hundred lines of unrelated entries, which is its own commit
and should not ride along with a code fix. Whoever takes it should start with
the `git log` for the two worktree branches the markers name.

## 153. A policy that always overruns its timeout now routes on registration order

**Status:** not a bug — the routing is the correct trade, recorded so nobody
reports it as a regression. **Severity: low.** It is the tail of 46. The
unwired `consecutiveFails` recovery described at the end IS a real gap; it is
held here rather than given an id of its own.

Before 46 was fixed, a policy that always exceeded `evalTimeout` was applied
anyway, late and silently. After the fix every such tick fails, so the slot
cache never fills, and `erpc/networks.go:1834` serves the documented
cold-start fallback — the raw registration order, unfiltered and unranked.

That is the right trade: the operator gets a warning and a
`selection_eval_errors_total{kind="timeout"}` increment on every tick, and
requests keep flowing. Silently publishing a stale verdict past the bound the
config declares is worse than routing on registration order while shouting
about it.

One gap stays open underneath. `Slot.consecutiveFails` now increments for
timeouts as well as throws, but nothing reads it: the comment at
`internal/policy/slot.go:87` says "spec §5.7 falls back to the default policy
after 3 consecutive failures. (Hookup deferred to Phase 5.15.)" So the
designed recovery — swap a broken user policy for the default one — is still
not wired. A permanently slow policy stays on the cold-start fallback until an
operator raises `evalTimeout` or simplifies the eval.

## 154. `EvalInterval: 0` does not freeze a slot, and a dozen test comments say it does

**Status:** open. **Severity: low.** A test that runs long enough gets a tick
it did not ask for.

`common/defaults.go:3030` rewrites a zero `EvalInterval` to 15 s. `SetDefaults`
runs before `RegisterNetwork`, and `Slot.start` reads the config AFTER that
rewrite, so `interval <= 0` is never true for a config that asked for zero.
The ticker starts.

Test fixtures across `internal/policy` and `internal/policy/stdlib` set
`EvalInterval: common.Duration(0)` with the comment "frozen — tests drive
manual ticks" (`stdlib_test.go:110`, `finality_dimension_test.go:49`,
`translator_e2e_test.go:49`, `sticky_scope_test.go:27`, and more). Every one
of those slots ticks in the background at 15 s. Nothing has failed yet only
because those tests finish first.

The real freeze knob is `DisableTickerForTest`, which `Slot.start` checks
before it reads the interval. `slot_eval_timeout_test.go` uses it. Either move
the fixtures onto that flag, or make `SetDefaults` leave an explicit zero
alone — the second is weaker, but it changes production defaulting, so the
flag is the safer half.

---

## 155. `TestInitializer_MultipleRapidFailures` asserts against a live auto-retry loop

**Status: FIXED in the fork** (`util/initializer_test.go`). Fork test only —
upstream carries the same test and the same fault. **Severity: low** as a
defect, **high** as an obstacle: it is the reason nobody could read a `util`
race run.

Entry 122 reported that `util` passes `-race` at `-count=4`. It does not, and
it did not before this work either. On a clean tree at `8fe74b2`,
`go test -race ./util/ -count=4` fails in this test, once or twice per run.

The test starts a task that always fails, lets the auto-retry loop run for
200ms, and then asserts on the result **while the loop is still running**. Two
assertions race it:

    err := init.WaitForTasks(ctx)
    require.Error(t, err, "task should eventually fail or context should time out")
    ...
    state := init.State()
    assert.True(t, state == StateFailed, ...)

`State()` reports `StateRetrying` whenever the loop has just re-claimed the
task, because `attempts > 1` and `running > 0`. `WaitForTasks` returns `nil`
for the reason in entry 157. Both were observed.

**Fix:** call `init.Stop(nil)` BEFORE asserting. `Stop` cancels the loop and
waits for it, so afterwards nothing can change the task. The assertions then
read a settled initializer: more than one attempt, a non-nil `Errors()`, and
`StateFailed`. Against the previous version the test fails two runs in twelve
at `-race -count=12`; the new version passes twelve for twelve, and
`go test -race ./util/ -count=4` is green.

## 156. A test returns while its task goroutine is still writing to its logger

**Status: FIXED in the fork** (`util/initializer_reporting_test.go`). Fork test
only. **Severity: low.** A data race in the test binary, not in the product.

`TestInitializer_StateRetryingAfterRepeatedAttempts` blocks its task on a
channel and releases it with `defer close(block)`. The deferred close is the
last thing the test does, so the task goroutine wakes up after the test
function has returned and logs "initialization task succeeded" into the
`zerolog.NewTestWriter(t)` the test owns. `testing.tRunner` has already marked
the test done, so that write is a data race:

    WARNING: DATA RACE
    Read at ... by goroutine 8204:
      testing.(*common).destination()
      zerolog.TestWriter.Write()
      util.(*Initializer).attemptRemainingTasks.func1.1()
    Previous write at ... by goroutine 8203:
      testing.tRunner.func1()

`go test -race ./util/ -run TestInitializer_StateRetryingAfterRepeatedAttempts
-count=20` reproduces it on a clean tree at `8fe74b2`.

This is entry 109's shape, but the cause here is the test, not `Stop`. The
product change in entry 158 is what makes a barrier possible; the test change
is what uses it.

**Fix:** close the channel explicitly and then `WaitForTasks` before returning.
Reverting either half brings the race back at `-count=20`.

## 157. `BootstrapTask.Wait` reports success for a task that never succeeded

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
medium.** A caller on the request path could be told a bootstrap finished
cleanly when every attempt failed.

`util/initializer.go`, in `Wait`:

    case <-ch.(chan struct{}):
        // The attempt ended. Check if we failed.
        if TaskState(t.state.Load()) == TaskFailed {
            ...
            return wr.err
        }
        return nil // Succeeded or otherwise finished

`ch` is the done channel of the attempt that was current when `Wait` loaded it.
Between that channel closing and the `state.Load()` on the next line, the
auto-retry loop can claim a new attempt and set the state to `TaskRunning`. The
comparison then fails, and `Wait` returns `nil` — "succeeded or otherwise
finished" — for a task that has failed every attempt so far and is failing
again right now.

`waitForTasks` compounds it. After `Wait` returns `nil` it checks
`state == TaskFailed` before recording an error, and the state is `TaskRunning`
by construction, so nothing is recorded. `ExecuteTasks` returns `nil` and the
caller treats the resource as ready.

Reproduced by the previous version of `TestInitializer_MultipleRapidFailures`
(entry 155) under `go test -race ./util/ -count=4`: every attempt logged
"initialization task failed", and `WaitForTasks` still returned no error.

**The fix.** `Wait` loops instead of returning: only a TERMINAL state ends the
wait. The worry recorded here — that the substitution lives in the `case <-ch`
branch and would have to move — turned out to resolve itself. The substitution
is stored in `lastErr`, and the top-of-loop terminal check reads `lastErr`, so
looping delivers it without moving anything.

**THREE routes to this defect, not one.** The entry above describes the race.
Reading the function turned up two more, both deterministic:

1. The `case <-ch` branch stored the substituted error and then ran
   `return wr.err` — the ORIGINAL nil. A failed task with no recorded reason
   reported success every single time, no race needed.
2. The terminal branch itself returned `lastErr` if set and `nil` otherwise. So
   a `Wait` called on an already-failed task that recorded no reason ALSO
   answered success. The cancellation path is a live producer of exactly that
   state — see `TestInitializer_CancelledTaskIsReportedWithoutItsReason`.
   `TaskFailed`, `TaskTimedOut` and `TaskFatal` now answer an error whatever
   `lastErr` holds; only `TaskSucceeded` answers nil.

**Fixed together with entry 70**, because they are the same function and they
interact: looping past an attempt's end re-reads `doneVal`, which can still
hold the closed channel just consumed, and selecting on it again is precisely
70's hot spin reached by a new route. `Wait` now remembers the channel it
consumed and sleeps until the next one is published.

**Two callers had to change with it.**

- `waitForTasks` treated ANY non-nil `Wait` error as "the context was
  cancelled, give up on the rest". That was true while `Wait` returned nil for
  task failures. Now it returns the task's own error, so the check is on
  `ctx.Err()` and a task failure falls through to be recorded — which keeps the
  aggregate "N/M tasks failed" message.
- That aggregate built its text with `%v` on the error slice, so the chain
  stopped there and `errors.Is(err, context.Canceled)` answered false for an
  aggregate that plainly contained it. It never showed before, because a task
  error used to leave `Wait` as a bare early return and never reached that line.
  It now wraps `errors.Join(errs...)` with `%w`.

Pinned by `TestBootstrapTask_WaitOnATerminalFailureWithNoRecordedReasonStillFails`
(all three terminal states), `TestBootstrapTask_WaitOnASuccessAnswersNil` as the
control, and `TestBootstrapTask_WaitDoesNotMistakeAnEndedAttemptForAnEndedTask`,
which drives the window directly rather than betting on a real race. All three
fixes are mutation-proven. `go test ./util/ -race` is clean.

## 158. A task's terminal state was published before the log line it summarises

**Status: FIXED in the fork** (`util/initializer.go`). **Severity: low.**

Every branch of the task goroutine stored the terminal state and only then
logged:

    bt.state.Store(int32(TaskSucceeded))
    lastAttempt, _ := bt.lastAttempt.Load().(time.Time)
    i.logger.Info().Str("task", bt.Name).Dur("durationMs", ...).Msg("...")

`Wait`, `State` and `Stop` all treat a terminal state as "this attempt is
done". It was not. A caller could observe `TaskSucceeded`, return, and tear
down the component whose logger the goroutine was still holding — the same
class of fault entry 109 describes, reached without any timeout.

**Fix:** publish the state last, in all three branches. The error and the log
line now happen before the `state.Store`, so a watcher that sees a terminal
state has, through that atomic, seen every side effect of the attempt. This is
a reordering, not a new mechanism, and it costs nothing.

It does not close entry 109. `Stop` can still give up while `bt.Fn` itself is
still running, and that `Fn` writes to the same logger.

**It has one cost, and it is worth naming.** Publishing the state later widens
any window in which an observer polls `State()` while a task is finishing —
the log write now sits inside that window, and a zerolog test writer holds a
mutex. `TestInitializer_MultipleTasksMixedResultsNoRetry` was latently racy on
exactly that window and never tripped: 0 failures in 200 runs before the
reorder, 33 in 200 after it. The test asserts `StatePartial` straight after
`ExecuteTasks` returns, but `waitForTasks` returns as soon as ONE task reports
an error, so a sibling task can still be running — and `StatePartial` requires
every task to be terminal. The test now waits for each task before it reads the
aggregate state. Reverting that wait brings back 31 failures in 200 runs.

The reorder is still the right trade. `State()` is a snapshot and no caller can
treat it as settled, whereas "a terminal state means the attempt is done" is a
guarantee three callers already assumed. Six consecutive
`go test -race ./util/ -count=4` runs are green.

## 159. The bug log itself carries unresolved merge conflicts (SAME DEFECT as 163)

**This is the first record of the defect that 163 fixed.** Both were written
against commit `8fe74b2`, from different sessions, and neither knew of the
other. 159 reports the markers and declines to resolve them; 163 resolves them
and records what the resolution uncovered. Read 163 for the repair.

**Status: FIXED in the fork.** Upstream still carries it. Fork document only
(`valve/upstream-bug-log.md`). **Severity was: low** for the product, high for
this file — it was unreadable from entry 118 onward.

At `8fe74b2` the file contains literal conflict markers:

    3827:<<<<<<< HEAD
    3836:>>>>>>> worktree-agent-a40ba5dcb41c740c9
    3993:<<<<<<< HEAD
    3994:<<<<<<< HEAD
    4338:>>>>>>> worktree-agent-a40ba5dcb41c740c9
    4550:>>>>>>> worktree-agent-a694d8f4044ea228a

The second region is nested and spans roughly 550 lines, so entries 125 to 144
sit inside a three-way conflict. The two sides differ in substance, not
formatting: entry 118's two versions disagree about whether the defect is open
or fixed. Resolving it needs a person who knows which sessions produced which
half, so it is recorded rather than guessed at.

**How it was resolved.** Nobody had to know which session produced which half.
Every side held DIFFERENT entries, so the merge only had to concatenate them.
Entry 118's status block was the sole real disagreement, and the code settled
it. Commit `63bd173` removed the marker lines; `2cf36f2` repaired entries 14,
99 and 118, which still carried stacked status paragraphs after the markers
went, and added the `pre-commit` gate that found them. 163 has the detail.

---

## 160. An empty result corrupts the whole cache trace record

**Status: FIXED in the fork.** Upstream still carries it. **Severity: low.** It
costs an operator the one log line they raised the level to read. **Confirmed by
direct probe**, not by reading the code. Pinned by
`architecture/evm/json_rpc_cache_nil_result_test.go:TestCacheSet_ATraceRecordStaysValidJsonWithNoResult`,
which parses every record the call emits.

`Cache.Set`'s trace branch wrote `RawJSON("result", rpcResp.GetResultBytes())`.
zerolog copies a `RawJSON` value into the record verbatim, so an empty slice
leaves a dangling key. With the nil guard removed the probe produced exactly
this line, and it is not JSON:

    {"level":"trace",…,"policies":[…],"result":,"finalityState":"unknown",…}

Everything after `"result":` is unreadable to any log pipeline, including the
`policies` array a cache investigation needs. Two inputs produce it: a response
eRPC could not read (entry 134's nil), and a response whose bytes live in a
`resultWriter` rather than in `result` — `GetResultBytes` returns nil for that
one while `ResultLength` reports a real size.

Fix: the branch substitutes the JSON literal `null` when the result is empty.
This is entry 15 again in a different function — a value written straight into a
log record without checking that it is what the encoder promised.

---

## 161. `protoTraceFromJSON` discards every hex-decode error, the same way it discarded quantities

**Status:** open. **Severity: low-medium.** Same class as 132, different helper.
Found while fixing 132.

`architecture/evm/eth_query_shim.go:567-575` — six sites in
`protoTraceFromJSON` hand a wire string to `common.HexToBytes` and drop the
error: `From`, `To`, `Input`, `Output`, `TransactionHash` and `BlockHash`. Only
`To` is guarded against absence (`decoded.To != nil && *decoded.To != ""`); the
other five cannot tell an absent field from a garbage one.

A garbage `from` becomes an empty byte slice, and `NativeTransfersFromTraces`
then reports a transfer with no sender. Entry 132's rule fits here unchanged:
absent stays absent, present-but-unreadable is reported. I did not apply it, to
keep 132's change reviewable.

`fetchTracesForBlock:443` has a seventh, on the block hash it stamps onto every
trace of the block.

---

## 162. The fallback-tier default selection policy assigns nil to nil

**Status:** not a bug. **Severity: low.** Dead code that reads like a live
default. Found while auditing 131. Closed as not a bug on 2026-08-21: the
code cannot change what an operator or a client sees, so there is nothing to
repair. It stays here because it still reads as live code. Deleting it in
the fork would cost a permanent rebase conflict and change nothing — see
`valve/open-entry-triage.md`.

`common/defaults.go` — `NetworkConfig.SetDefaults` installs a default selection
policy when any upstream carries `tier:fallback`:

    if anyUpstreamInFallbackTier && n.SelectionPolicy == nil {
        defCfg := NewDefaultNetworkConfig(upstreams)
        n.SelectionPolicy = defCfg.SelectionPolicy
    }

`NewDefaultNetworkConfig` (`common/defaults.go:3368`) is
`return &NetworkConfig{}`. Its `SelectionPolicy` is nil, so the branch assigns
nil to a field that is already nil. The function also ignores the `upstreams`
argument it takes.

An operator who tags an upstream `tier:fallback` and writes no
`selectionPolicy` therefore gets no policy from this branch. What they do get is
the placeholder identity policy, from the path
`Network.Bootstrap` -> `SetDefaults` -> `upgradeDefaultPolicy`, which is a
different mechanism than the branch above describes. Either delete the branch or
give `NewDefaultNetworkConfig` a body — but do not leave a reader believing a
default is installed here.

**A second dead pair in the same file.**
`SelectionPolicyConfig.SetDefaults` (`common/defaults.go:3063-3064`) computes
`aliasMethodSet` and `aliasFinalitySet`, writes a nine-line comment explaining
that explicit-false must count as "operator set the key" for a branch below,
and then discards both: `_ = aliasMethodSet` / `_ = aliasFinalitySet`. The
branch the comment describes is gone. The comment now documents behaviour the
code does not have, which is worse than no comment.

---

## 163. The bug log carried three unresolved merge-conflict markers on `main`

**Entry 159 is the same defect, recorded first** from another session against the
same commit `8fe74b2`. 159 declines to resolve it; this entry resolves it.

**Status: FIXED in the fork.** Upstream still carries it. **Severity: low for the product, high for
this file.** Found while opening entries 130-134.

`valve/upstream-bug-log.md` at commit `8fe74b2` contained six conflict-marker
lines — `<<<<<<< HEAD`, `=======`, `>>>>>>> worktree-agent-…` — in three places,
one of them nested. They were committed, not left in a working tree:

- entry 118's status block, where the two sides disagreed about whether the
  cache-key collision was fixed;
- entries 125-134 against 135-138 (the inner conflict);
- that whole block against 140-144 (the outer one).

Every side held DIFFERENT entries, so nothing had to be chosen — the merge just
concatenates. The 118 status block was the only real disagreement, and the
"fixed-in-fork" side is correct: commit `1e58054` carries the fix. The
re-confirmation sentence from the other side is kept.

The markers meant three worktree agents merged into `main` without reading the
result. A conflicted document still renders, which is why nobody noticed.

**Follow-up, same day.** Removing the marker LINES did not finish the job. A
`pre-commit` gate added afterwards found that three entries still carried more
than one status paragraph, left stacked when the marker lines were stripped:

- **14** — two copies, and the first was truncated mid-sentence where the
  second spliced in. Kept the copy naming the pinning test, carried over the
  mutation-test date from the other.
- **99** — the two sides DISAGREED: one said `open`, one said `FIXED`. The
  code decides. `erpc/init.go` has no `util.OsExit` call and `cmd/erpc`
  maps `ErrServerFailed` to the exit code, so `FIXED` is correct. The `open`
  side also named a test, `…_LibraryGoroutineExitsTheProcess`, whose name
  still asserted the bug. That test passes today only because the BINARY
  exits, which is the new, correct behaviour. Renamed to
  `TestStart_HttpServerCannotBind_InitReturnsTheErrorAndTheBinaryExits`.
- **118** — three copies of one paragraph. Kept the one carrying the
  out-of-tree recomputation of the double SHA-256.

This is the point of the entry. A marker check settles the SYNTAX, and the
syntax was already clean at `63bd173`. It says nothing about two sides that
merged without a marker and now disagree. Entry 99 is that case, and only
reading `erpc/init.go` settled it.

**Gate.** `.pre-commit-config.yaml` gained `check-merge-conflict` with
`args: [--assume-in-merge]`, and a local `check-bug-log` hook that asserts
unique entry ids and exactly one status per entry from a four-word
vocabulary. `--assume-in-merge` is load-bearing: without it the hook checks
only while `.git/MERGE_HEAD` exists, and these markers survived PAST the
merge commit. Verified by staging a bare `=======` outside a merge — the
hook passes without the flag and fails with it. All three checks were
proven against deliberate mutants.


---

## 164. Entry 96's shared-config premise is not reachable from any config path I traced

**Status:** not a bug — a question, and moot in the fork. **Severity: none
today.** Found while fixing 131. Closed as not a bug on 2026-08-21. The
question stands; the fork's own config path makes it moot. The entry stays
as the record of what was traced.

Entries 96 and 131 both rest on one claim: two networks can reach
`Engine.RegisterNetwork` holding the SAME `*SelectionPolicyConfig`. I could not
find a path that produces it.

- `PreparedProject.FindNetworkConfig` (`erpc/projects.go:86`) walks
  `Config.Networks` and returns the entry whose `NetworkId()` matches, so one
  `*NetworkConfig` serves one network id.
- `NetworkConfig.SetDefaults` copies rather than aliases when it inherits from
  `networkDefaults`: `n.SelectionPolicy = &SelectionPolicyConfig{}` then
  `*n.SelectionPolicy = *defaults.SelectionPolicy`.
- The fallback-tier branch that looks like it could share one is entry 162 — it
  assigns nil.
- `resolveNetworkConfig` builds a fresh `NetworkConfig` for a lazily-created
  network.
- No package-level `SelectionPolicyConfig` exists.

The race entry 96 reports is real — `go test -race` named it — but a TEST
fixture that reuses one config literal across two networks would produce exactly
that report, and test fixtures do reuse literals. That would make 96 a test
defect wearing a production label.

This is a question, not a correction: I did not read every config path, and a TS
config or an admin-API path could still alias. It stops mattering in the fork,
because entry 131's fix means `Network.Bootstrap` registers a copy and no shared
pointer can reach the engine. Recorded so the next reader does not inherit the
premise unexamined.

---



## 171. The shadow block-availability gate drops a request that names no block

**Status: FIXED in the fork.** Upstream carries it in NEW code — `8bbc04f4`,
merged after the fork's last sync. **Severity: medium.** Shadow comparison
silently stops for whole classes of method.

**Found by rebasing, not by reading.** The fork rebased onto upstream and
`TestProjectForward_MirrorsTheServedAnswerToTheShadowUpstream` failed. It passes
on the commit before the rebase. Nothing conflicted; the two sides merged
cleanly and disagreed about behaviour anyway.

`erpc/shadow.go`. Upstream added a gate so a recent-only shadow upstream is not
sent archive-depth traffic it can only refuse:

```go
if _, blockNumber, err := evm.ExtractBlockReferenceFromRequest(ctx, origReq); err == nil && blockNumber > 0 {
    available, err := ups.EvmAssertBlockAvailability(ctx, method, ..., blockNumber)
    if err == nil && !available { continue }
}
```

The intent is right and its comment states the intended bound exactly: "Only a
CONCRETE height the upstream states it does not have is skipped." The code does
not hold that bound, for two reasons.

**1. This is entry 72, third site.** A block-agnostic method has no block to be
missing. A finalized method carries block number **1** as a cache sentinel, and
`blockNumber > 0` reads it as a real height — the same misreading the fork fixed
in `checkUpstreamBlockAvailability` and `eligibleUpstreamIDsForBoundary`.

**2. Shadow is worse than the routing path**, because it runs AFTER the forward.
`origReq` now carries a block number cached from the **answer**, not from
anything the caller asked for. `eth_blockNumber` is the plain case: it is
`realtime`, so the request resolves to 0, but by shadow time the request holds
the head it just returned. The gate then asks whether the shadow upstream "has"
block 1100. Measured: `method=eth_blockNumber block=1100 available=false`.

**3. The fail-open does not fire.** It keys on `err != nil`. A shadow upstream
whose state poller has not learned a head yet answers `available=false` with a
**nil** error — a confident "no" resting on no data. That is not "a height the
upstream states it does not have"; it is an upstream that has not looked.

The fork's fix reuses the helper written for 72: ask
`evm.MethodHasNoBlockDependency(method, network)` first, and only gate a method
that actually depends on a block.

**Left for upstream, not fixed here:** point 3 is a separate defect and survives
this fix. An upstream with no polled head still answers a confident `false` to
`EvmAssertBlockAvailability`. Any caller that trusts that answer inherits the
same bug. The weak fix is for the assertion to distinguish "outside my range"
from "I do not know my range yet", so a caller can fail open on the second.

---

## 172. A fork test pinned the FIELD, not the property, and the rebase broke it

**Status:** not a bug — upstream's change is correct and the fork's test was
too specific. Recorded so nobody "fixes" the migration. **Severity: none for
the product.**

Found by the same rebase as 171. `TestProjectSetDefaults_UpstreamDefaultsReachEveryUpstream`
(`common/defaults_whole_config_test.go`) failed with `expected: 900, actual: 0`.

Upstream `e12b6b9c` migrates `maxAvailableRecentBlocks` into the new
`blockAvailability` window and then deliberately stops carrying BOTH — see
`maxRecentBlocksFor` in `common/defaults.go`, whose comment says why: the two
are enforced as independent lower bounds, so keeping both would narrow the
configured window to whichever is smaller. The 900-block window still reaches
every upstream. It arrives as `Evm.BlockAvailability.Lower.LatestBlockMinus`.

The fork's test asserted `Evm.MaxAvailableRecentBlocks == 900` — the field, not
the property its own name states. It now asserts through a `requireRecentWindow`
helper that accepts either carrier.

**The lesson generalises past this entry.** A test that names a property and
asserts an implementation detail passes for the wrong reason until someone
changes the detail, and then fails for the wrong reason. Both halves cost a
reader time. Compare 99, where a test name outlived the bug it asserted: the
same defect, at the other end.

---
## 170. `waitForTasks` calls `task.Error()` twice and dereferences the second

**Status: FIXED in the fork.** Upstream still carries it. **Severity was:
high.** A nil-pointer SIGSEGV on the shutdown path.

`util/initializer.go`, in `waitForTasks`:

```go
if state == TaskFailed && task.Error() != nil {
    errs = append(errs, task.Error().Err)
}
```

Two calls. `Error()` returns nil when `lastErr` holds no error, and the
initializer stores `wrappedError{nil}` before EVERY attempt. A retry landing
between the guard and the dereference therefore turns the second call into a
nil deref, and the process dies:

    panic: runtime error: invalid memory address or nil pointer dereference
    [signal SIGSEGV: segmentation violation]
    util.(*Initializer).waitForTasks(...)

**Found by fixing 157, not by reading.** While `Wait` returned early for task
errors this line was rarely reached, so the window almost never opened. Fixing
157 made task errors fall through to it, and
`TestInitializer_StopDoesNotDeadlockAgainstTheAutoRetryLoop` — which drives 400
initializers with a 1µs retry delay — crashed on the first run.

The fix is to call `Error()` once and keep the result.

`BootstrapTask.Error()` had a second latent panic on the same path:
`t.lastAttempt.Load().(time.Time)` is a bare type assertion, and `lastAttempt`
is empty until `beginAttempt` runs. `Wait` can now record a reason for a
terminal failure that never recorded its own, so an error without an attempt is
reachable. It uses the comma-ok form; a zero `Timestamp` is the honest answer.

**The lesson is about the ORDER.** This defect was always there. It was
unreachable only because another defect (157) short-circuited the path. Fixing
a bug can hand you the one it was hiding, so re-run the whole package after a
fix, not just the test you wrote for it.

---
## 169. `make test-fast` compiles to one shared path, so two checkouts overwrite each other

**Status: FIXED in the fork.** Upstream still carries it.
**Severity: high for anyone who trusts a test result.**

`Makefile` compiled the erpc test binary to `/tmp/erpc.test` — a single
hardcoded path shared by every checkout on the machine — and then ran six
shards from it:

    go test -c -o /tmp/erpc.test ./erpc
    /tmp/erpc.test -test.run "$SHARD_CONSENSUS" &
    /tmp/erpc.test -test.run "$SHARD_NETWORK"   &
    ...

Two worktrees running the target at once overwrite each other. The second
`go test -c` replaces the binary while the first target's shards are still
executing from it, so a run can report on code from a different worktree.

The failure is silent and it points the wrong way: a GREEN result proves
nothing about the tree you are standing in. That is the same shape as the
600-second truncation this log already records — a measurement that reads
like a pass.

It is not hypothetical. An agent hit it during this work: its `make
test-fast` collided with another agent's run of the same target, and it had
to run the `erpc` package directly to get a result it could trust.

Fix: `ERPC_TEST_BIN := $(CURDIR)/.erpc.test`. Unique per worktree, stable
across repeat runs in the same worktree, and added to `.gitignore`.
