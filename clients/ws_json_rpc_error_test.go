package clients

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubExtractor stands in for the architecture extractors, which cannot be
// imported here — architecture/evm depends on this package. It classifies the
// one code these tests care about and declines everything else, which is the
// same contract the real extractors follow: return nil and let the caller keep
// the upstream's own error.
//
// It also asserts the client hands over a usable HTTP response. The real
// extractor dereferences r.StatusCode without a nil check, so passing nil
// would panic in production and never here.
type stubExtractor struct{ sawStatus int }

func (e *stubExtractor) Extract(
	r *http.Response,
	nr *common.NormalizedResponse,
	jr *common.JsonRpcResponse,
	up common.Upstream,
) error {
	if r != nil {
		e.sawStatus = r.StatusCode
	}
	if jr == nil || jr.Error == nil {
		return nil
	}
	if jr.Error.Code == -32602 {
		return common.NewErrEndpointClientSideException(
			common.NewErrJsonRpcExceptionInternal(
				int(jr.Error.Code),
				common.JsonRpcErrorInvalidArgument,
				jr.Error.Message,
				nil,
				nil,
			),
		)
	}
	return nil
}

// A JSON-RPC error that arrives over a WebSocket must be classified the same
// way the HTTP client classifies one. The HTTP client runs the architecture's
// error extractor (http_json_rpc_client.go, `c.errorExtractor.Extract`); the
// WebSocket client stored an extractor and never called it, so every error
// reply reached the caller as a bare *ErrJsonRpcExceptionExternal.
//
// Unclassified means "unknown server fault" to everything downstream. A node's
// -32602 was therefore retried — against the same upstream and then against
// the next one — and finally reported to the client as -32603 Internal error.
// A caller could not tell "the parameters you sent are wrong" from "the
// gateway is broken", and the one thing that would have told them apart was
// the code the node had already supplied.
func TestWsSendRequest_ClassifiesAnErrorTheUpstreamReturns(t *testing.T) {
	srv := newFakeWsServer(t)
	// Exactly what reth answers msgboard_subscribe with when the kind is not
	// one it serves.
	srv.RefuseMethod("msgboard_subscribe", -32602, `unsupported subscription kind: "notAKind"`)

	ext := &stubExtractor{}
	c := newTestWsClientWithExtractor(t, srv.wsURL(t), ext)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	nq := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"msgboard_subscribe","params":["notAKind"]}`))
	_, err := c.SendRequest(ctx, nq)
	require.Error(t, err, "a refused request must surface an error")

	// The node said the request was wrong. That must survive the trip.
	assert.True(t, common.IsClientError(err),
		"a -32602 from the upstream must be classified as the caller's error, got %#v", err)

	// The upstream's own code and message must not be lost in the process —
	// losing them is what turned a -32602 into a -32603 for the client.
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException),
		"got %#v", err)
	assert.Contains(t, err.Error(), "unsupported subscription kind",
		"the upstream's message must reach the caller")

	// Deliberately NOT asserted: that this is unretryable. eRPC retries a
	// generic -32602 on purpose — see the "we retry" comment in
	// architecture/evm/error_normalizer.go, which reserves the no-retry answer
	// for messages it recognises. Callers that must not retry a client error
	// key on IsClientError, which is what this fix restores.

	// The extractor must receive a response it can read. nil would panic the
	// real one on its very first line.
	assert.Equal(t, http.StatusOK, ext.sawStatus,
		"a delivered frame is a successful transport; the extractor must see that")
}

// The counterweight. Classification must not swallow a genuine server fault:
// an upstream that answers -32603 is still failing, and that IS worth
// retrying elsewhere. Without this, the test above would pass on a change
// that simply marked every error non-retryable.
func TestWsSendRequest_LeavesAServerFaultRetryable(t *testing.T) {
	srv := newFakeWsServer(t)
	srv.RefuseMethod("eth_blockNumber", -32603, "internal error")

	c := newTestWsClientWithExtractor(t, srv.wsURL(t), &stubExtractor{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	nq := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	_, err := c.SendRequest(ctx, nq)
	require.Error(t, err)

	assert.False(t, common.IsClientError(err),
		"an upstream's own internal error is not the caller's fault")
	assert.True(t, common.IsRetryableTowardsUpstream(err),
		"a server-side fault must stay retryable, or one bad upstream fails the request")
	assert.Contains(t, err.Error(), "internal error",
		"an extractor that declines must leave the upstream's own error intact")
}
