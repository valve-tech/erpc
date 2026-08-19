# eRPC upstream bug log

Bugs that live in code the fork inherits from upstream `erpc/erpc`. Each one is
a candidate to report or to send back as a patch.

The fork tracks upstream and merges from it regularly, so fixing these locally
costs merge surface forever. Sending them upstream costs one pull request.

**Status key.** `open` — found, not fixed anywhere. `fixed-in-fork` — the fork
carries a fix that upstream still needs. `pinned` — a test asserts today's
behaviour and names the defect, so a fix breaks the test rather than passing
unnoticed.

Every entry below was verified in source, not inferred.

---

## 1. API keys cannot be revoked or used on a memory or Redis store

**Status:** open. **Severity: highest.** Security-adjacent.

`erpc/admin.go:334` and `:420` look up a record with
`connector.Get(ctx, data.ConnectorMainIndex, apiKey, "*", nil)`. The range key
is the literal string `"*"`, not a wildcard the connector expands.

The memory connector (`data/memory.go:169`) and the Redis connector
(`data/redis.go:426`) both search for the literal key `"<apiKey>:*"` and miss.
The consumer auth strategy reads with the same wildcard
(`auth/strategy_database.go:179`).

So on an in-memory or Redis auth store, `erpc_addApiKey` writes a record that
update, delete **and authentication** all fail to find. An operator can issue a
key and then neither use it nor revoke it.

**The defect is connector-dependent, which is why it survived.** PostgreSQL
expands the wildcard — `data/postgresql.go:1057-1058` rewrites `*` into a SQL
`LIKE '%'` — so the whole feature works there. Memory and Redis build the
literal key and miss. Anyone testing on Postgres sees nothing wrong.

The record is written at `erpc/admin.go:175` as `Set(ctx, apiKey, userId, …)`,
so the range key is the user id, and the reader does not know it.

Pinned by `TestAdmin_UpdateAndDeleteNeedAStoreThatResolvesTheRangeWildcard` and
`TestDatabaseStrategy_AuthenticateAgainstMemoryConnector_WildcardRangeKey`, with
`TestDatabaseStrategy_AuthenticateSucceedsWhenRangeKeyMatches` as the control.

---

## 2. `CopyResponseForRequest` deadlocks on an unparsed response

**Status:** fixed-in-fork. **Severity: high.** A hung request never returns.

`common/response.go:623` took `resp.RLockWithTrace(ctx)`, a read lock on the
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

**Status:** fixed-in-fork. **Severity: medium.** Operators lose diagnostics
exactly when they need them.

`common/response.go:64` declares `IsObjectNull(ctx ...context.Context) bool`.

`ErrUpstreamRequest.IsObjectNull()` (`common/errors.go:810`) and
`ErrUpstreamsExhausted.IsObjectNull()` (`:1041`) took **no argument**. Neither
type satisfied the interface, although both implemented every other method.

`common.LookupResponseMetadata` therefore returned nil for every error, so
`writeResponseMetadataHeaders` (`erpc/http_server.go:1290`) wrote no
`X-ERPC-Cache` and no `X-ERPC-Upstream` header on an error response. Checked:
`X-ERPC-Attempts` and the retry/hedge counters come from `ExecState`
(`:1258`), not from this interface, so they were never affected.

**The fix** makes both methods variadic and adds the two static assertions the
package lacked, so the signature cannot drift again without a compile error.

One consequence, deliberate: in a batch response the failed items now count
toward `withMeta` (`erpc/http_server.go:1565`) with `FromCache()` false, so a
batch mixing a cache hit with an upstream failure reports `X-ERPC-Cache:
PARTIAL:1` instead of `HIT`. That is the honest answer — the failed item was
not served from cache.

---

## 4. `WithRetryableTowardNetwork` throws the concrete type away

**Status:** fixed-in-fork. **Severity: medium.** The client reads the wrapper
instead of the real error.

`common/errors.go:210` returned the embedded `*BaseError`, not the receiver's
concrete type. Twenty-five call sites chain it: twenty-one in production code,
four in tests.

So a gRPC `InvalidArgument`, or a BDS `INVALID_PARAMETER`, reached the HTTP
layer as a bare `*BaseError`. That value implements neither `ErrorStatusCode()`
nor its own identity, so `errors.As` for `*ErrEndpointClientSideException` did
not find it.

The live consumer is `buildErrorResponseBody` (`erpc/http_server.go:1744`): it
calls `errors.As(err, &exe)` to lift an `*ErrEndpointExecutionException` out of
a failed bundle so the client reads the revert rather than the wrapper. The
four EVM paths that mark a revert retryable
(`architecture/evm/error_normalizer.go:268, 289, 375, 406`) all returned a
`*BaseError`, so that lift never fired for them.

Correction to the original report: `ErrorStatusCode()` has **no** production
consumer in this tree — `determineResponseStatusCode`
(`erpc/http_server.go:1624`) keys off `HasErrorCode`, and the `Code` field
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

`erpc/admin.go:721` asks each upstream for `CordonedReason("*")` only. An
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
the type is empty (`common/defaults.go:1913`). The override path does
`baseCfg = override.Copy()` (line 100) and skips `SetDefaults` entirely.

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

`erpc/config_analyzer.go:1087` hands the result of `Upstream.EvmGetChainId` to
`common.HexToInt64`. But `EvmGetChainId` returns a **decimal** string —
`upstream/evm_upstream_ops.go:51` formats it with `strconv.FormatUint(dec, 10)`
— and `HexToInt64` demands a `0x` prefix.

A healthy upstream on the **correct** chain therefore fails validation with
`invalid hex string: 123`, and the mismatch check at `:1094` is unreachable.

The same file gets it right on its other path: `GenerateValidationReport` uses
`strconv.ParseInt(chainStr, 0, 0)` at `:398`. Two readers of one value
disagreeing is the tell.

`AnalyseConfig` has no caller in this repo, but it is exported, so a fork or a
future CLI path that calls it refuses to start against a good fleet.

Pinned by `TestValidateUpstreamEndpoints_MisparsesTheChainIdItJustFetched`.

---

## 9. `NewErrUpstreamMalformedResponse` panics on a nil upstream

**Status:** open. **Severity: low.** Latent.

`common/errors.go:873` calls `upstream.Id()` with no nil guard. Its siblings
`NewErrEndpointMissingData` (`:2278`) and `NewErrEndpointContentValidation`
(`:3032`) both guard.

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

---

## 11. `guessVendorName`'s multi-level-TLD guard is off by one

**Status:** open. **Severity: medium.** Silently merges per-vendor metrics.

`upstream/upstream.go:1432` reads `if len(rooDomain) < 5`. The comment on the
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

**Status:** open. **Severity: none today.** Unexercised machinery.

`upstream/svm_upstream_ops.go:74-84` has three branches that cannot run: the
`resp.JsonRpcResponse()` error path, `if jrr.Error != nil`, and the `Unmarshal`
failure. `u.Forward` already converts a JSON-RPC error answer into a returned
error, so control never gets past the earlier `if err != nil`.

Confirmed by coverage: those three blocks stay at 0 while every other line in
the function reaches 1, across all five genesis-gate tests.

Worth reporting because it reads as a safety net and is not one.

---

## 13. Data race in `JsonRpcResponse.WriteTo`

**Status:** open. **Severity: highest of the read-path bugs.** Verified under `-race`.

`common/json_rpc.go` `WriteTo` takes `r.errMu.RLock()` — a **read** lock. In the
`else if r.Error != nil` branch it then WRITES the shared field:

```go
r.errBytes, err = SonicCfg.Marshal(r.Error)
```

Two concurrent `WriteTo` calls on a response that carries a typed `Error` and no
`errBytes` race on that field. The race detector reports the write at `:668`
against the read of `len(r.errBytes)` at `:653`.

Multiplexing shares one response across many waiting clients, so this is
reachable in production rather than theoretical.

**Deliberately not pinned by a test.** A race test would fail the baseline, and
the rule for this work is that a test asserts today's behaviour without
breaking the suite. Fix first, then pin.

Fix: take `errMu.Lock()` for that branch, or precompute `errBytes` before
entering the read-locked section.

---

## 14. A prefix in `ignoreFields` makes consensus agree on divergent data

**Status:** open. **Severity: high.** Silent wrong answer.

`common/json_rpc.go:1075-1086` builds a path tree from the ignore list. When one
path is a prefix of another and comes first — `["logs", "logs.*.blockTimestamp"]`
— the builder finds `pathTree["logs"]` already set to `true`, fails the map type
assertion, and keeps writing the remaining segments onto the **root**.

The root then carries a `"*"` entry, and `removeFieldsRecursive` applies it to
every other top-level member.

Two genuinely different responses therefore hash equal, so consensus reports
agreement on data that diverges. Reversing the order of the same two paths gives
a different answer.

The shipped defaults do not hit it. Any operator-written list can.

Pinned by `TestRemoveFieldsByPaths_APrefixPathPoisonsTheRootTree`.

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

`common/json_rpc.go:1636-1650`. `NewErrUpstreamMethodIgnored` stores `method`
and `upstreamId` in `Details`, but the translation passes `nil` details and
prefixes the deepest message with a phrase the message already contains.

The client reads `"method ignored by upstream: method ignored by upstream
configuration"` and never learns which method, or which upstream.

---

## 17. Two marshallers drop fields their YAML twins keep

**Status:** open. **Severity: low.** Misleading admin output.

- `ProviderConfig.MarshalJSON` (`common/config.go:881-889`) omits
  `ignoreNetworks`; `MarshalYAML` at `:892` includes it. An operator comparing
  the JSON and YAML admin dumps sees a network exclusion in one and not the
  other.
- `SecretStrategyConfig.MarshalJSON` (`:2953-2957`) emits only
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

**Status: FIXED IN FORK.** Upstream still needs it. **Severity was: high.**

Fixed together with 35, which is the same root cause. See "Fixes the fork
already carries" below. The pinning test is now
`AWrappedNonceExceptionReachesTheIdempotencyPath`.

`architecture/evm/eth_sendRawTransaction.go:80` gates the idempotency path with
`common.HasErrorCode(re, ErrCodeEndpointNonceException)`.

`HasErrorCode` (`common/errors.go:2543`) type-asserts `StandardError` and
`*BaseError`, and walks `Unwrap() []error`. It does **not** walk a plain
`fmt.Errorf("%w", …)` chain.

The `errors.As` seven lines below it, at `:87`, **does** walk that chain.

So any layer that wraps an `ErrEndpointNonceException` with `%w` fails the first
gate, returns early, and the caller re-broadcasts a transaction that is already
in the mempool. The two checks disagree about what counts as the same error.

Reordering the two checks fixes it, but that is a behaviour change, so it was
pinned rather than fixed: `AWrappedNonceExceptionIsNotRecognised`.

---

## 20. `binarySearchEarliest` reports the tip as the earliest retained block

**Status:** open. **Severity: high.** Two defects that compound.

`architecture/evm/evm_state_poller.go:1732-1752`.

**20a.** The loop narrows `[l, r]` and returns `l` without ever probing the
final value. If no block in the range answers, `l` converges on `high`, and the
current tip is recorded as the earliest block the node retains. That is the most
restrictive possible bound, not fail-open.

**20b.** `checkProbe` returns `(ok, unsupported, err)` and the search discards
`unsupported` entirely (`:1734`). A node with no tracing engine answers `-32601`
to every trace method at every height, so `probe: traceData` walks about log2(N)
requests and then declares earliest = latest — indistinguishable from a node
that pruned its whole history.

An operator who configures `blockAvailability.lower` therefore sees a node that
answers nothing recorded as retaining history from the tip upward.

**20c.** The escape hatch is dead. None of the five probe implementations ever
returns a non-nil error — each folds transport and JSON-RPC failures into
`(false, …, nil)`. So the two `else if err != nil` fast paths and the error
logging in `PollEarliestBlockNumber` (`:908`) cannot fire.

Pinned by `TestBinarySearchEarliest/NothingAvailableStillReportsHighAsEarliest`
and `/UnsupportedProbeIsIndistinguishableFromPrunedHistory`.

---

## 21. `GetDiagnostics` reports a deliberate opt-out as a failure

**Status:** open. **Severity: low.** False alarm on the admin surface.

`architecture/evm/evm_state_poller.go:1234` sets `diag.SkipSyncingCheck` from
the operator's own config. Line `:1239` then folds that flag into
`skipSyncingCheck`, and `:1248` emits *"syncing check disabled after consecutive
failures (method may not be supported)"*.

An operator who sets `skipSyncingCheck: true` on purpose reads a diagnostic
telling them their node may not support `eth_syncing`.

The latest and finalized equivalents are correct: they read the poller's own
`skipXCheck`, not a configured one.

---

## 22. Dead branches in the EVM block-number path

**Status:** open. **Severity: none.** Unexercised machinery.

- `architecture/evm/eth_getBlockByNumber.go:31` — `BuildGetBlockByNumberRequest`
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

**Status:** open. Pre-existing; reproduced with all new test files removed.

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

The drop-on-contention is deliberate in production — the next poll re-observes —
so the fix belongs in the test, not the poller. Compare bug 10: this is the
second flaky test found whose cause is a test racing a background poller.

---

## 24. `PostgreSQLConnector.List` never issues a pagination token

**Status:** open. **Severity: medium.** A caller believes it enumerated
everything.

`data/postgresql.go:1194`. The query at `:1241` asks for `limit+1` rows so it
can detect a next page. The scan loop at `:1252` calls `rows.Next()` a
`limit+1`-th time and breaks at `:1254` **without recording that row** — so the
extra row is already consumed. The probe at `:1284` then calls `rows.Next()`
again, which would need a `limit+2`-th row the query never fetched. It always
returns false, and `nextToken` stays empty.

Measured against a live container: 30 rows, `limit 1` returns 1 row and an empty
token; `limit 5` returns 5 rows and an empty token.

`erpc_admin_listApiKeys` (`erpc/admin.go:240`) on PostgreSQL therefore returns
at most one page and always reports "no more pages". A caller looping until the
token is empty stops after one page believing it saw everything.

Pinned by `TestPostgreSQLConnector_CRUD/List never issues a pagination token`.

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
package instead of recording anything.

- **DynamoDB** (`data/dynamodb.go:757-759`): `cleanup` runs `close(done)` then
  `close(updates)`. The polling goroutine can be inside `getSimpleValue` — a
  network round trip — when that happens. On return it executes
  `select { case updates <- st: default: }` at `:743-746`. **A send on a closed
  channel panics even inside a select with a default.** The agent confirmed
  that with a standalone program.
- **PostgreSQL** (`data/postgresql.go:667-685`, `:692-707`): the same race, plus
  a goroutine leak. The 30-second fallback poller exits only on `ctx.Done()`.
  `cleanup` calls `ticker.Stop()` then `close(updates)` at `:706`; `Stop`
  neither stops the goroutine nor drains a tick already delivered, so the poller
  can wake after the close and send at `:677`. One goroutine leaks per watch
  stopped without cancelling its context.

Cancelling a shared-state watch — an upstream removed, a config reload, a
network torn down — can therefore crash the process.

The LISTEN broadcast path at `:909-916` is safe: `cleanup` removes the watcher
under `listener.mu` before closing.

The PostgreSQL goroutine leak is pinned by
`TestPostgreSQLConnector_WatchCleanupLeaksItsFallbackPoller`: it starts 25
watches on one key, stops all 25, and asserts the goroutine count stays up by
25. Fixing the leak makes the test fail, which is the point.

---

## 27. Redis reverse-index TTL comparison is dead code

**Status:** open. **Severity: none today.** Worth fixing so a future change can
rely on it.

`data/redis.go:409` and `:415` compare a TTL against `-2` and `-1` seconds.

go-redis v9.22.0's `DurationCmd.readReply` (`command.go:1630-1642`) returns
`time.Duration(n)` for those sentinels — that is **−2 nanoseconds** and −1
nanosecond, not −2s and −1s. Both branches are unreachable.

The cost today is one wasted `GET`, because the fallthrough returns the same
`ErrRecordNotFound`. But the comment claims the branch handles an expired
reverse-index target, and a future change relying on it would silently not run.

---

## 28. An inverted condition drops the caller's request id over HTTP

**Status: FIXED IN FORK.** Upstream still needs it. **Severity was: high.**
One character: `err != nil` became `err == nil` at `erpc/http_server.go:580`.
Pinned by `TestHttpServer_ABlockedMethodEchoesTheCallersIdOverHttp` and a
batch case proving each entry gets its own id back.

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
`err == nil`, and the adjacent admin block at `:650` uses `if jrr != nil`.

Any method blocked by `ignoreMethods` or `allowMethods` therefore answers
`{"jsonrpc":"2.0","id":null,"error":{…}}` over HTTP. Every JSON-RPC client pairs
answers to calls by id, so the caller cannot match the refusal and waits out its
own timeout. In a batch, nothing matches at all.

The same project config behaves differently over HTTP and over WebSocket.

---

## 29. A truncated request body answers HTTP 200 and blames the server

**Status:** open. **Severity: medium.**

`erpc/http_server.go:412` with `:1876`. `util.ReadAll` fails on a truncated body
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

**Status:** open. **Severity: medium.** Every client collapses into one bucket.

`erpc/http_server.go:2386` defines `parseForwardedFor`, which parses the
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

**Status:** open. **Severity: high for the first.** Silent wrong answers.

- **`GetLogs` ignores `blockHash`** (`erpc/grpc_server.go:279-318`).
  `evm.GetLogsRequest` defines `blockHash` as the alternative to
  `fromBlock`/`toBlock`. The handler never reads it, so a client filtering by
  block hash sends an empty `{}` filter and the upstream applies its own default
  range — the latest block on most clients. The operator sees a 200 and a
  well-formed log list **for the wrong block**.
- **Every handler ignores `chainId` and `chainGenesisHash`**
  (`erpc/grpc_server.go:191-490`). Those fields exist on every BDS request so a
  client can pin the chain it expects. eRPC takes the chain only from the
  `x-erpc-chain-id` metadata, so a client sending `chainId: 1` in the body with
  `x-erpc-chain-id: 137` in the metadata receives Polygon data without complaint.

Either honour these fields or reject a mismatch. Accepting and discarding them
is the worst of the three options.

---

## 33. The two streaming services label the same failure differently

**Status:** open. **Severity: low.** Clients cannot retry on status code.

`erpc/grpc_server.go:432-480` return the processor's error raw, so gRPC labels
it `codes.Unknown`. `erpc/grpc_server.go:487` wraps it with `mapToGRPCStatus`,
so the same error reaches a `StreamBlocks` client as `codes.Internal`.

One line per handler fixes it.

---

## 34. Small, low-severity, easy

- `erpc/http_server.go:689` and `:724` — refusal paths call
  `common.EndRequestSpan(requestCtx, nil, err)` where the outer `err` is nil, so
  "admin is not enabled" and "architecture and chain must be provided" record as
  **successful** spans. Span-error-rate dashboards under-report them.
- `erpc/http_server.go:866-869` — `msg, err := common.SonicCfg.Marshal(err.Error())`
  shadows the error parameter, so the retry on the next line marshals the
  *marshal* error rather than the original failure. Theoretical, since
  marshalling a Go string essentially cannot fail, but the shadowing hides it.
- `erpc/http_server.go:722` — user-facing typo, `"configureed via domain aliasing"`.
- `erpc/ws_server.go:572` — `writeMessage` is dead code.
- `erpc/grpc_server.go:72` guards `if cfg != nil`, then `:103-104` and `:148`
  dereference the same config unconditionally. Unreachable after `SetDefaults`,
  but the guard states an invariant the next line breaks.

---

## 35. `HasErrorCode` does not follow a single `Unwrap() error`

**Status: FIXED IN FORK.** Upstream still needs it. **Severity was: medium.**
Found independently by two agents, and the fix repaired a third consumer
nobody was looking at — see below. **Severity was understated.**

`common/errors.go:2543-2569` handles `StandardError`, `*BaseError` and
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

**Status:** open. **Severity: high for Alchemy.** eRPC retries failures that
cannot succeed.

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

**Status:** open. **Severity: low.** Unexercised machinery.

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

**Status:** open. **Severity: medium.** Bootstrap crash instead of a config error.

`thirdparty/infura.go:81` and `thirdparty/llama.go:54` read
`upstream.Evm.ChainId` with no nil check.

Ankr, BlastAPI, BlockPi and Conduit all guard first and return
`"...requires upstream.evm to be defined"`.

So an operator who configures an Infura or Llama provider without an `evm` block
gets a panic at bootstrap rather than a message naming the missing field.

Pinned by `TestInfuraAndLlama_GenerateConfigs_PanicOnAMissingEvmBlock`, which
fails loudly once the guard is added.

---

## 42. Three smaller `thirdparty` defects

**Status:** open. **Severity: low.**

- **`thirdparty/chainstack.go:314` and `thirdparty/quicknode.go:466`** —
  `fetchChainIDs` collects per-node errors, logs them, then always returns
  `nil`. The callers at `chainstack.go:117` and `quicknode.go:373` guard with
  `if err != nil { logger.Warn()… }`, so that warning is dead. Nodes silently
  keep chain ID 0 and drop out of routing with no signal at the caller.
- **`thirdparty/chainstack.go:232`** — `defer resp.Body.Close()` sits inside the
  pagination loop, so every page's body stays open until `fetchNodes` returns.
  On a large account that holds one connection per page for the whole walk.
- **`thirdparty/quicknode.go:569`** — the normaliser shadows its own `details`
  parameter with `var details map[string]interface{} = make(...)`. The caller at
  `architecture/evm/error_normalizer.go:22` fills `details` with `statusCode`
  and the response headers, and none of it reaches a QuickNode error. QuickNode
  is the only vendor that does this.

---

## 43. The missing-`evm` panic is six more vendors, not two

**Status:** fixed-in-fork. **Severity: medium.** Bootstrap crash instead of a
config error.

Entry 41 named Infura and Llama. A sweep of every `upstream.Evm` dereference in
`thirdparty/` finds six more `GenerateConfigs` that read `upstream.Evm.ChainId`
with no nil check, against eighteen that guard first:

- `thirdparty/envio.go:223`
- `thirdparty/erpc.go:116` and `thirdparty/erpc.go:134`
- `thirdparty/etherspot.go:97`
- `thirdparty/pimlico.go:176`
- `thirdparty/routemesh.go:116`
- `thirdparty/thirdweb.go:100`

An operator who configures any of these six without an `evm` block crashes the
process at bootstrap instead of reading which field is missing. eRPC's own
vendor is the worst of the six: `erpc.go:116` sits on the preset-endpoint path,
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
  `thirdparty/provider.go:59` and `upstream/upstream.go:275` — so the ninth
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

**Status:** open. **Severity: medium.** A silent bad upstream, not a config error.

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

**Status:** open. **Severity: high.** Verified under `-race`. An operator
loses the "my policy is too slow" signal completely, and the existing
`internal/policy/stdlib` suite flakes because of it.

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

**Deliberately not pinned by a test**, on the same rule as entry 13: a test
that asserts the correct behaviour fails today, and a test that asserts
today's behaviour has to fire the timeout, which trips the race and fails
the suite under `-race`. Fix first, then pin.

The fix is small: have the goroutine publish through a result struct sent on
a channel, and let the parent choose between the timeout error and the
result. That removes the shared variables and makes the timeout the winner
when it fires.
## 47. A PostgreSQL listener connection is never released

**Status:** fixed-in-fork. **Severity: high.** Shared-state watches stop
working once the process has watched `maxConns` distinct counter keys.

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

`erpc/ws_server.go:583` — `writeNormalizedResponse` opens a frame with
`NextWriter`, streams the response into it, and closes the frame. When
`resp.WriteTo(w)` fails, eRPC logs at Debug and closes the frame anyway, so
gorilla ships a complete text message with a zero-length payload.

The client reads a message that is not JSON and carries no `id`. It cannot
match it to a call and it cannot report an error, so the call waits for the
client's own timeout. The HTTP path answers the same failure with a JSON-RPC
error envelope (`erpc/http_server.go:1913` hands over to `writeFatalError`).

Fix: on error, abandon the frame instead of closing it, or replace it with a
JSON-RPC error carrying the request id.

Pinned by `TestWsWriteNormalizedResponse_SendsAnEmptyFrameWhenItCannotSerialise`,
which records the empty payload as the current behaviour.

---

## 49. The WebSocket batch writer discards every error it can produce

**Status:** open. **Severity: medium.** No client signal, no server signal.

`erpc/ws_server.go:641-644`:

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

**Status:** open. **Severity: low.** An operator loses the log line.

`erpc/http_server.go:770-780`:

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

Fix: read the cause unconditionally and log it, or delete the inner block.

Pinned by `TestRequestHandler_WritesNothingWhenTheRequestContextIsAlreadyDone`,
which covers the reachable half — eRPC releases the responses and writes
nothing.

---

## 51. Three unreachable defensive branches in the WebSocket write path

**Status:** open. **Severity: none.** Unexercised machinery.

`erpc/ws_server.go:564`, `:598` and `:631` each guard
`wsc.conn.SetWriteDeadline(...)` and give up on an error. gorilla's
`Conn.SetWriteDeadline` (v1.5.3, `conn.go`) stores the deadline and returns
`nil` unconditionally; it never touches the socket. All three branches are dead.

The deadline itself is load-bearing and correct — only the error check is dead.

---

## 52. Two smaller write-path defects

**Status:** open. **Severity: low.**

- `erpc/http_batch_resp.go:71` — `fmt.Errorf("no bytes written for response %d
  error: %w", i, err)` is reached only when `err == nil`, because the line above
  returns on a non-nil `err`. Every message this produces ends in
  `%!w(<nil>)`, and the error it wraps is always nothing.
- `erpc/http_server.go:1909` and `:796`, both with `:864` — when a body fails
  part-way through, `writeFatalError` calls `w.WriteHeader` a second time
  (net/http logs "superfluous response.WriteHeader call") and appends a second
  JSON document after the partial first one. With a dead socket nothing
  arrives, so this is harmless; with a live socket and a non-transport failure
  — an entry the batch writer cannot marshal, or a response with nothing to
  write — the client receives one unparseable body.

## 53. The legacy upstream `failsafe:` object drops every key added since

**Status:** pinned. **Severity: medium.**
`TestUpstreamConfig_UnmarshalYAML_LegacyFailsafeObjectDropsNewerKeys`
(`common/config_backcompat_unmarshal_test.go`).

`UpstreamConfig.UnmarshalYAML` (`common/config.go:1096`) decodes into a shadow
struct. When that decode fails — which a legacy single-object `failsafe:`
always causes — it falls back to `oldShadow` (`common/config.go:1123`) and
copies field by field (`common/config.go:1150`).

`oldShadow` has not grown with `UpstreamConfig`. It lacks
`rateLimitCountMode` and `creditUnits`, so both are silently discarded for any
upstream that still writes `failsafe:` as an object.

An operator who configured credit-based rate limiting gets flat per-request
counting instead. No warning, no error — their budget simply drains at the
wrong rate. Every upstream key added after the fallback was written inherits
the same fate.

`NetworkDefaults.UnmarshalYAML` (`common/config.go:776`) and
`NetworkConfig.UnmarshalYAML` (`common/config.go:2372`) have the same
hand-listed fallback but escape the bug by accident: they decode into the
RECEIVER, so the failed strict pass leaves the newer keys populated and the
legacy pass only overwrites what it names. Fix the upstream path the same way,
or the two shapes keep diverging.

## 54. Two agent-name branches are shadowed and can never run

**Status:** pinned. **Severity: low.**
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

**Status:** open. **Severity: lowest.** No test, because no input reaches it.

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

**Status:** fixed-in-fork. **Severity: medium.** One leaked socket per race.

`clients/grpc_bds_resilience.go:497` closed `stopCh` and then walked the
connection slots. It never waited for the maintainer goroutine it started at
`:204`.

The maintainer wakes every `bdsMaintainInterval`, and a tick already in flight
dials a replacement (`recycleConn`, `:290`) and swaps it into a slot
(`swapInReplacement`, `:330`). Both steps run after `Shutdown` has read the
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

**Status:** pinned. **Severity: medium.** A wrong supported/unsupported verdict
at bootstrap, from config alone.

Six vendors answer `SupportsNetwork` by sending `eth_chainId` to the endpoint
and comparing the answer. Each caches the probe client in a `sync.Map`. Three
key that map on the URL and the chain; three key it on the chain alone:

| vendor | cache key | site |
| --- | --- | --- |
| erpc | url + chain | `thirdparty/erpc.go:251` |
| goldsky | url + chain | `thirdparty/goldsky.go:257` |
| routemesh | url + chain | `thirdparty/routemesh.go:153` |
| **envio** | **chain only** | `thirdparty/envio.go:265`, `:277` |
| **pimlico** | **chain only** | `thirdparty/pimlico.go:239`, `:251` |
| **thirdweb** | **chain only** | `thirdparty/thirdweb.go:136`, `:148` |

`ProvidersRegistry` hands every provider the SAME vendor instance
(`thirdparty/providers_registry.go:22` calls `vendorReg.LookupByName`), so two
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

**Status:** open. **Severity: low.** Dead work that reads as a feature.

`thirdparty/ankr.go:125`, `blastapi.go:160`, `chainstack.go:423`,
`erpc.go:145`, `goldsky.go:165`, `onfinality.go:102` and `tenderly.go:140`
share one `GetVendorSpecificErrorIfAny` body: copy `jrr.Error.Data` into
`details["data"]`, then return `nil`.

Their only caller is `architecture/evm/error_normalizer.go:29`. Returning `nil`
means control falls through to `:42-60`, which writes `details["data"]` from the
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
nil` at `error_normalizer.go:26` before it calls, so the dereference cannot
fire from the only path that reaches it.

---

## 61. Two smaller `thirdparty` divergences

**Status:** pinned. **Severity: low.**

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

**Status:** pinned. **Severity: high.** A hung shutdown and a hung request path.

`util/initializer.go:298` runs every schedulable task under one
`sync.WaitGroup`, and waits for all of them to *start* at `:397`. Each launched
goroutine calls `wg.Done()` at `:346`, after it has built the task context.

The goroutine returns early at `:337-342` when the app context is already
cancelled — **without calling `wg.Done()`**. `attemptRemainingTasks` then blocks
on `wg.Wait()` forever.

The blast radius is the whole Initializer, not one task.
`attemptRemainingTasks` takes `i.tasksMu` at `:299` and releases it through a
`defer`, so the mutex is never released either. Every later `ExecuteTasks`
(`:202`), `attemptRemainingTasks` and `Stop` (`:495`) on that Initializer blocks
on the same mutex.

One Initializer is shared across many resources (one bootstrap task per
network/upstream), and `NetworksRegistry.GetNetwork` calls `ExecuteTasks` on the
request path. So any request that races process shutdown — or any task
scheduled after the app context is cancelled — strands every subsequent caller
AND the shutdown sequence itself.

The task's own state is recorded correctly (`TaskFailed`, with the context
error); only the callers hang.

Fix: call `wg.Done()` on that path too, or `defer wg.Done()` once at the top of
the goroutine and drop the explicit call at `:346`.

Pinned by `TestInitializer_AppContextAlreadyCancelledWedgesTheInitializer`,
which asserts both the stranded `ExecuteTasks` and the stranded `Stop`.

Adjacent, same function, **severity: low** — `util/initializer.go:376`. When a
task returns `context.Canceled`, the handler tries
`bt.lastErr.CompareAndSwap(nil, wrappedError{err: err})`. The CAS can never
succeed: `:326` stored `wrappedError{err: nil}` into that `atomic.Value` before
the attempt, so the current value is a `wrappedError`, never `nil`. The
cancellation reason is dropped, and `Wait` (`:135`) substitutes
`"task failed without specific error"`. The task is still counted as failed;
only the reason is lost. Pinned by
`TestInitializer_CancelledTaskIsReportedWithoutItsReason`.

---

## 62. `SuggestFinalizedBlock` drops a suggestion under contention; the latest twin does not

**Status:** FIXED. **Severity: medium.** Raised from "low in production" —
see the second reproduction below.

`architecture/evm/evm_state_poller.go:825` — `SuggestFinalizedBlock` takes
`finalizedUpdateInProgress` with `TryLock` and RETURNS when the lock is held.
The suggestion is discarded, not queued. Nothing re-issues it, so the finalized
head stays where it was until the next successful poll — which the debounce can
push a full interval away.

`SuggestLatestBlock` at `:533` does not behave this way. It applies a small
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
(`architecture/evm/evm_state_poller_suggest_gate_test.go:226`) fails under
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

**Status:** pinned. **Severity: medium.**
`TestLoadConfig_TypeScriptWithNoExportsPanics`
(`common/config_load_errors_test.go`).

`loadConfigFromTypescript` (`common/config.go:3254`) calls
`runtime.Exports().Get("default")`. `Runtime.Exports` (`common/runtime.go:55`)
is `r.vm.GlobalObject().Get("exports").ToObject(r.vm)`. When the compiled
module declares no exports at all, that global is JS `null`, and sobek's
`ToObject` raises a TypeError that unwinds out of the Go call as a panic.

The very next statement (`common/config.go:3255`) exists to catch exactly this
mistake: it returns "config object must be default exported from TypeScript
code AND must be the last statement in the file". It never runs for the
no-export case, so the operator who forgot `export default` gets a Go stack
trace with `TypeError: Cannot convert undefined or null to object` instead of
the sentence that tells them what to type.

`export default undefined` and `export default null` DO reach the friendly
error, because those leave a real `exports` object behind. The difference is
invisible from the config file, which is what makes the panic surprising.

One guard in `Runtime.Exports` — return nil when the value is null or
undefined — fixes it, and the existing check at `:3255` then handles the rest.

## 64. A mistyped current-schema key is reported against the legacy shadow

**Status:** pinned. **Severity: medium.**
`TestNetworkDefaults_UnmarshalYAML_CurrentOnlyKeyReportsTheLegacyShadow` and
`TestNetworkConfig_UnmarshalYAML_CurrentOnlyKeyReportsTheLegacyShadow`
(`common/config_unmarshal_gaps_test.go`).

`NetworkDefaults.UnmarshalYAML` (`common/config.go:776`) and
`NetworkConfig.UnmarshalYAML` (`common/config.go:2372`) decode the node into
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
`return originalErr` (`common/config.go:812` and `:2411`) intends to deliver.
It does not arrive: the legacy attempt's unknown-field complaint is recorded
against the decoder and surfaces from the outer `Decode` instead. A key that
BOTH shapes declare (`rateLimitBudget`) reports correctly, which is why this
was never noticed.

The fix is to stop feeding unknown keys to the legacy shadow — decide from the
document which shape it is, or add the current-only keys to the shadow. Entry
53 above asks for the same structural change to the upstream path for a
different symptom.

## 65. A rate-limit rule logs nanoseconds under a key that says milliseconds

**Status:** pinned. **Severity: low.**
`TestRateLimitRuleConfig_MarshalZerologObject`
(`common/config_unmarshal_gaps_test.go`).

`RateLimitRuleConfig.MarshalZerologObject` (`common/config.go:3105`) writes
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

**Status:** fixed-in-fork (`common/config.go`). **Severity: high.** Bootstrap
crash instead of a config error.

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
(`common/defaults.go:95`):

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

**Status:** open. **Severity: low.** The repo's own vet gate is red.

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

## 72. A static method is refused as a missing historical block

**Status:** pinned. **Severity: high.** Every `eth_chainId` and `net_version`
fails on any upstream that declares a lower availability bound.

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
(`upstream/upstream.go:1022-1029`) — so any non-archive node in the pool is
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

`TestBlockAvailability_RefusesAStaticMethodOnAnUpstreamWithALowerBound`
(`erpc/networks_boundary_test.go`) pins both halves.

## 73. The gRPC query surface ignores every `queryShim` limit

**Status:** pinned. **Severity: medium.** A cost and blast-radius control that
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

**Status:** pinned. **Severity: medium.** Silent short answer.

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

**Status:** open. **Severity: low.** Dead code, one of it a bypassed wrapper.

`erpc/network_executor.go` declares `Timeout()` (`:112`), `HasHedge()`
(`:134`), `HasRetry()` (`:142`) and `shouldRetry()` (`:374`). None of them has
a single caller anywhere in the module. `networkExecutor` is unexported, so
nothing outside the package can reach them either.

`shouldRetry` is the interesting one. Its doc comment says "Returning true
causes the caller to emit a `network_retry_attempt_total{reason}` metric", but
`runRetry` (`:281`) calls `shouldRetryWithReason` directly and never goes
through the wrapper. The upstream-scope twin does use both — `upstreamExecutor`
calls its own `Timeout()` (`upstream/upstream.go:980`) and `shouldRetry()`
(`upstream/upstream_executor.go:201`) — so these four look like a copied
surface that was never wired.

Delete them, or wire `runRetry` through `shouldRetry` so the comment is true.

---

---

## 80. A TypeScript config with no exports panics instead of explaining itself

**Status:** fixed-in-fork. **Severity: medium.** Bootstrap crash instead of a
config error. (Another branch logs the same defect as entry 59.)

`loadConfigFromTypescript` (`common/config.go:3254`) called
`runtime.Exports().Get("default")`. `Runtime.Exports` (`common/runtime.go:55`)
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

**Status:** fixed-in-fork. **Severity: high.** Silent: later watchers of the
same key receive nothing and report nothing.

Found while fixing entry 47, and measured against a live container.

`getOrCreateListener` (`data/postgresql.go:872`) ran the listener goroutine on
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

**Status:** recorded. A correction to entries 41 and 43, not a new defect.

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

**Status:** fixed-in-fork (`erpc/query_executor_test.go:339`). The fork's own
code.

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

**Status: FIXED.** This was the fork's own code, so it is simply closed.
Cordon is now level-triggered against the upstream's live state; uncordon
stays edge-triggered on ownership, so a recovery lifts only a cordon the
poller placed.

`upstream/chain_family_bootstrap.go:226` guards the cordon with
`cordonedByProbe.CompareAndSwap(false, true)`.

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

**Status:** fixed-in-fork (`common/errors.go`).

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

**Status:** fixed-in-fork (`erpc/http_server.go:580`). One character.

See log entry 28.

## A. Transport failures lose their cause identity

**Status:** fixed-in-fork (`common/errors.go`, commit on `main`).

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

**Status:** fixed-in-fork (`common/json_rpc.go`, `common/json_rpc_null_error.go`).

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

**Status:** fixed-in-fork (`failsafe/breaker.go`).

The breaker returns early on `OutcomeIgnore` **without releasing the half-open
trial permit**. The breaker then stays open until the process restarts.

## D. A batch entry can be forwarded to the wrong chain

**Status:** fixed-in-fork (`erpc/http_server.go`).

The per-batch-entry goroutine captured `architecture` and `chainId` by
reference, so an entry could be routed to another entry's chain.

## 66. The state poller dereferences a nil response it just tested for

**Status:** FIXED. `architecture/evm/evm_state_poller.go:1303` and `:1368`.

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
(`upstream/upstream.go:~803`) — and then returns `nrs, nil` with `nrs` nil,
which `failsafeExecutor.Run` passes straight through.

The guard's own author meant to return the error member. Instead the poller
panics inside the `Poll` fan-out goroutine, which has no recover, so the
process dies. An operator sees eRPC exit with a SIGSEGV stack in
`(*EvmStatePoller).fetchBlock` and no upstream named in the log.

The same shape in the three availability probes
(`evm_state_poller.go:1568`, `:1616`, `:1666`) is written safely — it returns
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

**Status:** upstream candidate, latent. `architecture/evm/json_rpc_cache.go:818`.

`shouldCacheResponse` documents and guards the nil case explicitly
(`json_rpc_cache.go:1181-1189`: "both arguments can arrive nil together").
Its caller does not. With `resp == nil` and a policy whose empty behaviour is
`only` or `allow`, `shouldCacheResponse` returns `true`, and the very next
line runs `rpcResp.GetResultBytes()` on a nil `*JsonRpcResponse`.

The only production caller guards with `if resp != nil`
(`erpc/networks.go:2394`), so it cannot fire today — but that caller also
wraps the goroutine in a recover, which is what would keep the process alive
if it ever did. Either the guard at 818 is missing or the guard inside
`shouldCacheResponse` is unnecessary; the two disagree.

## 68. Four dead branches found while raising `architecture/evm` coverage

**Status:** upstream candidates, all cosmetic-to-behaviour but each hides a
guard that reads as active.

1. **`json_rpc_cache.go:565-586`** — the `CacheEmptyBehaviorIgnore` arm of the
   post-fan-out emptiness switch cannot run. Every winner is filtered by the
   identical condition inside the fan-out goroutine at `:375`
   (`jrr.IsResultEmptyish() && policy.EmptyState() == Ignore` reports a miss
   and returns), so no emptyish-under-ignore result ever reaches `:563`.
   Five statements of miss telemetry that never fire.
2. **`evm_state_poller.go:365-367`** — the `else` arm of the syncing-failure
   handler. It runs only when the local `skip` is true, but `skip` is read at
   `:324` and the function returns at `:335` whenever it is true. The
   `if !skip` at `:360` is therefore always taken.
3. **`eth_getLogs.go:280-282`** — capping the split threshold by
   `getLogsMaxAllowedRange` can never change the outcome. The cap only applies
   when `threshold > maxRange`, and a split needs `requestRange > threshold`,
   which implies `requestRange > maxRange` — already rejected with an error at
   `:254`.
4. **`eth_getLogs.go:224`** — `topicCount = 1` for a scalar `topics[0]` is
   unobservable. The only reader is `topicCount > maxTopics` with
   `maxTopics > 0`, so a count of one can never reject.

Also confirmed unreachable, and correctly documented as such in the source:
`evm_state_poller.go:1769` and `:1793` (`binarySearchEarliest`'s pass-through
of a `checkProbe` error no probe implementation produces).

---

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

## 69. A consensus round can return neither a response nor an error

**Status:** FIXED. **Severity: medium.** The caller got `(nil, nil)`.

`consensus/rules.go:740-742` — the "low participants + accept-most-common"
rule returns the best error group's `FirstError`:

```go
if bestError := a.getBestError(); bestError != nil {
    return &slotResult{Error: bestError.FirstError}
}
```

`getBestError` (`consensus/analysis.go:315-332`) ranks **infrastructure-error**
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

**Status:** pinned. **Severity: low.** Wasted CPU, and an uncancellable wait.

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

Fix: give the branch the same `select` the pending branch uses.

Pinned by
`TestBootstrapTask_WaitBusySpinsAndIgnoresItsContextBeforeTheDoneChannelExists`.

---

## 71. `PostgreSQLConnector.List` can never hand out a next-page token (DUPLICATE of 24 — independently rediscovered, kept as corroboration)

**Status:** pinned. **Severity: medium.** Silent truncation.

`data/postgresql.go:1241-1297`. The query asks for `limit+1` rows so the code
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
always false and `nextToken` is always `""`. Statements `:1284-1286` and
`:1289-1295` are unreachable.

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

`upstream/registry.go:402-421`. The lock-free fast path returns the stored
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

# Polyglot live run — entries 90–94

Found on 2026-08-17 while running one eRPC process against Ethereum mainnet,
Solana mainnet-beta and Bitcoin mainnet at once. Full run:
[polyglot-live-run.md](polyglot-live-run.md). Config:
[polyglot-live-pool.yaml](polyglot-live-pool.yaml).

## 90. `erpc/chain_families.go` says btc cannot serve, and btc serves

**Status:** stale comment in the fork (`erpc/chain_families.go:22-27`).

The file's SCOPE note reads:

> WHAT IS STILL NOT REGISTRY-DRIVEN — a btc UPSTREAM does not bootstrap.
> `Upstream.detectFeatures` (`upstream/upstream.go`) recognises only evm and
> svm and rejects everything else, so a btc upstream never reaches the pool and
> no btc request is ever forwarded.

That is no longer true. `detectFeatures` now ends in an `else` branch that calls
`detectChainFamilyFeatures` (`upstream/upstream.go:1400`), which is the
registry-driven path. In the live run five btc upstreams bootstrapped and
answered `getblockchaininfo`, `getblockhash` and `getblock` from real Bitcoin
nodes.

The comment matters because it tells the next reader not to bother. Anyone
adding a fourth family reads it and concludes the seam is unfinished.

The second claim in the same paragraph — `common.IsValidNetwork` still knows
only evm and svm — is still correct. See entry 91.

**Fix:** delete the first two sentences. Keep the `IsValidNetwork` sentence
until entry 91 is fixed.

## 91. `IsValidNetwork` enumerates two architectures, so a provider cannot name a third

**Status: FIXED in the fork.** Inherited from upstream (`common/network.go:95`).
Upstream still carries it.

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

**Status:** open, fork code (`internal/policy/prober.go:414`).

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

**Status:** open, inherited (`telemetry/metrics.go:681`).

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

**Status:** open, inherited. Semantics already pinned in
`telemetry/metrics_semantics_test.go:53`.

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

**Status:** upstream candidate. **Severity: medium.** `common/response.go:309`
and `:488`.

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

**Status:** upstream candidate, latent. **Severity: low.**
`consensus/executor.go:163-166`.

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

**Status: FIXED in the fork.** Upstream still carries it.
**Severity: medium.** A data race in the bootstrap path.

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

**Status: FIXED in the fork.** Upstream still carries it.
**Severity: high.** It affects the DEFAULT configuration of any project with
more than one network.

`internal/policy/default_policy.go` — `upgradeDefaultPolicy` rewrites three
fields on the config it is given:

    cfg.EvalFunc = DefaultPolicySource()
    cfg.EvalFuncOriginal = cfg.EvalFunc
    cfg.CompiledProgram = program

`Engine.RegisterNetwork` calls it BEFORE taking `e.mu`, so the write is
unguarded. Networks bootstrap concurrently (`erpc/networks_registry.go:284`
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

**Status:** open. The design behind 95 and 96, recorded so the shallow fixes
are not mistaken for the real one.

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

**Status:** open. **Severity: medium.** Every documented exit code is wrong.

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

**Status:** open. **Severity: medium for an embedder, high for a test run.**

`erpc/init.go:142`, `:155` and `:178` call `util.OsExit(...)` from inside the
`erpc` package. That package is a library: `cmd/erpc` is the binary, and
`erpc.Init` is what an embedder calls.

So a Go program that embeds eRPC and fails to bind its HTTP port does not get
an error it can handle. Its whole process ends, inside a goroutine the caller
never started.

This is not theoretical. `go test ./test/` dies with exit 234 and prints NO
assertion, no panic and no reason — the HTTP server cannot bind, the goroutine
calls `OsExit`, and the test binary is gone before it can report anything. The
package is excluded from `make test-fast`, so nobody sees it.

The `OsExit` indirection (`var OsExit = os.Exit`) exists so a test can replace
it, which shows the problem was already understood. The weakening is to return
the error to the caller and let `cmd/erpc` decide to exit — the one place that
owns the process.

Related: 43 and 63 are the same shape. A library turns an operator's mistake
into a fatal event instead of a value the caller can act on.


## 105. A negative or fractional JSON block number becomes a real block number

**Status:** open. Found while covering `util/`.

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

## 106. `ParseBlockHashHexToBytes` guards against its own guarantee

**Status:** open. Minor. Dead code, not a live defect.

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

**Status:** open. Minor. Dead code, verified by probe.

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

**Status:** open. Not a defect today. A hazard for the next edit.

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

## 109. `Initializer.Stop` returns while its task goroutines are still running and still logging

**Status:** open. **Severity: medium.** Found by `go test ./util/ -race -count=2`.

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

**Status: FIXED in the fork** (`erpc/healthcheck.go`).
**Severity: high.** A load balancer keeps a dead instance in rotation.

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

**Status: FIXED in the fork** (`erpc/config_analyzer.go`).
**Severity: medium.** `erpc validate` reports a fork check that never ran.

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

**Status:** open, inherited. This is the mechanism behind 93, not a second bug.

The counter is wired at exactly one site, `erpc/network_executor.go:312`,
inside `executeWithRetries`. Two conditions gate it, and each one silences it
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

**Status:** open, inherited (`erpc/healthcheck.go`, `checkEvmChainId`).
**Severity: low.** It misreads as failures on a mixed-architecture project.

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

**Status:** open, inherited. Recorded as dead weight, not as a live fault.

While covering the chain-identity paths I found three commitments that today's
data does not support.

First, `checkEvmChainId` guards against an unparsable chain id:

    actualChainId, err := strconv.ParseInt(upChainId, 0, 64)
    if err != nil { ... "invalid chain id format" ... }

`EvmGetChainId` already normalises the wire value and returns
`strconv.FormatUint(dec, 10)`. Its output is always a decimal string that
`ParseInt` accepts, so the branch cannot run. `GenerateValidationReport`
carries the same guard, and there it IS reachable: it parses into the platform
int, so a chain id above 2^63-1 fails. Only the health-check copy is dead.

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

**Fix:** delete the two unreachable branches rather than test them, and either
build the detail map inside `checkEvmChainId` or skip an upstream that has no
entry. All three are unforced commitments the caller's shape is currently
paying for.

## 100. A shadow test asserts an absolute value on a process-global counter

**Status:** open. Found while adding `TestProjectForward_*` in
`erpc/projects_shadow_forward_test.go`.

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

**Status:** open. Found while covering the tag branches in
`erpc/query_executor.go`.

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

**Status:** `fixed-in-fork` for the header map, `pinned` for the rest.

`upstream/registry.go:509` copies an upstream config before it bootstraps the
upstream, and its comment says why: "Deep copy to avoid race conditions when
detectFeatures modifies the config". Line 547 copies again per attempt, because
"NewUpstream mutates it (vendor detection)". Both rely on `Copy()` producing an
object that shares nothing with the operator's config or with the sibling
copies.

`Copy()` does not produce that object. It starts with `*copied = *c`, then
deep-copies seven fields and leaves the rest aliased.

The worst case was `JsonRpcUpstreamConfig.Copy`:

```go
copied := &JsonRpcUpstreamConfig{}
*copied = *c
if c.Headers != nil {
    maps.Copy(copied.Headers, c.Headers)   // dst IS src
}
```

`*copied = *c` gives `copied.Headers` the same map header as `c.Headers`, so
`maps.Copy` copies the map into itself and does nothing. Every copy of an
upstream shares one header map. Headers carry credentials — `thirdparty/
blockdaemon.go:116` writes `Authorization: Bearer <key>`, `thirdparty/
satelink.go:132` writes `X-API-Key` — and two bootstrap attempts writing that
map concurrently is a Go fatal concurrent map write, not a recoverable error.
The sibling `GrpcUpstreamConfig.Copy` allocates first and gets it right, which
is what shows the intent.

**Fixed here:** `common/config.go` now allocates the destination map before
copying into it.

Still aliased after the fix, and recorded rather than changed because each one
needs a decision about what the field means:

- `UpstreamConfig.Tags` — a slice, so `append` on a copy can write into the
  original's backing array.
- `UpstreamConfig.CreditUnits` — a map.
- `UpstreamConfig.Svm`, `.Shadow`, `.Routing` — pointers to structs with their
  own maps and slices inside.
- `RetryPolicyConfig.EmptyResultAccept` and `.EmptyResultIgnore` — slices.

No caller mutates any of those in place today, so this half is a latent hazard
rather than a live fault. `TestUpstreamConfigCopy_SharesNoMemoryWithTheOriginal`
walks the whole config tree by reflection and fails on any NEW aliased field;
the seven above sit in an allowlist that names this entry.

## 116. `ErrUpstreamsExhausted.DeepestMessage` can never reach its single-cause branch

**Status:** pinned.

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

**Status:** pinned.

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

**Status:** pinned.

`JsonRpcRequest.CacheHash` (`common/json_rpc.go:1405`) hashes the parameters by
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

The fix is a delimiter that cannot occur in a value (a length prefix, or a byte
outside the hex/JSON alphabet) between every written piece. It changes every
cache key, so it wants its own change and a cache-generation bump.

Pinned by `TestCacheHash_ConcatenatesAdjacentParamsWithoutASeparator`, whose
assertion is the defect: when the separator lands, that test fails and should
be rewritten as a `NotEqual`.

## 120. The query shim advertises an uppercase hex prefix it always rejects

**Status:** open. Pinned by
`architecture/evm/eth_query_parse_test.go:TestParseUint64Value_RejectsTheUppercaseHexPrefixItClaimsToAccept`,
which fails once the parser honours the prefix it checks for.
**Severity: low.** It costs one client a clear error message, not a wrong answer.

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

**Status:** open. Pinned by
`internal/policy/stdlib/duration_knob_test.go:TestStickyPrimary_MinSwitchInterval_DecidesTheHandover`,
whose `UnparseableString` and `WrongType` cases assert today's handover.
**Severity: medium.** It turns a one-character config mistake into fleet-wide
primary flapping, with no log line and no metric.

`internal/policy/stdlib/install.go` — the `durationMs` global returns 0 for
every value it cannot read: a string `time.ParseDuration` refuses, an empty
string, a boolean, an object. `stdlib.js` feeds it the operator's
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
