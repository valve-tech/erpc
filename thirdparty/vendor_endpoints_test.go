package thirdparty

import (
	"context"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests continue the sampling doctrine set out at the top of
// vendor_configs_test.go: a vendor's chain table is data, so listing its chains
// back only restates the data. What is checked here is the shape — the order
// the guards run in, the format of the URL that comes out, the per-chain
// exceptions to that format, and what happens to input the vendor cannot use.

// -----------------------------------------------------------------------------
// dwellir — a chain table that also picks the subdomain
// -----------------------------------------------------------------------------

// Dwellir is the only vendor whose table maps a chain to a SUBDOMAIN rather
// than a path segment, and one chain then takes an extra path on top. Both are
// sampled; the other eighty-odd rows are data.
func TestDwellirVendor_GenerateConfigs_TheChainPicksTheSubdomainAndAvalancheTakesAnExtraPath(t *testing.T) {
	v := CreateDwellirVendor()
	logger := zerolog.Nop()
	ctx := context.Background()
	settings := common.VendorSettings{"apiKey": "k3y"}

	mainnet, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)
	require.NoError(t, err)
	require.Len(t, mainnet, 1)
	assert.Equal(t, "https://api-ethereum-mainnet.n.dwellir.com/k3y", mainnet[0].Endpoint)
	assert.Equal(t, common.UpstreamTypeEvm, mainnet[0].Type)
	assert.NotNil(t, mainnet[0].JsonRpc, "the vendor fills in the json-rpc block it needs")

	// Avalanche's C-Chain lives behind an extra path. Without it the upstream
	// reaches the node but not the EVM.
	avalanche, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 43114}}, settings)
	require.NoError(t, err)
	assert.Equal(t,
		"https://api-avalanche-mainnet-archive.n.dwellir.com/k3y/ext/bc/C/rpc",
		avalanche[0].Endpoint)
}

// The guards run in a fixed order, and each one names the field it wants. An
// operator reading the message has to be able to act on it.
func TestDwellirVendor_GenerateConfigs_ChecksItsInputsInOrder(t *testing.T) {
	v := CreateDwellirVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	// A preset endpoint is taken as-is and skips every later check, including
	// the api key. That is the escape hatch for an operator who has their own
	// credentialed URL.
	preset, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Endpoint: "https://my.dwellir.com/key"}, common.VendorSettings{})
	require.NoError(t, err)
	require.Len(t, preset, 1)
	assert.Equal(t, "https://my.dwellir.com/key", preset[0].Endpoint)

	_, errNoKey := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{})
	require.Error(t, errNoKey)
	assert.Contains(t, errNoKey.Error(), "apiKey")

	// Unlike the six vendors pinned by
	// TestSixVendors_GenerateConfigs_PanicOnAMissingEvmBlock, dwellir guards.
	_, errNoEvm := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{}, common.VendorSettings{"apiKey": "k"})
	require.Error(t, errNoEvm)
	assert.Contains(t, errNoEvm.Error(), "evm")

	_, errNoChain := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, common.VendorSettings{"apiKey": "k"})
	require.Error(t, errNoChain)
	assert.Contains(t, errNoChain.Error(), "chainId")

	_, errUnknown := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}},
		common.VendorSettings{"apiKey": "k"})
	require.Error(t, errUnknown)
	assert.Contains(t, errUnknown.Error(), "424242")
}

// Dwellir swallows the chain-ID parse error that ankr, blastapi, blockpi,
// etherspot, infura, llama, onfinality and thirdweb all report. A typo in a
// network ID therefore reads as "dwellir does not serve this chain" rather than
// as a config mistake — see the report.
func TestDwellirVendor_SupportsNetwork_AMalformedChainIdIsASilentNoNotAnError(t *testing.T) {
	v := CreateDwellirVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	supported, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "solana:mainnet")
	require.NoError(t, err)
	assert.False(t, supported)

	supported, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:0xdeadbeef")
	require.NoError(t, err, "today dwellir reports nothing; the other vendors return the parse error")
	assert.False(t, supported)

	supported, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:1")
	require.NoError(t, err)
	assert.True(t, supported)

	supported, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:424242")
	require.NoError(t, err)
	assert.False(t, supported)
}

// -----------------------------------------------------------------------------
// etherspot — two tables, one answer
// -----------------------------------------------------------------------------

// Etherspot splits its chains across a mainnet table and a testnet table, and
// SupportsNetwork must consult both. Consulting only the first would strand
// every testnet it serves.
func TestEtherspotVendor_SupportsNetwork_ConsultsBothTables(t *testing.T) {
	v := CreateEtherspotVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	mainnet, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:1")
	require.NoError(t, err)
	assert.True(t, mainnet)

	testnet, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:11155111")
	require.NoError(t, err)
	assert.True(t, testnet, "sepolia is in the testnet table only")

	unknown, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:424242")
	require.NoError(t, err)
	assert.False(t, unknown)

	nonEvm, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "solana:mainnet")
	require.NoError(t, err)
	assert.False(t, nonEvm)

	// Etherspot reports a chain ID it cannot parse, where dwellir swallows it.
	malformed, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:0xdead")
	require.Error(t, err)
	assert.False(t, malformed)
}

// -----------------------------------------------------------------------------
// goldsky — one secret, every chain, three ways to configure it
// -----------------------------------------------------------------------------

// Goldsky Edge takes the chain as a path segment and the secret as a query
// parameter, so a single credential covers every chain. The three ways to
// supply it must land on the same URL, or an operator who switches from the
// shorthand to settings silently loses the secret.
func TestGoldskyVendor_GenerateConfigs_EveryWayOfSupplyingTheSecretReachesTheSameUrl(t *testing.T) {
	v := CreateGoldskyVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	fromSettings, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 8453}},
		common.VendorSettings{"secret": "s3cret"})
	require.NoError(t, err)
	require.Len(t, fromSettings, 1)
	assert.Equal(t, "https://edge.goldsky.com/standard/evm/8453?secret=s3cret", fromSettings[0].Endpoint)
	assert.Equal(t, common.UpstreamTypeEvm, fromSettings[0].Type)

	// The shorthand's authority IS the secret, not a host. Dialling it as a
	// host would send the credential to whatever DNS resolves it to.
	fromShorthand, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{
			Endpoint: "evm+goldsky://s3cret",
			Evm:      &common.EvmUpstreamConfig{ChainId: 8453},
		}, common.VendorSettings{})
	require.NoError(t, err)
	assert.Equal(t, "https://edge.goldsky.com/standard/evm/8453?secret=s3cret", fromShorthand[0].Endpoint)

	// The tier is a route prefix, so it must survive both paths.
	tiered, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}},
		common.VendorSettings{"secret": "s3cret", "tier": "premium"})
	require.NoError(t, err)
	assert.Equal(t, "https://edge.goldsky.com/premium/evm/1?secret=s3cret", tiered[0].Endpoint)

	// A concrete endpoint that is not the shorthand passes through whole; only
	// the type is filled in.
	preset, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{
			Endpoint: "https://edge.goldsky.com/standard/evm/1?secret=already",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}, common.VendorSettings{})
	require.NoError(t, err)
	assert.Equal(t, "https://edge.goldsky.com/standard/evm/1?secret=already", preset[0].Endpoint)
	assert.Equal(t, common.UpstreamTypeEvm, preset[0].Type)
}

func TestGoldskyVendor_GenerateConfigs_RejectsAnUpstreamItCannotAddress(t *testing.T) {
	v := CreateGoldskyVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	// Goldsky guards, unlike the six vendors that panic here.
	_, errNoEvm := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{}, common.VendorSettings{"secret": "s"})
	require.Error(t, errNoEvm)
	assert.Contains(t, errNoEvm.Error(), "evm")

	// The shorthand path guards too, on its own line.
	_, errShorthandNoEvm := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Endpoint: "goldsky://s3cret"}, common.VendorSettings{})
	require.Error(t, errShorthandNoEvm)
	assert.Contains(t, errShorthandNoEvm.Error(), "evm")

	_, errNoChain := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, common.VendorSettings{"secret": "s"})
	require.Error(t, errNoChain)
	assert.Contains(t, errNoChain.Error(), "chainId")
}

// -----------------------------------------------------------------------------
// onfinality and thirdweb — the last two static-table config builders
// -----------------------------------------------------------------------------

// OnFinality escapes its key into the query string, so a key containing a query
// separator cannot append parameters of its own.
func TestOnFinalityVendor_GenerateConfigs_EscapesTheKeyAndChecksItsInputsInOrder(t *testing.T) {
	v := CreateOnFinalityVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	built, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}},
		common.VendorSettings{"apiKey": "a&b=c"})
	require.NoError(t, err)
	require.Len(t, built, 1)
	assert.Equal(t, "https://eth.api.onfinality.io/rpc?apikey=a%26b%3Dc", built[0].Endpoint)
	assert.Equal(t, common.UpstreamTypeEvm, built[0].Type)

	preset, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Endpoint: "https://eth.api.onfinality.io/rpc?apikey=k"},
		common.VendorSettings{})
	require.NoError(t, err)
	assert.Equal(t, "https://eth.api.onfinality.io/rpc?apikey=k", preset[0].Endpoint)

	_, errNoKey := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{})
	require.Error(t, errNoKey)
	assert.Contains(t, errNoKey.Error(), "apiKey")

	_, errNoEvm := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{}, common.VendorSettings{"apiKey": "k"})
	require.Error(t, errNoEvm)
	assert.Contains(t, errNoEvm.Error(), "evm")

	_, errNoChain := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, common.VendorSettings{"apiKey": "k"})
	require.Error(t, errNoChain)
	assert.Contains(t, errNoChain.Error(), "chainId")

	_, errUnknown := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}},
		common.VendorSettings{"apiKey": "k"})
	require.Error(t, errUnknown)
	assert.Contains(t, errUnknown.Error(), "424242")
}

// Thirdweb has no chain table at all: the chain is a subdomain and the client
// ID is the whole path. Any chain builds a URL, and only the probe decides
// whether it is served.
func TestThirdwebVendor_GenerateConfigs_TheChainIsTheSubdomainAndAnyChainBuilds(t *testing.T) {
	v := CreateThirdwebVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	built, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 424242}},
		common.VendorSettings{"clientId": "cli3nt"})
	require.NoError(t, err)
	require.Len(t, built, 1)
	assert.Equal(t, "https://424242.rpc.thirdweb.com/cli3nt", built[0].Endpoint)
	assert.Equal(t, common.UpstreamTypeEvm, built[0].Type)

	preset, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Endpoint: "https://1.rpc.thirdweb.com/other"}, common.VendorSettings{})
	require.NoError(t, err)
	assert.Equal(t, "https://1.rpc.thirdweb.com/other", preset[0].Endpoint)

	_, errNoClient := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{})
	require.Error(t, errNoClient)
	assert.Contains(t, errNoClient.Error(), "clientId")
}

// -----------------------------------------------------------------------------
// drpc — the two things it does that no other vendor does
// -----------------------------------------------------------------------------

// dRPC load-balances across nodes that do not all carry the same methods, so a
// "method not supported" from one node says nothing about the next. Every other
// vendor lets eRPC learn from that answer and stop asking. dRPC must not, and
// it turns the learning off on EVERY upstream it is handed — including one that
// arrived with its own endpoint and skips the whole build below.
func TestDrpcVendor_GenerateConfigs_TurnsOffMethodLearningEvenForAPresetEndpoint(t *testing.T) {
	v := CreateDrpcVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	preset, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Endpoint: "https://lb.drpc.org/ogrpc?network=ethereum&dkey=k"},
		common.VendorSettings{})
	require.NoError(t, err)
	require.Len(t, preset, 1)
	require.NotNil(t, preset[0].AutoIgnoreUnsupportedMethods)
	assert.False(t, *preset[0].AutoIgnoreUnsupportedMethods)

	// dRPC also fills in the type for an endpoint it recognises as its own,
	// which no other vendor does on the preset path.
	assert.Equal(t, common.UpstreamTypeEvm, preset[0].Type)

	// An endpoint it does not own keeps whatever type it arrived with.
	foreign, err := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Endpoint: "https://not-drpc.example/rpc"}, common.VendorSettings{})
	require.NoError(t, err)
	assert.Empty(t, string(foreign[0].Type))
}

// The chains URL is operator-supplied, so it is the one input that could point
// eRPC's bootstrap at an arbitrary scheme. It is validated before any fetch.
func TestDrpcVendor_GenerateConfigs_ChecksItsInputsBeforeFetchingAnything(t *testing.T) {
	v := CreateDrpcVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	_, errNoKey := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{})
	require.Error(t, errNoKey)
	assert.Contains(t, errNoKey.Error(), "apiKey")

	_, errNoEvm := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{}, common.VendorSettings{"apiKey": "k"})
	require.Error(t, errNoEvm)
	assert.Contains(t, errNoEvm.Error(), "evm")

	_, errNoChain := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, common.VendorSettings{"apiKey": "k"})
	require.Error(t, errNoChain)
	assert.Contains(t, errNoChain.Error(), "chainId")

	_, errBadURL := v.GenerateConfigs(ctx, &logger,
		&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}},
		common.VendorSettings{"apiKey": "k", "chainsUrl": "file:///etc/passwd"})
	require.Error(t, errBadURL)
	assert.NotContains(t, errBadURL.Error(), "unsupported network",
		"the URL must be rejected before the chain lookup, not after")
}

// -----------------------------------------------------------------------------
// ownership
// -----------------------------------------------------------------------------

// OwnsUpstream is how an endpoint with no vendor name finds its vendor. Two
// vendors claiming one endpoint hands it to whichever the registry reaches
// first; none claiming it drops the vendor's error handling and credit
// accounting silently. So the shape checked here is exclusivity, not the
// individual host strings.
func TestEveryVendor_OwnsUpstream_ClaimsItsOwnSchemeAndNobodyElses(t *testing.T) {
	registry := NewVendorsRegistry()

	claimantsOf := func(endpoint string) []string {
		ups := &common.UpstreamConfig{Endpoint: endpoint}
		claimants := []string{}
		for _, v := range registry.thirdparty {
			if v.OwnsUpstream(ups) {
				claimants = append(claimants, v.Name())
			}
		}
		return claimants
	}

	// Llama is the one built-in with no scheme claim: common/defaults.go turns
	// "llama://<key>" into a provider before ownership is ever consulted, so
	// the vendor only ever sees a concrete llamarpc.com URL.
	for _, name := range []string{
		"alchemy", "ankr", "blastapi", "blockdaemon", "blockpi", "chainstack",
		"conduit", "drpc", "dwellir", "envio", "erpc", "etherspot", "goldsky",
		"infura", "onfinality", "pimlico", "quicknode", "repository",
		"satelink", "superchain", "tenderly", "thirdweb",
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, []string{name}, claimantsOf(name+"://anything"),
				"the bare scheme must be claimed by exactly one vendor")

			// Dwellir is the only one of the twenty-two that does not also
			// claim the "evm+" form, though common/defaults.go accepts
			// "evm+dwellir://" as a shorthand. Today nothing observes the gap,
			// because that conversion strips the scheme before ownership is
			// consulted — see the report.
			want := []string{name}
			if name == "dwellir" {
				want = []string{}
			}
			assert.Equal(t, want, claimantsOf("evm+"+name+"://anything"),
				"the evm+ form must be claimed by the same single vendor")
		})
	}
}

// A vendor named explicitly in the config owns its upstream whatever the
// endpoint looks like. Without this an operator who points a named provider at
// a private mirror loses that vendor's error classification.
func TestVendorsWithANameClaim_OwnsUpstream_HonourTheConfiguredVendorName(t *testing.T) {
	for _, name := range []string{"alchemy", "conduit", "erpc", "goldsky", "routemesh", "superchain", "tenderly"} {
		t.Run(name, func(t *testing.T) {
			v := NewVendorsRegistry().LookupByName(name)
			require.NotNil(t, v)
			assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{
				Endpoint:   "https://private-mirror.internal/rpc",
				VendorName: name,
			}))
			assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{
				Endpoint: "https://private-mirror.internal/rpc",
			}), "with no name and no recognised host there is nothing to claim on")
		})
	}
}

// -----------------------------------------------------------------------------
// the pass-through normalisers
// -----------------------------------------------------------------------------

// Seven vendors share one normaliser body: copy the error's data field into
// details, then return nil so the generic normaliser decides the class. They
// never classify, which is the part that matters — a vendor that returned an
// error here would override eRPC's own retry decision.
func TestPassThroughNormalisers_CarryTheDataFieldAndNeverClassify(t *testing.T) {
	for _, name := range []string{"ankr", "blastapi", "chainstack", "erpc", "goldsky", "onfinality", "tenderly"} {
		t.Run(name, func(t *testing.T) {
			v := NewVendorsRegistry().LookupByName(name)
			require.NotNil(t, v)

			details := map[string]interface{}{}
			err := classifyWith(t, v, 400, -32000, "execution reverted", "0xdeadbeef", details)
			require.NoError(t, err, "this vendor must leave the classification to the generic normaliser")
			assert.Equal(t, "0xdeadbeef", details["data"])

			// An error with no data leaves the caller's details untouched but
			// for the key the vendor always writes.
			empty := map[string]interface{}{}
			require.NoError(t, classifyWith(t, v, 500, -32603, "internal", "", empty))
			assert.NotContains(t, empty, "data")
		})
	}
}

// The normaliser is handed whatever the response parsed into. A body that is
// not a JSON-RPC response must not reach the field access below it.
func TestPassThroughNormalisers_IgnoreANonJsonRpcBody(t *testing.T) {
	for _, name := range []string{"ankr", "blastapi", "chainstack", "erpc", "goldsky", "onfinality", "tenderly"} {
		t.Run(name, func(t *testing.T) {
			v := NewVendorsRegistry().LookupByName(name)
			require.NotNil(t, v)
			assert.NoError(t, v.GetVendorSpecificErrorIfAny(nil,
				&http.Response{StatusCode: 500}, map[string]interface{}{"error": "x"},
				map[string]interface{}{}))
		})
	}
}
