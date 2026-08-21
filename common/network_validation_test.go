package common

import (
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// IsValidNetwork gates providers[].onlyNetworks and ignoreNetworks. It used to
// match "evm:" and then "svm:" and then return false, so a config naming any
// other family there failed to load. These tests pin the two questions the
// function now keeps apart: the family owns the ID shape, and config owns the
// chain-id policy.

// TestIsValidNetwork_AsksTheRegistryNotAHardCodedList registers a fake family
// rather than testing btc directly. common cannot import architecture/btc —
// that package imports common — and naming one real family would test the
// wrong thing anyway. What matters is that ANY registered family is accepted,
// including the next one nobody has written.
func TestIsValidNetwork_AsksTheRegistryNotAHardCodedList(t *testing.T) {
	const family = "testfamily"

	require.False(t, IsValidNetwork(family+":mainnet"),
		"precondition: the family must be unknown before it registers")

	require.NoError(t, util.RegisterNetworkIdShape(family, func(body string) bool {
		return body == "mainnet"
	}))
	t.Cleanup(func() { util.UnregisterNetworkIdShape(family) })

	require.True(t, IsValidNetwork(family+":mainnet"),
		"a registered family decides its own IDs, with no edit to IsValidNetwork")
	require.False(t, IsValidNetwork(family+":nonsense"),
		"the family rejects a body it does not recognise, and that answer is honoured")
}

// TestIsValidNetwork_KeepsRejectingANonPositiveChainId pins the rule that does
// NOT live in the registry. util.IsEvmNetworkIdBody accepts a negative integer
// on purpose, because it answers "is this well formed" only. If IsValidNetwork
// ever delegates outright, these configs start loading.
func TestIsValidNetwork_KeepsRejectingANonPositiveChainId(t *testing.T) {
	for _, id := range []string{"evm:0", "evm:-1", "evm:-99999"} {
		require.False(t, IsValidNetwork(id), "config must reject %q", id)
	}
	require.True(t, util.IsValidNetworkId("evm:-1"),
		"precondition: the shape check accepts it, so IsValidNetwork is the only gate")
}

func TestIsValidNetwork_AcceptsTheBuiltinFamilies(t *testing.T) {
	for _, id := range []string{"evm:1", "evm:137", "svm:mainnet-beta", "svm:solana:devnet"} {
		require.True(t, IsValidNetwork(id), "must accept %q", id)
	}
	for _, id := range []string{"", "evm:", "evm:abc", "svm:", "nocolon", ":mainnet"} {
		require.False(t, IsValidNetwork(id), "must reject %q", id)
	}
}
