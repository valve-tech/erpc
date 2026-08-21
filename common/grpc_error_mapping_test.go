package common

import (
	"errors"
	"net/http"
	"testing"

	bdscommon "github.com/blockchain-data-standards/manifesto/common"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A gRPC upstream reports failures as status codes. The mapping below decides
// whether eRPC retries, fails over, or hands the client a 4xx — so a code that
// lands in the wrong bucket either hammers a dead upstream or gives up on a
// healthy one. The tests assert the eRPC error CODE, not the message, because
// that code is what the retry and severity logic reads.

func TestExtractGrpcErrorFromGrpcStatus_NilAndOkProduceNoError(t *testing.T) {
	require.Nil(t, ExtractGrpcErrorFromGrpcStatus(nil, nil))
	require.Nil(t, ExtractGrpcErrorFromGrpcStatus(status.New(codes.OK, ""), nil))
}

func TestExtractGrpcErrorFromGrpcStatus_MapsEveryStatusCode(t *testing.T) {
	cases := []struct {
		code     codes.Code
		wantCode ErrorCode
	}{
		{codes.Canceled, ErrCodeEndpointRequestCanceled},
		{codes.Unimplemented, ErrCodeEndpointUnsupported},
		{codes.InvalidArgument, ErrCodeEndpointClientSideException},
		{codes.ResourceExhausted, ErrCodeEndpointCapacityExceeded},
		{codes.DeadlineExceeded, ErrCodeEndpointRequestTimeout},
		{codes.Unauthenticated, ErrCodeEndpointUnauthorized},
		{codes.PermissionDenied, ErrCodeEndpointUnauthorized},
		{codes.NotFound, ErrCodeEndpointMissingData},
		{codes.OutOfRange, ErrCodeEndpointMissingData},
		{codes.Internal, ErrCodeEndpointServerSideException},
		{codes.Unknown, ErrCodeEndpointServerSideException},
		{codes.Unavailable, ErrCodeEndpointServerSideException},
		// The unknown-code fallthrough is the primary path: a new gRPC code
		// must land somewhere safe and retryable, never be dropped.
		{codes.Aborted, ErrCodeEndpointServerSideException},
		{codes.AlreadyExists, ErrCodeEndpointServerSideException},
		{codes.DataLoss, ErrCodeEndpointServerSideException},
		{codes.FailedPrecondition, ErrCodeEndpointServerSideException},
	}

	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			err := ExtractGrpcErrorFromGrpcStatus(status.New(tc.code, "boom"), nil)
			require.Error(t, err)
			require.True(t, HasErrorCode(err, tc.wantCode),
				"%s must map to %s, got %s", tc.code, tc.wantCode, err.(StandardError).CodeChain())
		})
	}
}

// InvalidArgument means the request itself is wrong. Failing over to another
// upstream would re-send the same bad request to every node in the pool.
func TestExtractGrpcErrorFromGrpcStatus_InvalidArgumentDoesNotFailOver(t *testing.T) {
	err := ExtractGrpcErrorFromGrpcStatus(status.New(codes.InvalidArgument, "bad block tag"), nil)
	require.False(t, IsRetryableTowardNetwork(err),
		"a malformed request must not be re-sent to another upstream")
	require.True(t, IsClientError(err), "the caller must be told it is their request that is wrong")

	// An Internal error is the opposite: the node may well answer next time.
	server := ExtractGrpcErrorFromGrpcStatus(status.New(codes.Internal, "boom"), nil)
	require.True(t, IsRetryableTowardNetwork(server))
	require.True(t, IsRetryableTowardsUpstream(server))
	require.False(t, IsClientError(server))
}

// Marking an error non-retryable must not change what the error IS. The layers
// above match on the concrete type: erpc/http_server.go calls errors.As to lift
// the real endpoint error out of a failed bundle, and the type is what carries
// ErrorStatusCode. An error that arrives as a bare *BaseError answers neither.
func TestExtractGrpcErrorFromGrpcStatus_InvalidArgumentKeepsItsTypeAndStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"transport code", ExtractGrpcErrorFromGrpcStatus(status.New(codes.InvalidArgument, "bad block tag"), nil)},
		{"bds detail code", ExtractGrpcErrorFromGrpcStatus(
			bdscommon.NewError(bdscommon.ErrorCode_INVALID_PARAMETER, "bad parameter").ToGRPCStatus(), nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cse *ErrEndpointClientSideException
			require.True(t, errors.As(tc.err, &cse),
				"the concrete type must survive WithRetryableTowardNetwork, got %T", tc.err)

			sc, ok := tc.err.(interface{ ErrorStatusCode() int })
			require.Truef(t, ok, "the error must still carry a status code, got %T", tc.err)
			require.Equal(t, http.StatusBadRequest, sc.ErrorStatusCode())

			// The flag it was chained with must still be set.
			require.False(t, IsRetryableTowardNetwork(tc.err))
		})
	}
}

// WithRetryableTowardNetwork returns the receiver, not the embedded BaseError,
// for every error type that is chained with it today.
func TestWithRetryableTowardNetwork_ReturnsTheReceiver(t *testing.T) {
	cause := NewErrJsonRpcExceptionInternal(0, JsonRpcErrorClientSideException, "boom", nil, nil)

	for _, tc := range []struct {
		name string
		err  RetryableError
	}{
		{"client side", NewErrEndpointClientSideException(cause)},
		{"capacity exceeded", NewErrEndpointCapacityExceeded(cause).(RetryableError)},
		{"execution exception", NewErrEndpointExecutionException(cause).(RetryableError)},
		{"missing data", NewErrEndpointMissingData(cause, nil).(RetryableError)},
		{"invalid request", NewErrInvalidRequest(cause).(RetryableError)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marked := tc.err.WithRetryableTowardNetwork(false)
			require.IsTypef(t, tc.err, marked,
				"the concrete type must survive, got %T", marked)
			require.Same(t, tc.err, marked, "the receiver itself must come back")
			require.False(t, IsRetryableTowardNetwork(marked))

			// And the flag must be settable back the other way.
			require.True(t, IsRetryableTowardNetwork(marked.WithRetryableTowardNetwork(true)))
		})
	}
}

// The upstream id and the gRPC code must reach the error details. Without them
// an operator reading a log line cannot tell which node produced the failure.
func TestExtractGrpcErrorFromGrpcStatus_CarriesTheUpstreamAndCode(t *testing.T) {
	up := NewFakeUpstream("bds-alpha")
	err := ExtractGrpcErrorFromGrpcStatus(status.New(codes.Internal, "node exploded"), up)

	se := err.(StandardError)
	require.Equal(t, "bds-alpha", se.DeepSearch("upstreamId"))
	require.Equal(t, codes.Internal.String(), se.DeepSearch("grpcCode"))
	require.Equal(t, "node exploded", se.DeepSearch("grpcMessage"))
	require.Contains(t, se.DeepestMessage(), "node exploded",
		"the vendor's own text must survive to the deepest message")
}

// With no upstream the details must still be present with a placeholder. An
// absent key and a nil upstream look identical in a log, which is the mistake
// this assertion prevents.
func TestExtractGrpcErrorFromGrpcStatus_NoUpstreamStillRecordsTheField(t *testing.T) {
	err := ExtractGrpcErrorFromGrpcStatus(status.New(codes.Internal, "boom"), nil)
	require.Equal(t, "n/a", err.(StandardError).DeepSearch("upstreamId"))
}

// A BDS server sends a richer error code inside the status details. That code
// wins over the transport code, because the transport code is coarse: BDS
// reports "range outside available" as NotFound-shaped traffic that must be
// treated as missing data, not as a dead node.
func TestExtractGrpcErrorFromGrpcStatus_BdsDetailWinsOverTheTransportCode(t *testing.T) {
	cases := []struct {
		name     string
		bdsCode  bdscommon.ErrorCode
		wantCode ErrorCode
	}{
		{"unsupported block tag", bdscommon.ErrorCode_UNSUPPORTED_BLOCK_TAG, ErrCodeEndpointUnsupported},
		{"unsupported method", bdscommon.ErrorCode_UNSUPPORTED_METHOD, ErrCodeEndpointUnsupported},
		{"range outside available", bdscommon.ErrorCode_RANGE_OUTSIDE_AVAILABLE, ErrCodeEndpointMissingData},
		{"invalid parameter", bdscommon.ErrorCode_INVALID_PARAMETER, ErrCodeEndpointClientSideException},
		{"invalid request", bdscommon.ErrorCode_INVALID_REQUEST, ErrCodeEndpointClientSideException},
		{"rate limited", bdscommon.ErrorCode_RATE_LIMITED, ErrCodeEndpointCapacityExceeded},
		{"timeout", bdscommon.ErrorCode_TIMEOUT_ERROR, ErrCodeEndpointRequestTimeout},
		{"range too large", bdscommon.ErrorCode_RANGE_TOO_LARGE, ErrCodeEndpointRequestTooLarge},
		{"internal", bdscommon.ErrorCode_INTERNAL_ERROR, ErrCodeEndpointServerSideException},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := bdscommon.NewError(tc.bdsCode, "bds says so").ToGRPCStatus()
			// Fixture check: the detail really is attached, otherwise this test
			// would silently be exercising the plain transport-code path.
			_, hasDetail := bdscommon.FromGRPCStatus(st)
			require.True(t, hasDetail, "the BDS detail must be present for this test to mean anything")

			err := ExtractGrpcErrorFromGrpcStatus(st, NewFakeUpstream("bds-alpha"))
			require.True(t, HasErrorCode(err, tc.wantCode),
				"BDS %s must map to %s, got %s", tc.name, tc.wantCode, err.(StandardError).CodeChain())
			require.Equal(t, tc.bdsCode, err.(StandardError).DeepSearch("bdsErrorCode"),
				"the BDS code must be recorded for the operator")
		})
	}
}

// A BDS code the mapping does not name must fall through to the transport-code
// switch rather than vanish. This is the unknown-case path, so it is tested
// explicitly.
func TestExtractGrpcErrorFromGrpcStatus_UnknownBdsCodeFallsThroughToTheTransportCode(t *testing.T) {
	st := bdscommon.NewError(bdscommon.ErrorCode_ERROR_CODE_UNSPECIFIED, "who knows").ToGRPCStatus()

	err := ExtractGrpcErrorFromGrpcStatus(st, nil)
	require.Error(t, err)
	require.True(t, HasErrorCode(err, ErrCodeEndpointServerSideException),
		"an unrecognised BDS code must still produce a retryable server-side error")
}
