package evm

import (
	"errors"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExtractJsonRpcError turns whatever an upstream said into one of eRPC's own
// error types. Every routing decision downstream reads that type and nothing
// else: whether to retry the same upstream, fail over to another, split the
// range, or hand the client a permanent failure. A misclassification is silent
// — the request still gets an answer, just from the wrong place or after a
// pointless retry storm.
//
// The classifier is a long if/else chain, so ORDER is part of its behaviour.
// Several tests below exist only to pin an ordering: a fixture that trips an
// earlier branch would make a later branch untestable while still looking green.

// classify runs the normalizer over a synthetic upstream reply.
func classify(t *testing.T, statusCode int, jsonRpcCode int, message string, data interface{}, method string) error {
	t.Helper()

	var nr *common.NormalizedResponse
	if method != "" {
		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","method":"` + method + `","params":[],"id":1}`))
		nr = common.NewNormalizedResponse().WithRequest(req)
	}

	r := &http.Response{StatusCode: statusCode, Header: http.Header{}}
	jrErr := common.NewErrJsonRpcExceptionExternal(jsonRpcCode, message, "")
	if data != nil {
		jrErr.Data = data
	}
	jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

	return ExtractJsonRpcError(r, nr, jr, nil)
}

// serverSide is the code the classifier's fallback produces; using it as the
// "neutral" JSON-RPC code in a table keeps each row about its message.
const neutralJsonRpcCode = int(common.JsonRpcErrorServerSideException)

func TestExtractJsonRpcError_ClassifiesEachUpstreamComplaint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		status   int
		code     int
		message  string
		wantCode common.ErrorCode
		// why records what an operator loses if this row is misclassified.
		why string
	}{
		{
			name:     "range too wide",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "query returned more than 10000 results, block range is too wide",
			wantCode: common.ErrCodeEndpointRequestTooLarge,
			why:      "only this type lets the network layer split the eth_getLogs range and retry; anything else fails the query outright",
		},
		{
			name:     "too many addresses",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "exceed max addresses or topics per search position",
			wantCode: common.ErrCodeEndpointRequestTooLarge,
			why:      "an address-count complaint needs a different split than a range complaint",
		},
		{
			name:     "sequencer per-sender rate limit",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "sender is over rate limit",
			wantCode: common.ErrCodeEndpointCapacityExceeded,
			why:      "every OP-stack provider fronts the same sequencer, so this must not become a generic server error that triggers failover",
		},
		{
			name:     "http 402 billing",
			status:   402,
			code:     neutralJsonRpcCode,
			message:  "payment required",
			wantCode: common.ErrCodeEndpointBillingIssue,
			why:      "a billing failure is not transient; retrying the same key burns latency and never succeeds",
		},
		{
			name:     "free tier exhausted",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "you have reached the free tier limit",
			wantCode: common.ErrCodeEndpointBillingIssue,
			why:      "same as 402 but the vendor signalled it in the body with a 200",
		},
		{
			name:     "http 429",
			status:   429,
			code:     neutralJsonRpcCode,
			message:  "slow down",
			wantCode: common.ErrCodeEndpointCapacityExceeded,
			why:      "rate limits must cool the upstream down rather than mark it broken",
		},
		{
			name:     "body-only rate limit",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "your app has exceeded its compute units per second capacity",
			wantCode: common.ErrCodeEndpointCapacityExceeded,
			why:      "many vendors answer 200 with a rate-limit body; missing that treats a throttle as a node fault",
		},
		{
			name:     "unsupported block tag",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "safe block not found",
			wantCode: common.ErrCodeEndpointClientSideException,
			why:      "another upstream may support the tag, so this stays retryable toward the network",
		},
		{
			name:     "pruned state",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "missing trie node 0xdead",
			wantCode: common.ErrCodeEndpointMissingData,
			why:      "the request must move to a node that holds the history, and the upstream must be recorded as not holding it",
		},
		{
			name:     "node execution timeout",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "execution timeout",
			wantCode: common.ErrCodeEndpointServerSideException,
			why:      "a node-side timeout is retryable; classifying it as a client error would return it to the caller",
		},
		{
			name:     "revert",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "execution reverted: ERC20: transfer amount exceeds balance",
			wantCode: common.ErrCodeEndpointExecutionException,
			why:      "a revert is the chain's real answer; retrying it across upstreams wastes the whole pool on a deterministic result",
		},
		{
			name:     "berachain invalid jump",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "EVM error: InvalidJump",
			wantCode: common.ErrCodeEndpointExecutionException,
			why:      "rewritten to the standard revert wording so subgraph tooling understands it",
		},
		{
			name:     "already known transaction",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "already known",
			wantCode: common.ErrCodeEndpointNonceException,
			why:      "the idempotency path turns this into a success; any other type surfaces a duplicate broadcast as a failure",
		},
		{
			name:     "nonce too low",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "nonce too low",
			wantCode: common.ErrCodeEndpointNonceException,
			why:      "needs the verification path to tell a duplicate broadcast from a genuine nonce conflict",
		},
		{
			name:     "insufficient funds",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "insufficient funds for gas * price + value",
			wantCode: common.ErrCodeEndpointExecutionException,
			why:      "a deterministic account-state failure; retrying it elsewhere cannot change the answer",
		},
		{
			name:     "out of gas",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "out of gas",
			wantCode: common.ErrCodeEndpointExecutionException,
			why:      "the caller must see the gas problem, not a server error that hides it behind retries",
		},
		{
			name:     "method not found",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "the method trace_block does not exist",
			wantCode: common.ErrCodeEndpointUnsupported,
			why:      "marks the upstream as not offering the method so later requests skip it instead of re-learning each time",
		},
		{
			name:     "module disabled",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "the module debug is disabled",
			wantCode: common.ErrCodeEndpointUnsupported,
			why:      "same as method-not-found, phrased by a geth operator who turned the namespace off",
		},
		{
			name:     "block not available",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "requested block is not available on this node",
			wantCode: common.ErrCodeEndpointMissingData,
			why:      "a not-found that names a block is pruned history, not an unsupported method",
		},
		{
			name:     "bare not found",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "not found",
			wantCode: common.ErrCodeEndpointClientSideException,
			why:      "Tenderly answers a bare 'not found'; with nothing to disambiguate it the safe move is a retryable client error",
		},
		{
			name:     "http 415 unsupported media",
			status:   415,
			code:     neutralJsonRpcCode,
			message:  "unsupported",
			wantCode: common.ErrCodeEndpointUnsupported,
			why:      "the transport itself rejected the shape; another upstream may accept it",
		},
		{
			name:     "malformed raw transaction",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "typed transaction too short",
			wantCode: common.ErrCodeEndpointClientSideException,
			why:      "the bytes are invalid everywhere; broadcasting them to every upstream in the pool achieves nothing",
		},
		{
			name:     "unknown tx type phrased without the word 'supported'",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "tx of type 4 rejected by this node",
			wantCode: common.ErrCodeEndpointClientSideException,
			why:      "a newer transaction type another upstream may already understand, so this one stays retryable",
		},
		{
			// Ordering fact, not a preference: the unsupported-method branch runs
			// BEFORE the tx-type branch, so any wording containing "not supported"
			// lands as Unsupported. Recorded so a future reader does not "fix" the
			// tx-type branch to catch a message it can never see.
			name:     "unknown tx type phrased as 'not supported'",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "tx of type 4 not supported",
			wantCode: common.ErrCodeEndpointUnsupported,
			why:      "the earlier unsupported-method branch claims this wording first",
		},
		{
			name:     "invalid params by code",
			status:   200,
			code:     -32602,
			message:  "invalid params",
			wantCode: common.ErrCodeEndpointClientSideException,
			why:      "providers disagree on parameter shapes, so a coded invalid-params is worth trying elsewhere",
		},
		{
			name:     "invalid argument by message",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "invalid argument 0: hex string has odd length",
			wantCode: common.ErrCodeEndpointClientSideException,
			why:      "the client wrote the request wrong; failing over just repeats the mistake",
		},
		{
			// The message deliberately says nothing about authorisation, so only
			// the HTTP status can produce this verdict.
			name:     "http 401 with an unhelpful body",
			status:   401,
			code:     neutralJsonRpcCode,
			message:  "denied",
			wantCode: common.ErrCodeEndpointUnauthorized,
			why:      "a bad credential must page the operator, not look like a flaky node",
		},
		{
			name:     "http 403 with an unhelpful body",
			status:   403,
			code:     neutralJsonRpcCode,
			message:  "denied",
			wantCode: common.ErrCodeEndpointUnauthorized,
			why:      "some vendors answer a revoked key with 403 rather than 401",
		},
		{
			name:     "invalid api key in body",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "invalid api key supplied",
			wantCode: common.ErrCodeEndpointUnauthorized,
			why:      "same as a 401 but delivered with a 200 body",
		},
		{
			name:     "nothing matches",
			status:   200,
			code:     neutralJsonRpcCode,
			message:  "the node fell over in a way nobody has catalogued",
			wantCode: common.ErrCodeEndpointServerSideException,
			why:      "the unknown-reply fallthrough is the primary path: an uncatalogued failure must still fail over",
		},
		{
			name:     "http 5xx with no json-rpc error at all",
			status:   503,
			code:     0,
			message:  "",
			wantCode: common.ErrCodeEndpointServerSideException,
			why:      "a bare gateway failure carries no body; it must still be recognised as the upstream's fault",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var err error
			if tc.code == 0 && tc.message == "" {
				// No JSON-RPC error object at all — only the HTTP status.
				r := &http.Response{StatusCode: tc.status, Header: http.Header{}}
				jr := common.MustNewJsonRpcResponse(1, nil, nil)
				err = ExtractJsonRpcError(r, nil, jr, nil)
			} else {
				err = classify(t, tc.status, tc.code, tc.message, nil, "")
			}

			if err == nil {
				t.Fatalf("classifier returned nil; %s", tc.why)
			}
			if !common.HasErrorCode(err, tc.wantCode) {
				t.Fatalf("classified as %T (%v), want %s — %s", err, err, tc.wantCode, tc.why)
			}
		})
	}
}

func TestExtractJsonRpcError_DoesNotFailOverASequencerRateLimit(t *testing.T) {
	t.Parallel()

	// "sender is over rate limit" comes from the OP-stack SEQUENCER, and every
	// provider on that chain proxies the same sequencer. Failing over just
	// re-asks the machine that already said no, once per upstream, while the
	// client waits. The type alone does not carry this: an ordinary rate limit
	// produces the same type and IS worth failing over. The retryability flag is
	// the whole difference, so this test asserts it on both.
	sequencer := classify(t, 200, neutralJsonRpcCode, "sender is over rate limit", nil, "eth_sendRawTransaction")
	if sequencer == nil {
		t.Fatal("expected an error")
	}
	if !common.HasErrorCode(sequencer, common.ErrCodeEndpointCapacityExceeded) {
		t.Fatalf("got %T (%v), want a capacity-exceeded error", sequencer, sequencer)
	}
	if common.IsRetryableTowardNetwork(sequencer) {
		t.Fatal("a sequencer rate limit is retryable toward the network; every upstream fronts the same sequencer")
	}

	// The contrast case. A per-provider quota IS worth another provider.
	provider := classify(t, 429, neutralJsonRpcCode, "your key exceeded its compute units", nil, "eth_call")
	if !common.HasErrorCode(provider, common.ErrCodeEndpointCapacityExceeded) {
		t.Fatalf("got %T (%v), want a capacity-exceeded error", provider, provider)
	}
	if !common.IsRetryableTowardNetwork(provider) {
		t.Fatal("an ordinary provider rate limit must stay retryable toward the network")
	}
}

func TestExtractJsonRpcError_LabelsANodeTimeoutWithItsOwnNormalizedCode(t *testing.T) {
	t.Parallel()

	// A node timeout and an uncatalogued failure both become server-side
	// exceptions, so the eRPC type cannot tell them apart. The normalized
	// JSON-RPC code is what reaches the client and what an operator groups on
	// when deciding whether a node is slow or broken.
	timeout := classify(t, 200, neutralJsonRpcCode, "execution timeout", nil, "")
	var jre *common.ErrJsonRpcExceptionInternal
	if !errors.As(timeout, &jre) {
		t.Fatalf("no internal json-rpc error in the chain: %T", timeout)
	}
	if jre.NormalizedCode() != common.JsonRpcErrorNodeTimeout {
		t.Fatalf("normalized code = %v, want JsonRpcErrorNodeTimeout; a timeout must be distinguishable from a generic failure",
			jre.NormalizedCode())
	}

	other := classify(t, 200, neutralJsonRpcCode, "the node fell over in a way nobody has catalogued", nil, "")
	if !errors.As(other, &jre) {
		t.Fatalf("no internal json-rpc error in the chain: %T", other)
	}
	if jre.NormalizedCode() == common.JsonRpcErrorNodeTimeout {
		t.Fatal("an uncatalogued failure was labelled a node timeout")
	}
}

func TestExtractJsonRpcError_RetriesACodedInvalidParamsButNotASpelledOutOne(t *testing.T) {
	t.Parallel()

	// Both produce a client-side exception, so the type hides the difference.
	// Code -32602 is what a provider returns when it dislikes a parameter SHAPE
	// that another provider accepts, so it is worth another upstream. A message
	// that spells out the mistake is the caller's own error and retrying it
	// burns the pool's quota for nothing.
	coded := classify(t, 200, -32602, "the node did not like something", nil, "")
	if !common.HasErrorCode(coded, common.ErrCodeEndpointClientSideException) {
		t.Fatalf("got %T (%v), want a client-side exception", coded, coded)
	}
	if !common.IsRetryableTowardNetwork(coded) {
		t.Fatal("a coded -32602 must stay retryable; providers disagree on parameter shapes")
	}

	spelled := classify(t, 200, neutralJsonRpcCode, "invalid params: fromBlock after toBlock", nil, "")
	if !common.HasErrorCode(spelled, common.ErrCodeEndpointClientSideException) {
		t.Fatalf("got %T (%v), want a client-side exception", spelled, spelled)
	}
	if common.IsRetryableTowardNetwork(spelled) {
		t.Fatal("a spelled-out parameter mistake must not be retried across the whole pool")
	}
}

func TestExtractJsonRpcError_KeepsTheUpstreamsOwnWordsReachable(t *testing.T) {
	t.Parallel()

	// The recurring failure mode in this codebase is a layer swallowing the
	// cause and returning its own tidy error. An operator debugging a 3am
	// failover needs the vendor's exact sentence, and the status code, to
	// tell a quota problem from a node problem.
	err := classify(t, 429, neutralJsonRpcCode, "compute units per second capacity exceeded for key abc", nil, "")
	if err == nil {
		t.Fatal("expected an error")
	}

	var jre *common.ErrJsonRpcExceptionInternal
	if !errors.As(err, &jre) {
		t.Fatalf("the upstream's own json-rpc error is no longer reachable through the chain: %T", err)
	}
	if jre.Message != "compute units per second capacity exceeded for key abc" {
		t.Fatalf("message = %q; the vendor's exact wording must survive normalisation", jre.Message)
	}
	if got := jre.Details["statusCode"]; got != 429 {
		t.Fatalf("details[statusCode] = %v, want 429; the HTTP status must survive too", got)
	}
}

func TestExtractJsonRpcError_UnwrapsAlchemysRevertedPrefixIntoTheDataField(t *testing.T) {
	t.Parallel()

	// Alchemy prefixes revert data with "Reverted ". Passing that through would
	// make the ABI blob undecodable for every client that reads details.data,
	// and would differ from every other vendor for the identical revert.
	err := classify(t, 200, neutralJsonRpcCode, "execution reverted", "Reverted 0x08c379a0deadbeef", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	var jre *common.ErrJsonRpcExceptionInternal
	if !errors.As(err, &jre) {
		t.Fatalf("no internal json-rpc error in the chain: %T", err)
	}
	if got := jre.Details["data"]; got != "0x08c379a0deadbeef" {
		t.Fatalf("details[data] = %v, want the prefix stripped", got)
	}
}

func TestExtractJsonRpcError_ReadsTheDataFieldWhenTheMessageAloneIsUseless(t *testing.T) {
	t.Parallel()

	// Several vendors answer with a generic message and put the real complaint
	// in `data`. The classifier appends data to the text it matches on. Without
	// that, these replies fall through to a generic server error and eRPC
	// retries a request that can never succeed as sent.
	err := classify(t, 200, neutralJsonRpcCode, "server error", "block range is too wide, try with this block range [0x1, 0x2]", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointRequestTooLarge) {
		t.Fatalf("classified as %T (%v); a complaint carried only in `data` must still be seen", err, err)
	}
}

func TestExtractJsonRpcError_PrefersTheNonceReadingOverEveryLaterBranch(t *testing.T) {
	t.Parallel()

	// Ordering guard. Real upstreams send "nonce too low" and "already known"
	// under code -32003, which a later branch would classify as a plain
	// execution exception. That would break eth_sendRawTransaction idempotency:
	// a duplicate broadcast would surface to the client as a hard failure
	// instead of the original transaction hash. This test only proves the
	// ordering because the code is the one a later branch also claims.
	const transactionRejected = int(common.JsonRpcErrorTransactionRejected)

	for _, tc := range []struct {
		message    string
		wantReason common.NonceExceptionReason
	}{
		{message: "already known", wantReason: common.NonceExceptionReasonAlreadyKnown},
		{message: "known transaction: 0xdead", wantReason: common.NonceExceptionReasonAlreadyKnown},
		{message: "transaction already exists", wantReason: common.NonceExceptionReasonAlreadyKnown},
		{message: "nonce too low", wantReason: common.NonceExceptionReasonNonceTooLow},
		{message: "nonce has already been used", wantReason: common.NonceExceptionReasonNonceTooLow},
	} {
		tc := tc
		t.Run(tc.message, func(t *testing.T) {
			t.Parallel()
			err := classify(t, 200, transactionRejected, tc.message, nil, "eth_sendRawTransaction")
			if err == nil {
				t.Fatal("expected an error")
			}
			var ne *common.ErrEndpointNonceException
			if !errors.As(err, &ne) {
				t.Fatalf("got %T (%v); the nonce branch must win over the later transaction-rejected branch", err, err)
			}
			if got := ne.Details["nonceExceptionReason"]; got != string(tc.wantReason) {
				t.Fatalf("reason = %v, want %v; the two reasons take different idempotency paths", got, tc.wantReason)
			}
		})
	}
}

func TestExtractJsonRpcError_LeavesReplacementUnderpricedAsARealFailure(t *testing.T) {
	t.Parallel()

	// "replacement transaction underpriced" is deliberately NOT in the
	// already-known list. It means a DIFFERENT transaction holds the nonce, so
	// treating it as idempotent success would tell the client its transaction
	// was accepted when a stranger's was.
	err := classify(t, 200, neutralJsonRpcCode, "replacement transaction underpriced", nil, "eth_sendRawTransaction")
	if err == nil {
		t.Fatal("expected an error")
	}
	if common.HasErrorCode(err, common.ErrCodeEndpointNonceException) {
		t.Fatalf("got %T (%v); an underpriced replacement is a real failure, never an idempotent success", err, err)
	}
}

func TestExtractJsonRpcError_RetriesARevertedSendRawTransactionButNotARevertedCall(t *testing.T) {
	t.Parallel()

	// Providers run different pre-flight simulations before accepting a raw
	// transaction, so one rejecting it says little about the next. An eth_call
	// revert is the chain's deterministic answer and must not be retried.
	for _, tc := range []struct {
		method        string
		wantRetryable bool
	}{
		{method: "eth_sendRawTransaction", wantRetryable: true},
		{method: "eth_sendrawtransaction", wantRetryable: true}, // casing must not change the verdict
		{method: "eth_call", wantRetryable: false},
		{method: "eth_estimateGas", wantRetryable: false},
	} {
		tc := tc
		t.Run(tc.method, func(t *testing.T) {
			t.Parallel()
			err := classify(t, 200, neutralJsonRpcCode, "execution reverted", nil, tc.method)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
				t.Fatalf("got %T (%v), want an execution exception", err, err)
			}
			if got := common.IsRetryableTowardNetwork(err); got != tc.wantRetryable {
				t.Fatalf("IsRetryableTowardNetwork = %v, want %v for %s", got, tc.wantRetryable, tc.method)
			}
		})
	}
}

func TestExtractJsonRpcError_StopsRetryingWhatCannotSucceedAnywhere(t *testing.T) {
	t.Parallel()

	// These two classes are client mistakes or invalid bytes. Marking them
	// retryable turns one bad client request into N upstream calls and can
	// exhaust a whole pool's rate limit on a request that will never work.
	for _, tc := range []struct {
		name    string
		message string
	}{
		{name: "malformed rlp", message: "rlp: expected input list for types.Transaction"},
		{name: "explicit invalid params text", message: "invalid params: fromBlock after toBlock"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classify(t, 200, neutralJsonRpcCode, tc.message, nil, "eth_sendRawTransaction")
			if err == nil {
				t.Fatal("expected an error")
			}
			if common.IsRetryableTowardNetwork(err) {
				t.Fatalf("%T (%v) is retryable toward the network; it can never succeed on another upstream", err, err)
			}
		})
	}
}

func TestExtractJsonRpcError_SplitsABatchRatherThanFailingItWhole(t *testing.T) {
	t.Parallel()

	// Code -32600 carrying a nested "validation errors in batch" message means
	// one member of a batch was bad, not the whole request. A retryable client
	// error lets the caller split and re-send, so the good members still get
	// answered. Both the code AND the nested data must be read: this test fails
	// if either check is dropped.
	err := classify(t, 200, -32600, "Invalid Request",
		map[string]interface{}{"message": "there were validation errors in batch member 3"}, "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
		t.Fatalf("got %T (%v), want a client-side exception the caller can split on", err, err)
	}
	if !common.IsRetryableTowardNetwork(err) {
		t.Fatal("a batch validation error must stay retryable so the caller can split and re-send")
	}
}

func TestExtractJsonRpcError_ReturnsNilForACleanReply(t *testing.T) {
	t.Parallel()

	// The classifier runs on EVERY response. If it invented an error for a
	// healthy 200 with a result, no request would ever succeed.
	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jr := common.MustNewJsonRpcResponse(1, "0x1234", nil)
	if err := ExtractJsonRpcError(r, nil, jr, nil); err != nil {
		t.Fatalf("a healthy reply was classified as an error: %T %v", err, err)
	}
}

func TestExtractJsonRpcError_CatchesARevertHiddenInASuccessfulResult(t *testing.T) {
	t.Parallel()

	// Some clients answer a reverted eth_call with HTTP 200, no error object,
	// and the ABI-encoded Error(string) selector in the RESULT. Without this
	// check eRPC would cache that revert blob as a valid answer and serve it to
	// every later caller.
	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jr := common.MustNewJsonRpcResponse(1,
		"0x08c379a00000000000000000000000000000000000000000000000000000000000000020", nil)

	err := ExtractJsonRpcError(r, nil, jr, nil)
	if err == nil {
		t.Fatal("a revert delivered inside a 200 result was not detected; it would be cached as a valid answer")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
		t.Fatalf("got %T (%v), want an execution exception", err, err)
	}
}

func TestExtractJsonRpcError_CatchesATraceTimeoutHiddenInASuccessfulResult(t *testing.T) {
	t.Parallel()

	// Trace and debug replies can be 50MB, so the classifier scans the raw text
	// for a timeout rather than parsing it. A timeout served as a result would
	// be cached as a complete trace and would silently truncate an indexer's
	// data.
	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","method":"debug_traceBlockByNumber","params":[],"id":1}`))
	nr := common.NewNormalizedResponse().WithRequest(req)
	jr := common.MustNewJsonRpcResponse(1, "execution timeout", nil)

	err := ExtractJsonRpcError(r, nr, jr, nil)
	if err == nil {
		t.Fatal("a trace timeout delivered as a result was not detected; it would be cached as a complete trace")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointServerSideException) {
		t.Fatalf("got %T (%v), want a server-side exception so retry and failover engage", err, err)
	}

	// The same text on a NON-tracing method is a legitimate string result.
	// Scanning every response body for the phrase would corrupt normal reads.
	readReq := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","method":"eth_call","params":[],"id":1}`))
	readNr := common.NewNormalizedResponse().WithRequest(readReq)
	readJr := common.MustNewJsonRpcResponse(1, "execution timeout", nil)
	if err := ExtractJsonRpcError(r, readNr, readJr, nil); err != nil {
		t.Fatalf("an eth_call result that merely contains the words was classified as a timeout: %T %v", err, err)
	}
}

// ExtractGrpcError is the same job for the BDS gRPC transport. It has no HTTP
// status and no JSON-RPC code to read, so the gRPC status code carries the whole
// decision. Every branch here maps to the same eRPC types the HTTP path
// produces, because routing must not care which transport delivered the answer.

func TestExtractGrpcError_MapsEveryStatusCodeToTheSameVerdictAsHttp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code     codes.Code
		wantCode common.ErrorCode
		why      string
	}{
		{code: codes.Unimplemented, wantCode: common.ErrCodeEndpointUnsupported,
			why: "the server does not offer the method; later requests must skip it"},
		{code: codes.InvalidArgument, wantCode: common.ErrCodeEndpointClientSideException,
			why: "the caller's request is wrong; failing over repeats the mistake"},
		{code: codes.ResourceExhausted, wantCode: common.ErrCodeEndpointCapacityExceeded,
			why: "a quota, not a fault; the upstream must cool down rather than be marked broken"},
		{code: codes.DeadlineExceeded, wantCode: common.ErrCodeEndpointServerSideException,
			why: "a server-side timeout is retryable"},
		{code: codes.Unauthenticated, wantCode: common.ErrCodeEndpointUnauthorized,
			why: "a credential problem must reach the operator"},
		{code: codes.PermissionDenied, wantCode: common.ErrCodeEndpointUnauthorized,
			why: "same as unauthenticated from the router's point of view"},
		{code: codes.NotFound, wantCode: common.ErrCodeEndpointMissingData,
			why: "the server does not hold this data; move to one that does"},
		{code: codes.OutOfRange, wantCode: common.ErrCodeEndpointMissingData,
			why: "the request fell outside the server's retained range"},
		{code: codes.Internal, wantCode: common.ErrCodeEndpointServerSideException,
			why: "the server's fault; fail over"},
		{code: codes.Unavailable, wantCode: common.ErrCodeEndpointServerSideException,
			why: "a dead connection must fail over, never reach the client"},
		{code: codes.Unknown, wantCode: common.ErrCodeEndpointServerSideException,
			why: "an unclassified failure still belongs to the server"},
		{code: codes.Aborted, wantCode: common.ErrCodeEndpointServerSideException,
			why: "the default fallthrough: an unlisted gRPC code must still fail over, not reach the client"},
		{code: codes.DataLoss, wantCode: common.ErrCodeEndpointServerSideException,
			why: "same fallthrough; new gRPC codes must be safe by default"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.code.String(), func(t *testing.T) {
			t.Parallel()
			err := ExtractGrpcError(status.New(tc.code, "upstream said something"), nil)
			if err == nil {
				t.Fatalf("classifier returned nil; %s", tc.why)
			}
			if !common.HasErrorCode(err, tc.wantCode) {
				t.Fatalf("classified as %T (%v), want %s — %s", err, err, tc.wantCode, tc.why)
			}
		})
	}
}

func TestExtractGrpcError_ReturnsNilForASuccessfulCall(t *testing.T) {
	t.Parallel()

	// Runs on every gRPC reply. Inventing an error on OK would break the whole
	// transport.
	if err := ExtractGrpcError(nil, nil); err != nil {
		t.Fatalf("a nil status became an error: %T %v", err, err)
	}
	if err := ExtractGrpcError(status.New(codes.OK, ""), nil); err != nil {
		t.Fatalf("codes.OK became an error: %T %v", err, err)
	}
}

func TestExtractGrpcError_KeepsTheServersMessageAndCodeReachable(t *testing.T) {
	t.Parallel()

	// Same debugging need as the HTTP path: without the gRPC code and message in
	// details, an operator cannot tell a quota from a crash.
	err := ExtractGrpcError(status.New(codes.ResourceExhausted, "per-key quota exhausted"), nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var jre *common.ErrJsonRpcExceptionInternal
	if !errors.As(err, &jre) {
		t.Fatalf("the internal json-rpc error is not reachable through the chain: %T", err)
	}
	if jre.Message != "per-key quota exhausted" {
		t.Fatalf("message = %q; the server's own wording must survive", jre.Message)
	}
	if got := jre.Details["grpcCode"]; got != codes.ResourceExhausted.String() {
		t.Fatalf("details[grpcCode] = %v, want %v", got, codes.ResourceExhausted)
	}
	if got := jre.Details["grpcMessage"]; got != "per-key quota exhausted" {
		t.Fatalf("details[grpcMessage] = %v, want the server's message", got)
	}
	if got := jre.Details["upstreamId"]; got != "n/a" {
		t.Fatalf("details[upstreamId] = %v, want the \"n/a\" placeholder when no upstream is known", got)
	}
}
