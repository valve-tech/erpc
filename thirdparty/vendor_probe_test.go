package thirdparty

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Five vendors — envio, goldsky, pimlico, routemesh and thirdweb — answer
// SupportsNetwork by asking the endpoint itself for its chain ID. eRPC is a
// sixth, covered in erpc_test.go, and it is the only one whose endpoint can
// resolve to plain http.
//
// The other five build a hard-coded https:// host, so a local httptest server
// cannot stand in for them. Neither a TLS fixture nor a client-injection seam
// is needed: clients.NewGenericHttpJsonRpcClient already routes through
// http.DefaultTransport whenever util.IsTest() holds, and gock — already a
// direct dependency, already used this way in clients/ and upstream/ — patches
// exactly that. So these tests change no production code at all.
//
// One ordering rule follows from that. The client copies http.DefaultTransport
// into its own http.Client at construction, so gock.New must run BEFORE the
// vendor builds its probe client. Every test here registers its mock first.
//
// These tests check the probe shape, not the chain tables: a matching chain ID
// is a yes, a mismatch is a settled no rather than an error, a transport
// failure is reported rather than swallowed, a result that is not a hex chain
// ID is an error rather than a silent no, and the guards run before anything
// touches the network.

type probeVendorCase struct {
	name string
	// newVendor is called per subtest so the client cache always starts cold.
	newVendor func() common.Vendor
	settings  common.VendorSettings
	// chainId is chosen to miss the vendor's static table, so the probe runs.
	chainId   int64
	networkId string
	// host and path are what the vendor's generated URL must resolve to.
	host string
	path string
}

func probeVendorCases() []probeVendorCase {
	return []probeVendorCase{
		{
			name:      "envio",
			newVendor: CreateEnvioVendor,
			settings:  common.VendorSettings{},
			chainId:   424242,
			networkId: "evm:424242",
			host:      "https://424242.rpc.hypersync.xyz",
			path:      "/",
		},
		{
			name:      "goldsky",
			newVendor: CreateGoldskyVendor,
			settings:  common.VendorSettings{"secret": "s3cret"},
			chainId:   424242,
			networkId: "evm:424242",
			host:      "https://edge.goldsky.com",
			path:      "/standard/evm/424242",
		},
		{
			name:      "pimlico",
			newVendor: CreatePimlicoVendor,
			settings:  common.VendorSettings{"apiKey": "public"},
			chainId:   424242,
			networkId: "evm:424242",
			host:      "https://public.pimlico.io",
			path:      "/v2/424242/rpc",
		},
		{
			name:      "routemesh",
			newVendor: CreateRoutemeshVendor,
			settings:  common.VendorSettings{"apiKey": "k"},
			chainId:   424242,
			networkId: "evm:424242",
			host:      "https://lb.routemes.sh",
			path:      "/rpc/424242/k",
		},
		{
			name:      "thirdweb",
			newVendor: CreateThirdwebVendor,
			settings:  common.VendorSettings{"clientId": "c"},
			chainId:   424242,
			networkId: "evm:424242",
			host:      "https://424242.rpc.thirdweb.com",
			path:      "/c",
		},
	}
}

// A vendor that probes must send the request to the URL it generated. If the
// mock does not match, gock refuses the request and the call errors, so the
// host and path here are asserted by the probe succeeding at all.
func TestProbeVendors_SupportsNetwork_AMatchingChainIdIsAYes(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()

	for _, tc := range probeVendorCases() {
		t.Run(tc.name, func(t *testing.T) {
			defer gock.Off()
			gock.New(tc.host).
				Post(tc.path).
				Reply(200).
				JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x67932"})

			supported, err := tc.newVendor().SupportsNetwork(ctx, &logger, tc.settings, tc.networkId)
			require.NoError(t, err)
			assert.True(t, supported)
			assert.True(t, gock.IsDone(), "the probe must reach the URL the vendor generated")
		})
	}
}

// An endpoint that answers with a different chain is a definite no. Reporting
// an error instead would make the bootstrap loop retry an answer that is
// already settled.
func TestProbeVendors_SupportsNetwork_AMismatchedChainIdIsASettledNoNotAnError(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()

	for _, tc := range probeVendorCases() {
		t.Run(tc.name, func(t *testing.T) {
			defer gock.Off()
			gock.New(tc.host).
				Post(tc.path).
				Reply(200).
				JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})

			supported, err := tc.newVendor().SupportsNetwork(ctx, &logger, tc.settings, tc.networkId)
			require.NoError(t, err, "the endpoint answered; it just serves another chain")
			assert.False(t, supported)
		})
	}
}

// A transport failure says nothing about whether the vendor serves the chain,
// so it must surface as an error. Folding it into a plain false would strand
// the network for the life of the process.
func TestProbeVendors_SupportsNetwork_ATransportFailureIsReportedNotSwallowed(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()

	for _, tc := range probeVendorCases() {
		t.Run(tc.name, func(t *testing.T) {
			defer gock.Off()
			gock.New(tc.host).
				Post(tc.path).
				ReplyError(errors.New("connection refused by the fixture"))

			supported, err := tc.newVendor().SupportsNetwork(ctx, &logger, tc.settings, tc.networkId)
			require.Error(t, err)
			assert.False(t, supported)
		})
	}
}

// Envio alone reads the transport error's text: its load balancer serves a
// certificate for a host it does not hold, and that is how an unsupported
// chain presents. Every other vendor here reports the same failure as an error.
func TestEnvioVendor_SupportsNetwork_ACertificateFailureIsAFlatNoNotAnError(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()
	defer gock.Off()

	gock.New("https://424242.rpc.hypersync.xyz").
		Post("/").
		ReplyError(errors.New("tls: failed to verify certificate: x509: certificate is valid for other.example"))

	supported, err := CreateEnvioVendor().SupportsNetwork(ctx, &logger,
		common.VendorSettings{}, "evm:424242")
	require.NoError(t, err, "envio treats a certificate mismatch as 'this chain is not served'")
	assert.False(t, supported)
}

// A 200 whose result is not a hex quantity means the endpoint is not the RPC
// the vendor expected. Answering a flat false would hide a misconfigured host
// behind "this vendor does not serve the chain".
//
// Three guards in a row produce that error — PeekStringByPath, NormalizeHex and
// HexToInt64 — and each covers for the next, so dropping any one alone leaves
// this test green. Dropping all three turns it red. The observable behaviour is
// what is pinned, not any single line.
func TestProbeVendors_SupportsNetwork_AResultThatIsNotAHexChainIdIsAnError(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()

	bodies := map[string]map[string]interface{}{
		"a string that is not hex": {"jsonrpc": "2.0", "id": 1, "result": "mainnet"},
		"an object where a quantity belongs": {"jsonrpc": "2.0", "id": 1,
			"result": map[string]interface{}{"chainId": 1}},
	}

	for _, tc := range probeVendorCases() {
		for body, payload := range bodies {
			t.Run(tc.name+"/"+body, func(t *testing.T) {
				defer gock.Off()
				gock.New(tc.host).Post(tc.path).Reply(200).JSON(payload)

				supported, err := tc.newVendor().SupportsNetwork(ctx, &logger, tc.settings, tc.networkId)
				require.Error(t, err)
				assert.False(t, supported)
			})
		}
	}
}

// The probe client owns a connection pool. Building a new one per call would
// leak one pool per bootstrap attempt, and every network retry is another call.
// Counting the cache is not enough to see that — a rebuild overwrites the same
// key — so this compares the stored client's identity across calls.
func TestProbeVendors_SupportsNetwork_BuildsOneProbeClientHoweverManyTimesItIsAsked(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()

	for _, tc := range probeVendorCases() {
		t.Run(tc.name, func(t *testing.T) {
			defer gock.Off()
			gock.New(tc.host).
				Post(tc.path).
				Times(3).
				Reply(200).
				JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x67932"})

			v := tc.newVendor()
			ask := func() {
				supported, err := v.SupportsNetwork(ctx, &logger, tc.settings, tc.networkId)
				require.NoError(t, err)
				require.True(t, supported)
			}

			ask()
			first := headlessClients(t, v)
			require.Len(t, first, 1)

			ask()
			ask()
			after := headlessClients(t, v)
			require.Len(t, after, 1)
			assert.Same(t, first[0], after[0],
				"a rebuilt client leaks the pool the first one still holds")
		})
	}
}

// Every guard that can answer without the network must answer without the
// network. A vendor that dials before checking the network family or its own
// credentials spends a bootstrap round trip on a question it can already
// answer, once per configured network.
func TestProbeVendors_SupportsNetwork_TheGuardsAnswerWithoutTouchingTheNetwork(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()

	cases := []struct {
		name      string
		newVendor func() common.Vendor
		host      string
		settings  common.VendorSettings
		networkId string
		wantErr   bool
	}{
		// A non-EVM network is out of scope for all five, and none of them
		// needs to ask an endpoint to know that.
		{"envio/non-evm", CreateEnvioVendor, "https://424242.rpc.hypersync.xyz", common.VendorSettings{}, "solana:mainnet", false},
		{"goldsky/non-evm", CreateGoldskyVendor, "https://edge.goldsky.com", common.VendorSettings{"secret": "s"}, "solana:mainnet", false},
		{"pimlico/non-evm", CreatePimlicoVendor, "https://public.pimlico.io", common.VendorSettings{"apiKey": "public"}, "solana:mainnet", false},
		{"routemesh/non-evm", CreateRoutemeshVendor, "https://lb.routemes.sh", common.VendorSettings{"apiKey": "k"}, "solana:mainnet", false},
		{"thirdweb/non-evm", CreateThirdwebVendor, "https://424242.rpc.thirdweb.com", common.VendorSettings{"clientId": "c"}, "solana:mainnet", false},

		// A chain ID that is not a decimal integer is a config mistake. Envio
		// and goldsky report it; so do pimlico, routemesh and thirdweb.
		{"envio/malformed-chain-id", CreateEnvioVendor, "https://424242.rpc.hypersync.xyz", common.VendorSettings{}, "evm:0xdead", true},
		{"goldsky/malformed-chain-id", CreateGoldskyVendor, "https://edge.goldsky.com", common.VendorSettings{"secret": "s"}, "evm:0xdead", true},
		{"routemesh/malformed-chain-id", CreateRoutemeshVendor, "https://lb.routemes.sh", common.VendorSettings{"apiKey": "k"}, "evm:0xdead", true},

		// The credential is required to build the URL at all. Pimlico and
		// routemesh call a missing key an error; goldsky calls a missing secret
		// a flat no, because probing without one only ever earns a 401.
		{"pimlico/no-api-key", CreatePimlicoVendor, "https://public.pimlico.io", common.VendorSettings{}, "evm:424242", true},
		{"routemesh/no-api-key", CreateRoutemeshVendor, "https://lb.routemes.sh", common.VendorSettings{}, "evm:424242", true},
		{"thirdweb/no-client-id", CreateThirdwebVendor, "https://424242.rpc.thirdweb.com", common.VendorSettings{}, "evm:424242", true},
		{"goldsky/no-secret", CreateGoldskyVendor, "https://edge.goldsky.com", common.VendorSettings{}, "evm:424242", false},

		// Envio and pimlico keep a static table in front of the probe, so a
		// chain they already know needs no round trip either.
		{"envio/chain-in-the-static-table", CreateEnvioVendor, "https://1.rpc.hypersync.xyz", common.VendorSettings{}, "evm:1", false},
		{"pimlico/chain-in-the-static-table", CreatePimlicoVendor, "https://public.pimlico.io", common.VendorSettings{}, "evm:1", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer gock.Off()
			// The mock stays pending unless something dials.
			gock.New(tc.host).Reply(200).
				JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x67932"})

			supported, err := tc.newVendor().SupportsNetwork(ctx, &logger, tc.settings, tc.networkId)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			// Only the two static-table hits are a yes; every other guard here
			// answers no.
			if tc.networkId == "evm:1" {
				assert.True(t, supported)
			} else {
				assert.False(t, supported)
			}
			assert.True(t, gock.IsPending(), "this answer needs no endpoint, so none may be dialled")
		})
	}
}

// -----------------------------------------------------------------------------
// The probe client cache
// -----------------------------------------------------------------------------

func headlessClients(t *testing.T, v common.Vendor) []any {
	t.Helper()
	out := []any{}
	collect := func(m interface{ Range(func(any, any) bool) }) {
		m.Range(func(_, value any) bool { out = append(out, value); return true })
	}
	switch tv := v.(type) {
	case *EnvioVendor:
		collect(&tv.headlessClients)
	case *GoldskyVendor:
		collect(&tv.headlessClients)
	case *PimlicoVendor:
		collect(&tv.headlessClients)
	case *RoutemeshVendor:
		collect(&tv.headlessClients)
	case *ThirdwebVendor:
		collect(&tv.headlessClients)
	default:
		t.Fatalf("no headlessClients on %T", v)
	}
	return out
}

func countHeadlessClients(t *testing.T, v common.Vendor) int {
	t.Helper()
	return len(headlessClients(t, v))
}

// The cache key is where the five vendors stop agreeing, and the difference is
// observable. Goldsky and routemesh key on the URL plus the chain, so a second
// endpoint for the same chain gets its own client. Envio, pimlico and thirdweb
// key on the chain alone, so the first URL wins forever — see the report.
func TestProbeVendors_getOrCreateClient_TheCacheKeyDecidesWhetherASecondUrlIsHonoured(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()

	first := mustParseURL(t, "https://first.example/rpc")
	second := mustParseURL(t, "https://second.example/rpc")

	t.Run("keyed by url and chain", func(t *testing.T) {
		goldsky := CreateGoldskyVendor().(*GoldskyVendor)
		a, err := goldsky.getOrCreateClient(ctx, &logger, 1, first)
		require.NoError(t, err)
		b, err := goldsky.getOrCreateClient(ctx, &logger, 1, first)
		require.NoError(t, err)
		assert.Same(t, a, b, "the same url and chain reuse one client")

		c, err := goldsky.getOrCreateClient(ctx, &logger, 1, second)
		require.NoError(t, err)
		assert.NotSame(t, a, c, "a different url must get its own client")
		assert.Equal(t, 2, countHeadlessClients(t, goldsky))

		routemesh := CreateRoutemeshVendor().(*RoutemeshVendor)
		ra, err := routemesh.getOrCreateClient(ctx, &logger, 1, first)
		require.NoError(t, err)
		rc, err := routemesh.getOrCreateClient(ctx, &logger, 1, second)
		require.NoError(t, err)
		assert.NotSame(t, ra, rc)
	})

	t.Run("keyed by chain alone", func(t *testing.T) {
		envio := CreateEnvioVendor().(*EnvioVendor)
		ea, err := envio.getOrCreateClient(ctx, &logger, 1, first)
		require.NoError(t, err)
		eb, err := envio.getOrCreateClient(ctx, &logger, 1, second)
		require.NoError(t, err)
		assert.Same(t, ea, eb, "envio hands back the first url's client for the second url")
		assert.Equal(t, 1, countHeadlessClients(t, envio))

		pimlico := CreatePimlicoVendor().(*PimlicoVendor)
		pa, err := pimlico.getOrCreateClient(ctx, &logger, 1, first)
		require.NoError(t, err)
		pb, err := pimlico.getOrCreateClient(ctx, &logger, 1, second)
		require.NoError(t, err)
		assert.Same(t, pa, pb)

		thirdweb := CreateThirdwebVendor().(*ThirdwebVendor)
		ta, err := thirdweb.getOrCreateClient(ctx, &logger, 1, first)
		require.NoError(t, err)
		tb, err := thirdweb.getOrCreateClient(ctx, &logger, 1, second)
		require.NoError(t, err)
		assert.Same(t, ta, tb)
	})

	t.Run("a different chain always gets a different client", func(t *testing.T) {
		envio := CreateEnvioVendor().(*EnvioVendor)
		one, err := envio.getOrCreateClient(ctx, &logger, 1, first)
		require.NoError(t, err)
		two, err := envio.getOrCreateClient(ctx, &logger, 137, first)
		require.NoError(t, err)
		assert.NotSame(t, one, two)
		assert.Equal(t, 2, countHeadlessClients(t, envio))
	})
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}
