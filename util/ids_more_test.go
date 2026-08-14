package util

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// EvmNetworkId builds the network ID that keys the cache, the metrics
// labels and the upstream registry. A change in its shape silently splits
// one network into two across every one of those surfaces.
func TestEvmNetworkId_FormatsIntegerChainIds(t *testing.T) {
	require.Equal(t, "evm:1", EvmNetworkId(1))
	require.Equal(t, "evm:42161", EvmNetworkId(int64(42161)))
	require.Equal(t, "evm:0", EvmNetworkId(uint64(0)))
}

func TestEvmNetworkId_RoundTripsThroughIsValidNetworkId(t *testing.T) {
	// The two functions must agree. If the builder emitted something the
	// validator rejects, every generated provider upstream would fail
	// config validation at boot.
	require.True(t, IsValidNetworkId(EvmNetworkId(137)))
	require.True(t, IsValidNetworkId(SvmNetworkId("", "mainnet")))
	require.True(t, IsValidNetworkId(SvmNetworkId("fogo", "testnet")))
}

// IsValidIdentifier gates upstream, network and project IDs. These land in
// Prometheus label values and in URL paths, so a permissive answer lets a
// config inject a separator into either.
func TestIsValidIdentifier_AcceptsOnlyAlnumDashUnderscore(t *testing.T) {
	for _, s := range []string{"a", "Z", "0", "rpc-1", "rpc_1", "Alchemy-Mainnet_2"} {
		require.True(t, IsValidIdentifier(s), "%q should be a valid identifier", s)
	}
	for _, s := range []string{
		"",             // empty must not pass — it would produce a blank metric label
		"rpc 1",        // space
		"evm:1",        // colon is the network-ID separator
		"rpc/1",        // slash would escape a URL path segment
		"rpc.1",        // dot
		"rpc\n1",       // newline would forge a log line
		"rpc1\nrpc2",   // ditto, embedded
		"rpc1;drop",    // semicolon
		"rpcé1",        // non-ASCII
		"<VENDOR>",     // template placeholder that was never substituted
		"rpc1 ",        // trailing space
		"\ttab",        // leading tab
		"a+b",          // plus
		"a%20b",        // percent
		"tier:main",    // tag syntax
		"rpc1,rpc2",    // comma
		"*",            // wildcard
		"rpc1\rrpc2",   // carriage return
		"rpc1\x00rpc2", // NUL
	} {
		require.False(t, IsValidIdentifier(s), "%q must be rejected as an identifier", s)
	}
}

// IncrementAndGetIndex names auto-generated upstreams. Two upstreams that
// collide on a name overwrite each other in the registry, so the counter
// must be per-key and must never repeat a value under concurrency.
func TestIncrementAndGetIndex_CountsPerKeyAndStartsAtOne(t *testing.T) {
	require.Equal(t, "1", IncrementAndGetIndex("test-per-key", "alpha"))
	require.Equal(t, "2", IncrementAndGetIndex("test-per-key", "alpha"))
	require.Equal(t, "1", IncrementAndGetIndex("test-per-key", "beta"),
		"a different key must keep its own counter")
	require.Equal(t, "3", IncrementAndGetIndex("test-per-key", "alpha"))
}

func TestIncrementAndGetIndex_SeparatesPartsUnambiguously(t *testing.T) {
	// ("ab","c") and ("a","bc") must not share a counter. A naive
	// concatenation would merge them and hand out a duplicate index.
	require.Equal(t, "1", IncrementAndGetIndex("test-sep-ab", "c"))
	require.Equal(t, "1", IncrementAndGetIndex("test-sep-a", "bc"))
}

func TestIncrementAndGetIndex_NeverRepeatsUnderConcurrency(t *testing.T) {
	// Every goroutine waits on the same barrier, so they all hit the map
	// at once. Without the lock the read-modify-write loses updates and
	// two auto-named upstreams collide in the registry.
	const n = 2000
	var wg sync.WaitGroup
	start := make(chan struct{})
	out := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out[i] = IncrementAndGetIndex("test-concurrent")
		}(i)
	}
	close(start)
	wg.Wait()
	seen := make(map[string]bool, n)
	for _, v := range out {
		require.False(t, seen[v], "index %q was handed out twice — two upstreams would collide", v)
		seen[v] = true
	}
	require.Len(t, seen, n)
}
