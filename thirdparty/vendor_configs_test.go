package thirdparty

import (
	"context"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The static-table vendors (ankr, blastapi, blockpi, infura, llama and the
// rest) share one shape: look the chain up in a built-in map, then build a
// URL. Their chain tables are data, and a test that lists the same chains
// restates the data instead of checking behaviour. So these tests sample the
// shape rather than enumerate the vendors: the guard order, the URL format,
// the per-chain URL exceptions and the nil-input handling. A fault in any of
// those repeats across every vendor built the same way.

func TestStaticTableVendors_SupportsNetwork_RejectNonEvmAndMalformedIds(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	registry := NewVendorsRegistry()

	// Every vendor answers the same two questions the same way, whatever its
	// chain table holds.
	for _, name := range []string{"ankr", "blastapi", "blockpi", "infura", "llama", "onfinality", "thirdweb"} {
		t.Run(name, func(t *testing.T) {
			v := registry.LookupByName(name)
			require.NotNil(t, v)

			supported, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "solana:mainnet")
			require.NoError(t, err, "a non-EVM network is out of scope, not an error")
			assert.False(t, supported)

			supported, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:0xdeadbeef")
			require.Error(t, err, "a chain ID that is not a decimal integer must be reported, not ignored")
			assert.False(t, supported)
		})
	}
}

func TestAnkrVendor_GenerateConfigs_BuildsTheEndpointAndEscapesTheKey(t *testing.T) {
	v := CreateAnkrVendor()
	logger := zerolog.Nop()

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}},
		common.VendorSettings{"apiKey": "a b/c"})

	require.NoError(t, err)
	require.Len(t, configs, 1)
	// Without the escape the slash in the key would split into an extra path
	// segment and Ankr would receive a different key.
	assert.Equal(t, "https://rpc.ankr.com/eth/a+b%2Fc", configs[0].Endpoint)
	assert.Equal(t, common.UpstreamTypeEvm, configs[0].Type)
}

func TestAnkrVendor_GenerateConfigs_ChecksItsInputsInOrder(t *testing.T) {
	v := CreateAnkrVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	_, errNoKey := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{})
	require.Error(t, errNoKey)
	assert.Contains(t, errNoKey.Error(), "apiKey")

	// Ankr guards the nil evm block before it reads the chain ID.
	_, errNoEvm := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{}, common.VendorSettings{"apiKey": "k"})
	require.Error(t, errNoEvm)
	assert.Contains(t, errNoEvm.Error(), "upstream.evm")

	_, errNoChain := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, common.VendorSettings{"apiKey": "k"})
	require.Error(t, errNoChain)
	assert.Contains(t, errNoChain.Error(), "chainId")

	_, errUnknown := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}}, common.VendorSettings{"apiKey": "k"})
	require.Error(t, errUnknown)
	assert.Contains(t, errUnknown.Error(), "424242")
}

func TestAnkrVendor_GenerateConfigs_APresetEndpointSkipsTheApiKeyRequirement(t *testing.T) {
	v := CreateAnkrVendor()
	logger := zerolog.Nop()

	configs, err := v.GenerateConfigs(context.Background(), &logger,
		&common.UpstreamConfig{Endpoint: "https://my.node/rpc"}, common.VendorSettings{})

	require.NoError(t, err, "an operator who supplies the URL has already supplied the credentials")
	require.Len(t, configs, 1)
	assert.Equal(t, "https://my.node/rpc", configs[0].Endpoint)
	assert.NotNil(t, configs[0].JsonRpc)
}

func TestBlastApiVendor_GenerateConfigs_AvalancheGetsTheExtraCChainPath(t *testing.T) {
	v := CreateBlastApiVendor()
	logger := zerolog.Nop()
	ctx := context.Background()
	settings := common.VendorSettings{"apiKey": "k"}

	// Avalanche C-Chain is the one chain whose URL is not the common shape.
	ava, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 43114}}, settings)
	require.NoError(t, err)
	require.Len(t, ava, 1)
	assert.True(t, strings.HasSuffix(ava[0].Endpoint, "/ext/bc/C/rpc"), "got %s", ava[0].Endpoint)

	eth, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)
	require.NoError(t, err)
	require.Len(t, eth, 1)
	assert.False(t, strings.HasSuffix(eth[0].Endpoint, "/ext/bc/C/rpc"),
		"only the Avalanche endpoints take the extra path")
	assert.Equal(t, "https://eth-mainnet.blastapi.io/k", eth[0].Endpoint)
}

func TestBlockPiVendor_GenerateConfigs_MovementTakesADifferentPathOrder(t *testing.T) {
	v := CreateBlockPiVendor()
	logger := zerolog.Nop()
	ctx := context.Background()
	settings := common.VendorSettings{"apiKey": "k"}

	eth, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)
	require.NoError(t, err)
	require.Len(t, eth, 1)
	assert.Equal(t, "https://ethereum.blockpi.network/v1/rpc/k", eth[0].Endpoint)

	// Movement swaps the version and the word "rpc" and adds a trailing /v1.
	mv, ok := BlockPiNetworkNames[126]
	if ok && mv == "movement" {
		mvCfg, err := v.GenerateConfigs(ctx, &logger,
			&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 126}}, settings)
		require.NoError(t, err)
		require.Len(t, mvCfg, 1)
		assert.Equal(t, "https://movement.blockpi.network/rpc/v1/k/v1", mvCfg[0].Endpoint)
	}
}

// infura.go:81 and llama.go:54 read upstream.Evm.ChainId before they check
// whether upstream.Evm is nil, unlike ankr, blastapi and blockpi. A provider
// configured without an evm block panics instead of reporting a config
// error. This test pins today's behaviour; see the report.
// A provider configured with no `evm` block is an operator mistake at
// bootstrap. Every vendor must name the missing field instead of panicking.
func TestInfuraAndLlama_GenerateConfigs_AMissingEvmBlockIsAConfigError(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()
	settings := common.VendorSettings{"apiKey": "k"}

	for _, v := range []common.Vendor{CreateInfuraVendor(), CreateLlamaVendor()} {
		t.Run(v.Name(), func(t *testing.T) {
			var cfgs []*common.UpstreamConfig
			var err error
			require.NotPanics(t, func() {
				cfgs, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, settings)
			}, "a missing evm block must not crash the process")

			require.Error(t, err)
			assert.Nil(t, cfgs)
			assert.Equal(t, v.Name()+" vendor requires upstream.evm to be defined", err.Error(),
				"the message must match the wording the other vendors already use")
		})
	}
}

func TestInfuraVendor_GenerateConfigs_BuildsTheEndpointAndReportsUnknownChains(t *testing.T) {
	v := CreateInfuraVendor()
	logger := zerolog.Nop()
	ctx := context.Background()
	settings := common.VendorSettings{"apiKey": "proj-id"}

	configs, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)
	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Equal(t, "https://mainnet.infura.io/v3/proj-id", configs[0].Endpoint)
	assert.Equal(t, common.UpstreamTypeEvm, configs[0].Type)

	_, errUnknown := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}}, settings)
	require.Error(t, errUnknown)
	assert.Contains(t, errUnknown.Error(), "424242")

	_, errNoChain := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, settings)
	require.Error(t, errNoChain)
	assert.Contains(t, errNoChain.Error(), "chainId")

	_, errNoKey := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{})
	require.Error(t, errNoKey)
	assert.Contains(t, errNoKey.Error(), "apiKey")
}

func TestLlamaVendor_GenerateConfigs_BuildsTheEndpoint(t *testing.T) {
	v := CreateLlamaVendor()
	logger := zerolog.Nop()
	ctx := context.Background()
	settings := common.VendorSettings{"apiKey": "k"}

	configs, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)
	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Equal(t, "https://eth.llamarpc.com/k", configs[0].Endpoint)

	_, errUnknown := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}}, settings)
	require.Error(t, errUnknown)
	assert.Contains(t, errUnknown.Error(), "424242")
}

func TestStaticTableVendors_SupportsNetworkAgreesWithGenerateConfigs(t *testing.T) {
	// A vendor that says it supports a network must be able to build a config
	// for it. A drift between the two tables strands the network at bootstrap.
	ctx := context.Background()
	logger := zerolog.Nop()
	settings := common.VendorSettings{"apiKey": "k"}
	registry := NewVendorsRegistry()

	for _, name := range []string{"ankr", "blastapi", "blockpi", "infura", "llama"} {
		t.Run(name, func(t *testing.T) {
			v := registry.LookupByName(name)
			require.NotNil(t, v)

			supported, err := v.SupportsNetwork(ctx, &logger, settings, "evm:1")
			require.NoError(t, err)
			require.True(t, supported, "every one of these vendors serves Ethereum mainnet")

			configs, err := v.GenerateConfigs(ctx, &logger,
				&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)
			require.NoError(t, err)
			require.Len(t, configs, 1)
			assert.NotEmpty(t, configs[0].Endpoint)

			unsupported, err := v.SupportsNetwork(ctx, &logger, settings, "evm:424242")
			require.NoError(t, err)
			assert.False(t, unsupported)

			_, err = v.GenerateConfigs(ctx, &logger,
				&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}}, settings)
			assert.Error(t, err, "an unsupported chain must fail config generation too")
		})
	}
}

// -----------------------------------------------------------------------------
// The remaining static-table vendors, sampled by shape
// -----------------------------------------------------------------------------
//
// The vendors above cover the plain "look up a chain, build a URL" shape.
// Three shapes are left, and each one is a different way for a config to go
// wrong. Enumerating the other nineteen vendors would only restate their chain
// tables, so these tests take one vendor per shape:
//
//   - the api key is OPTIONAL and changes which host is used (pimlico, envio);
//   - the base domain comes from settings, not from a constant (routemesh);
//   - the chain decides the host, and an unknown chain has no host at all
//     (etherspot).

// Six vendors read upstream.Evm.ChainId with no nil check, unlike the eighteen
// that guard first: envio.go:223, erpc.go:116 and :134, etherspot.go:97,
// pimlico.go:176, routemesh.go:116 and thirdweb.go:100. A provider configured
// without an evm block crashes the process at bootstrap instead of naming the
// missing field. This test pins today's behaviour and fails once a guard lands;
// see the report.
func TestSixVendors_GenerateConfigs_PanicOnAMissingEvmBlock(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()

	// Each entry supplies the settings that get past the vendor's earlier
	// guards, so the nil dereference is what the call reaches.
	cases := []struct {
		name     string
		vendor   common.Vendor
		settings common.VendorSettings
		upstream *common.UpstreamConfig
	}{
		{"envio", CreateEnvioVendor(), common.VendorSettings{}, &common.UpstreamConfig{}},
		{"erpc-from-settings", CreateErpcVendor(), common.VendorSettings{"endpoint": "http://erpc.example"}, &common.UpstreamConfig{}},
		// erpc dereferences on its preset-endpoint path too, which is the
		// normal way to configure it.
		{"erpc-preset-endpoint", CreateErpcVendor(), common.VendorSettings{}, &common.UpstreamConfig{Endpoint: "erpc://erpc.example:8545"}},
		{"etherspot", CreateEtherspotVendor(), common.VendorSettings{"apiKey": "k"}, &common.UpstreamConfig{}},
		{"pimlico", CreatePimlicoVendor(), common.VendorSettings{"apiKey": "k"}, &common.UpstreamConfig{}},
		{"routemesh", CreateRoutemeshVendor(), common.VendorSettings{"apiKey": "k"}, &common.UpstreamConfig{}},
		{"thirdweb", CreateThirdwebVendor(), common.VendorSettings{"clientId": "c"}, &common.UpstreamConfig{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Panics(t, func() {
				_, _ = tc.vendor.GenerateConfigs(ctx, &logger, tc.upstream, tc.settings)
			}, "an operator who omits the evm block should get a config error, not a crash")
		})
	}
}

// Pimlico's key is not a credential in the usual sense: the literal "public"
// selects a different host with no key in the URL. Mixing the two branches
// either leaks the key to the public host or sends "public" as a real key.
func TestPimlicoVendor_GenerateConfigs_ThePublicKeywordSelectsADifferentHost(t *testing.T) {
	v := CreatePimlicoVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	public, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}},
		common.VendorSettings{"apiKey": "public"})
	require.NoError(t, err)
	require.Len(t, public, 1)
	assert.Equal(t, "https://public.pimlico.io/v2/1/rpc", public[0].Endpoint)

	keyed, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 137}},
		common.VendorSettings{"apiKey": "secret-key"})
	require.NoError(t, err)
	require.Len(t, keyed, 1)
	assert.Equal(t, "https://api.pimlico.io/v2/137/rpc?apikey=secret-key", keyed[0].Endpoint)

	// Pimlico is a bundler, so it must not receive general EVM traffic.
	assert.Equal(t, []string{"*"}, keyed[0].IgnoreMethods)
	assert.Contains(t, keyed[0].AllowMethods, "eth_sendUserOperation")

	_, errNoKey := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{})
	require.Error(t, errNoKey)
	assert.Contains(t, errNoKey.Error(), "apiKey")

	_, errNoChain := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, common.VendorSettings{"apiKey": "k"})
	require.Error(t, errNoChain)
	assert.Contains(t, errNoChain.Error(), "chainId")
}

// Envio's key is optional and its root domain is configurable. An operator who
// points the vendor at a self-hosted HyperSync needs both to hold.
func TestEnvioVendor_GenerateConfigs_TheKeyIsOptionalAndTheRootDomainIsConfigurable(t *testing.T) {
	v := CreateEnvioVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	anon, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{})
	require.NoError(t, err, "envio serves anonymous traffic, so a missing key is not an error")
	require.Len(t, anon, 1)
	assert.Equal(t, "https://1.rpc.hypersync.xyz", anon[0].Endpoint)

	keyed, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 10}},
		common.VendorSettings{"apiKey": "secret-key"})
	require.NoError(t, err)
	assert.Equal(t, "https://10.rpc.hypersync.xyz/secret-key", keyed[0].Endpoint,
		"the key is a path segment, not a query parameter")

	custom, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 8453}},
		common.VendorSettings{"rootDomain": "hypersync.internal"})
	require.NoError(t, err)
	assert.Equal(t, "https://8453.hypersync.internal", custom[0].Endpoint)

	// Envio is an indexer, so it answers block and log reads only.
	assert.Equal(t, []string{"*"}, custom[0].IgnoreMethods)
	assert.Contains(t, custom[0].AllowMethods, "eth_getLogs")

	_, errNoChain := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, common.VendorSettings{})
	require.Error(t, errNoChain)
	assert.Contains(t, errNoChain.Error(), "chainId")
}

func TestRoutemeshVendor_GenerateConfigs_TheBaseUrlSettingOverridesTheDefault(t *testing.T) {
	v := CreateRoutemeshVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	def, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}},
		common.VendorSettings{"apiKey": "secret-key"})
	require.NoError(t, err)
	require.Len(t, def, 1)
	assert.Equal(t, "https://lb.routemes.sh/rpc/1/secret-key", def[0].Endpoint)

	custom, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}},
		common.VendorSettings{"apiKey": "secret-key", "baseURL": "lb.internal"})
	require.NoError(t, err)
	assert.Equal(t, "https://lb.internal/rpc/1/secret-key", custom[0].Endpoint)

	// The key is checked before the chain ID, so an upstream missing both must
	// name the key.
	_, errNoKey := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, common.VendorSettings{})
	require.Error(t, errNoKey)
	assert.Contains(t, errNoKey.Error(), "apiKey")

	_, errNoChain := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}},
		common.VendorSettings{"apiKey": "k"})
	require.Error(t, errNoChain)
	assert.Contains(t, errNoChain.Error(), "chainId")
}

// Etherspot picks the host from the chain: a mainnet gets a named bundler, a
// testnet gets one shared host. An unknown chain matches neither, and
// generateUrl then formats an EMPTY string and parses it without complaint.
// This test pins that; see the report.
func TestEtherspotVendor_GenerateConfigs_TheChainPicksTheHostAndAnUnknownChainPicksNone(t *testing.T) {
	v := CreateEtherspotVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	mainnet, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 137}},
		common.VendorSettings{"apiKey": "public"})
	require.NoError(t, err)
	require.Len(t, mainnet, 1)
	assert.Equal(t, "https://polygon-bundler.etherspot.io/", mainnet[0].Endpoint,
		"a mainnet is addressed by name, and the public keyword adds no query")

	testnet, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 84532}},
		common.VendorSettings{"apiKey": "secret-key"})
	require.NoError(t, err)
	assert.Equal(t, "https://testnet-rpc.etherspot.io/v1/84532?apikey=secret-key", testnet[0].Endpoint,
		"a testnet is addressed by number on one shared host")

	unknown, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}},
		common.VendorSettings{"apiKey": "secret-key"})
	require.NoError(t, err, "today an unknown chain is accepted")
	require.Len(t, unknown, 1)
	assert.Equal(t, "?apikey=secret-key", unknown[0].Endpoint,
		"the host is missing entirely; every other vendor reports an unsupported chain instead")
}

// dRPC dropped its tier structure in 2025, so the model is flat: 20 CU for a
// billable method, 0 for an informational one, and the "*" default for
// anything unlisted. The unlisted method is the case that matters, because
// dRPC keeps adding methods.
func TestDrpcVendor_CreditUnits_ChargesTheFlatRateAndHonoursAnOverride(t *testing.T) {
	v := CreateDrpcVendor().(*DrpcVendor)

	req := func(method string) *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`))
	}

	assert.Equal(t, int64(20), v.CreditUnits(req("eth_getLogs"), nil))
	assert.Equal(t, int64(0), v.CreditUnits(req("eth_chainId"), nil),
		"informational methods are free")
	assert.Equal(t, int64(20), v.CreditUnits(req("drpc_somethingNew"), nil),
		"an unlisted method falls back to the documented default, not to zero")
	assert.Equal(t, int64(20), v.CreditUnits(req("debug_traceTransaction"), nil),
		"flat pricing means debug and trace cost the same as a plain read")

	ups := &common.UpstreamConfig{CreditUnits: map[string]int64{"eth_getLogs": 5}}
	assert.Equal(t, int64(5), v.CreditUnits(req("eth_getLogs"), ups),
		"the operator's per-method override wins")
	assert.Equal(t, int64(20), v.CreditUnits(req("eth_call"), ups),
		"an override for one method leaves the rest on the vendor table")
}
