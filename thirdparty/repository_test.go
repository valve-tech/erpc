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

func repositoryFixture(t *testing.T) (*RepositoryVendor, common.VendorSettings, *atomic.Int32) {
	t.Helper()
	srv, hits := jsonServer(t, http.StatusOK, `{
		"1":{"endpoints":["https://one-a.example","https://one-b.example","wss://one-ws.example"]},
		"137":{"endpoints":[]}
	}`)
	v := CreateRepositoryVendor().(*RepositoryVendor)
	settings := common.VendorSettings{
		"repositoryUrl":   srv.URL,
		"recheckInterval": time.Hour,
	}
	return v, settings, hits
}

func warmRepository(t *testing.T, v *RepositoryVendor, settings common.VendorSettings) {
	t.Helper()
	logger := zerolog.Nop()
	require.Eventually(t, func() bool {
		ok, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:1")
		return err == nil && ok
	}, fetcherTestWait, 10*time.Millisecond, "the async refresh never populated the repository cache")
}

func TestRepositoryVendor_SupportsNetwork_ColdStartIsRetryable(t *testing.T) {
	v, settings, _ := repositoryFixture(t)
	logger := zerolog.Nop()

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:1")

	assert.False(t, supported)
	assert.ErrorIs(t, err, ErrRemoteCacheCold)
}

func TestRepositoryVendor_SupportsNetwork_AChainWithAnEmptyEndpointListIsUnsupported(t *testing.T) {
	v, settings, hits := repositoryFixture(t)
	logger := zerolog.Nop()
	warmRepository(t, v, settings)

	// Chain 137 is present in the payload but carries no endpoints.
	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:137")
	require.NoError(t, err, "a listed chain with no endpoints is a definite no, not a retry")
	assert.False(t, supported)

	missing, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:424242")
	require.NoError(t, err)
	assert.False(t, missing)

	assert.Equal(t, int32(1), hits.Load(), "a fresh snapshot serves every later question offline")
}

func TestRepositoryVendor_SupportsNetwork_IgnoresNonEvmNetworks(t *testing.T) {
	v, settings, hits := repositoryFixture(t)
	logger := zerolog.Nop()

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "solana:mainnet")

	require.NoError(t, err)
	assert.False(t, supported)
	assert.Equal(t, int32(0), hits.Load())
}

func TestRepositoryVendor_SupportsNetwork_AMalformedChainIdIsAParseError(t *testing.T) {
	v, settings, _ := repositoryFixture(t)
	logger := zerolog.Nop()

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:0x1")

	require.Error(t, err)
	assert.False(t, supported)
	assert.NotErrorIs(t, err, ErrRemoteCacheCold)
}

func TestRepositoryVendor_GenerateConfigs_MakesOneUpstreamPerHttpEndpoint(t *testing.T) {
	v, settings, _ := repositoryFixture(t)
	logger := zerolog.Nop()
	warmRepository(t, v, settings)

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Id: "pub", Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)

	require.NoError(t, err)
	require.Len(t, configs, 2, "the wss:// entry must be dropped; only http endpoints become upstreams")
	assert.Equal(t, "https://one-a.example", configs[0].Endpoint)
	assert.Equal(t, "https://one-b.example", configs[1].Endpoint)
	assert.NotEqual(t, configs[0].Id, configs[1].Id, "each endpoint needs its own upstream id")
	for _, c := range configs {
		assert.Equal(t, common.UpstreamTypeEvm, c.Type)
		assert.Contains(t, c.Id, "pub")
	}
}

func TestRepositoryVendor_GenerateConfigs_DefaultsAutoIgnoreUnsupportedMethodsOn(t *testing.T) {
	v, settings, _ := repositoryFixture(t)
	logger := zerolog.Nop()
	warmRepository(t, v, settings)

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)

	require.NoError(t, err)
	require.NotEmpty(t, configs)
	require.NotNil(t, configs[0].AutoIgnoreUnsupportedMethods)
	assert.True(t, *configs[0].AutoIgnoreUnsupportedMethods,
		"public endpoints are patchy, so the vendor turns method auto-ignore on by default")
}

func TestRepositoryVendor_GenerateConfigs_KeepsAnExplicitAutoIgnoreChoice(t *testing.T) {
	v, settings, _ := repositoryFixture(t)
	logger := zerolog.Nop()
	warmRepository(t, v, settings)

	off := false
	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{
			Evm:                          &common.EvmUpstreamConfig{ChainId: 1},
			AutoIgnoreUnsupportedMethods: &off,
		}, settings)

	require.NoError(t, err)
	require.NotEmpty(t, configs)
	require.NotNil(t, configs[0].AutoIgnoreUnsupportedMethods)
	assert.False(t, *configs[0].AutoIgnoreUnsupportedMethods, "an explicit false must survive the default")
}

func TestRepositoryVendor_GenerateConfigs_CopiesTheEvmBlockRatherThanSharingIt(t *testing.T) {
	v, settings, _ := repositoryFixture(t)
	logger := zerolog.Nop()
	warmRepository(t, v, settings)

	src := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
	configs, err := v.GenerateConfigs(context.Background(), &logger, src, settings)

	require.NoError(t, err)
	require.Len(t, configs, 2)
	assert.NotSame(t, src.Evm, configs[0].Evm, "each upstream needs its own evm block")
	assert.NotSame(t, configs[0].Evm, configs[1].Evm, "two upstreams must not share one evm block")
	assert.Equal(t, int64(1), configs[0].Evm.ChainId)
}

func TestRepositoryVendor_GenerateConfigs_ColdStartIsRetryable(t *testing.T) {
	v, settings, _ := repositoryFixture(t)
	logger := zerolog.Nop()

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)

	assert.Nil(t, configs)
	assert.ErrorIs(t, err, ErrRemoteCacheCold)
}

func TestRepositoryVendor_GenerateConfigs_AnUnknownChainOnAWarmCacheIsPermanent(t *testing.T) {
	v, settings, _ := repositoryFixture(t)
	logger := zerolog.Nop()
	warmRepository(t, v, settings)

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}}, settings)

	assert.Nil(t, configs)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRemoteCacheCold)
	assert.Contains(t, err.Error(), "424242")
}

func TestRepositoryVendor_GenerateConfigs_RejectsIncompleteInput(t *testing.T) {
	v, settings, _ := repositoryFixture(t)
	logger := zerolog.Nop()
	ctx := context.Background()

	_, errNoEvm := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, settings)
	require.Error(t, errNoEvm)
	assert.Contains(t, errNoEvm.Error(), "upstream.evm")

	_, errNoChain := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, settings)
	require.Error(t, errNoChain)
	assert.Contains(t, errNoChain.Error(), "chainId")
}

func TestRepositoryVendor_OwnsUpstream_ClaimsBySchemeOnly(t *testing.T) {
	v := CreateRepositoryVendor()

	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "repository://default"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "evm+repository://default"}))
	// Unlike most vendors, repository does not claim by VendorName.
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{VendorName: "repository"}))
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://one-a.example"}))
}

func TestRepositoryVendor_GetVendorSpecificErrorIfAny_NeverClassifies(t *testing.T) {
	v := CreateRepositoryVendor()
	jrr, err := common.NewJsonRpcResponse(1, nil,
		common.NewErrJsonRpcExceptionExternal(-32000, "anything at all", "d"))
	require.NoError(t, err)

	// Public endpoints have no shared error dialect, so the vendor stays out
	// of the way and lets the generic normaliser decide.
	assert.NoError(t, v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 500}, jrr, map[string]interface{}{}))
}
