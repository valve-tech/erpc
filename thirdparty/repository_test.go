package thirdparty

import (
	"context"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
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

func TestRepository_FetchRemoteData(t *testing.T) {
	ctx := context.Background()

	t.Run("parses numeric chain keys and skips others", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{
			"1": {"endpoints": ["https://eth.a", "https://eth.b"]},
			"137": {"endpoints": []},
			"name": {"endpoints": ["https://ignored"]}
		}`)
		got, err := fetchRemoteData(ctx, srv.URL)
		require.NoError(t, err)
		assert.Equal(t, map[int64][]string{
			1:   {"https://eth.a", "https://eth.b"},
			137: {},
		}, got)
	})

	t.Run("non-2xx status is an error", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusInternalServerError, `{}`)
		_, err := fetchRemoteData(ctx, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
	})

	t.Run("invalid json is an error", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `[`)
		_, err := fetchRemoteData(ctx, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse remote repository data")
	})

	t.Run("invalid url and unreachable host are errors", func(t *testing.T) {
		_, err := fetchRemoteData(ctx, "http://bad host/")
		assert.Error(t, err)
		_, err = fetchRemoteData(ctx, "http://127.0.0.1:1/")
		assert.Error(t, err)
	})
}

func TestRepositoryVendor_SupportsNetwork(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("rejects non-evm and malformed ids", func(t *testing.T) {
		v := CreateRepositoryVendor()
		ok, err := v.SupportsNetwork(ctx, &logger, nil, "solana:mainnet")
		assert.NoError(t, err)
		assert.False(t, ok)
		_, err = v.SupportsNetwork(ctx, &logger, nil, "evm:1x")
		assert.Error(t, err)
	})

	t.Run("cold cache then populated from repository url", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{"1":{"endpoints":["https://eth.a"]},"10":{"endpoints":[]}}`)
		v := CreateRepositoryVendor()
		settings := common.VendorSettings{"repositoryUrl": srv.URL, "recheckInterval": time.Hour}

		ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:1")
		assert.ErrorIs(t, err, ErrRemoteCacheCold)
		assert.False(t, ok)

		require.Eventually(t, func() bool {
			ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:1")
			return err == nil && ok
		}, 5*time.Second, 10*time.Millisecond)

		ok, err = v.SupportsNetwork(ctx, &logger, settings, "evm:10")
		assert.NoError(t, err)
		assert.False(t, ok, "chain with no endpoints is unsupported")
	})

	t.Run("default repository url is used when unset", func(t *testing.T) {
		v := CreateRepositoryVendor().(*RepositoryVendor)
		primeRemoteCache(t, v.cache, DefaultRepositoryURL, map[int64][]string{1: {"https://eth.a"}})
		ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:1")
		require.NoError(t, err)
		assert.True(t, ok)
	})
}

func TestRepositoryVendor_GenerateConfigs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	const repoUrl = "https://repo.example/endpoints"

	newPrimed := func(t *testing.T) *RepositoryVendor {
		v := CreateRepositoryVendor().(*RepositoryVendor)
		primeRemoteCache(t, v.cache, repoUrl, map[int64][]string{
			1:  {"https://eth.a/key", "wss://eth.ws", "HTTP://eth.b"},
			10: {},
		})
		return v
	}

	t.Run("generates one http upstream per endpoint with copied settings", func(t *testing.T) {
		v := newPrimed(t)
		budget := "public"
		ups := &common.UpstreamConfig{
			Id:                "repo",
			Tags:              []string{"public"},
			Evm:               &common.EvmUpstreamConfig{ChainId: 1},
			JsonRpc:           &common.JsonRpcUpstreamConfig{BatchMaxSize: 7},
			Failsafe:          []*common.FailsafeConfig{{MatchMethod: "eth_call"}},
			RateLimitAutoTune: &common.RateLimitAutoTuneConfig{},
			RateLimitBudget:   budget,
			IgnoreMethods:     []string{"debug_*"},
		}
		got, err := v.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"repositoryUrl": repoUrl, "recheckInterval": time.Hour})
		require.NoError(t, err)
		require.Len(t, got, 2, "non-http endpoints are skipped")

		assert.Equal(t, "https://eth.a/key", got[0].Endpoint)
		assert.Equal(t, "HTTP://eth.b", got[1].Endpoint)
		for _, c := range got {
			assert.Equal(t, common.UpstreamTypeEvm, c.Type)
			assert.Equal(t, "repo-"+util.RedactEndpoint(c.Endpoint), c.Id)
			assert.Equal(t, []string{"public"}, c.Tags)
			assert.Equal(t, budget, c.RateLimitBudget)
			assert.Equal(t, []string{"debug_*"}, c.IgnoreMethods)
			require.NotNil(t, c.AutoIgnoreUnsupportedMethods)
			assert.True(t, *c.AutoIgnoreUnsupportedMethods, "auto-ignore defaults to true")
			require.NotNil(t, c.Evm)
			assert.NotSame(t, ups.Evm, c.Evm, "evm config is deep-copied")
			assert.Equal(t, int64(1), c.Evm.ChainId)
			require.NotNil(t, c.JsonRpc)
			assert.NotSame(t, ups.JsonRpc, c.JsonRpc)
			assert.Equal(t, 7, c.JsonRpc.BatchMaxSize)
			require.Len(t, c.Failsafe, 1)
			assert.NotSame(t, ups.Failsafe[0], c.Failsafe[0])
			assert.Equal(t, "eth_call", c.Failsafe[0].MatchMethod)
			require.NotNil(t, c.RateLimitAutoTune)
			assert.NotSame(t, ups.RateLimitAutoTune, c.RateLimitAutoTune)
		}
	})

	t.Run("explicit auto-ignore false is kept", func(t *testing.T) {
		v := newPrimed(t)
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}, AutoIgnoreUnsupportedMethods: util.BoolPtr(false)}
		got, err := v.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"repositoryUrl": repoUrl})
		require.NoError(t, err)
		require.NotEmpty(t, got)
		assert.False(t, *got[0].AutoIgnoreUnsupportedMethods)
		assert.Nil(t, got[0].Tags)
		assert.Nil(t, got[0].Failsafe)
	})

	t.Run("validation errors", func(t *testing.T) {
		v := newPrimed(t)
		s := common.VendorSettings{"repositoryUrl": repoUrl}

		_, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, s)
		assert.ErrorContains(t, err, "requires upstream.evm to be defined")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, s)
		assert.ErrorContains(t, err, "requires upstream.evm.chainId")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 10}}, s)
		assert.ErrorContains(t, err, "chain ID 10 not found")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 99}}, s)
		assert.ErrorContains(t, err, "chain ID 99 not found")
	})

	t.Run("cold cache on default url is retryable", func(t *testing.T) {
		v := CreateRepositoryVendor().(*RepositoryVendor)
		// Mark a refresh for the default URL as in flight so nothing dials out.
		v.cache.inflight[DefaultRepositoryURL] = struct{}{}
		_, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{})
		assert.ErrorIs(t, err, ErrRemoteCacheCold)
	})
}

func TestRepositoryVendor_OwnershipAndErrors(t *testing.T) {
	v := CreateRepositoryVendor()
	assert.Equal(t, "repository", v.Name())
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "repository://evm-public-endpoints.erpc.cloud"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "evm+repository://x"}))
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://evm-public-endpoints.erpc.cloud"}))

	err, _ := vendorErr(t, v, "eth_call", 500, -32000, "boom", "")
	assert.NoError(t, err)
}
