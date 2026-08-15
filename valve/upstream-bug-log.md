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

**Status:** open. **Severity: high.** A hung request never returns.

`common/response.go:623` takes `resp.RLockWithTrace(ctx)`, a read lock on the
response's `sync.RWMutex`. If the response is not parsed yet, it then calls
`resp.JsonRpcResponse(ctx)`, which takes `r.Lock()` on the same non-reentrant
mutex at `common/response.go:331`.

The goroutine blocks forever while holding the read lock.

The comment at `:638` says this path is "common with multiplexed requests", so
the intended case is the one that hangs.

The unparsed path is deliberately left uncovered: a test for it would hang the
suite until this is fixed. A comment at the test names both line numbers.

---

## 3. No error type satisfies `common.ResponseMetadata`

**Status:** open. **Severity: medium.** Operators lose diagnostics exactly when
they need them.

`common/response.go:64` declares `IsObjectNull(ctx ...context.Context) bool`.

`ErrUpstreamRequest.IsObjectNull()` (`common/errors.go:810`) and
`ErrUpstreamsExhausted.IsObjectNull()` (`:1041`) take **no argument**. Neither
type satisfies the interface, although both implement every other method.

`common.LookupResponseMetadata` therefore returns nil for every error, and
`erpc/http_server.go:1316` and `:1321` emit no `X-ERPC-Attempts`,
`X-ERPC-Retries`, `X-ERPC-Hedges` or `X-ERPC-Upstream` header on an error
response.

The compiler cannot catch this, because nothing asserts the interface
statically. A `var _ ResponseMetadata = (*ErrUpstreamRequest)(nil)` line would
have.

---

## 4. `WithRetryableTowardNetwork` throws the concrete type away

**Status:** open. **Severity: medium.** Wrong HTTP status to the client.

`common/errors.go:210` returns the embedded `*BaseError`, not the receiver's
concrete type. `common/grpc_errors.go:75` and `:156` chain it.

So a gRPC `InvalidArgument`, or a BDS `INVALID_PARAMETER`, reaches the HTTP
layer as a bare `*BaseError`. That value no longer implements
`ErrorStatusCode()`, so the client sees **500 instead of 400**, and
`errors.As` for `*ErrEndpointClientSideException` no longer finds it.

Classification still works, because it reads the code rather than the type.

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

**Status:** open. **Severity: medium.** Not pinned — a deterministic
reproduction needs to interpose on a network read.

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

# Redundant guards — not defects, recorded so they are not re-derived

Each of these is shadowed by another check, so a single-line mutation of it is
unobservable. They are not bugs, and a test cannot pin them.

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
