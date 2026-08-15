package thirdparty

import (
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A vendor error normaliser decides whether eRPC retries a failed request on a
// sibling upstream. Getting that wrong either burns a whole network's quota on
// a request that can never succeed, or gives up on one that would. These tests
// assert the classification, not merely that an error came back.

// classifyWith runs a vendor normaliser over one JSON-RPC error.
func classifyWith(t *testing.T, v common.Vendor, status, code int, msg, data string, details map[string]interface{}) error {
	t.Helper()
	jrr, err := common.NewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(code, msg, data))
	require.NoError(t, err)
	if details == nil {
		details = map[string]interface{}{}
	}
	return v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: status}, jrr, details)
}

// -----------------------------------------------------------------------------
// quicknode
// -----------------------------------------------------------------------------

func TestQuicknodeVendor_GetVendorSpecificErrorIfAny_ClassifiesEachKnownCode(t *testing.T) {
	v := CreateQuicknodeVendor()

	cases := []struct {
		name      string
		code      int
		msg       string
		wantCode  common.ErrorCode
		retryable bool
	}{
		{"block range too large", -32614, "range too wide", common.ErrCodeEndpointRequestTooLarge, true},
		{"rate limited", -32009, "too many requests", common.ErrCodeEndpointCapacityExceeded, true},
		{"quota exhausted", -32007, "out of credits", common.ErrCodeEndpointCapacityExceeded, true},
		{"method not on plan", -32612, "add-on required", common.ErrCodeEndpointUnsupported, true},
		{"method disabled", -32613, "add-on required", common.ErrCodeEndpointUnsupported, true},
		{"gas limit", -32010, "transaction cost exceeds current gas limit", common.ErrCodeEndpointClientSideException, true},
		{"reverted", 3, "execution reverted", common.ErrCodeEndpointExecutionException, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyWith(t, v, 400, tc.code, tc.msg, "", nil)
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, tc.wantCode), "got %v", err)
			assert.Equal(t, tc.retryable, common.IsRetryableTowardNetwork(err),
				"retryability decides whether a sibling upstream sees this request")
		})
	}
}

func TestQuicknodeVendor_GetVendorSpecificErrorIfAny_ParseAndArgumentErrorsStopAtTheNetwork(t *testing.T) {
	v := CreateQuicknodeVendor()

	// A malformed request fails identically on every upstream, so eRPC must
	// not spend the whole network's budget re-asking.
	parseErr := classifyWith(t, v, 400, -32700, "failed to parse request body", "", nil)
	require.Error(t, parseErr)
	assert.True(t, common.HasErrorCode(parseErr, common.ErrCodeEndpointClientSideException), "got %v", parseErr)
	assert.False(t, common.IsRetryableTowardNetwork(parseErr))

	argErr := classifyWith(t, v, 400, -32602, "cannot unmarshal hex string of odd length", "", nil)
	require.Error(t, argErr)
	assert.True(t, common.HasErrorCode(argErr, common.ErrCodeEndpointClientSideException), "got %v", argErr)
	assert.False(t, common.IsRetryableTowardNetwork(argErr))

	// -32602 without the unmarshal phrase is a different fault and must not
	// borrow the non-retryable classification.
	assert.Nil(t, classifyWith(t, v, 400, -32602, "invalid params", "", nil))
}

func TestQuicknodeVendor_GetVendorSpecificErrorIfAny_UnauthorizedMatchesOnTheMessage(t *testing.T) {
	v := CreateQuicknodeVendor()

	err := classifyWith(t, v, 401, -1, "UNAUTHORIZED", "", nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized), "got %v", err)

	// The match is case-sensitive, so a lower-case message falls through.
	assert.Nil(t, classifyWith(t, v, 401, -1, "unauthorized", "", nil))
}

func TestQuicknodeVendor_GetVendorSpecificErrorIfAny_CodeZeroMeansNoVendorError(t *testing.T) {
	v := CreateQuicknodeVendor()

	assert.Nil(t, classifyWith(t, v, 200, 0, "UNAUTHORIZED", "", nil),
		"code 0 means the body carried no JSON-RPC error to classify")
}

func TestQuicknodeVendor_GetVendorSpecificErrorIfAny_IgnoresANonJsonRpcBody(t *testing.T) {
	v := CreateQuicknodeVendor()

	assert.NoError(t, v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 500},
		"a plain string body", map[string]interface{}{}))
}

// The quicknode normaliser shadows its `details` argument with a fresh map at
// quicknode.go:569, so nothing the caller put in `details` reaches the error.
// This test pins what an operator sees today; see the report.
func TestQuicknodeVendor_GetVendorSpecificErrorIfAny_DropsTheCallersDetails(t *testing.T) {
	v := CreateQuicknodeVendor()
	details := map[string]interface{}{"statusCode": 429, "headers": "x-qn-request-id: abc"}

	err := classifyWith(t, v, 429, -32009, "too many requests", "", details)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "x-qn-request-id",
		"the caller's request context never reaches the quicknode error")
	assert.NotContains(t, details, "data", "the shadowed map also swallows the vendor data field")
}

// -----------------------------------------------------------------------------
// infura
// -----------------------------------------------------------------------------

func TestInfuraVendor_GetVendorSpecificErrorIfAny_ClassifiesEachKnownCode(t *testing.T) {
	v := CreateInfuraVendor()

	authErr := classifyWith(t, v, 401, -32600, "request must be authenticated", "", nil)
	require.Error(t, authErr)
	assert.True(t, common.HasErrorCode(authErr, common.ErrCodeEndpointUnauthorized), "got %v", authErr)

	for _, code := range []int{-32001, -32004} {
		unsup := classifyWith(t, v, 400, code, "method not supported", "", nil)
		require.Error(t, unsup)
		assert.True(t, common.HasErrorCode(unsup, common.ErrCodeEndpointUnsupported), "code %d gave %v", code, unsup)
	}

	capErr := classifyWith(t, v, 429, -32005, "daily request count exceeded", "", nil)
	require.Error(t, capErr)
	assert.True(t, common.HasErrorCode(capErr, common.ErrCodeEndpointCapacityExceeded), "got %v", capErr)

	// -32600 alone is a bare invalid-request and must not read as unauthorized.
	assert.Nil(t, classifyWith(t, v, 400, -32600, "invalid request", "", nil))
	assert.Nil(t, classifyWith(t, v, 400, -32000, "something else", "", nil))
}

func TestInfuraVendor_GetVendorSpecificErrorIfAny_CarriesTheDataFieldIntoDetails(t *testing.T) {
	v := CreateInfuraVendor()
	details := map[string]interface{}{}

	err := classifyWith(t, v, 429, -32005, "daily request count exceeded", "reset-at=1700", details)

	require.Error(t, err)
	assert.Equal(t, "reset-at=1700", details["data"])
}

// -----------------------------------------------------------------------------
// drpc
// -----------------------------------------------------------------------------

func TestDrpcVendor_GetVendorSpecificErrorIfAny_ClassifiesByMessage(t *testing.T) {
	v := CreateDrpcVendor()

	authErr := classifyWith(t, v, 401, -32000, "provided token is invalid", "", nil)
	require.Error(t, authErr)
	assert.True(t, common.HasErrorCode(authErr, common.ErrCodeEndpointUnauthorized), "got %v", authErr)

	for _, msg := range []string{
		"ChainException: Unexpected error (code=40000)",
		"invalid block range requested",
	} {
		missing := classifyWith(t, v, 400, -32000, msg, "", nil)
		require.Error(t, missing, "message %q", msg)
		assert.True(t, common.HasErrorCode(missing, common.ErrCodeEndpointMissingData), "%q gave %v", msg, missing)
		assert.True(t, common.IsRetryableTowardNetwork(missing),
			"missing data may live on a sibling upstream, so the request must stay retryable")
	}

	// An unrecognised drpc message belongs to the generic normaliser.
	assert.Nil(t, classifyWith(t, v, 500, -32000, "internal server error", "", nil))
	// Code 0 short-circuits before any message match.
	assert.Nil(t, classifyWith(t, v, 401, 0, "provided token is invalid", "", nil))
}

// -----------------------------------------------------------------------------
// blockpi
// -----------------------------------------------------------------------------

func TestBlockPiVendor_GetVendorSpecificErrorIfAny_WrongChainKeyIsUnauthorized(t *testing.T) {
	v := CreateBlockPiVendor()

	// BlockPi issues one key per chain, so a key aimed at the wrong chain is
	// a config fault, not a transient one.
	for _, msg := range []string{
		"ApiKey is on another chain",
		"The API key is on another chain",
		"APIKEY IS ON ANOTHER CHAIN",
	} {
		err := classifyWith(t, v, 403, -32000, msg, "", nil)
		require.Error(t, err, "message %q", msg)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized), "%q gave %v", msg, err)
	}

	assert.Nil(t, classifyWith(t, v, 403, -32000, "api key is invalid", "", nil),
		"a different key fault falls through to the generic handler")
	assert.Nil(t, classifyWith(t, v, 200, 0, "", "", nil))
}

// -----------------------------------------------------------------------------
// llama
// -----------------------------------------------------------------------------

func TestLlamaVendor_GetVendorSpecificErrorIfAny_CloudflareRateLimitIsCapacity(t *testing.T) {
	v := CreateLlamaVendor()

	// Llama sits behind Cloudflare, which reports rate limiting as code 1015
	// inside the message rather than as a JSON-RPC code.
	err := classifyWith(t, v, 429, -32000, "error code: 1015", "", nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded), "got %v", err)

	assert.Nil(t, classifyWith(t, v, 429, -32000, "error code: 1020", "", nil),
		"a different Cloudflare code is not a rate limit")
}

func TestLlamaVendor_OwnsUpstream_ClaimsByHostOnly(t *testing.T) {
	v := CreateLlamaVendor()

	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://eth.llamarpc.com/k"}))
	// Llama has no scheme form and does not claim by vendor name.
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "llama://eth"}))
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{VendorName: "llama"}))
}

// -----------------------------------------------------------------------------
// blockdaemon and satelink classify on the HTTP status, not the JSON-RPC code
// -----------------------------------------------------------------------------

func TestBlockdaemonVendor_GetVendorSpecificErrorIfAny_ClassifiesOnTheHttpStatus(t *testing.T) {
	v := CreateBlockdaemonVendor()

	err := classifyWith(t, v, http.StatusUnauthorized, 0, "invalid token", "", nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized), "got %v", err)

	// 403 is a different fault and must not borrow the 401 classification.
	assert.Nil(t, classifyWith(t, v, http.StatusForbidden, 0, "forbidden", "", nil))
	assert.Nil(t, classifyWith(t, v, http.StatusOK, -32000, "invalid token", "", nil))
}

func TestBlockdaemonVendor_GetVendorSpecificErrorIfAny_ToleratesANilResponse(t *testing.T) {
	v := CreateBlockdaemonVendor()
	jrr, err := common.NewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(-32000, "boom", "d"))
	require.NoError(t, err)
	details := map[string]interface{}{}

	classified := v.GetVendorSpecificErrorIfAny(nil, nil, jrr, details)

	assert.NoError(t, classified, "no HTTP response means no status to classify on")
	assert.Equal(t, "d", details["data"], "the data field still reaches the operator")
}

func TestSatelinkVendor_GetVendorSpecificErrorIfAny_ClassifiesOnTheHttpStatus(t *testing.T) {
	v := CreateSatelinkVendor()

	billing := classifyWith(t, v, http.StatusPaymentRequired, -32000, "credits exhausted", "", nil)
	require.Error(t, billing)
	assert.True(t, common.HasErrorCode(billing, common.ErrCodeEndpointBillingIssue), "got %v", billing)

	capacity := classifyWith(t, v, http.StatusTooManyRequests, -32000, "daily limit", "", nil)
	require.Error(t, capacity)
	assert.True(t, common.HasErrorCode(capacity, common.ErrCodeEndpointCapacityExceeded), "got %v", capacity)

	auth := classifyWith(t, v, http.StatusUnauthorized, -32000, "unknown key", "", nil)
	require.Error(t, auth)
	assert.True(t, common.HasErrorCode(auth, common.ErrCodeEndpointUnauthorized), "got %v", auth)

	assert.Nil(t, classifyWith(t, v, http.StatusInternalServerError, -32000, "boom", "", nil),
		"an unmapped status belongs to the generic normaliser")
}

func TestSatelinkVendor_GetVendorSpecificErrorIfAny_A402CarriesThePaymentPointers(t *testing.T) {
	v := CreateSatelinkVendor()
	details := map[string]interface{}{}

	err := classifyWith(t, v, http.StatusPaymentRequired, -32000, "credits exhausted", "", details)

	require.Error(t, err)
	// An automated payer reads these out of the error metadata, so they are
	// part of the contract, not decoration.
	assert.Equal(t, 137, details["paymentChainId"])
	assert.NotEmpty(t, details["paymentVaultAddress"])
	assert.NotEmpty(t, details["paymentTokenAddress"])
	assert.NotEmpty(t, details["paymentCalldataUrl"])
	assert.NotEmpty(t, details["paymentManifestUrl"])
}

func TestSatelinkVendor_GetVendorSpecificErrorIfAny_ToleratesANilResponse(t *testing.T) {
	v := CreateSatelinkVendor()
	jrr, err := common.NewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(-32000, "boom", "d"))
	require.NoError(t, err)
	details := map[string]interface{}{}

	assert.NoError(t, v.GetVendorSpecificErrorIfAny(nil, nil, jrr, details))
	assert.Equal(t, "d", details["data"])
}

// -----------------------------------------------------------------------------
// Every normaliser must ignore a body it did not produce.
// -----------------------------------------------------------------------------

func TestEveryVendorNormaliser_IgnoresANonJsonRpcBody(t *testing.T) {
	registry := NewVendorsRegistry()
	names := registry.SupportedVendors()
	require.NotEmpty(t, names)

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			v := registry.LookupByName(name)
			require.NotNil(t, v)
			err := v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 500},
				map[string]interface{}{"error": "boom"}, map[string]interface{}{})
			assert.NoError(t, err, "a body that is not a JSON-RPC response is not a vendor error")
		})
	}
}
