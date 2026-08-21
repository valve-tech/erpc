package evm

import (
	"testing"

	bdscommon "github.com/blockchain-data-standards/manifesto/common"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A BDS server can attach an ErrorDetails payload to a gRPC status. That payload
// says what actually went wrong, while the transport code is often just
// "Internal". eRPC routes on the payload when it is present, because the two
// disagree in the cases that matter most: a range that is too large is the
// client's problem, and failing it over to every upstream in turn wastes the
// whole fleet on a request none of them can answer.

// bdsStatus builds a gRPC status carrying a BDS error payload under the given
// transport code.
func bdsStatus(t *testing.T, transport codes.Code, bdsCode bdscommon.ErrorCode, msg string) *status.Status {
	t.Helper()
	st, err := status.New(transport, msg).WithDetails(&bdscommon.ErrorDetails{
		Code:    bdsCode,
		Message: msg,
	})
	require.NoError(t, err)
	return st
}

func TestExtractGrpcError_RoutesOnTheBdsPayloadNotTheTransportCode(t *testing.T) {
	// Every case rides on codes.Internal, which on its own means
	// "server-side, fail over". Only the payload separates them.
	for _, tc := range []struct {
		name     string
		bdsCode  bdscommon.ErrorCode
		wantCode common.ErrorCode
		why      string
	}{
		{"UnsupportedMethod", bdscommon.ErrorCode_UNSUPPORTED_METHOD, common.ErrCodeEndpointUnsupported,
			"the server does not offer the method, so later requests must skip it"},
		{"UnsupportedBlockTag", bdscommon.ErrorCode_UNSUPPORTED_BLOCK_TAG, common.ErrCodeEndpointUnsupported,
			"an unsupported tag is the same skip decision as an unsupported method"},
		{"RangeOutsideAvailable", bdscommon.ErrorCode_RANGE_OUTSIDE_AVAILABLE, common.ErrCodeEndpointMissingData,
			"the server does not hold this range, so another upstream might"},
		{"InvalidParameter", bdscommon.ErrorCode_INVALID_PARAMETER, common.ErrCodeEndpointClientSideException,
			"the caller's request is wrong, so failing over just repeats the mistake"},
		{"InvalidRequest", bdscommon.ErrorCode_INVALID_REQUEST, common.ErrCodeEndpointClientSideException,
			"same as an invalid parameter from the router's point of view"},
		{"RateLimited", bdscommon.ErrorCode_RATE_LIMITED, common.ErrCodeEndpointCapacityExceeded,
			"a quota is not a fault, so the upstream cools down rather than being marked broken"},
		{"Timeout", bdscommon.ErrorCode_TIMEOUT_ERROR, common.ErrCodeEndpointServerSideException,
			"a server-side timeout is retryable"},
		{"RangeTooLarge", bdscommon.ErrorCode_RANGE_TOO_LARGE, common.ErrCodeEndpointRequestTooLarge,
			"the request must be split, not retried against the whole fleet"},
		{"InternalError", bdscommon.ErrorCode_INTERNAL_ERROR, common.ErrCodeEndpointServerSideException,
			"the server's own fault, so fail over"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ExtractGrpcError(bdsStatus(t, codes.Internal, tc.bdsCode, "server said "+tc.name), nil)
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, tc.wantCode),
				"classified as %T (%v), want %s — %s", err, err, tc.wantCode, tc.why)
			assert.Contains(t, err.Error(), "server said "+tc.name,
				"the server's own message must stay readable")
		})
	}
}

// A payload code eRPC does not know about must not swallow the transport code.
// The gRPC status is the fallback, and it has to stay a working one, or a new
// BDS code would silently become "unknown server error".
func TestExtractGrpcError_FallsBackToTheTransportCodeForAnUnknownPayload(t *testing.T) {
	err := ExtractGrpcError(
		bdsStatus(t, codes.ResourceExhausted, bdscommon.ErrorCode_DATA_NOT_FOUND, "quota gone"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded),
		"an unhandled payload code must fall through to the transport verdict, got %T (%v)", err, err)
}

// A range-too-large verdict must carry the EVM reason the splitter reads. The
// classification alone is not enough: the request path decides to split on the
// reason attached to the error.
func TestExtractGrpcError_MarksARangeTooLargeAsABlockRangeProblem(t *testing.T) {
	err := ExtractGrpcError(
		bdsStatus(t, codes.Internal, bdscommon.ErrorCode_RANGE_TOO_LARGE, "range of 50000 blocks is too wide"), nil)
	require.Error(t, err)

	var tooLarge *common.ErrEndpointRequestTooLarge
	require.ErrorAs(t, err, &tooLarge)
	assert.Equal(t, common.EvmBlockRangeTooLarge, tooLarge.Details["complaint"],
		"the splitter reads this complaint to decide it may retry with a narrower range")
}

// The payload's own details and cause must reach the eRPC error. They are what
// an operator reads when the message alone does not say which limit was hit.
func TestExtractGrpcError_CarriesThePayloadDetailsAndTheUpstreamId(t *testing.T) {
	st, stErr := status.New(codes.Internal, "range too wide").WithDetails(&bdscommon.ErrorDetails{
		Code:    bdscommon.ErrorCode_RANGE_TOO_LARGE,
		Message: "range too wide",
		Details: map[string]string{"maxRange": "1000"},
		Cause: &bdscommon.ErrorDetails{
			Code:    bdscommon.ErrorCode_INVALID_PARAMETER,
			Message: "fromBlock is older than the pruning horizon",
		},
	})
	require.NoError(t, stErr)

	up := staleBlockNumberUpstream("bds-node-1", "bdsvendor")
	err := ExtractGrpcError(st, up)
	require.Error(t, err)

	rendered := err.Error()
	assert.Contains(t, rendered, "maxRange", "the payload's own details must survive the translation")
	assert.Contains(t, rendered, "1000")
	assert.Contains(t, rendered, "pruning horizon", "the payload's cause must survive too")
	assert.Contains(t, rendered, "bds-node-1", "the failing upstream must be named in the error")
}
