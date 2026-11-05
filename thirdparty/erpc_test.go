package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErpcVendor_parseEndpointURL(t *testing.T) {
	v := &ErpcVendor{}
	chainId := int64(1)

	testCases := []struct {
		name           string
		endpoint       string
		secret         string
		expectedScheme string
		expectedHost   string
		expectedPath   string
		expectedQuery  string
	}{
		{
			name:           "erpc:// without port defaults to https",
			endpoint:       "erpc://domain.com",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "erpc:// with port 443 uses https",
			endpoint:       "erpc://domain.com:443",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com:443",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "erpc:// with port 80 uses http",
			endpoint:       "erpc://domain.com:80",
			secret:         "",
			expectedScheme: "http",
			expectedHost:   "domain.com:80",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "erpc:// with custom port uses http",
			endpoint:       "erpc://domain.com:8545",
			secret:         "",
			expectedScheme: "http",
			expectedHost:   "domain.com:8545",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "erpc:// with query params preserves them",
			endpoint:       "erpc://domain.com?param1=value1&param2=value2",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/1",
			expectedQuery:  "param1=value1&param2=value2",
		},
		{
			name:           "erpc:// with query params and secret adds secret",
			endpoint:       "erpc://domain.com?param1=value1",
			secret:         "mysecret",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/1",
			expectedQuery:  "param1=value1&secret=mysecret",
		},
		{
			name:           "erpc:// with port 443 and query params",
			endpoint:       "erpc://domain.com:443?param1=value1&param2=value2",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com:443",
			expectedPath:   "/1",
			expectedQuery:  "param1=value1&param2=value2",
		},
		{
			name:           "plain domain without port defaults to https",
			endpoint:       "domain.com",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "plain domain with port 80 uses http",
			endpoint:       "domain.com:80",
			secret:         "",
			expectedScheme: "http",
			expectedHost:   "domain.com:80",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "http:// URL preserves scheme",
			endpoint:       "http://domain.com:8080",
			secret:         "",
			expectedScheme: "http",
			expectedHost:   "domain.com:8080",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "https:// URL preserves scheme",
			endpoint:       "https://domain.com",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/1",
			expectedQuery:  "",
		},
		{
			name:           "https:// URL with existing path appends chainId",
			endpoint:       "https://domain.com/api/v1",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/api/v1/1",
			expectedQuery:  "",
		},
		{
			name:           "erpc:// with path and query params",
			endpoint:       "erpc://domain.com:8545/rpc?apikey=xyz&timeout=30",
			secret:         "",
			expectedScheme: "http",
			expectedHost:   "domain.com:8545",
			expectedPath:   "/rpc/1",
			expectedQuery:  "apikey=xyz&timeout=30",
		},
		{
			name:           "erpc:// with secret in URL and separate secret param",
			endpoint:       "erpc://domain.com?secret=url_secret",
			secret:         "param_secret",
			expectedScheme: "https",
			expectedHost:   "domain.com",
			expectedPath:   "/1",
			expectedQuery:  "secret=param_secret",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			parsedURL, err := v.parseEndpointURL(tc.endpoint, tc.secret, chainId)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedScheme, parsedURL.Scheme)
			assert.Equal(t, tc.expectedHost, parsedURL.Host)
			assert.Equal(t, tc.expectedPath, parsedURL.Path)
			assert.Equal(t, tc.expectedQuery, parsedURL.RawQuery)
		})
	}
}

// Seven vendors — dwellir, envio, erpc, goldsky, pimlico, routemesh and
// thirdweb — answer SupportsNetwork by asking the endpoint itself for its
// chain ID and comparing the answer. eRPC is the one whose endpoint comes from
// settings and can resolve to plain http, so it is the only one a local server
// can stand in for. The shape is what these tests check: a matching chain is a
// yes, a mismatched chain is a no rather than an error, and the probe client is
// built once per endpoint instead of once per call.
func erpcProbeServer(t *testing.T, chainIDHex string) (*httptest.Server, *atomic.Int32, *[]string) {
	t.Helper()
	var hits atomic.Int32
	paths := make([]string, 0, 4)
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"%s"}`, chainIDHex)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &paths
}

func TestErpcVendor_SupportsNetwork_AsksTheEndpointAndComparesTheChainId(t *testing.T) {
	srv, hits, paths := erpcProbeServer(t, "0x1")
	v := CreateErpcVendor().(*ErpcVendor)
	logger := zerolog.Nop()
	ctx := context.Background()
	settings := common.VendorSettings{"endpoint": strings.TrimPrefix(srv.URL, "http://")}

	supported, err := v.SupportsNetwork(ctx, &logger, settings, "evm:1")
	require.NoError(t, err)
	assert.True(t, supported)
	// The chain ID is a path segment, so one eRPC instance can serve many
	// networks. Dropping it would probe whatever the instance defaults to.
	require.NotEmpty(t, *paths)
	assert.Equal(t, "/1", (*paths)[0])

	// The endpoint reports chain 1, so chain 137 is a definite no. Reporting an
	// error instead would make the bootstrap loop retry a settled answer.
	supported, err = v.SupportsNetwork(ctx, &logger, settings, "evm:137")
	require.NoError(t, err)
	assert.False(t, supported)

	assert.Equal(t, int32(2), hits.Load(), "each network needs its own probe")
}

func TestErpcVendor_SupportsNetwork_ReusesOneProbeClientPerEndpointAndChain(t *testing.T) {
	srv, _, _ := erpcProbeServer(t, "0x1")
	v := CreateErpcVendor().(*ErpcVendor)
	logger := zerolog.Nop()
	ctx := context.Background()
	settings := common.VendorSettings{"endpoint": strings.TrimPrefix(srv.URL, "http://")}

	for i := 0; i < 3; i++ {
		supported, err := v.SupportsNetwork(ctx, &logger, settings, "evm:1")
		require.NoError(t, err)
		require.True(t, supported)
	}

	clients := 0
	v.headlessClients.Range(func(_, _ any) bool { clients++; return true })
	assert.Equal(t, 1, clients,
		"a new client per call would leak one connection pool per bootstrap probe")
}

func TestErpcVendor_SupportsNetwork_SkipsTheProbeWithoutAnEndpointOrANetworkItHandles(t *testing.T) {
	srv, hits, _ := erpcProbeServer(t, "0x1")
	v := CreateErpcVendor().(*ErpcVendor)
	logger := zerolog.Nop()
	ctx := context.Background()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	supported, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:1")
	require.NoError(t, err, "no endpoint means the operator did not configure erpc, not that it failed")
	assert.False(t, supported)

	supported, err = v.SupportsNetwork(ctx, &logger,
		common.VendorSettings{"endpoint": endpoint}, "solana:mainnet")
	require.NoError(t, err)
	assert.False(t, supported)

	supported, err = v.SupportsNetwork(ctx, &logger,
		common.VendorSettings{"endpoint": endpoint}, "evm:0xdead")
	require.Error(t, err)
	assert.False(t, supported)

	assert.Equal(t, int32(0), hits.Load(),
		"none of these three answers needs the endpoint, so none may probe it")
}

// The secret travels as a query parameter. An operator who sets it in settings
// and an operator who puts it in the endpoint URL must both reach an
// authenticated eRPC.
func TestErpcVendor_GenerateConfigs_CarriesTheSecretAndTheChainIdIntoTheEndpoint(t *testing.T) {
	v := CreateErpcVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	fromSettings, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 137}},
		common.VendorSettings{"endpoint": "erpc.internal:8545", "secret": "s3cret"})
	require.NoError(t, err)
	require.Len(t, fromSettings, 1)
	assert.Equal(t, "http://erpc.internal:8545/137?secret=s3cret", fromSettings[0].Endpoint)
	assert.Equal(t, common.UpstreamTypeEvm, fromSettings[0].Type)

	// A port of 443, or none at all, means TLS.
	tls, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}},
		common.VendorSettings{"endpoint": "erpc.example"})
	require.NoError(t, err)
	assert.Equal(t, "https://erpc.example/1", tls[0].Endpoint)

	preset, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{
			Endpoint: "evm+erpc://erpc.internal:8545?secret=inline",
			Evm:      &common.EvmUpstreamConfig{ChainId: 10},
		}, common.VendorSettings{})
	require.NoError(t, err)
	assert.Equal(t, "http://erpc.internal:8545/10?secret=inline", preset[0].Endpoint,
		"a secret already in the endpoint survives the rewrite")

	// A plain https endpoint is not an erpc:// URL, so it passes through whole.
	plain, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Endpoint: "https://erpc.example/1", Evm: &common.EvmUpstreamConfig{ChainId: 1}},
		common.VendorSettings{})
	require.NoError(t, err)
	assert.Equal(t, "https://erpc.example/1", plain[0].Endpoint)

	_, errNoEndpoint := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{})
	require.Error(t, errNoEndpoint)
	assert.Contains(t, errNoEndpoint.Error(), "endpoint")
}

func TestErpcVendor_SupportsNetwork(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("matching chain id", func(t *testing.T) {
		srv := newChainIdServer(t, "0x1")
		ok, err := CreateErpcVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"endpoint": srv.URL}, "evm:1")
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("cached client is reused across calls", func(t *testing.T) {
		srv := newChainIdServer(t, "0x1")
		v := CreateErpcVendor()
		settings := common.VendorSettings{"endpoint": srv.URL, "secret": "s"}
		for range 2 {
			ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:1")
			require.NoError(t, err)
			assert.True(t, ok)
		}
	})

	t.Run("mismatching chain id", func(t *testing.T) {
		srv := newChainIdServer(t, "0x89")
		ok, err := CreateErpcVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"endpoint": srv.URL}, "evm:1")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("json-rpc error response", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}`)
		ok, err := CreateErpcVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"endpoint": srv.URL}, "evm:1")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("non-hex result", func(t *testing.T) {
		srv := newChainIdServer(t, "not-a-number")
		ok, err := CreateErpcVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"endpoint": srv.URL}, "evm:1")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("non-string result", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"a":1}}`)
		ok, err := CreateErpcVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"endpoint": srv.URL}, "evm:1")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("server down", func(t *testing.T) {
		srv := newChainIdServer(t, "0x1")
		url := srv.URL
		srv.Close()
		ok, err := CreateErpcVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"endpoint": url}, "evm:1")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("non-evm network", func(t *testing.T) {
		ok, err := CreateErpcVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"endpoint": "http://127.0.0.1:1"}, "solana:mainnet")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("unparsable chain id", func(t *testing.T) {
		ok, err := CreateErpcVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"endpoint": "http://127.0.0.1:1"}, "evm:abc")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("missing endpoint skips probe", func(t *testing.T) {
		ok, err := CreateErpcVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:1")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("malformed endpoint", func(t *testing.T) {
		ok, err := CreateErpcVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"endpoint": "http://[::1"}, "evm:1")
		assert.Error(t, err)
		assert.False(t, ok)
	})
}

func TestErpcVendor_GenerateConfigs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	cases := []struct {
		name     string
		endpoint string
		settings common.VendorSettings
		expected string
	}{
		{"settings endpoint with secret", "", common.VendorSettings{"endpoint": "erpc://edge.example.com", "secret": "s1"}, "https://edge.example.com/42161?secret=s1"},
		{"settings endpoint custom port", "", common.VendorSettings{"endpoint": "edge.example.com:8545"}, "http://edge.example.com:8545/42161"},
		{"preset erpc:// with port 443", "erpc://edge.example.com:443/main?x=1", nil, "https://edge.example.com:443/main/42161?x=1"},
		{"preset evm+erpc:// with custom port", "evm+erpc://edge.example.com:4000", nil, "http://edge.example.com:4000/42161"},
		{"preset https endpoint passes through", "https://rpc.example.com/v1", nil, "https://rpc.example.com/v1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ups := &common.UpstreamConfig{Endpoint: c.endpoint, Evm: &common.EvmUpstreamConfig{ChainId: 42161}}
			cfgs, err := CreateErpcVendor().GenerateConfigs(ctx, &logger, ups, c.settings)
			require.NoError(t, err)
			require.Len(t, cfgs, 1)
			assert.Equal(t, c.expected, cfgs[0].Endpoint)
			assert.Equal(t, common.UpstreamTypeEvm, cfgs[0].Type)
			assert.NotNil(t, cfgs[0].JsonRpc)
		})
	}

	t.Run("missing endpoint setting", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, err := CreateErpcVendor().GenerateConfigs(ctx, &logger, ups, common.VendorSettings{})
		assert.ErrorContains(t, err, "endpoint is required in erpc settings")
	})

	t.Run("malformed settings endpoint", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, err := CreateErpcVendor().GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"endpoint": "https://[::1"})
		assert.Error(t, err)
	})

	t.Run("malformed preset erpc:// endpoint", func(t *testing.T) {
		ups := &common.UpstreamConfig{Endpoint: "erpc://[::1", Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, err := CreateErpcVendor().GenerateConfigs(ctx, &logger, ups, nil)
		assert.Error(t, err)
	})
}

func TestErpcVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	v := CreateErpcVendor()

	err, details := vendorErr(t, v, "eth_getLogs", 500, -32000, "boom", "extra-info")
	assert.NoError(t, err)
	assert.Equal(t, "extra-info", details["data"])

	err, details = vendorErr(t, v, "eth_getLogs", 500, -32000, "boom", "")
	assert.NoError(t, err)
	assert.NotContains(t, details, "data")

	assert.NoError(t, v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 500}, map[string]interface{}{}, map[string]interface{}{}))
}

func TestErpcVendor_OwnsUpstream(t *testing.T) {
	v := CreateErpcVendor()
	assert.Equal(t, "erpc", v.Name())
	cases := []struct {
		ups   *common.UpstreamConfig
		owned bool
	}{
		{&common.UpstreamConfig{Endpoint: "erpc://a.example.com"}, true},
		{&common.UpstreamConfig{Endpoint: "evm+erpc://a.example.com"}, true},
		{&common.UpstreamConfig{Endpoint: "https://x.example.com", VendorName: "erpc"}, true},
		{&common.UpstreamConfig{Endpoint: "https://edge.erpc.cloud/1"}, true},
		{&common.UpstreamConfig{Endpoint: "https://rpc.example.com"}, false},
	}
	for _, c := range cases {
		assert.Equal(t, c.owned, v.OwnsUpstream(c.ups), c.ups.Endpoint)
	}
}
