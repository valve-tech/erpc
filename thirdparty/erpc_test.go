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
