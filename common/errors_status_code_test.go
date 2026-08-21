package common

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The HTTP status an error maps to is a client-facing contract. A client
// library decides from it whether to retry, whether to back off, and whether to
// surface the failure to a user. Changing 429 to 500, or 200 to 400, changes
// every caller's behaviour without changing a single log line, so each mapping
// is pinned here.

// statusOf reads the status an error advertises. Every error type in this table
// must implement the interface; a type that stops implementing it silently
// falls back to 500 at the HTTP layer, so the assertion is deliberate.
func statusOf(t *testing.T, err error) int {
	t.Helper()
	sc, ok := err.(interface{ ErrorStatusCode() int })
	require.True(t, ok, "%T must advertise an HTTP status", err)
	return sc.ErrorStatusCode()
}

func TestErrorStatusCode_ClientFaultsMapToFourHundred(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"invalid request", NewErrInvalidRequest(errors.New("bad body")), http.StatusBadRequest},
		{"invalid url path", NewErrInvalidUrlPath("no network", "/x"), http.StatusBadRequest},
		{"unknown network id", NewErrUnknownNetworkID(ArchitectureEvm), http.StatusBadRequest},
		{"unknown architecture", NewErrUnknownNetworkArchitecture("btc"), http.StatusBadRequest},
		{"invalid evm chain id", NewErrInvalidEvmChainId("mainnet"), http.StatusBadRequest},
		{"json-rpc unmarshal", NewErrJsonRpcRequestUnmarshal(errors.New("eof"), []byte("{")), http.StatusBadRequest},
		// NOTE: a nil upstream panics this constructor (common/errors.go:873
		// dereferences it without a guard, unlike its siblings), so the test
		// passes a real one.
		{"malformed upstream response", NewErrUpstreamMalformedResponse(errors.New("html"), NewFakeUpstream("up1")), http.StatusBadRequest},
		{"no websocket upstream", NewErrNoWsUpstreamAvailable("evm:1"), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, statusOf(t, tc.err))
		})
	}
}

func TestErrorStatusCode_AuthAndBillingKeepTheirOwnCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		// 401 tells a client to re-authenticate; a 500 would make it retry.
		{"auth unauthorized", NewErrAuthUnauthorized("secret", "bad token"), http.StatusUnauthorized},
		{"endpoint unauthorized", NewErrEndpointUnauthorized(errors.New("bad key")), 401},
		// 402 separates "you ran out of plan" from "we are broken", which is
		// what an operator's alerting needs to distinguish.
		{"endpoint billing", NewErrEndpointBillingIssue(errors.New("plan exhausted")), http.StatusPaymentRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, statusOf(t, tc.err))
		})
	}
}

func TestErrorStatusCode_RateLimitsAlwaysReturnTooManyRequests(t *testing.T) {
	// A rate limit that returned anything but 429 would make every client's
	// backoff logic inert, which turns a soft limit into an outage.
	cases := []struct {
		name string
		err  error
	}{
		{"auth budget", NewErrAuthRateLimitRuleExceeded("p", "secret", "b", "r", "u", "1.2.3.4")},
		{"project budget", NewErrProjectRateLimitRuleExceeded("p", "b", "r")},
		{"network budget", NewErrNetworkRateLimitRuleExceeded("p", "evm:1", "b", "r")},
		{"upstream budget", NewErrUpstreamRateLimitRuleExceeded("up1", "b", "r")},
		{"endpoint capacity", NewErrEndpointCapacityExceeded(errors.New("429 from vendor"))},
		{"subscription limit", NewErrSubscriptionLimitExceeded(10)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, http.StatusTooManyRequests, statusOf(t, tc.err))
		})
	}
}

func TestErrorStatusCode_NotFoundAndUnsupported(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"project not found", NewErrProjectNotFound("p"), http.StatusNotFound},
		{"network not found", NewErrNetworkNotFound("evm:1"), http.StatusNotFound},
		{"no upstreams defined", NewErrNoUpstreamsDefined("p"), http.StatusNotFound},
		{"no upstreams found", NewErrNoUpstreamsFound("p", "evm:1"), http.StatusNotFound},
		{"no upstreams available", NewErrNetworkNoUpstreamsAvailable("p", "evm:1"), http.StatusNotFound},
		{"network not supported", NewErrNetworkNotSupported("p", "evm:1"), http.StatusNotFound},
		{"subscription not found", NewErrSubscriptionNotFound("0xsub"), http.StatusNotFound},
		{"not implemented", NewErrNotImplemented("no ws"), http.StatusNotImplemented},
		// 406 marks "this upstream will not take it", which is distinct from
		// "nobody has it" — the routing layer relies on that difference.
		{"request skipped", NewErrUpstreamRequestSkipped(errors.New("no match"), "up1"), http.StatusNotAcceptable},
		{"method ignored", NewErrUpstreamMethodIgnored("eth_getProof", "up1"), http.StatusNotAcceptable},
		{"endpoint unsupported", NewErrEndpointUnsupported(errors.New("no trace")), http.StatusNotAcceptable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, statusOf(t, tc.err))
		})
	}
}

func TestErrorStatusCode_TimeoutsMapToGatewayTimeout(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		err  error
	}{
		{"request timeout", NewErrRequestTimeout(5 * time.Second)},
		{"network request timeout", NewErrNetworkRequestTimeout(5*time.Second, nil)},
		{"endpoint request timeout", NewErrEndpointRequestTimeout(5*time.Second, nil)},
		{"failsafe timeout", NewErrFailsafeTimeoutExceeded(ScopeNetwork, nil, &now)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, http.StatusGatewayTimeout, statusOf(t, tc.err))
		})
	}
}

// Several conditions deliberately answer 200 with a JSON-RPC error body,
// because that is what EVM clients expect. Turning any of them into a 4xx or
// 5xx breaks web3 libraries that only read the body on a 200.
func TestErrorStatusCode_ExpectedJsonRpcFailuresStayTwoHundred(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"execution exception", NewErrEndpointExecutionException(errors.New("execution reverted"))},
		{"missing data", NewErrEndpointMissingData(errors.New("not indexed"), nil)},
		{"nonce already known", NewErrEndpointNonceException(errors.New("already known"), NonceExceptionReasonAlreadyKnown)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, http.StatusOK, statusOf(t, tc.err))
		})
	}
}

// A client-side exception is a 400 by default, but a revert is the chain's real
// answer to a valid call. Sending 400 for a revert makes every eth_call that
// reverts look like a malformed request.
func TestErrEndpointClientSideException_RevertsAreTwoHundred(t *testing.T) {
	revertCodes := []JsonRpcErrorNumber{
		JsonRpcErrorEvmReverted,
		JsonRpcErrorCallException,
		JsonRpcErrorTransactionRejected,
	}
	for _, code := range revertCodes {
		inner := NewErrJsonRpcExceptionInternal(3, code, "execution reverted", nil, nil)
		err := NewErrEndpointClientSideException(inner)
		require.Equal(t, http.StatusOK, statusOf(t, err), "normalized code %d", code)
	}

	// Any other normalized code is a genuine client mistake.
	other := NewErrJsonRpcExceptionInternal(-32602, JsonRpcErrorInvalidArgument, "bad params", nil, nil)
	require.Equal(t, http.StatusBadRequest, statusOf(t, NewErrEndpointClientSideException(other)))

	// A non-JSON-RPC cause, or none at all, must also stay a 400.
	require.Equal(t, http.StatusBadRequest, statusOf(t, NewErrEndpointClientSideException(errors.New("bad params"))))
	require.Equal(t, http.StatusBadRequest, statusOf(t, NewErrEndpointClientSideException(nil)))
}

// A server-side exception forwards the vendor's own status when it has one.
// Collapsing every vendor 5xx to 500 would hide a 502 vs 503 distinction that
// tells an operator whether the vendor is down or overloaded.
func TestErrEndpointServerSideException_ForwardsTheOriginalStatus(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504} {
		err := NewErrEndpointServerSideException(errors.New("vendor down"), nil, code)
		require.Equal(t, code, statusOf(t, err))
	}

	// Zero means "no status was observed" (a transport failure, say), and must
	// fall back to 500 rather than emitting a nonsense status 0.
	err := NewErrEndpointServerSideException(errors.New("dial fail"), nil, 0)
	require.Equal(t, http.StatusInternalServerError, statusOf(t, err))
}

func TestErrorStatusCode_SizeAndConsensusVerdicts(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"request too large", NewErrEndpointRequestTooLarge(errors.New("range"), EvmBlockRangeTooLarge), http.StatusRequestEntityTooLarge},
		{"getLogs range", NewErrGetLogsExceededMaxAllowedRange(5000, 1000), http.StatusRequestEntityTooLarge},
		{"getLogs addresses", NewErrGetLogsExceededMaxAllowedAddresses(500, 100), http.StatusRequestEntityTooLarge},
		{"getLogs topics", NewErrGetLogsExceededMaxAllowedTopics(50, 10), http.StatusRequestEntityTooLarge},
		// 409 says "the upstreams disagree", which is a different operator
		// action from "no upstream answered".
		{"consensus dispute", NewErrConsensusDispute("disagreement", nil, nil), http.StatusConflict},
		{"consensus composition dispute", NewErrConsensusCompositionDispute("quota unmet", nil, nil), http.StatusConflict},
		// 412 says "the precondition (enough participants) was not met".
		{"consensus low participants", NewErrConsensusLowParticipants("too few", nil, nil), http.StatusPreconditionFailed},
		{"content validation", NewErrEndpointContentValidation(errors.New("hash mismatch"), nil), http.StatusBadGateway},
		{"block unavailable", NewErrUpstreamBlockUnavailable("up1", 100, 90, 80), http.StatusServiceUnavailable},
		{"network initializing", NewErrNetworkInitializing("p", "evm:1"), http.StatusServiceUnavailable},
		{"upstream syncing", NewErrUpstreamSyncing("up1"), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, statusOf(t, tc.err))
		})
	}
}
