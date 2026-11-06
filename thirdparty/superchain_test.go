package thirdparty

import (
	"context"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
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

// The fetcher drops entries with no RPC, but SupportsNetwork guards again in
// case the registry shape changes. Publishing straight into the cache is the
// only way to reach that guard.
func TestSuperchainVendor_SupportsNetwork_AChainWithNoRpcIsUnsupported(t *testing.T) {
	v := CreateSuperchainVendor().(*SuperchainVendor)
	logger := zerolog.Nop()
	settings := common.VendorSettings{"registryUrl": "https://cache.only/list.json", "recheckInterval": time.Hour}
	publishAndWait(t, v.cache, "https://cache.only/list.json", map[int64][]string{
		10:  {"https://opt.example"},
		999: {},
	})

	usable, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:10")
	require.NoError(t, err)
	assert.True(t, usable)

	empty, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:999")
	require.NoError(t, err)
	assert.False(t, empty, "a chain listed with no RPC must never be reported as supported")
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

func TestParseSuperchainSpec(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want string
	}{
		{"shorthand org/repo defaults branch and file", "github.com/acme/registry", "https://raw.githubusercontent.com/acme/registry/main/chainList.json"},
		{"https github org/repo", "https://github.com/acme/registry", "https://raw.githubusercontent.com/acme/registry/main/chainList.json"},
		{"http github org/repo", "http://github.com/acme/registry/", "https://raw.githubusercontent.com/acme/registry/main/chainList.json"},
		{"branch only", "github.com/acme/registry/dev", "https://raw.githubusercontent.com/acme/registry/dev/chainList.json"},
		{"branch and file", "github.com/acme/registry/dev/custom.json", "https://raw.githubusercontent.com/acme/registry/dev/custom.json"},
		{"branch and nested file", "github.com/acme/registry/dev/lists/custom.json", "https://raw.githubusercontent.com/acme/registry/dev/lists/custom.json"},
		{"file without branch implies main", "github.com/acme/registry/custom.json", "https://raw.githubusercontent.com/acme/registry/main/custom.json"},
		{"blob UI url drops blob segment", "https://github.com/acme/registry/blob/release/chainList.json", "https://raw.githubusercontent.com/acme/registry/release/chainList.json"},
		{"tree UI url drops tree segment", "github.com/acme/registry/tree/release", "https://raw.githubusercontent.com/acme/registry/release/chainList.json"},
		{"raw url kept verbatim", "https://raw.githubusercontent.com/a/b/main/x.json", "https://raw.githubusercontent.com/a/b/main/x.json"},
		{"http custom url kept verbatim", "http://registry.local/chains.json", "http://registry.local/chains.json"},
		{"bare host gets https", "mysuperchain.com/chainList.json", "https://mysuperchain.com/chainList.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseSuperchainSpec(c.spec)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}

	t.Run("github prefix without repo is rejected", func(t *testing.T) {
		_, err := parseSuperchainSpec("github.com/acme")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "org/repo not found")
	})
}

const superchainRegistryBody = `[
	{"chainId": 10, "rpc": ["https://mainnet.optimism.io", "https://op.backup.io"]},
	{"chainId": 8453, "rpc": ["https://mainnet.base.org"]},
	{"chainId": 999, "rpc": []},
	{"chainId": 0, "rpc": ["https://ignored.example"]}
]`

func TestSuperchainVendor_FetchNetworks(t *testing.T) {
	v := CreateSuperchainVendor().(*SuperchainVendor)
	ctx := context.Background()

	t.Run("keeps chains with positive id and at least one rpc", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, superchainRegistryBody)
		got, err := v.fetchSuperchainNetworks(ctx, srv.URL)
		require.NoError(t, err)
		assert.Equal(t, map[int64][]string{
			10:   {"https://mainnet.optimism.io", "https://op.backup.io"},
			8453: {"https://mainnet.base.org"},
		}, got)
	})

	t.Run("non-2xx status is an error", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusNotFound, `not found`)
		_, err := v.fetchSuperchainNetworks(ctx, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "404")
	})

	t.Run("invalid json is an error", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{"not":"a list"}`)
		_, err := v.fetchSuperchainNetworks(ctx, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse Superchain registry data")
	})

	t.Run("invalid url is an error", func(t *testing.T) {
		_, err := v.fetchSuperchainNetworks(ctx, "http://bad host/")
		require.Error(t, err)
	})

	t.Run("unreachable server is an error", func(t *testing.T) {
		_, err := v.fetchSuperchainNetworks(ctx, "http://127.0.0.1:1/chainList.json")
		require.Error(t, err)
	})
}

func TestSuperchainVendor_SupportsNetwork(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("rejects non-evm and malformed ids", func(t *testing.T) {
		v := CreateSuperchainVendor()
		ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "solana:mainnet")
		assert.NoError(t, err)
		assert.False(t, ok)

		ok, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:abc")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("invalid registry spec is an error", func(t *testing.T) {
		v := CreateSuperchainVendor()
		ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{"registryUrl": "github.com/only-org"}, "evm:10")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse superchain registry URL")
		assert.False(t, ok)
	})

	t.Run("cold cache then populated from registry", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, superchainRegistryBody)
		v := CreateSuperchainVendor()
		settings := common.VendorSettings{"registryUrl": srv.URL, "recheckInterval": time.Hour}

		ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:10")
		assert.ErrorIs(t, err, ErrRemoteCacheCold)
		assert.False(t, ok)

		require.Eventually(t, func() bool {
			ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:10")
			return err == nil && ok
		}, 5*time.Second, 10*time.Millisecond)

		ok, err = v.SupportsNetwork(ctx, &logger, settings, "evm:999")
		assert.NoError(t, err)
		assert.False(t, ok, "chain without rpc endpoints is unsupported")

		ok, err = v.SupportsNetwork(ctx, &logger, settings, "evm:12345")
		assert.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("default registry url is used when unset", func(t *testing.T) {
		v := CreateSuperchainVendor().(*SuperchainVendor)
		primeRemoteCache(t, v.cache, DefaultSuperchainRegistryURL, map[int64][]string{10: {"https://op.example"}})
		ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:10")
		require.NoError(t, err)
		assert.True(t, ok)
	})
}

func TestSuperchainVendor_GenerateConfigs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	const registry = "https://registry.example/chainList.json"
	networks := map[int64][]string{10: {"https://rpc-a.example", "https://rpc-b.example"}, 77: {}}

	newPrimed := func(t *testing.T) *SuperchainVendor {
		v := CreateSuperchainVendor().(*SuperchainVendor)
		primeRemoteCache(t, v.cache, registry, networks)
		return v
	}

	t.Run("generates one upstream per rpc with indexed ids", func(t *testing.T) {
		v := newPrimed(t)
		ups := &common.UpstreamConfig{Id: "op", Evm: &common.EvmUpstreamConfig{ChainId: 10}}
		got, err := v.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"registryUrl": registry})
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "op-0", got[0].Id)
		assert.Equal(t, "https://rpc-a.example", got[0].Endpoint)
		assert.Equal(t, "op-1", got[1].Id)
		assert.Equal(t, "https://rpc-b.example", got[1].Endpoint)
		for _, c := range got {
			assert.Equal(t, common.UpstreamTypeEvm, c.Type)
			assert.Equal(t, "superchain", c.VendorName)
			assert.NotNil(t, c.JsonRpc)
		}
	})

	t.Run("ids are derived from chain id when upstream id is empty", func(t *testing.T) {
		v := newPrimed(t)
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 10}}
		got, err := v.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"registryUrl": registry, "recheckInterval": time.Hour})
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "superchain-10-0", got[0].Id)
		assert.Equal(t, "superchain-10-1", got[1].Id)
	})

	t.Run("superchain:// endpoint selects the registry", func(t *testing.T) {
		v := CreateSuperchainVendor().(*SuperchainVendor)
		raw := "https://raw.githubusercontent.com/acme/registry/main/chainList.json"
		primeRemoteCache(t, v.cache, raw, networks)
		for _, ep := range []string{"superchain://github.com/acme/registry", "evm+superchain://github.com/acme/registry"} {
			ups := &common.UpstreamConfig{Endpoint: ep, Evm: &common.EvmUpstreamConfig{ChainId: 10}}
			got, err := v.GenerateConfigs(ctx, &logger, ups, nil)
			require.NoError(t, err, ep)
			require.Len(t, got, 2, ep)
			assert.Equal(t, "https://rpc-a.example", got[0].Endpoint)
		}
	})

	t.Run("superchain:// endpoint with bad spec is an error", func(t *testing.T) {
		v := CreateSuperchainVendor()
		ups := &common.UpstreamConfig{Endpoint: "superchain://github.com/only-org", Evm: &common.EvmUpstreamConfig{ChainId: 10}}
		_, err := v.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{})
		require.Error(t, err)
	})

	t.Run("plain endpoint passes through", func(t *testing.T) {
		v := CreateSuperchainVendor()
		ups := &common.UpstreamConfig{Endpoint: "https://custom.example", Evm: &common.EvmUpstreamConfig{ChainId: 10}}
		got, err := v.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Same(t, ups, got[0])
		assert.NotNil(t, got[0].JsonRpc)
	})

	t.Run("validation errors", func(t *testing.T) {
		v := newPrimed(t)
		settings := common.VendorSettings{"registryUrl": registry}

		_, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, settings)
		assert.ErrorContains(t, err, "requires upstream.evm")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, settings)
		assert.ErrorContains(t, err, "requires upstream.evm.chainId")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 10}}, common.VendorSettings{"registryUrl": "github.com/x"})
		assert.ErrorContains(t, err, "failed to parse superchain registry URL")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 77}}, settings)
		assert.ErrorContains(t, err, "chain ID 77 not found")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 5}}, settings)
		assert.ErrorContains(t, err, "chain ID 5 not found")
	})

	t.Run("cold cache is retryable", func(t *testing.T) {
		v := CreateSuperchainVendor()
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 10}}
		_, err := v.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"registryUrl": "http://127.0.0.1:1/never.json"})
		assert.ErrorIs(t, err, ErrRemoteCacheCold)
	})
}

func TestSuperchainVendor_OwnershipAndErrors(t *testing.T) {
	v := CreateSuperchainVendor()
	assert.Equal(t, "superchain", v.Name())
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "superchain://github.com/a/b"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "evm+superchain://github.com/a/b"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://rpc.example", VendorName: "superchain"}))
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://mainnet.optimism.io"}))

	err, _ := vendorErr(t, v, "eth_call", 500, -32000, "boom", "")
	assert.NoError(t, err, "superchain defers all errors to generic handling")
}
