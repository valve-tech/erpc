package common

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Error classification decides three operator-visible things: whether eRPC
// retries the same upstream, how loud the alert is, and what the metrics label
// says. Each is a decision the request path makes silently, so each is pinned
// here against a concrete error rather than a mock.

func TestIsRetryableTowardsUpstream_TerminalCodesAreNotRetried(t *testing.T) {
	// Retrying any of these on the SAME upstream cannot succeed: the upstream
	// already answered definitively, so a retry only burns latency and budget.
	cases := []struct {
		name string
		err  error
	}{
		{"circuit breaker open", NewErrFailsafeCircuitBreakerOpen(ScopeUpstream, nil, nil)},
		{"request skipped", NewErrUpstreamRequestSkipped(errors.New("no match"), "up1")},
		{"method ignored", NewErrUpstreamMethodIgnored("eth_getProof", "up1")},
		{"endpoint unsupported", NewErrEndpointUnsupported(errors.New("no trace"))},
		{"billing issue", NewErrEndpointBillingIssue(errors.New("plan exhausted"))},
		{"json-rpc unmarshal", NewErrJsonRpcRequestUnmarshal(errors.New("eof"), []byte("{"))},
		{"invalid request", NewErrInvalidRequest(errors.New("bad params"))},
		{"execution exception", NewErrEndpointExecutionException(errors.New("reverted"))},
		{"unauthorized", NewErrEndpointUnauthorized(errors.New("bad key"))},
		{"request too large", NewErrEndpointRequestTooLarge(errors.New("range"), EvmBlockRangeTooLarge)},
		{"content validation", NewErrEndpointContentValidation(errors.New("hash mismatch"), nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, IsRetryableTowardsUpstream(tc.err))
		})
	}
}

// Every capacity error is a "come back later on someone else" signal, so the
// same upstream must not be hammered again immediately.
func TestIsRetryableTowardsUpstream_CapacityIssuesAreNotRetried(t *testing.T) {
	capacity := []error{
		NewErrProjectRateLimitRuleExceeded("p", "b", "r"),
		NewErrNetworkRateLimitRuleExceeded("p", "evm:1", "b", "r"),
		NewErrUpstreamRateLimitRuleExceeded("up1", "b", "r"),
		NewErrAuthRateLimitRuleExceeded("p", "secret", "b", "r", "u", "1.2.3.4"),
		NewErrEndpointCapacityExceeded(errors.New("429")),
	}
	for _, err := range capacity {
		require.True(t, IsCapacityIssue(err), "%T must count as a capacity issue", err)
		require.False(t, IsRetryableTowardsUpstream(err), "%T must not be retried on the same upstream", err)
	}

	// A server-side 500 is NOT a capacity issue — the upstream may well answer
	// the very next call, so it stays retryable.
	server := NewErrEndpointServerSideException(errors.New("boom"), nil, 500)
	require.False(t, IsCapacityIssue(server))
	require.True(t, IsRetryableTowardsUpstream(server))
}

// An exhausted bundle is retryable if ANY child is. Collapsing it to "not
// retryable" because the first child is terminal would abandon a request that
// one healthy upstream could still serve.
func TestIsRetryableTowardsUpstream_ExhaustedFollowsItsChildren(t *testing.T) {
	terminal := NewErrEndpointUnsupported(errors.New("no trace"))
	transient := NewErrEndpointServerSideException(errors.New("boom"), nil, 500)

	allTerminal := NewErrUpstreamsExhaustedWithCause(errors.Join(terminal, terminal))
	require.False(t, IsRetryableTowardsUpstream(allTerminal))

	// The retryable child is deliberately last, so an implementation that only
	// inspected the first child would report false here.
	mixed := NewErrUpstreamsExhaustedWithCause(errors.Join(terminal, transient))
	require.True(t, IsRetryableTowardsUpstream(mixed))

	// An exhausted error with no children at all is terminal: nothing was
	// tried, so there is no evidence that a retry could make progress.
	require.False(t, IsRetryableTowardsUpstream(NewErrUpstreamsExhaustedWithCause(nil)))
}

// The default answer is "retry". A new error type must not become silently
// terminal just because nobody added it to a list.
func TestIsRetryableTowardsUpstream_UnknownErrorsStayRetryable(t *testing.T) {
	require.True(t, IsRetryableTowardsUpstream(errors.New("something new")))
	require.True(t, IsRetryableTowardsUpstream(NewErrEndpointRequestTimeout(time.Second, nil)))
	require.True(t, IsRetryableTowardsUpstream(nil))
}

func TestIsClientError_OnlyTheCallersOwnMistakes(t *testing.T) {
	clientFaults := []error{
		NewErrEndpointClientSideException(errors.New("bad params")),
		NewErrJsonRpcRequestUnmarshal(errors.New("eof"), []byte("{")),
		NewErrInvalidRequest(errors.New("bad body")),
		NewErrGetLogsExceededMaxAllowedRange(5000, 1000),
		NewErrGetLogsExceededMaxAllowedAddresses(500, 100),
		NewErrGetLogsExceededMaxAllowedTopics(50, 10),
	}
	for _, err := range clientFaults {
		require.True(t, IsClientError(err), "%T is the caller's fault", err)
	}

	// Infrastructure failures must NOT be misread as client errors, or the
	// operator's alerting downgrades a real outage to info.
	notClient := []error{
		nil,
		NewErrEndpointServerSideException(errors.New("boom"), nil, 500),
		NewErrEndpointCapacityExceeded(errors.New("429")),
		errors.New("dial tcp: connection refused"),
	}
	for _, err := range notClient {
		require.False(t, IsClientError(err), "%T is not a client error", err)
	}
}

func TestClassifySeverity_TellsAnOperatorWhatToActOn(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Severity
	}{
		{"nil", nil, SeverityInfo},
		// The caller's own mistake is not an incident.
		{"client error", NewErrInvalidRequest(errors.New("bad body")), SeverityInfo},
		{"execution exception", NewErrEndpointExecutionException(errors.New("reverted")), SeverityInfo},
		// Self-healing conditions: the block simply has not landed yet.
		{"block unavailable", NewErrUpstreamBlockUnavailable("up1", 100, 90, 80), SeverityWarning},
		{"missing data", NewErrEndpointMissingData(errors.New("not indexed"), nil), SeverityWarning},
		{"network not supported", NewErrNetworkNotSupported("p", "evm:1"), SeverityWarning},
		// A terminal error is a warning, not critical: retrying cannot help, but
		// nothing is on fire either.
		{"unsupported method", NewErrEndpointUnsupported(errors.New("no trace")), SeverityWarning},
		{"rate limited", NewErrEndpointCapacityExceeded(errors.New("429")), SeverityWarning},
		// A discarded hedge is expected traffic, not an outage.
		{"endpoint canceled", NewErrEndpointRequestCanceled(context.Canceled), SeverityWarning},
		{"bare context cancel", context.Canceled, SeverityWarning},
		// Everything that is retryable AND not a known benign case is the case
		// an operator must look at.
		{"server side exception", NewErrEndpointServerSideException(errors.New("boom"), nil, 500), SeverityCritical},
		{"endpoint timeout", NewErrEndpointRequestTimeout(time.Second, nil), SeverityCritical},
		{"unknown error", errors.New("something new"), SeverityCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ClassifySeverity(tc.err))
		})
	}
}

func TestHasErrorCode_ReachesThroughWrappersAndBundles(t *testing.T) {
	require.False(t, HasErrorCode(nil, ErrCodeInvalidRequest))
	require.False(t, HasErrorCode(errors.New("plain"), ErrCodeInvalidRequest))

	inner := NewErrEndpointCapacityExceeded(errors.New("429"))
	require.True(t, HasErrorCode(inner, ErrCodeEndpointCapacityExceeded))
	require.False(t, HasErrorCode(inner, ErrCodeInvalidRequest))

	// A bare *BaseError is not a StandardError value, so it takes its own path.
	base := &BaseError{Code: ErrCodeInvalidRequest}
	require.True(t, HasErrorCode(base, ErrCodeInvalidRequest))
	require.False(t, HasErrorCode(base, ErrCodeEndpointBillingIssue))

	// errors.Join bundles must be searched child by child. The match is placed
	// last so a first-child-only implementation fails.
	bundle := errors.Join(errors.New("plain"), inner)
	require.True(t, HasErrorCode(bundle, ErrCodeEndpointCapacityExceeded))
	require.False(t, HasErrorCode(bundle, ErrCodeInvalidRequest))

	// Several codes at once: any one hit is enough.
	require.True(t, HasErrorCode(inner, ErrCodeInvalidRequest, ErrCodeEndpointCapacityExceeded))
}

func TestErrorSummary_CompactModeKeepsCardinalityLow(t *testing.T) {
	SetErrorLabelMode(ErrorLabelModeCompact)
	t.Cleanup(func() { SetErrorLabelMode(ErrorLabelModeVerbose) })

	// A compact label must be the code alone, so a per-request message can
	// never explode the metric's cardinality.
	require.Equal(t, "ErrEndpointCapacityExceeded",
		ErrorSummary(NewErrEndpointCapacityExceeded(errors.New("rate limit for key 0xabc123"))))

	// Non-StandardError values still have to resolve to a bounded set.
	require.Equal(t, "ContextDeadlineExceeded", ErrorSummary(context.DeadlineExceeded))
	require.Equal(t, "ContextCanceled", ErrorSummary(context.Canceled))
	require.Equal(t, "GenericError", ErrorSummary(errors.New("dial tcp 10.0.0.1:8545: refused")))
	require.Equal(t, "StringError", ErrorSummary("a raw string"))
	require.Equal(t, "UnknownError", ErrorSummary(42))
	require.Equal(t, "", ErrorSummary(nil))
}

// In verbose mode the label carries the code chain and a scrubbed message. The
// scrubbing is what keeps hashes, IPs and numbers out of Prometheus.
func TestErrorSummary_VerboseModeScrubsVariableText(t *testing.T) {
	SetErrorLabelMode(ErrorLabelModeVerbose)
	t.Cleanup(func() { SetErrorLabelMode(ErrorLabelModeVerbose) })

	err := NewErrEndpointServerSideException(
		errors.New("node 10.1.2.3 failed on 0xdeadbeefcafe at height 1234567"), nil, 500)
	got := ErrorSummary(err)

	require.Contains(t, got, "ErrEndpointServerSideException", "the code must survive scrubbing")
	require.Contains(t, got, "X.X.X.X", "an IP must be replaced by a placeholder, not deleted")
	require.Contains(t, got, "0xREDACTED", "a hash must be replaced by a placeholder, not deleted")
	require.NotContains(t, got, "10.1.2.3")
	require.NotContains(t, got, "deadbeef")
	require.NotContains(t, got, "1234567")
}

func TestErrorSummary_JoinedErrorsListEveryCode(t *testing.T) {
	SetErrorLabelMode(ErrorLabelModeVerbose)
	t.Cleanup(func() { SetErrorLabelMode(ErrorLabelModeVerbose) })

	a := NewErrEndpointUnsupported(errors.New("no trace"))
	b := NewErrEndpointBillingIssue(errors.New("plan exhausted"))

	// A single-child bundle must read exactly like the child alone.
	require.Equal(t, ErrorSummary(a), ErrorSummary(errors.Join(a)))

	both := ErrorSummary(errors.Join(a, b))
	require.Contains(t, both, "ErrEndpointUnsupported")
	require.Contains(t, both, "ErrEndpointBillingIssue")
}

// The fingerprint is what groups alerts. It must stay printable and bounded, or
// one pathological upstream message becomes an unusable alert group name.
func TestErrorFingerprint_IsBoundedAndPrintable(t *testing.T) {
	SetErrorLabelMode(ErrorLabelModeVerbose)
	t.Cleanup(func() { SetErrorLabelMode(ErrorLabelModeVerbose) })

	fp := ErrorFingerprint(errors.New("weird\x01message{with}[punctuation]"))
	require.NotContains(t, fp, "{")
	require.NotContains(t, fp, "\x01")
	require.Contains(t, fp, "weird", "the readable part must survive")

	long := ErrorFingerprint(errors.New(strings.Repeat("ab ", 500)))
	require.LessOrEqual(t, len(long), 256, "a fingerprint must never exceed the label budget")
}

func TestIsNull_DistinguishesUnsetFromReal(t *testing.T) {
	require.True(t, IsNull(nil))
	require.True(t, IsNull(""))
	// A BaseError with no code is the zero value — treat it as absent.
	require.True(t, IsNull(&BaseError{}))
	require.False(t, IsNull(NewErrInvalidRequest(errors.New("bad"))))
	require.False(t, IsNull(errors.New("plain")))
}

// A client that hangs up mid-response must not raise an infra alert. These are
// the shapes a closed connection actually produces.
func TestIsClientDisconnect_RecognisesTheBenignShapes(t *testing.T) {
	require.False(t, IsClientDisconnect(nil))
	require.True(t, IsClientDisconnect(context.Canceled))
	require.True(t, IsClientDisconnect(context.DeadlineExceeded))
	require.True(t, IsClientDisconnect(errors.New("use of closed network connection")))
	require.True(t, IsClientDisconnect(fmt.Errorf("write tcp: broken pipe")))
	require.True(t, IsClientDisconnect(fmt.Errorf("read tcp: connection reset by peer")))

	// A genuine upstream failure must not be waved through as a disconnect.
	require.False(t, IsClientDisconnect(errors.New("dial tcp: connection refused")))
}

func TestBaseError_DeepSearch_WalksCausesAndBundles(t *testing.T) {
	leaf := &BaseError{Code: "ErrLeaf", Details: map[string]interface{}{"upstreamId": "up-deep"}}

	// Straight cause chain.
	chained := &BaseError{Code: "ErrOuter", Cause: leaf}
	require.Equal(t, "up-deep", chained.DeepSearch("upstreamId"))

	// The outer error's own detail wins over a deeper one.
	shadowed := &BaseError{Code: "ErrOuter", Cause: leaf, Details: map[string]interface{}{"upstreamId": "up-outer"}}
	require.Equal(t, "up-outer", shadowed.DeepSearch("upstreamId"))

	// A multi-error cause must be searched child by child. The carrier is last,
	// so a first-child-only walk returns nil here.
	bundled := &BaseError{Code: "ErrOuter", Cause: errors.Join(&BaseError{Code: "ErrOther"}, leaf)}
	require.Equal(t, "up-deep", bundled.DeepSearch("upstreamId"))

	require.Nil(t, chained.DeepSearch("missingKey"))
}

func TestErrUpstreamsExhausted_SummarizesCausesByCategory(t *testing.T) {
	causes := errors.Join(
		NewErrEndpointUnsupported(errors.New("no trace")),
		NewErrEndpointMissingData(errors.New("not indexed"), nil),
		NewErrEndpointRequestTimeout(time.Second, nil),
		NewErrEndpointServerSideException(errors.New("boom"), nil, 500),
		NewErrEndpointCapacityExceeded(errors.New("429")),
		NewErrFailsafeCircuitBreakerOpen(ScopeUpstream, nil, nil),
		NewErrEndpointBillingIssue(errors.New("plan")),
		NewErrUpstreamRequestSkipped(errors.New("no match"), "up1"),
		NewErrUpstreamMethodIgnored("eth_getProof", "up2"),
		NewErrEndpointUnauthorized(errors.New("bad key")),
		NewErrUpstreamSyncing("up3"),
		NewErrUpstreamExcludedByPolicy("up4"),
		NewErrEndpointRequestTooLarge(errors.New("range"), EvmBlockRangeTooLarge),
		NewErrEndpointContentValidation(errors.New("mismatch"), nil),
		NewErrInvalidRequest(errors.New("bad params")),
		errors.New("something nobody classified"),
	)
	exhausted := NewErrUpstreamsExhaustedWithCause(causes).(*ErrUpstreamsExhausted)

	summary := exhausted.SummarizeCauses()
	// An operator reads this line to decide what to do next, so every distinct
	// failure mode has to appear with its own count.
	for _, want := range []string{
		"1 upstream unsupported method",
		"1 upstream missing data",
		"1 upstream timeout",
		"1 upstream server errors",
		"1 upstream rate limited",
		"1 upstream circuit breaker open",
		"1 upstream billing issues",
		"1 upstream skipped",
		"1 upstream method ignored",
		"1 upstream unauthorized",
		"1 upstream not synced",
		"1 upstream excluded by policy",
		"1 upstream too large complaints",
		"1 upstream validation mismatch",
		"1 user errors",
		"1 upstream unknown errors",
	} {
		require.Contains(t, summary, want)
	}

	// The summary becomes the deepest message, which is what the JSON-RPC error
	// body shows the client.
	require.Equal(t, summary, exhausted.DeepestMessage())
}

// With no cause at all there is nothing to summarize, and the message must fall
// back to the generic text instead of an empty string.
func TestErrUpstreamsExhausted_NoCauseFallsBackToItsOwnMessage(t *testing.T) {
	e := NewErrUpstreamsExhaustedWithCause(nil).(*ErrUpstreamsExhausted)
	require.Equal(t, "", e.SummarizeCauses())
	require.Equal(t, "all upstream attempts failed", e.DeepestMessage())
	require.Nil(t, e.Errors())
	require.Equal(t, ErrCodeUpstreamsExhausted, ErrorCode(e.CodeChain()))
	require.False(t, e.IsObjectNull())
	require.True(t, (*ErrUpstreamsExhausted)(nil).IsObjectNull())
}

// A single unclassified child has no category, so the bundle must surface that
// child's own message rather than an empty body.
func TestErrUpstreamsExhausted_SingleUnclassifiedChildSurfacesItsMessage(t *testing.T) {
	e := NewErrUpstreamsExhaustedWithCause(errors.Join(errors.New("dial tcp: refused"))).(*ErrUpstreamsExhausted)
	// A lone unknown error still counts as one "unknown errors" reason.
	require.Equal(t, "1 upstream unknown errors", e.DeepestMessage())
	require.Len(t, e.Errors(), 1)
}

func TestErrUpstreamsExhausted_CarriesCountersAndUpstreams(t *testing.T) {
	up1 := NewFakeUpstream("up1")
	up2 := NewFakeUpstream("up2")

	causes := &sync.Map{}
	causes.Store(up1, NewErrEndpointServerSideException(errors.New("boom"), nil, 500))
	causes.Store(up2, NewErrEndpointUnsupported(errors.New("no trace")))

	err := NewErrUpstreamsExhausted(nil, causes, "p", "evm:1", "eth_call", 250*time.Millisecond, 4, 2, 1, 2)
	e := err.(*ErrUpstreamsExhausted)

	// These counters land in the response headers an operator uses to see how
	// hard eRPC tried before giving up.
	require.Equal(t, 4, e.Attempts())
	require.Equal(t, 2, e.Retries())
	require.Equal(t, 1, e.Hedges())
	require.False(t, e.FromCache())
	require.Nil(t, e.Request())

	// The message must already name what went wrong; the caller logs it as-is.
	require.Contains(t, e.Error(), "upstream server errors")
	require.Contains(t, e.Error(), "upstream unsupported method")

	require.Len(t, e.Errors(), 2)
}

// orderCauses puts network-retryable causes first so the message an operator
// reads leads with the failures another upstream could still have served.
func TestErrUpstreamsExhausted_RetryableCausesSortFirst(t *testing.T) {
	// ErrInvalidRequest opts out of network retry; the server-side exception
	// does not. The non-retryable one gets the alphabetically FIRST key, so a
	// plain key sort would put it first and the assertion below would fail.
	causes := &sync.Map{}
	causes.Store("a-not-retryable", NewErrInvalidRequest(errors.New("bad params")))
	causes.Store("z-retryable", NewErrEndpointServerSideException(errors.New("boom"), nil, 500))

	require.False(t, IsRetryableTowardNetwork(NewErrInvalidRequest(errors.New("bad params"))),
		"fixture check: the first cause must really be non-retryable")

	err := NewErrUpstreamsExhausted(nil, causes, "p", "evm:1", "eth_call", time.Second, 2, 0, 0, 2)
	children := err.(*ErrUpstreamsExhausted).Errors()
	require.Len(t, children, 2)

	require.True(t, HasErrorCode(children[0], ErrCodeEndpointServerSideException),
		"the retryable cause must lead")
	require.True(t, HasErrorCode(children[1], ErrCodeInvalidRequest))
}

// Two causes that are equally retryable fall back to their key — the upstream
// id — so the bundle reads the same way on every run. Without it the sync.Map
// iteration order would reshuffle the error message between identical failures.
//
// Both causes are the same error type, and the message text is ordered AGAINST
// the key order on purpose: the last-resort Error() comparison would put
// "z-upstream" first, so only the key rule can produce the expected result.
func TestErrUpstreamsExhausted_TiedCausesSortByKey(t *testing.T) {
	causes := &sync.Map{}
	causes.Store("z-upstream", NewErrEndpointServerSideException(errors.New("aaa broken"), nil, 500))
	causes.Store("a-upstream", NewErrEndpointServerSideException(errors.New("zzz slow"), nil, 500))

	err := NewErrUpstreamsExhausted(nil, causes, "p", "evm:1", "eth_call", time.Second, 2, 0, 0, 2)
	children := err.(*ErrUpstreamsExhausted).Errors()
	require.Len(t, children, 2)

	require.Contains(t, children[0].(StandardError).DeepestMessage(), "zzz slow",
		"the 'a-upstream' cause must come first, by key and not by message")
	require.Contains(t, children[1].(StandardError).DeepestMessage(), "aaa broken")
}

// A nil map must produce no causes rather than panicking during error
// construction — the exhausted path runs when things are already going wrong.
func TestErrUpstreamsExhausted_NilCauseMapIsSafe(t *testing.T) {
	err := NewErrUpstreamsExhausted(nil, nil, "p", "evm:1", "eth_call", time.Second, 0, 0, 0, 0)
	require.NotNil(t, err)
	require.Empty(t, err.(*ErrUpstreamsExhausted).Errors())
}

func TestErrUpstreamsExhausted_UpstreamIdsComeFromTheCauses(t *testing.T) {
	up1 := NewFakeUpstream("up-alpha")
	causes := &sync.Map{}
	causes.Store(up1, NewErrEndpointMissingData(errors.New("not indexed"), up1))

	e := NewErrUpstreamsExhausted(nil, causes, "p", "evm:1", "eth_call", time.Second, 1, 0, 0, 1).(*ErrUpstreamsExhausted)

	require.Equal(t, "up-alpha", e.UpstreamId(),
		"the failing upstream id must reach the metrics label through the bundle")

	ups := e.Upstreams()
	require.Len(t, ups, 1)
	require.Equal(t, "up-alpha", ups[0].Id())
}

// ErrUpstreamRequest carries the per-attempt counters an operator reads in the
// X-ERPC-* response headers.
//
// BUG (reported, not fixed here): it implements every ResponseMetadata method
// except the variadic-context form of IsObjectNull (common/errors.go:810), so
// it does NOT satisfy common.ResponseMetadata. LookupResponseMetadata therefore
// returns nil for it, and erpc/http_server.go:1316 emits no counter headers on
// an error response — the case where they matter most.
func TestErrUpstreamRequest_ExposesAttemptCounters(t *testing.T) {
	up := NewFakeUpstream("up1")
	cause := NewErrEndpointServerSideException(errors.New("boom"), nil, 500)

	err := NewErrUpstreamRequest(cause, up, "evm:1", "eth_call", 100*time.Millisecond, 3, 2, 1)
	e := err.(*ErrUpstreamRequest)

	require.Equal(t, 3, e.Attempts())
	require.Equal(t, 2, e.Retries())
	require.Equal(t, 1, e.Hedges())
	require.Equal(t, "up1", e.UpstreamId())
	require.False(t, e.FromCache())
	require.False(t, e.IsObjectNull())
	require.Same(t, up, e.Upstream())

	// The original cause must stay reachable — the layer above classifies on it.
	require.True(t, HasErrorCode(err, ErrCodeEndpointServerSideException))
	require.Same(t, cause, e.GetCause())
}

// metadataCarrier is an error that DOES satisfy ResponseMetadata. It exists
// because no error type in this package currently does — see the bug note in
// TestErrUpstreamRequest_ExposesAttemptCounters. Using a local carrier keeps
// this test about the traversal, not about which types happen to qualify.
type metadataCarrier struct{ error }

func (m metadataCarrier) FromCache() bool                        { return true }
func (m metadataCarrier) Attempts() int                          { return 5 }
func (m metadataCarrier) Retries() int                           { return 4 }
func (m metadataCarrier) Hedges() int                            { return 3 }
func (m metadataCarrier) UpstreamId() string                     { return "up1" }
func (m metadataCarrier) IsObjectNull(_ ...context.Context) bool { return false }

func TestLookupResponseMetadata_FindsTheCarrierThroughWrappers(t *testing.T) {
	require.Nil(t, LookupResponseMetadata(nil))
	require.Nil(t, LookupResponseMetadata(errors.New("plain")))

	carrier := metadataCarrier{errors.New("boom")}

	md := LookupResponseMetadata(carrier)
	require.NotNil(t, md)
	require.Equal(t, 5, md.Attempts())

	// Through an outer StandardError, which is how the HTTP layer sees it when
	// it fills in the X-ERPC-Attempts / X-ERPC-Upstream headers.
	outer := NewErrFailsafeRetryExceeded(ScopeNetwork, carrier, nil)
	md = LookupResponseMetadata(outer)
	require.NotNil(t, md, "the counters must survive the failsafe wrapper")
	require.Equal(t, 5, md.Attempts())
	require.Equal(t, "up1", md.UpstreamId())

	// Two wrappers deep — the walk must recurse, not peek one level.
	deeper := NewErrUpstreamsExhaustedWithCause(outer)
	md = LookupResponseMetadata(deeper)
	require.NotNil(t, md)
	require.Equal(t, "up1", md.UpstreamId())

	// A StandardError with no cause has nothing to find.
	require.Nil(t, LookupResponseMetadata(NewErrProjectNotFound("p")))
}
