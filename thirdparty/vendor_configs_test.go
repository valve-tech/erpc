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
