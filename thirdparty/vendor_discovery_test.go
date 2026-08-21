package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tenderly, QuickNode and Chainstack discover their upstreams over HTTP at
// bootstrap. Each fetcher meets the same four hostile answers — a non-2xx, a
// body that is not the JSON it expects, a body that stops early, and a server
// that never answers — and each one must report rather than publish a partial
// result. A partial publish is the dangerous case: the cache then reads fresh
// and the missing upstreams stay missing for the life of the process.
//
// The second axis is the cache. Every vendor here reads through
// RemoteDataCache, so a warm, fresh snapshot must answer without touching the
// network. These tests count the server hits to prove it.

// truncatedBodyServer sends prefix, then drops the connection mid-body. The
// caller chooses the prefix so it is valid JSON for the fetcher under test up
// to the cut.
func truncatedBodyServer(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(prefix))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	srv.Config.ErrorLog = quietLogger()
	t.Cleanup(srv.Close)
	return srv
}

// waitForRefreshes blocks until the cache has no async refresh in flight. A
// refresh goroutine reads the package-level endpoint URL, so a test that
// restores that URL while one is still running races with it. The cache
// deletes the in-flight marker under refreshMu after the fetcher returns, so
// an empty map orders every read the goroutine made before the restore.
func waitForRefreshes[T any](t *testing.T, c *RemoteDataCache[T]) {
	t.Helper()
	require.Eventually(t, func() bool {
		c.refreshMu.Lock()
		defer c.refreshMu.Unlock()
		return len(c.inflight) == 0
	}, fetcherTestWait, 5*time.Millisecond, "an async refresh outlived its test")
}

// pointTenderlyAt redirects the package-level endpoint for one test.
func pointTenderlyAt(t *testing.T, url string) {
	t.Helper()
	prev := tenderlyApiUrl
	tenderlyApiUrl = url
	t.Cleanup(func() { tenderlyApiUrl = prev })
}

// pointQuicknodeAt redirects the package-level endpoint for one test.
func pointQuicknodeAt(t *testing.T, url string) {
	t.Helper()
	prev := quicknodeEndpointsApiUrl
	quicknodeEndpointsApiUrl = url
	t.Cleanup(func() { quicknodeEndpointsApiUrl = prev })
}

// pointChainstackAt redirects the package-level endpoint for one test.
func pointChainstackAt(t *testing.T, url string) {
	t.Helper()
	prev := chainstackNodesApiUrl
	chainstackNodesApiUrl = url
	t.Cleanup(func() { chainstackNodesApiUrl = prev })
}

// -----------------------------------------------------------------------------
// tenderly: fetchTenderlyNetworks
// -----------------------------------------------------------------------------

// Tenderly publishes three slugs per network and the fetcher picks one. The
// order matters: node_rpc_slug is the RPC gateway, the other two are
// fallbacks. Picking the wrong one builds a gateway URL that does not resolve.
func TestFetchTenderlyNetworks_PicksTheNodeRpcSlugFirstAndFallsBackInOrder(t *testing.T) {
	srv, hits := jsonServer(t, http.StatusOK, `[
		{"chain_id":"1","network_slugs":{"explorer_slug":"e1","node_rpc_slug":"n1","vnet_rpc_slug":"v1"}},
		{"chain_id":"10","network_slugs":{"explorer_slug":"e10","vnet_rpc_slug":"v10"}},
		{"chain_id":"137","network_slugs":{"explorer_slug":"e137"}},
		{"chain_id":"8453","network_slugs":{}},
		{"chain_id":"","network_slugs":{"node_rpc_slug":"nothing"}},
		{"chain_id":"not-a-number","network_slugs":{"node_rpc_slug":"nothing"}}
	]`)
	pointTenderlyAt(t, srv.URL)

	v := CreateTenderlyVendor().(*TenderlyVendor)
	networks, err := v.fetchTenderlyNetworks(context.Background())

	require.NoError(t, err)
	assert.Equal(t, map[int64]string{1: "n1", 10: "v10", 137: "e137"}, networks,
		"node_rpc beats vnet beats explorer; an entry with no slug, no chain ID or a non-numeric chain ID is dropped")
	assert.Equal(t, int32(1), hits.Load())
}

func TestFetchTenderlyNetworks_ReportsTheStatusCodeOnANon2xx(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusServiceUnavailable, `[]`)
	pointTenderlyAt(t, srv.URL)

	v := CreateTenderlyVendor().(*TenderlyVendor)
	networks, err := v.fetchTenderlyNetworks(context.Background())

	require.Error(t, err)
	assert.Nil(t, networks)
	assert.Contains(t, err.Error(), "503", "the operator needs the code to tell a rate limit from an outage")
}

func TestFetchTenderlyNetworks_AJsonObjectWhereAListIsExpectedIsAParseFailure(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusOK, `{"networks":[]}`)
	pointTenderlyAt(t, srv.URL)

	v := CreateTenderlyVendor().(*TenderlyVendor)
	networks, err := v.fetchTenderlyNetworks(context.Background())

	require.Error(t, err)
	assert.Nil(t, networks)
	assert.Contains(t, err.Error(), "failed to parse Tenderly API data")
}

// A body that stops early must not publish the entries that already decoded.
// A short map reads as fresh and silently strands every network past the cut.
func TestFetchTenderlyNetworks_ATruncatedBodyIsAnErrorNotAPartialMap(t *testing.T) {
	srv := truncatedBodyServer(t, `[{"chain_id":"1","network_slugs":{"node_rpc_slug":"mainnet"}},{"chain_id":"10","network_sl`)
	pointTenderlyAt(t, srv.URL)

	v := CreateTenderlyVendor().(*TenderlyVendor)
	networks, err := v.fetchTenderlyNetworks(context.Background())

	require.Error(t, err)
	assert.Nil(t, networks, "chain 1 decoded cleanly, but publishing it alone hides the loss of chain 10")
}

func TestFetchTenderlyNetworks_HonoursTheCallersDeadline(t *testing.T) {
	srv := hangingServer(t)
	pointTenderlyAt(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	v := CreateTenderlyVendor().(*TenderlyVendor)
	start := time.Now()
	networks, err := v.fetchTenderlyNetworks(ctx)

	require.Error(t, err)
	assert.Nil(t, networks)
	assert.Less(t, time.Since(start), fetcherTestWait,
		"the caller's deadline must win over the fetcher's own 10s budget")
}

// -----------------------------------------------------------------------------
// tenderly: the vendor over the cache
// -----------------------------------------------------------------------------

func tenderlyFixture(t *testing.T) (*TenderlyVendor, common.VendorSettings, *atomic.Int32) {
	t.Helper()
	srv, hits := jsonServer(t, http.StatusOK, `[
		{"chain_id":"1","network_slugs":{"node_rpc_slug":"mainnet"}},
		{"chain_id":"10","network_slugs":{"node_rpc_slug":"optimistic"}}
	]`)
	pointTenderlyAt(t, srv.URL)
	v := CreateTenderlyVendor().(*TenderlyVendor)
	// Registered after pointTenderlyAt, so it runs before the URL is restored.
	t.Cleanup(func() { waitForRefreshes(t, v.cache) })
	return v, common.VendorSettings{"apiKey": "secret-key", "recheckInterval": time.Hour}, hits
}

func warmTenderly(t *testing.T, v *TenderlyVendor, settings common.VendorSettings) {
	t.Helper()
	logger := zerolog.Nop()
	require.Eventually(t, func() bool {
		ok, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:1")
		return err == nil && ok
	}, fetcherTestWait, 10*time.Millisecond, "the async refresh never populated the tenderly cache")
}

func TestTenderlyVendor_SupportsNetwork_ColdStartIsRetryableThenAnswersFromTheSnapshot(t *testing.T) {
	v, settings, hits := tenderlyFixture(t)
	logger := zerolog.Nop()

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:1")
	assert.False(t, supported)
	// The bootstrap loop reschedules on this exact sentinel. A flat "no" would
	// drop the network for good.
	assert.ErrorIs(t, err, ErrRemoteCacheCold)

	warmTenderly(t, v, settings)

	supported, err = v.SupportsNetwork(context.Background(), &logger, settings, "evm:10")
	require.NoError(t, err)
	assert.True(t, supported)

	unknown, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:424242")
	require.NoError(t, err, "a chain missing from a warm snapshot is a definite no, not a retry")
	assert.False(t, unknown)

	assert.Equal(t, int32(1), hits.Load(), "a warm, fresh snapshot must not re-hit the network")
}

func TestTenderlyVendor_SupportsNetwork_IgnoresNonEvmAndReportsAMalformedChainId(t *testing.T) {
	v, settings, hits := tenderlyFixture(t)
	logger := zerolog.Nop()

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "solana:mainnet")
	require.NoError(t, err, "a non-EVM network is out of scope, not an error")
	assert.False(t, supported)

	supported, err = v.SupportsNetwork(context.Background(), &logger, settings, "evm:0xdead")
	require.Error(t, err)
	assert.False(t, supported)

	assert.Equal(t, int32(0), hits.Load(),
		"neither answer needs the network, so neither may start a fetch")
}

func TestTenderlyVendor_GenerateConfigs_BuildsTheGatewayUrlFromTheDiscoveredSlug(t *testing.T) {
	v, settings, _ := tenderlyFixture(t)
	logger := zerolog.Nop()
	warmTenderly(t, v, settings)

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 10}}, settings)

	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Equal(t, "https://optimistic.gateway.tenderly.co/secret-key", configs[0].Endpoint)
	assert.Equal(t, common.UpstreamTypeEvm, configs[0].Type)
	assert.Equal(t, "tenderly", configs[0].VendorName)
}

func TestTenderlyVendor_GenerateConfigs_ChecksItsInputsInOrder(t *testing.T) {
	v, settings, _ := tenderlyFixture(t)
	logger := zerolog.Nop()
	ctx := context.Background()
	warmTenderly(t, v, settings)

	// The api key is checked before the evm block, so an upstream missing both
	// must name the key. Reversing the order sends the operator to the wrong
	// field.
	_, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, common.VendorSettings{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apiKey")

	_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, settings)
	require.Error(t, err)
	assert.Equal(t, "tenderly vendor requires upstream.evm to be defined", err.Error())

	_, err = v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, settings)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chainId")

	_, err = v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}}, settings)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "424242",
		"an unknown chain on a warm cache is permanent, so the message must name it")
	assert.NotErrorIs(t, err, ErrRemoteCacheCold)
}

func TestTenderlyVendor_GenerateConfigs_ColdStartIsRetryable(t *testing.T) {
	v, settings, _ := tenderlyFixture(t)
	logger := zerolog.Nop()

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)

	assert.Nil(t, configs)
	assert.ErrorIs(t, err, ErrRemoteCacheCold)
}

func TestTenderlyVendor_GenerateConfigs_APresetEndpointBypassesDiscovery(t *testing.T) {
	v, settings, hits := tenderlyFixture(t)
	logger := zerolog.Nop()

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Endpoint: "https://custom.example/rpc"}, settings)

	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Equal(t, "https://custom.example/rpc", configs[0].Endpoint)
	assert.Equal(t, "tenderly", configs[0].VendorName, "the vendor still claims the upstream")
	assert.Equal(t, int32(0), hits.Load(), "a preset endpoint needs no discovery")
}

// -----------------------------------------------------------------------------
// quicknode: fetchEndpoints
// -----------------------------------------------------------------------------

// quicknodeEndpointsBody renders one page of QuickNode's endpoint listing.
func quicknodeEndpointsBody(idPrefix string, count int) string {
	items := make([]string, 0, count)
	for i := 0; i < count; i++ {
		items = append(items, fmt.Sprintf(`{"id":"%s%d","http_url":"https://%s%d.quiknode.example","chain":"ethereum"}`,
			idPrefix, i, idPrefix, i))
	}
	return `{"data":[` + strings.Join(items, ",") + `]}`
}

// The walk stops when a page returns fewer rows than the limit, so the offset
// arithmetic only shows up across a page boundary. A full first page that is
// mistaken for the last one silently truncates a large account's endpoints.
func TestQuicknodeFetchEndpoints_WalksPastAFullPageAndStopsOnAShortOne(t *testing.T) {
	var offsets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		offsets = append(offsets, offset)
		w.Header().Set("Content-Type", "application/json")
		if offset == "0" {
			_, _ = w.Write([]byte(quicknodeEndpointsBody("a", 100)))
			return
		}
		_, _ = w.Write([]byte(quicknodeEndpointsBody("b", 3)))
	}))
	t.Cleanup(srv.Close)
	pointQuicknodeAt(t, srv.URL)

	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	endpoints, err := v.fetchEndpoints(context.Background(), "k", nil)

	require.NoError(t, err)
	assert.Len(t, endpoints, 103, "both pages must land in the result")
	assert.Equal(t, []string{"0", "100"}, offsets,
		"the second request must advance the offset by the limit")
}

func TestQuicknodeFetchEndpoints_DropsAnEndpointWithNoHttpUrl(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusOK, `{"data":[
		{"id":"good","http_url":"https://good.quiknode.example","chain":"ethereum"},
		{"id":"wsonly","http_url":"","chain":"ethereum"}
	]}`)
	pointQuicknodeAt(t, srv.URL)

	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	endpoints, err := v.fetchEndpoints(context.Background(), "k", nil)

	require.NoError(t, err)
	require.Len(t, endpoints, 1, "an endpoint with no HTTP URL cannot be routed to")
	assert.Equal(t, "good", endpoints[0].ID)
}

func TestQuicknodeFetchEndpoints_SendsTheApiKeyHeaderAndBothTagFilters(t *testing.T) {
	var gotKey, gotTagIDs, gotTagLabels string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotTagIDs = r.URL.Query().Get("tag_ids")
		gotTagLabels = r.URL.Query().Get("tag_labels")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	pointQuicknodeAt(t, srv.URL)

	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	_, err := v.fetchEndpoints(context.Background(), "secret-key",
		&QuicknodeFilterParams{TagIDs: []int{7, 9}, TagLabels: []string{"prod", "eu"}})

	require.NoError(t, err)
	// Without the key QuickNode answers 401. Without the filters the operator
	// gets every endpoint on the account, not the ones they asked for.
	assert.Equal(t, "secret-key", gotKey)
	assert.Equal(t, "7,9", gotTagIDs)
	assert.Equal(t, "prod,eu", gotTagLabels)
}

func TestQuicknodeFetchEndpoints_ReportsTheStatusCodeOnANon2xx(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusUnauthorized, `{"error":"bad key"}`)
	pointQuicknodeAt(t, srv.URL)

	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	endpoints, err := v.fetchEndpoints(context.Background(), "k", nil)

	require.Error(t, err)
	assert.Nil(t, endpoints)
	assert.Contains(t, err.Error(), "401")
	assert.Contains(t, err.Error(), "bad key", "the body carries QuickNode's own reason")
}

func TestQuicknodeFetchEndpoints_MalformedJsonIsAParseFailure(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusOK, `<html>gateway timeout</html>`)
	pointQuicknodeAt(t, srv.URL)

	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	endpoints, err := v.fetchEndpoints(context.Background(), "k", nil)

	require.Error(t, err)
	assert.Nil(t, endpoints)
	assert.Contains(t, err.Error(), "failed to decode QuickNode endpoints response")
}

// QuickNode reports some failures in the body of a 200. Treating that as an
// empty account would publish an empty endpoint list as if it were the truth.
func TestQuicknodeFetchEndpoints_AnErrorFieldInsideATwoHundredIsStillAnError(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusOK, `{"data":[],"error":"rate limited"}`)
	pointQuicknodeAt(t, srv.URL)

	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	endpoints, err := v.fetchEndpoints(context.Background(), "k", nil)

	require.Error(t, err)
	assert.Nil(t, endpoints)
	assert.Contains(t, err.Error(), "rate limited")
}

func TestQuicknodeFetchEndpoints_ATruncatedBodyIsAnErrorNotAPartialList(t *testing.T) {
	srv := truncatedBodyServer(t, `{"data":[{"id":"a","http_url":"https://a.example","chain":"ethereum"},{"id":"b","http_ur`)
	pointQuicknodeAt(t, srv.URL)

	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	endpoints, err := v.fetchEndpoints(context.Background(), "k", nil)

	require.Error(t, err)
	assert.Nil(t, endpoints, "endpoint a decoded cleanly, but publishing it alone hides the loss of b")
}

func TestQuicknodeFetchEndpoints_HonoursTheCallersDeadline(t *testing.T) {
	srv := hangingServer(t)
	pointQuicknodeAt(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	start := time.Now()
	endpoints, err := v.fetchEndpoints(ctx, "k", nil)

	require.Error(t, err)
	assert.Nil(t, endpoints)
	assert.Less(t, time.Since(start), fetcherTestWait,
		"the caller's deadline must win over the client's own 30s timeout")
}

// -----------------------------------------------------------------------------
// quicknode: the vendor over the cache
// -----------------------------------------------------------------------------

// quicknodeFixture serves two endpoints and answers the eth_chainId probe that
// fetchChainIDs sends to each of them, so a warm snapshot carries real chain
// IDs. Both endpoints point back at the same server; the probe replies with
// the chain ID encoded in the request path.
func quicknodeFixture(t *testing.T) (*QuicknodeVendor, common.VendorSettings, *atomic.Int32) {
	t.Helper()
	var listHits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			// eth_chainId probe. The path names the chain in decimal.
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/probe/"), 10, 64)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"0x%x"}`, id)
			return
		}
		listHits.Add(1)
		_, _ = fmt.Fprintf(w, `{"data":[
			{"id":"ep-one","http_url":"%s/probe/1","chain":"ethereum"},
			{"id":"ep-two","http_url":"%s/probe/10","chain":"optimism"}
		]}`, srv.URL, srv.URL)
	}))
	t.Cleanup(srv.Close)
	pointQuicknodeAt(t, srv.URL)

	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	// Registered after pointQuicknodeAt, so it runs before the URL is restored.
	t.Cleanup(func() {
		waitForRefreshes(t, v.cache)
		waitForRefreshes(t, v.cuCache)
	})
	return v, common.VendorSettings{"apiKey": "secret-key", "recheckInterval": time.Hour}, &listHits
}

func warmQuicknode(t *testing.T, v *QuicknodeVendor, settings common.VendorSettings) {
	t.Helper()
	logger := zerolog.Nop()
	require.Eventually(t, func() bool {
		ok, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:1")
		return err == nil && ok
	}, fetcherTestWait, 10*time.Millisecond, "the async refresh never populated the quicknode cache")
}

func TestQuicknodeVendor_SupportsNetwork_ColdStartIsRetryableThenAnswersFromTheSnapshot(t *testing.T) {
	v, settings, hits := quicknodeFixture(t)
	logger := zerolog.Nop()

	supported, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:1")
	assert.False(t, supported)
	assert.ErrorIs(t, err, ErrRemoteCacheCold)

	warmQuicknode(t, v, settings)

	supported, err = v.SupportsNetwork(context.Background(), &logger, settings, "evm:10")
	require.NoError(t, err)
	assert.True(t, supported, "the probe filled chain 10 in from the second endpoint")

	unknown, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:424242")
	require.NoError(t, err, "a chain missing from a warm snapshot is a definite no, not a retry")
	assert.False(t, unknown)

	assert.Equal(t, int32(1), hits.Load(), "a warm, fresh snapshot must not re-list the endpoints")
}

func TestQuicknodeVendor_SupportsNetwork_AMissingApiKeyIsAFlatNoNotAFetch(t *testing.T) {
	v, _, hits := quicknodeFixture(t)
	logger := zerolog.Nop()

	supported, err := v.SupportsNetwork(context.Background(), &logger, common.VendorSettings{}, "evm:1")

	require.NoError(t, err, "no key means the operator did not configure quicknode, not that it failed")
	assert.False(t, supported)
	assert.Equal(t, int32(0), hits.Load())
}

func TestQuicknodeVendor_GenerateConfigs_MakesOneUpstreamPerMatchingEndpoint(t *testing.T) {
	v, settings, _ := quicknodeFixture(t)
	logger := zerolog.Nop()
	warmQuicknode(t, v, settings)

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Id: "qn", Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)

	require.NoError(t, err)
	require.Len(t, configs, 1, "only the endpoint whose probe returned chain 1 belongs to this network")
	assert.Equal(t, "qn-ep-one", configs[0].Id, "the endpoint id must be suffixed so two endpoints stay distinct")
	assert.Contains(t, configs[0].Endpoint, "/probe/1")
	assert.Equal(t, common.UpstreamTypeEvm, configs[0].Type)
}

func TestQuicknodeVendor_GenerateConfigs_SynthesisesAnIdWhenTheUpstreamHasNone(t *testing.T) {
	v, settings, _ := quicknodeFixture(t)
	logger := zerolog.Nop()
	warmQuicknode(t, v, settings)

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 10}}, settings)

	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Equal(t, "quicknode-10-ep-two", configs[0].Id)
}

func TestQuicknodeVendor_GenerateConfigs_ChecksItsInputsInOrder(t *testing.T) {
	v, settings, _ := quicknodeFixture(t)
	logger := zerolog.Nop()
	ctx := context.Background()
	warmQuicknode(t, v, settings)

	_, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, common.VendorSettings{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apiKey")

	_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, settings)
	require.Error(t, err)
	assert.Equal(t, "quicknode vendor requires upstream.evm to be defined", err.Error())

	_, err = v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, settings)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chainId")
}

func TestQuicknodeVendor_GenerateConfigs_ColdStartIsRetryable(t *testing.T) {
	v, settings, _ := quicknodeFixture(t)
	logger := zerolog.Nop()

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)

	assert.Nil(t, configs)
	assert.ErrorIs(t, err, ErrRemoteCacheCold)
}

func TestQuicknodeVendor_GenerateConfigs_APresetEndpointBypassesDiscovery(t *testing.T) {
	v, settings, hits := quicknodeFixture(t)
	logger := zerolog.Nop()

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Endpoint: "https://custom.quiknode.example/rpc"}, settings)

	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Equal(t, "https://custom.quiknode.example/rpc", configs[0].Endpoint)
	assert.Equal(t, int32(0), hits.Load())
}

// The credit-unit fetch needs QuickNode's chain slug, and the only place that
// slug exists is the already-discovered endpoint list. Before discovery there
// is no slug, so the refresh must skip rather than guess a path.
func TestQuicknodeVendor_ChainSlug_ComesFromTheDiscoveredEndpointsOrIsEmpty(t *testing.T) {
	v, settings, _ := quicknodeFixture(t)

	assert.Equal(t, "", v.chainSlug("secret-key", 1), "nothing is discovered yet")

	warmQuicknode(t, v, settings)

	assert.Equal(t, "ethereum", v.chainSlug("secret-key", 1))
	assert.Equal(t, "optimism", v.chainSlug("secret-key", 10))
	assert.Equal(t, "", v.chainSlug("secret-key", 424242), "no endpoint carries that chain")
	assert.Equal(t, "", v.chainSlug("another-key", 1), "the list is per account")
}

// The credit-unit URL is built from the slug, so an empty slug would fetch
// from the bare base path and cache whatever came back under a real chain ID.
// The refresh must wait for discovery instead.
//
// The test does not cover the "" key and the 0 chain ID in the same guard.
// Both are shadowed: chainSlug finds nothing for either, so the slug gate
// stops them anyway and removing them from the guard changes no behaviour.
func TestQuicknodeVendor_RefreshCreditUnitsAsync_WaitsForTheSlugThenAdoptsTheAccountTable(t *testing.T) {
	v, settings, _ := quicknodeFixture(t)
	logger := zerolog.Nop()

	var cuHits atomic.Int32
	cuSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cuHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"method":"eth_getLogs","credits":75}],"error":""}`))
	}))
	t.Cleanup(cuSrv.Close)
	prev := quicknodeApiCreditsBaseURL
	quicknodeApiCreditsBaseURL = cuSrv.URL + "/"
	t.Cleanup(func() { quicknodeApiCreditsBaseURL = prev })

	// Nothing is discovered yet, so there is no slug. The refresh is
	// asynchronous, so the check has to hold over a window rather than at one
	// instant.
	v.refreshCreditUnitsAsync(&logger, "secret-key", 1)
	require.Never(t, func() bool { return cuHits.Load() > 0 },
		500*time.Millisecond, 20*time.Millisecond,
		"an unknown slug must stop before the request, not fetch from the bare base path")

	warmQuicknode(t, v, settings)

	v.refreshCreditUnitsAsync(&logger, "secret-key", 1)
	require.Eventually(t, func() bool {
		table, _ := v.cuCache.Lookup("1", DefaultQuicknodeCreditUnitsRecheckInterval)
		return table != nil
	}, fetcherTestWait, 10*time.Millisecond, "the slug is known now, so the fetch must run")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs"}`))
	ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
	assert.Equal(t, int64(75), v.CreditUnits(req, ups),
		"the account-accurate table must beat the built-in fallback")

	// This test restores the credit URL in its own cleanup, which runs before
	// the fixture's drain, so it drains here instead.
	waitForRefreshes(t, v.cuCache)
}

// -----------------------------------------------------------------------------
// chainstack: fetchNodes
// -----------------------------------------------------------------------------

func TestChainstackFetchNodes_ReportsTheStatusCodeOnANon2xx(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusForbidden, `{"detail":"invalid token"}`)
	pointChainstackAt(t, srv.URL)

	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()
	nodes, err := v.fetchNodes(context.Background(), &logger, "k", nil)

	require.Error(t, err)
	assert.Nil(t, nodes)
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), "invalid token")
}

func TestChainstackFetchNodes_MalformedJsonIsAParseFailure(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusOK, `<html>bad gateway</html>`)
	pointChainstackAt(t, srv.URL)

	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()
	nodes, err := v.fetchNodes(context.Background(), &logger, "k", nil)

	require.Error(t, err)
	assert.Nil(t, nodes)
	assert.Contains(t, err.Error(), "failed to decode Chainstack nodes response")
}

func TestChainstackFetchNodes_ATruncatedBodyIsAnErrorNotAPartialList(t *testing.T) {
	srv := truncatedBodyServer(t, `{"next":null,"results":[{"id":"a","details":{"https_endpoint":"https://a.example"}},{"id":"b","deta`)
	pointChainstackAt(t, srv.URL)

	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()
	nodes, err := v.fetchNodes(context.Background(), &logger, "k", nil)

	require.Error(t, err)
	assert.Nil(t, nodes, "node a decoded cleanly, but publishing it alone hides the loss of b")
}

// A node with no id or no HTTPS endpoint cannot be routed to. Keeping it would
// produce an upstream with an empty endpoint at bootstrap.
func TestChainstackFetchNodes_DropsNodesWithNoIdOrNoHttpsEndpoint(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusOK, `{"next":null,"results":[
		{"id":"good","status":"running","details":{"https_endpoint":"https://good.example"}},
		{"id":"","status":"running","details":{"https_endpoint":"https://anon.example"}},
		{"id":"nourl","status":"running","details":{"https_endpoint":""}},
		"not-an-object"
	]}`)
	pointChainstackAt(t, srv.URL)

	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()
	nodes, err := v.fetchNodes(context.Background(), &logger, "k", nil)

	require.NoError(t, err, "one unusable row must not fail the whole walk")
	require.Len(t, nodes, 1)
	assert.Equal(t, "good", nodes[0].ID)
}

func TestChainstackFetchNodes_SendsTheFilterParamsAndTheBearerToken(t *testing.T) {
	var gotAuth string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"next":null,"results":[]}`))
	}))
	t.Cleanup(srv.Close)
	pointChainstackAt(t, srv.URL)

	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()
	_, err := v.fetchNodes(context.Background(), &logger, "secret-key", &ChainstackFilterParams{
		Project: "p1", Organization: "o1", Region: "eu", Provider: "aws", Type: "dedicated",
	})

	require.NoError(t, err)
	// Drop any one of these and Chainstack returns the whole account instead
	// of the nodes the operator asked for.
	assert.Equal(t, "Bearer secret-key", gotAuth)
	assert.Equal(t, "p1", gotQuery.Get("project"))
	assert.Equal(t, "o1", gotQuery.Get("organization"))
	assert.Equal(t, "eu", gotQuery.Get("region"))
	assert.Equal(t, "aws", gotQuery.Get("provider"))
	assert.Equal(t, "dedicated", gotQuery.Get("type"))
}

func TestChainstackFetchNodes_HonoursTheCallersDeadline(t *testing.T) {
	srv := hangingServer(t)
	pointChainstackAt(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()
	start := time.Now()
	nodes, err := v.fetchNodes(ctx, &logger, "k", nil)

	require.Error(t, err)
	assert.Nil(t, nodes)
	assert.Less(t, time.Since(start), fetcherTestWait,
		"the caller's deadline must win over the client's own 30s timeout")
}

// -----------------------------------------------------------------------------
// chainstack: the vendor over the cache
// -----------------------------------------------------------------------------

// chainstackFixture lists three nodes and answers the eth_chainId probe that
// fetchChainIDs sends to each, so a warm snapshot carries real chain IDs. The
// probe replies with the chain ID named in the request path. One node is still
// provisioning, because that is the row GenerateConfigs must drop.
func chainstackFixture(t *testing.T) (*ChainstackVendor, common.VendorSettings, *atomic.Int32) {
	t.Helper()
	var listHits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			// Chainstack appends the node's auth key to the endpoint, so the
			// probe arrives at /probe/<chainId>/<authKey>.
			segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			id, _ := strconv.ParseInt(segments[1], 10, 64)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"0x%x"}`, id)
			return
		}
		listHits.Add(1)
		_, _ = fmt.Fprintf(w, `{"next":null,"results":[
			{"id":"node-a","status":"running","details":{"https_endpoint":"%s/probe/1","auth_key":"key-a"}},
			{"id":"node-b","status":"running","details":{"https_endpoint":"%s/probe/1","auth_key":"key-b"}},
			{"id":"node-c","status":"provisioning","details":{"https_endpoint":"%s/probe/1","auth_key":"key-c"}}
		]}`, srv.URL, srv.URL, srv.URL)
	}))
	t.Cleanup(srv.Close)
	pointChainstackAt(t, srv.URL)

	v := CreateChainstackVendor().(*ChainstackVendor)
	// Registered after pointChainstackAt, so it runs before the URL is restored.
	t.Cleanup(func() { waitForRefreshes(t, v.cache) })
	return v, common.VendorSettings{"apiKey": "secret-key", "recheckInterval": time.Hour}, &listHits
}

func warmChainstack(t *testing.T, v *ChainstackVendor, settings common.VendorSettings) {
	t.Helper()
	logger := zerolog.Nop()
	require.Eventually(t, func() bool {
		ok, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:1")
		return err == nil && ok
	}, fetcherTestWait, 10*time.Millisecond, "the async refresh never populated the chainstack cache")
}

// A chain missing from a warm snapshot is a settled no. Returning the cold-start
// error instead would make the bootstrap loop retry an answer it already has.
func TestChainstackVendor_SupportsNetwork_AWarmSnapshotSettlesTheAnswer(t *testing.T) {
	v, settings, hits := chainstackFixture(t)
	logger := zerolog.Nop()
	warmChainstack(t, v, settings)

	unknown, err := v.SupportsNetwork(context.Background(), &logger, settings, "evm:424242")
	require.NoError(t, err)
	assert.False(t, unknown)

	assert.Equal(t, int32(1), hits.Load(), "a warm, fresh snapshot must not re-list the nodes")
}

// Chainstack gives one account many nodes on the same chain, and each is a
// separate upstream. Collapsing them to one would throw away the redundancy the
// operator paid for; sharing one ID would make them collide in the registry.
//
// A node that is not running never joins the network, and two guards say so
// independently: fetchChainIDs skips it, so it keeps chain ID 0, and
// GenerateConfigs checks the status again. Mutating either one alone leaves
// this test green — the other still holds the line. Mutating both turns it red,
// which is the behaviour actually being pinned.
func TestChainstackVendor_GenerateConfigs_MakesOneUpstreamPerRunningNodeOnTheChain(t *testing.T) {
	v, settings, _ := chainstackFixture(t)
	logger := zerolog.Nop()
	warmChainstack(t, v, settings)

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Id: "cs", Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)

	require.NoError(t, err)
	require.Len(t, configs, 2, "node-c is still provisioning, so only two of the three qualify")

	assert.ElementsMatch(t, []string{"cs-node-a", "cs-node-b"},
		[]string{configs[0].Id, configs[1].Id},
		"each node's id must be suffixed onto the operator's, or the two collide")
	// The auth key is a path segment, not a header. Dropping it reaches the
	// host and is refused there.
	assert.ElementsMatch(t, []string{"key-a", "key-b"},
		[]string{path.Base(configs[0].Endpoint), path.Base(configs[1].Endpoint)})
	assert.Equal(t, common.UpstreamTypeEvm, configs[0].Type)
	assert.NotNil(t, configs[0].JsonRpc)

	// With no id of their own the nodes still need distinct ids.
	anon, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)
	require.NoError(t, err)
	require.Len(t, anon, 2)
	assert.ElementsMatch(t, []string{"chainstack-1-node-a", "chainstack-1-node-b"},
		[]string{anon[0].Id, anon[1].Id})

	// A chain the account has no node for is an empty list, not an error: the
	// operator simply has nothing on it.
	none, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}}, settings)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestChainstackVendor_GenerateConfigs_ChecksItsInputsBeforeListingAnyNodes(t *testing.T) {
	v, settings, hits := chainstackFixture(t)
	logger := zerolog.Nop()

	_, errNoKey := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{})
	require.Error(t, errNoKey)
	assert.Contains(t, errNoKey.Error(), "apiKey")

	_, errNoEvm := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{}, settings)
	require.Error(t, errNoEvm)
	assert.Contains(t, errNoEvm.Error(), "evm")

	_, errNoChain := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, settings)
	require.Error(t, errNoChain)
	assert.Contains(t, errNoChain.Error(), "chainId")

	assert.Equal(t, int32(0), hits.Load(),
		"none of these three answers needs the node list, so none may fetch it")
}
