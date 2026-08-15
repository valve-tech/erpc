package thirdparty

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// warmConduit drives the vendor until the async refresh has published, so the
// tests that follow read a warm snapshot rather than a cold one.
func warmConduit(t *testing.T, v *ConduitVendor, settings common.VendorSettings) {
	t.Helper()
	logger := zerolog.Nop()
	require.Eventually(t, func() bool {
		ok, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:8453")
		return err == nil && ok
	}, fetcherTestWait, 10*time.Millisecond, "the async refresh never populated the conduit cache")
}

func conduitFixture(t *testing.T) (*ConduitVendor, common.VendorSettings, *atomic.Int32) {
	t.Helper()
	srv, hits := jsonServer(t, http.StatusOK, `{"endpoints":[
		{"id":"base","name":"Base","chainId":"8453","httpEndpoint":"https://base.conduit.example"},
		{"id":"opt","name":"Opt","chainId":"10","httpEndpoint":"https://opt.conduit.example"}
	]}`)
	v := CreateConduitVendor().(*ConduitVendor)
	settings := common.VendorSettings{
		"apiKey":          "secret-key",
		"networksUrl":     srv.URL,
		"recheckInterval": time.Hour,
	}
	return v, settings, hits
}

func TestConduitVendor_SupportsNetwork_ColdStartIsRetryableNotAFlatNo(t *testing.T) {
	v, settings, _ := conduitFixture(t)
	logger := zerolog.Nop()

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:8453")

	assert.False(t, supported)
	// The bootstrap loop reschedules on this exact sentinel. A plain
	// "unsupported" answer would drop the network for good.
	assert.ErrorIs(t, err, ErrRemoteCacheCold)
}

func TestConduitVendor_SupportsNetwork_AnswersFromTheRefreshedSnapshot(t *testing.T) {
	v, settings, hits := conduitFixture(t)
	logger := zerolog.Nop()
	warmConduit(t, v, settings)

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:10")
	require.NoError(t, err)
	assert.True(t, supported, "chain 10 is in the fixture")

	unknown, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:424242")
	require.NoError(t, err, "a chain missing from a warm snapshot is a definite no, not a retry")
	assert.False(t, unknown)

	assert.Equal(t, int32(1), hits.Load(), "a warm, fresh snapshot must not re-hit the network")
}

// The fetcher drops entries with no endpoint, but SupportsNetwork guards
// again in case the payload shape changes. Publishing straight into the cache
// is the only way to reach that guard.
func TestConduitVendor_SupportsNetwork_AnEntryWithNoEndpointIsUnsupported(t *testing.T) {
	v := CreateConduitVendor().(*ConduitVendor)
	logger := zerolog.Nop()
	settings := common.VendorSettings{"networksUrl": "cache-only", "recheckInterval": time.Hour}
	publishAndWait(t, v.cache, "cache-only", map[int64]*ConduitNetwork{
		998: nil,
		999: {ID: "no-endpoint", ChainID: "999"},
		10:  {ID: "ok", ChainID: "10", HttpEndpoint: "https://opt.example"},
	})

	usable, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:10")
	require.NoError(t, err)
	assert.True(t, usable)

	noEndpoint, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:999")
	require.NoError(t, err)
	assert.False(t, noEndpoint, "an entry with no endpoint must never be routed to")

	nilEntry, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:998")
	require.NoError(t, err)
	assert.False(t, nilEntry, "a nil entry must never be routed to")
}

func TestConduitVendor_SupportsNetwork_IgnoresNonEvmNetworksWithoutTouchingTheNetwork(t *testing.T) {
	v, settings, hits := conduitFixture(t)
	logger := zerolog.Nop()

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "btc:mainnet")

	require.NoError(t, err, "a non-EVM network is out of scope, not an error")
	assert.False(t, supported)
	assert.Equal(t, int32(0), hits.Load(), "a non-EVM network must not start a refresh")
}

func TestConduitVendor_SupportsNetwork_AMalformedChainIdIsAParseErrorNotAColdCache(t *testing.T) {
	v, settings, _ := conduitFixture(t)
	logger := zerolog.Nop()

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:mainnet")

	require.Error(t, err)
	assert.False(t, supported)
	assert.NotErrorIs(t, err, ErrRemoteCacheCold, "a bad chain ID must not look retryable")
	assert.Contains(t, err.Error(), "mainnet")
}

func TestConduitVendor_GenerateConfigs_AppendsTheApiKeyToTheDiscoveredEndpoint(t *testing.T) {
	v, settings, _ := conduitFixture(t)
	logger := zerolog.Nop()
	warmConduit(t, v, settings)

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Id: "cd", Evm: &common.EvmUpstreamConfig{ChainId: 8453}}, settings)

	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Equal(t, "https://base.conduit.example/secret-key", configs[0].Endpoint)
	assert.Equal(t, common.UpstreamTypeEvm, configs[0].Type)
	assert.Equal(t, "conduit", configs[0].VendorName)
}

func TestConduitVendor_GenerateConfigs_ColdStartIsRetryable(t *testing.T) {
	v, settings, _ := conduitFixture(t)
	logger := zerolog.Nop()

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 8453}}, settings)

	assert.Nil(t, configs)
	assert.ErrorIs(t, err, ErrRemoteCacheCold)
}

func TestConduitVendor_GenerateConfigs_AnUnknownChainOnAWarmCacheIsPermanent(t *testing.T) {
	v, settings, _ := conduitFixture(t)
	logger := zerolog.Nop()
	warmConduit(t, v, settings)

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}}, settings)

	assert.Nil(t, configs)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRemoteCacheCold, "a warm cache that lacks the chain must not ask for a retry")
	assert.Contains(t, err.Error(), "424242")
}

func TestConduitVendor_GenerateConfigs_APresetEndpointBypassesDiscovery(t *testing.T) {
	v, settings, hits := conduitFixture(t)
	logger := zerolog.Nop()

	upstream := &common.UpstreamConfig{Endpoint: "https://my.own.node/rpc"}
	configs, err := v.GenerateConfigs(context.Background(), &logger, upstream, settings)

	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Equal(t, "https://my.own.node/rpc", configs[0].Endpoint)
	assert.Equal(t, int32(0), hits.Load(), "an explicit endpoint must not trigger discovery")
	assert.NotNil(t, configs[0].JsonRpc, "the vendor still fills in the JSON-RPC defaults")
}

func TestConduitVendor_GenerateConfigs_RejectsIncompleteInputWithDistinctMessages(t *testing.T) {
	v, settings, _ := conduitFixture(t)
	logger := zerolog.Nop()
	ctx := context.Background()

	_, errNoEvm := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, settings)
	require.Error(t, errNoEvm)
	assert.Contains(t, errNoEvm.Error(), "upstream.evm")

	_, errNoChain := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, settings)
	require.Error(t, errNoChain)
	assert.Contains(t, errNoChain.Error(), "chainId")

	_, errNoKey := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 8453}},
		common.VendorSettings{"networksUrl": settings["networksUrl"]})
	require.Error(t, errNoKey)
	assert.Contains(t, errNoKey.Error(), "apiKey")
}

func TestConduitVendor_OwnsUpstream(t *testing.T) {
	v := CreateConduitVendor()

	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "conduit://base"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "evm+conduit://base"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{VendorName: "conduit"}))
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://base.conduit.example/k"}),
		"conduit claims by scheme or vendor name only, never by host")
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{VendorName: "alchemy"}))
}

func TestConduitVendor_GetVendorSpecificErrorIfAny_ClassifiesByCodeAndMessage(t *testing.T) {
	v := CreateConduitVendor()

	classify := func(code int, msg string) error {
		jrr, err := common.NewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(code, msg, ""))
		require.NoError(t, err)
		return v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 400}, jrr, map[string]interface{}{})
	}

	// -32600 plus an auth phrase is unauthorized: never retry it elsewhere.
	authErr := classify(-32600, "request must be authenticated")
	require.Error(t, authErr)
	assert.True(t, common.HasErrorCode(authErr, common.ErrCodeEndpointUnauthorized), "got %v", authErr)

	// -32600 without an auth phrase is not unauthorized.
	assert.Nil(t, classify(-32600, "invalid request"),
		"a plain invalid-request must fall through to the generic handler")

	// A capacity phrase wins regardless of the code.
	capErr := classify(-32005, "monthly limit exceeded")
	require.Error(t, capErr)
	assert.True(t, common.HasErrorCode(capErr, common.ErrCodeEndpointCapacityExceeded), "got %v", capErr)

	// Code 0 means the response carried no error at all.
	assert.Nil(t, classify(0, "monthly limit exceeded"))
}

// JSON-RPC 2.0 reserves -32099..-32000 for implementation-defined server
// errors. Conduit classifies that whole band as a server-side exception, which
// stays retryable toward the network because a sibling upstream may answer.
func TestConduitVendor_GetVendorSpecificErrorIfAny_TheServerErrorBandIsServerSide(t *testing.T) {
	v := CreateConduitVendor()

	// Both endpoints of the band, plus a code inside it.
	for _, code := range []int{-32099, -32050, -32000} {
		jrr, err := common.NewJsonRpcResponse(1, nil,
			common.NewErrJsonRpcExceptionExternal(code, "internal failure", ""))
		require.NoError(t, err)

		classified := v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 500}, jrr, map[string]interface{}{})

		require.Error(t, classified, "code %d must be classified", code)
		assert.True(t, common.HasErrorCode(classified, common.ErrCodeEndpointServerSideException),
			"code %d must be a server-side exception, got %v", code, classified)
		assert.True(t, common.IsRetryableTowardNetwork(classified),
			"code %d is the upstream's fault, so a sibling upstream is worth trying", code)
	}
}

// Just outside the band the vendor stays silent and the generic normaliser
// decides. -31999 and -32100 pin both edges.
func TestConduitVendor_GetVendorSpecificErrorIfAny_JustOutsideTheServerErrorBandFallsThrough(t *testing.T) {
	v := CreateConduitVendor()

	for _, code := range []int{-31999, -32100} {
		jrr, err := common.NewJsonRpcResponse(1, nil,
			common.NewErrJsonRpcExceptionExternal(code, "internal failure", ""))
		require.NoError(t, err)

		classified := v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 500}, jrr, map[string]interface{}{})

		assert.NoError(t, classified, "code %d sits outside the reserved server-error band", code)
	}
}

func TestConduitVendor_GetVendorSpecificErrorIfAny_IgnoresANonJsonRpcBody(t *testing.T) {
	v := CreateConduitVendor()

	err := v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 500},
		map[string]interface{}{"error": "boom"}, map[string]interface{}{})

	assert.NoError(t, err, "a body that is not a JSON-RPC response is not the vendor's to classify")
}

func TestConduitVendor_GetVendorSpecificErrorIfAny_CarriesTheDataFieldIntoDetails(t *testing.T) {
	v := CreateConduitVendor()
	jrr, err := common.NewJsonRpcResponse(1, nil,
		common.NewErrJsonRpcExceptionExternal(-32600, "missing access key", "key-id-7"))
	require.NoError(t, err)

	details := map[string]interface{}{}
	classified := v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 401}, jrr, details)

	require.Error(t, classified)
	assert.Equal(t, "key-id-7", details["data"], "the operator needs the vendor's data payload")
}
