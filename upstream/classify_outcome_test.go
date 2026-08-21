package upstream

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// classifyUpstreamOutcome is the only thing that decides the `outcome` label on
// erpc_upstream_attempt_outcome_total, and the ExecState an operator reads in a
// trace. Every row below is a different operator conclusion: "rate_limited"
// means buy more quota, "transport_error" means fix the network, and
// "server_error" — the fallthrough — means look at the node.

func TestClassifyUpstreamOutcome_MapsEachFailureToItsOwnLabel(t *testing.T) {
	endpoint := &url.URL{Host: "node"}

	for _, tc := range []struct {
		name string
		err  error
		want common.UpstreamAttemptOutcome
	}{
		{
			name: "client cancelled the request",
			err:  common.NewErrEndpointRequestCanceled(errors.New("client hung up")),
			want: common.UpstreamOutcomeCancelled,
		},
		{
			name: "bare context cancellation",
			err:  context.Canceled,
			want: common.UpstreamOutcomeCancelled,
		},
		{
			name: "the dynamic timeout fired",
			err:  common.ErrDynamicTimeoutExceeded,
			want: common.UpstreamOutcomeTimeout,
		},
		{
			name: "the breaker is open",
			err:  common.NewErrFailsafeCircuitBreakerOpen(common.ScopeUpstream, errors.New("open"), nil),
			want: common.UpstreamOutcomeBreakerOpen,
		},
		{
			name: "the method is excluded",
			err:  common.NewErrUpstreamMethodIgnored("eth_getLogs", "u1"),
			want: common.UpstreamOutcomeSkipped,
		},
		{
			name: "the upstream was skipped",
			err:  common.NewErrUpstreamRequestSkipped(errors.New("shadow"), "u1"),
			want: common.UpstreamOutcomeSkipped,
		},
		{
			name: "the vendor throttled us",
			err:  common.NewErrEndpointCapacityExceeded(errors.New("429")),
			want: common.UpstreamOutcomeRateLimited,
		},
		{
			name: "the node does not hold the data",
			err:  common.NewErrEndpointMissingData(errors.New("pruned"), nil),
			want: common.UpstreamOutcomeMissingData,
		},
		{
			name: "the contract reverted",
			err:  common.NewErrEndpointExecutionException(errors.New("execution reverted")),
			want: common.UpstreamOutcomeExecRevert,
		},
		{
			name: "the block is outside the node's range",
			err:  common.NewErrUpstreamBlockUnavailable("u1", 100, 900, 800),
			want: common.UpstreamOutcomeBlockUnavailable,
		},
		{
			name: "the connection failed",
			err:  common.NewErrEndpointTransportFailure(endpoint, errors.New("connection reset")),
			want: common.UpstreamOutcomeTransportError,
		},
		{
			name: "the node returned a 5xx",
			err:  common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500),
			want: common.UpstreamOutcomeServerError,
		},
		{
			name: "the node rejected our request",
			err:  common.NewErrEndpointClientSideException(errors.New("bad params")),
			want: common.UpstreamOutcomeClientError,
		},
		{
			name: "an error nothing recognises",
			// The fallthrough is the primary path: a vendor error shape nobody
			// has classified yet must still produce a usable label, not an
			// empty one that disappears from every dashboard.
			err:  errors.New("some brand new vendor error"),
			want: common.UpstreamOutcomeServerError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyUpstreamOutcome(nil, tc.err))
		})
	}
}

func TestClassifyUpstreamOutcome_SeparatesSuccessFromAnEmptyAnswer(t *testing.T) {
	// An empty answer is not a failure, but it is not a hit either — the
	// rotation rule re-asks another upstream on `empty`, and counting it as
	// `success` would hide every wrong-empty node in the fleet.
	assert.Equal(t, common.UpstreamOutcomeSuccess, classifyUpstreamOutcome(nil, nil))

	empty := responseWith(`{"jsonrpc":"2.0","id":1,"result":null}`)
	require.True(t, empty.IsResultEmptyish(), "sanity: a null result is emptyish")
	assert.Equal(t, common.UpstreamOutcomeEmpty, classifyUpstreamOutcome(empty, nil))

	full := responseWith(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	require.False(t, full.IsResultEmptyish())
	assert.Equal(t, common.UpstreamOutcomeSuccess, classifyUpstreamOutcome(full, nil))
}

func TestClassifyUpstreamOutcome_AnErrorWinsOverTheResponse(t *testing.T) {
	// tryForward can hand back both. The error is what the caller acts on, so
	// the label has to follow the error, not the half-built response.
	full := responseWith(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	err := common.NewErrEndpointTransportFailure(&url.URL{Host: "node"}, errors.New("reset"))
	assert.Equal(t, common.UpstreamOutcomeTransportError, classifyUpstreamOutcome(full, err))
}

// responseWith builds a NormalizedResponse from a raw JSON-RPC body, the same
// way the client path does.
func responseWith(body string) *common.NormalizedResponse {
	return common.NewNormalizedResponse().
		WithBody(io.NopCloser(strings.NewReader(body))).
		WithExpectedSize(len(body))
}
