package util

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// IsNativeProtocol decides whether eRPC dials an endpoint directly or hands
// it to a vendor for expansion. A wrong answer either dials a vendor alias
// as a URL or asks a vendor to expand a plain URL — both break the upstream
// at boot.
func TestIsNativeProtocol_AcceptsEveryDialableScheme(t *testing.T) {
	for _, ep := range []string{
		"http://localhost:8545",
		"https://example.com",
		"ws://localhost:8546",
		"wss://example.com",
		"grpc://example.com:443",
		"grpc+bds://example.com",
	} {
		require.True(t, IsNativeProtocol(ep), "%q is dialable directly", ep)
	}
}

func TestIsNativeProtocol_RejectsVendorAliasesAndLookalikes(t *testing.T) {
	// The lookalikes matter: `httpsx://` and `wsss://` share a prefix with
	// no real scheme, and a substring match rather than a prefix match
	// would wrongly accept `x-http://`.
	for _, ep := range []string{
		"alchemy://key",
		"drpc://key",
		"envio://rpc.hypersync.xyz",
		"repository://evm-public-endpoints.erpc.cloud",
		"x-http://example.com",
		"HTTP://example.com",
		"",
		"example.com",
	} {
		require.False(t, IsNativeProtocol(ep), "%q must not be treated as dialable", ep)
	}
}
