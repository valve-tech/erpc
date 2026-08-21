package common

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// SummarizeCauses turns the pile of per-upstream failures behind a
// "all upstream attempts failed" response into the one line an operator reads
// first. Every bucket it can name is pinned here, because a miscounted or
// missing bucket sends the operator to the wrong subsystem.

func causeWithCode(code ErrorCode) error {
	return &BaseError{Code: code, Message: "test cause"}
}

func exhaustedWith(causes ...error) *ErrUpstreamsExhausted {
	return &ErrUpstreamsExhausted{
		BaseError: BaseError{
			Code:    ErrCodeUpstreamsExhausted,
			Message: "all upstream attempts failed",
			Cause:   errors.Join(causes...),
		},
	}
}

func TestSummarizeCauses_NamesEveryBucket(t *testing.T) {
	tests := []struct {
		name  string
		cause error
		want  string
	}{
		{"unsupported method", causeWithCode(ErrCodeEndpointUnsupported), "1 upstream unsupported method"},
		{"missing data", causeWithCode(ErrCodeEndpointMissingData), "1 upstream missing data"},
		{"endpoint capacity", causeWithCode(ErrCodeEndpointCapacityExceeded), "1 upstream rate limited"},
		{"upstream rate limit rule", causeWithCode(ErrCodeUpstreamRateLimitRuleExceeded), "1 upstream rate limited"},
		{"billing", causeWithCode(ErrCodeEndpointBillingIssue), "1 upstream billing issues"},
		{"circuit breaker", causeWithCode(ErrCodeFailsafeCircuitBreakerOpen), "1 upstream circuit breaker open"},
		{"endpoint timeout", causeWithCode(ErrCodeEndpointRequestTimeout), "1 upstream timeout"},
		{"failsafe timeout", causeWithCode(ErrCodeFailsafeTimeoutExceeded), "1 upstream timeout"},
		{"context deadline", context.DeadlineExceeded, "1 upstream timeout"},
		{"server side", causeWithCode(ErrCodeEndpointServerSideException), "1 upstream server errors"},
		{"hedge cancelled", causeWithCode(ErrCodeUpstreamHedgeCancelled), "1 hedges cancelled"},
		{"client side", causeWithCode(ErrCodeEndpointClientSideException), "1 user errors"},
		{"invalid request", causeWithCode(ErrCodeInvalidRequest), "1 user errors"},
		{"transport failure", causeWithCode(ErrCodeEndpointTransportFailure), "1 upstream transport errors"},
		{"syncing", causeWithCode(ErrCodeUpstreamSyncing), "1 upstream not synced"},
		{"block unavailable", causeWithCode(ErrCodeUpstreamBlockUnavailable), "1 upstream not synced"},
		{"excluded by policy", causeWithCode(ErrCodeUpstreamExcludedByPolicy), "1 upstream excluded by policy"},
		{"node type mismatch", causeWithCode(ErrCodeUpstreamNodeTypeMismatch), "1 node type mismatches"},
		{"method ignored", causeWithCode(ErrCodeUpstreamMethodIgnored), "1 upstream method ignored"},
		{"request skipped", causeWithCode(ErrCodeUpstreamRequestSkipped), "1 upstream skipped"},
		{"request too large", causeWithCode(ErrCodeEndpointRequestTooLarge), "1 upstream too large complaints"},
		{"unauthorized", causeWithCode(ErrCodeEndpointUnauthorized), "1 upstream unauthorized"},
		{"content validation", causeWithCode(ErrCodeEndpointContentValidation), "1 upstream validation mismatch"},
		{"an unclassified error", errors.New("something else"), "1 upstream unknown errors"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, exhaustedWith(tt.cause).SummarizeCauses())
		})
	}
}

// TestSummarizeCauses_CountsAndOrdersSeveralBuckets pins the aggregate line an
// operator sees when a request fanned out and failed for mixed reasons.
func TestSummarizeCauses_CountsAndOrdersSeveralBuckets(t *testing.T) {
	err := exhaustedWith(
		causeWithCode(ErrCodeEndpointServerSideException),
		causeWithCode(ErrCodeEndpointServerSideException),
		causeWithCode(ErrCodeEndpointTransportFailure),
		causeWithCode(ErrCodeUpstreamNodeTypeMismatch),
		causeWithCode(ErrCodeUpstreamHedgeCancelled),
		causeWithCode(ErrCodeEndpointMissingData),
	)

	require.Equal(t,
		"1 upstream missing data, 2 upstream server errors, 1 upstream transport errors, 1 node type mismatches, 1 hedges cancelled",
		err.SummarizeCauses())
}

// TestSummarizeCauses_ClassifiesEachCauseOnce proves the classifier's
// continue-chain: an error carrying two matching codes is counted in the first
// bucket only, so the counts add up to the number of attempts.
func TestSummarizeCauses_ClassifiesEachCauseOnce(t *testing.T) {
	both := &BaseError{
		Code:  ErrCodeEndpointUnsupported,
		Cause: causeWithCode(ErrCodeEndpointMissingData),
	}

	require.Equal(t, "1 upstream unsupported method", exhaustedWith(both).SummarizeCauses())
}

// TestSummarizeCauses_ReturnsEmptyWhenTheCauseIsNotAJoinedError pins the
// fallthrough: a single non-joined cause produces no summary, and the callers
// fall back to their own message.
func TestSummarizeCauses_ReturnsEmptyWhenTheCauseIsNotAJoinedError(t *testing.T) {
	err := &ErrUpstreamsExhausted{
		BaseError: BaseError{
			Code:    ErrCodeUpstreamsExhausted,
			Message: "all upstream attempts failed",
			Cause:   causeWithCode(ErrCodeEndpointServerSideException),
		},
	}

	require.Equal(t, "", err.SummarizeCauses())
}

func TestUpstreamsExhaustedDeepestMessage(t *testing.T) {
	t.Run("no cause falls back to the error's own message", func(t *testing.T) {
		err := &ErrUpstreamsExhausted{
			BaseError: BaseError{Code: ErrCodeUpstreamsExhausted, Message: "all upstream attempts failed"},
		}
		require.Equal(t, "all upstream attempts failed", err.DeepestMessage())
	})

	t.Run("a joined cause reports the bucket summary", func(t *testing.T) {
		err := exhaustedWith(
			causeWithCode(ErrCodeEndpointServerSideException),
			causeWithCode(ErrCodeUpstreamSyncing),
		)
		require.Equal(t, "1 upstream server errors, 1 upstream not synced", err.DeepestMessage())
	})

	t.Run("a single joined cause still reports the bucket summary, not the upstream's own message", func(t *testing.T) {
		// Recorded as bug 116: the branch that would surface the one failing
		// upstream's own message is unreachable, because SummarizeCauses
		// classifies every child and therefore never returns "" for a joined
		// cause. An operator debugging a one-upstream network sees the bucket
		// phrase instead of the node's text.
		err := exhaustedWith(&BaseError{
			Code:    ErrCodeEndpointServerSideException,
			Message: "execution reverted at 0xdeadbeef",
		})
		require.Equal(t, "1 upstream server errors", err.DeepestMessage())
	})

	t.Run("a non-joined cause yields an empty deepest message", func(t *testing.T) {
		err := &ErrUpstreamsExhausted{
			BaseError: BaseError{
				Code:    ErrCodeUpstreamsExhausted,
				Message: "all upstream attempts failed",
				Cause:   errors.New("plain"),
			},
		}
		require.Equal(t, "", err.DeepestMessage())
	})
}

// TestUpstreamsExhaustedErrors returns the joined children so a caller can walk
// them; a non-joined cause must yield nil rather than a one-element slice.
func TestUpstreamsExhaustedErrors(t *testing.T) {
	joined := exhaustedWith(
		causeWithCode(ErrCodeEndpointServerSideException),
		causeWithCode(ErrCodeUpstreamSyncing),
	)
	require.Len(t, joined.Errors(), 2)

	plain := &ErrUpstreamsExhausted{
		BaseError: BaseError{Code: ErrCodeUpstreamsExhausted, Cause: errors.New("plain")},
	}
	require.Nil(t, plain.Errors())

	var nilErr *ErrUpstreamsExhausted
	require.Nil(t, nilErr.Errors())
}

// TestNewErrJsonRpcRequestPreparation pins the retry hint the network executor
// reads off this error. The default must be "do not retry toward the network":
// a request eRPC could not even build will not build on a second upstream.
func TestNewErrJsonRpcRequestPreparation(t *testing.T) {
	t.Run("defaults to not retryable toward the network", func(t *testing.T) {
		err := NewErrJsonRpcRequestPreparation(errors.New("bad params"), nil)

		se, ok := err.(StandardError)
		require.True(t, ok)
		require.Equal(t, ErrorCode("ErrJsonRpcRequestPreparation"), se.Base().Code)
		require.Equal(t, false, se.Base().Details["retryableTowardNetwork"])
	})

	t.Run("keeps a caller-supplied retry hint", func(t *testing.T) {
		err := NewErrJsonRpcRequestPreparation(errors.New("bad params"), map[string]interface{}{
			"retryableTowardNetwork": true,
		})

		se, ok := err.(StandardError)
		require.True(t, ok)
		require.Equal(t, true, se.Base().Details["retryableTowardNetwork"])
	})
}
