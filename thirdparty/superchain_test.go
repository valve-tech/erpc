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

// parseSuperchainSpec turns whatever an operator pastes into a raw-content
// URL. It is the only place a typo in the config becomes a wrong fetch, so
// each accepted shape is pinned here.
func TestParseSuperchainSpec_ResolvesEveryAcceptedShape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"shorthand org/repo defaults to main and chainList.json",
			"github.com/ethereum-optimism/superchain-registry",
			"https://raw.githubusercontent.com/ethereum-optimism/superchain-registry/main/chainList.json",
		},
		{
			"https github URL takes the same defaults",
			"https://github.com/org/repo",
			"https://raw.githubusercontent.com/org/repo/main/chainList.json",
		},
		{
			"http github URL takes the same defaults",
			"http://github.com/org/repo",
			"https://raw.githubusercontent.com/org/repo/main/chainList.json",
		},
		{
			"a third segment without .json is a branch",
			"github.com/org/repo/my-branch",
			"https://raw.githubusercontent.com/org/repo/my-branch/chainList.json",
		},
		{
			"branch plus file",
			"github.com/org/repo/my-branch/custom.json",
			"https://raw.githubusercontent.com/org/repo/my-branch/custom.json",
		},
		{
			"branch plus nested file",
			"github.com/org/repo/main/dir/custom.json",
			"https://raw.githubusercontent.com/org/repo/main/dir/custom.json",
		},
		{
			"a third segment ending in .json is a file on main",
			"github.com/org/repo/custom.json",
			"https://raw.githubusercontent.com/org/repo/main/custom.json",
		},
		{
			"a blob segment copied from the GitHub UI is dropped",
			"https://github.com/org/repo/blob/main/chainList.json",
			"https://raw.githubusercontent.com/org/repo/main/chainList.json",
		},
		{
			"a tree segment copied from the GitHub UI is dropped",
			"github.com/org/repo/tree/dev/chainList.json",
			"https://raw.githubusercontent.com/org/repo/dev/chainList.json",
		},
		{
			"a trailing slash does not create an empty segment",
			"github.com/org/repo/",
			"https://raw.githubusercontent.com/org/repo/main/chainList.json",
		},
		{
			"a raw URL passes through untouched",
			"https://raw.githubusercontent.com/org/repo/main/chainList.json",
			"https://raw.githubusercontent.com/org/repo/main/chainList.json",
		},
		{
			"any other full URL passes through untouched",
			"https://mysuperchain.example/chainList.json",
			"https://mysuperchain.example/chainList.json",
		},
		{
			"a bare host gains an https scheme",
			"mysuperchain.example/chainList.json",
			"https://mysuperchain.example/chainList.json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSuperchainSpec(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseSuperchainSpec_RejectsAGithubSpecWithoutARepo(t *testing.T) {
	got, err := parseSuperchainSpec("github.com/onlyorg")

	require.Error(t, err, "org without repo cannot name a file")
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), "org/repo not found")
}

func superchainFixture(t *testing.T) (*SuperchainVendor, common.VendorSettings, *atomic.Int32) {
	t.Helper()
	srv, hits := jsonServer(t, http.StatusOK, `[
		{"chainId":8453,"rpc":["https://base-a.example","https://base-b.example"]},
		{"chainId":10,"rpc":["https://opt.example"]}
	]`)
	v := CreateSuperchainVendor().(*SuperchainVendor)
	settings := common.VendorSettings{
		"registryUrl":     srv.URL,
		"recheckInterval": time.Hour,
	}
	return v, settings, hits
}

func warmSuperchain(t *testing.T, v *SuperchainVendor, settings common.VendorSettings) {
	t.Helper()
	logger := zerolog.Nop()
	require.Eventually(t, func() bool {
		ok, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:8453")
		return err == nil && ok
	}, fetcherTestWait, 10*time.Millisecond, "the async refresh never populated the superchain cache")
}

func TestSuperchainVendor_SupportsNetwork_ColdStartIsRetryable(t *testing.T) {
	v, settings, _ := superchainFixture(t)
	logger := zerolog.Nop()

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:8453")

	assert.False(t, supported)
	assert.ErrorIs(t, err, ErrRemoteCacheCold)
}

func TestSuperchainVendor_SupportsNetwork_AnswersFromTheRefreshedSnapshot(t *testing.T) {
	v, settings, hits := superchainFixture(t)
	logger := zerolog.Nop()
	warmSuperchain(t, v, settings)

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:10")
	require.NoError(t, err)
	assert.True(t, supported)

	unknown, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:424242")
	require.NoError(t, err)
	assert.False(t, unknown)

	assert.Equal(t, int32(1), hits.Load(), "a fresh snapshot must not re-hit the registry")
}

func TestSuperchainVendor_SupportsNetwork_RejectsAnUnparseableRegistrySpec(t *testing.T) {
	v := CreateSuperchainVendor().(*SuperchainVendor)
	logger := zerolog.Nop()
	settings := common.VendorSettings{"registryUrl": "github.com/onlyorg"}

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:8453")

	require.Error(t, err)
	assert.False(t, supported)
	assert.NotErrorIs(t, err, ErrRemoteCacheCold, "a bad config must not be reported as a transient cold cache")
	assert.Contains(t, err.Error(), "failed to parse superchain registry URL")
}

func TestSuperchainVendor_SupportsNetwork_IgnoresNonEvmNetworks(t *testing.T) {
	v, settings, hits := superchainFixture(t)
	logger := zerolog.Nop()

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "btc:mainnet")

	require.NoError(t, err)
	assert.False(t, supported)
	assert.Equal(t, int32(0), hits.Load())
}

func TestSuperchainVendor_GenerateConfigs_MakesOneUpstreamPerRpcWithUniqueIds(t *testing.T) {
	v, settings, _ := superchainFixture(t)
	logger := zerolog.Nop()
	warmSuperchain(t, v, settings)

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Id: "sc", Evm: &common.EvmUpstreamConfig{ChainId: 8453}}, settings)

	require.NoError(t, err)
	require.Len(t, configs, 2)
	assert.Equal(t, "https://base-a.example", configs[0].Endpoint)
	assert.Equal(t, "https://base-b.example", configs[1].Endpoint)
	assert.Equal(t, "sc-0", configs[0].Id)
	assert.Equal(t, "sc-1", configs[1].Id)
	assert.Equal(t, "superchain", configs[0].VendorName)
}

func TestSuperchainVendor_GenerateConfigs_SynthesisesIdsWhenTheUpstreamHasNone(t *testing.T) {
	v, settings, _ := superchainFixture(t)
	logger := zerolog.Nop()
	warmSuperchain(t, v, settings)

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 8453}}, settings)

	require.NoError(t, err)
	require.Len(t, configs, 2)
	assert.Equal(t, "superchain-8453-0", configs[0].Id)
	assert.Equal(t, "superchain-8453-1", configs[1].Id)
}

func TestSuperchainVendor_GenerateConfigs_ASuperchainSchemeEndpointBecomesTheRegistryUrl(t *testing.T) {
	srv, hits := jsonServer(t, http.StatusOK, `[{"chainId":8453,"rpc":["https://base.example"]}]`)
	v := CreateSuperchainVendor().(*SuperchainVendor)
	logger := zerolog.Nop()
	settings := common.VendorSettings{"recheckInterval": time.Hour}

	upstream := &common.UpstreamConfig{
		Endpoint: "superchain://" + srv.URL,
		Evm:      &common.EvmUpstreamConfig{ChainId: 8453},
	}
	// The first call clears the endpoint, rewrites settings and starts the
	// refresh, so it must report a cold cache rather than pass the scheme
	// endpoint through as a live RPC URL.
	configs, err := v.GenerateConfigs(context.Background(), &logger, upstream, settings)
	assert.Nil(t, configs)
	require.ErrorIs(t, err, ErrRemoteCacheCold)
	assert.Empty(t, upstream.Endpoint, "the scheme endpoint must be cleared, not routed to")
	assert.Equal(t, srv.URL, settings["registryUrl"], "the scheme payload becomes the registry URL")

	require.Eventually(t, func() bool { return hits.Load() > 0 }, fetcherTestWait, 10*time.Millisecond,
		"the scheme endpoint never triggered a registry fetch")
	require.Eventually(t, func() bool {
		cfgs, err := v.GenerateConfigs(context.Background(), &logger,
			&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 8453}}, settings)
		return err == nil && len(cfgs) == 1
	}, fetcherTestWait, 10*time.Millisecond)
}

func TestSuperchainVendor_GenerateConfigs_APlainEndpointPassesThroughUntouched(t *testing.T) {
	v, settings, hits := superchainFixture(t)
	logger := zerolog.Nop()

	upstream := &common.UpstreamConfig{Endpoint: "https://my.own.node/rpc"}
	configs, err := v.GenerateConfigs(context.Background(), &logger, upstream, settings)

	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Equal(t, "https://my.own.node/rpc", configs[0].Endpoint)
	assert.Equal(t, int32(0), hits.Load(), "an explicit endpoint must not trigger a registry fetch")
}

func TestSuperchainVendor_GenerateConfigs_ColdStartIsRetryable(t *testing.T) {
	v, settings, _ := superchainFixture(t)
	logger := zerolog.Nop()

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 8453}}, settings)

	assert.Nil(t, configs)
	assert.ErrorIs(t, err, ErrRemoteCacheCold)
}

func TestSuperchainVendor_GenerateConfigs_AnUnknownChainOnAWarmCacheIsPermanent(t *testing.T) {
	v, settings, _ := superchainFixture(t)
	logger := zerolog.Nop()
	warmSuperchain(t, v, settings)

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}}, settings)

	assert.Nil(t, configs)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRemoteCacheCold)
	assert.Contains(t, err.Error(), "424242")
}

func TestSuperchainVendor_GenerateConfigs_RejectsIncompleteInput(t *testing.T) {
	v, settings, _ := superchainFixture(t)
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

func TestSuperchainVendor_OwnsUpstream(t *testing.T) {
	v := CreateSuperchainVendor()

	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "superchain://github.com/org/repo"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "evm+superchain://github.com/org/repo"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{VendorName: "superchain"}))
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://base-a.example"}))
}

func TestSuperchainVendor_GetVendorSpecificErrorIfAny_NeverClassifies(t *testing.T) {
	v := CreateSuperchainVendor()
	jrr, err := common.NewJsonRpcResponse(1, nil,
		common.NewErrJsonRpcExceptionExternal(-32000, "anything at all", "d"))
	require.NoError(t, err)

	// The registry only supplies URLs; the endpoints behind them belong to
	// many operators with no shared error dialect.
	assert.NoError(t, v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 500}, jrr, map[string]interface{}{}))
}
