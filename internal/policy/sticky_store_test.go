package policy

import (
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// The sticky register is what stops the primary upstream flapping between
// ticks. Its key resolution decides HOW MANY slots share one primary: a
// wrong axis either fragments the register (every slot flaps on its own)
// or over-shares it (one method's outage drags every other method with it).

func TestResolveStickyKey_NetworkScopeCollapsesMethodAndFinality(t *testing.T) {
	a := resolveStickyKey(common.EvalScopeNetwork, "evm:1", "eth_call", "realtime")
	b := resolveStickyKey(common.EvalScopeNetwork, "evm:1", "eth_getLogs", "finalized")
	require.Equal(t, a, b, "network scope must give every slot on a network the same key")
	require.Equal(t, stickyKey{network: "evm:1", method: "*", finality: "*"}, a)
}

func TestResolveStickyKey_NetworkMethodScopeKeepsMethodsApart(t *testing.T) {
	call := resolveStickyKey(common.EvalScopeNetworkMethod, "evm:1", "eth_call", "realtime")
	logs := resolveStickyKey(common.EvalScopeNetworkMethod, "evm:1", "eth_getLogs", "realtime")
	require.NotEqual(t, call, logs, "two methods must not share a primary under network-method scope")
	require.Equal(t, stickyKey{network: "evm:1", method: "eth_call", finality: "*"}, call)

	sameMethodOtherFinality := resolveStickyKey(common.EvalScopeNetworkMethod, "evm:1", "eth_call", "finalized")
	require.Equal(t, call, sameMethodOtherFinality, "finality must be collapsed at this scope")
}

func TestResolveStickyKey_NetworkFinalityScopeKeepsFinalitiesApart(t *testing.T) {
	realtime := resolveStickyKey(common.EvalScopeNetworkFinality, "evm:1", "eth_call", "realtime")
	finalized := resolveStickyKey(common.EvalScopeNetworkFinality, "evm:1", "eth_call", "finalized")
	require.NotEqual(t, realtime, finalized)
	require.Equal(t, stickyKey{network: "evm:1", method: "*", finality: "realtime"}, realtime)

	otherMethod := resolveStickyKey(common.EvalScopeNetworkFinality, "evm:1", "eth_getLogs", "realtime")
	require.Equal(t, realtime, otherMethod, "method must be collapsed at this scope")
}

func TestResolveStickyKey_MostGranularScopeSharesNothing(t *testing.T) {
	k := resolveStickyKey(common.EvalScopeNetworkMethodFinality, "evm:1", "eth_call", "realtime")
	require.Equal(t, stickyKey{network: "evm:1", method: "eth_call", finality: "realtime"}, k)
}

func TestResolveStickyKey_UnknownScopeDegradesToPerSlotIndependence(t *testing.T) {
	// An unrecognised scope value must not blow up an in-flight eval, and
	// must not accidentally over-share. Per-slot independence is the safe
	// answer: worst case each slot picks its own primary.
	for _, scope := range []common.EvalScope{"", "galaxy", "network-method-finality-extra"} {
		got := resolveStickyKey(scope, "evm:1", "eth_call", "realtime")
		require.Equal(t, stickyKey{network: "evm:1", method: "eth_call", finality: "realtime"}, got,
			"scope %q must fall through to the most granular key", scope)
	}
}

func TestResolveStickyKey_NeverMixesNetworks(t *testing.T) {
	// Sharing a primary across networks would route one chain's traffic
	// using another chain's health.
	for _, scope := range []common.EvalScope{
		common.EvalScopeNetwork,
		common.EvalScopeNetworkMethod,
		common.EvalScopeNetworkFinality,
		common.EvalScopeNetworkMethodFinality,
	} {
		one := resolveStickyKey(scope, "evm:1", "eth_call", "realtime")
		ten := resolveStickyKey(scope, "evm:10", "eth_call", "realtime")
		require.NotEqual(t, one, ten, "scope %q leaked across networks", scope)
	}
}

func TestStickyStore_ColdStartIsDistinguishableFromAnEmptyPrimary(t *testing.T) {
	// "no primary yet" and "primary deliberately cleared" drive different
	// JS branches. Collapsing them would restart the anti-flap cooldown
	// on every tick.
	s := newStickyStore()
	k := stickyKey{network: "evm:1", method: "*", finality: "*"}

	primary, at, ok := s.Get(k)
	require.False(t, ok, "an unwritten key must report cold start")
	require.Equal(t, "", primary)
	require.Equal(t, int64(0), at)

	s.Set(k, "", 1234)
	primary, at, ok = s.Get(k)
	require.True(t, ok, "a written key must report present even with an empty primary")
	require.Equal(t, "", primary)
	require.Equal(t, int64(1234), at)
}

func TestStickyStore_SetReplacesBothFields(t *testing.T) {
	// LastSwitchAt is the anti-flap clock. A Set that kept the old value
	// would let the primary switch again immediately.
	s := newStickyStore()
	k := stickyKey{network: "evm:1", method: "eth_call", finality: "realtime"}

	s.Set(k, "rpc1", 100)
	s.Set(k, "rpc2", 200)

	primary, at, ok := s.Get(k)
	require.True(t, ok)
	require.Equal(t, "rpc2", primary)
	require.Equal(t, int64(200), at)
}

func TestStickyStore_KeysAreIndependent(t *testing.T) {
	s := newStickyStore()
	a := stickyKey{network: "evm:1", method: "eth_call", finality: "realtime"}
	b := stickyKey{network: "evm:1", method: "eth_getLogs", finality: "realtime"}

	s.Set(a, "rpc1", 100)
	s.Set(b, "rpc2", 200)

	pa, _, _ := s.Get(a)
	pb, _, _ := s.Get(b)
	require.Equal(t, "rpc1", pa)
	require.Equal(t, "rpc2", pb)
}

func TestStickyStore_SnapshotIsACopy(t *testing.T) {
	// The admin surface holds the snapshot while ticks keep writing. A
	// shared map would race and would show entries that appeared later.
	s := newStickyStore()
	k := stickyKey{network: "evm:1", method: "*", finality: "*"}
	s.Set(k, "rpc1", 100)

	snap := s.snapshot()
	require.Len(t, snap, 1)
	require.Equal(t, stickyEntry{Primary: "rpc1", LastSwitchAt: 100}, snap[k])

	s.Set(k, "rpc2", 200)
	s.Set(stickyKey{network: "evm:10"}, "rpc3", 300)
	require.Len(t, snap, 1, "the snapshot must not gain entries written after it was taken")
	require.Equal(t, "rpc1", snap[k].Primary, "the snapshot must not follow later writes")

	snap[k] = stickyEntry{Primary: "tampered"}
	live, _, _ := s.Get(k)
	require.Equal(t, "rpc2", live, "mutating the snapshot must not reach the register")
}

func TestStickyStore_SnapshotOfAnEmptyStoreIsEmptyNotNil(t *testing.T) {
	// Callers range over the result. A nil map is fine to range, but an
	// admin surface that marshals it would emit `null` instead of `{}`.
	snap := newStickyStore().snapshot()
	require.NotNil(t, snap)
	require.Empty(t, snap)
}

func TestStickyStore_EvictIdleDropsOnlyEntriesOlderThanTheCutoff(t *testing.T) {
	// Idle scopes must age out or the register grows without bound across
	// a long-running fleet. Evicting a live scope instead would reseed its
	// primary and cause exactly the flap the register exists to prevent.
	s := newStickyStore()
	old := stickyKey{network: "evm:1", method: "old", finality: "*"}
	edge := stickyKey{network: "evm:1", method: "edge", finality: "*"}
	fresh := stickyKey{network: "evm:1", method: "fresh", finality: "*"}
	s.Set(old, "rpc1", 100)
	s.Set(edge, "rpc2", 500)
	s.Set(fresh, "rpc3", 900)

	s.evictIdle(500)

	_, _, ok := s.Get(old)
	require.False(t, ok, "an entry older than the cutoff must be dropped")
	_, _, ok = s.Get(edge)
	require.True(t, ok, "an entry exactly at the cutoff must survive")
	_, _, ok = s.Get(fresh)
	require.True(t, ok, "a fresh entry must survive")
}

func TestStickyStore_EvictIdleOnAnEmptyStoreIsSafe(t *testing.T) {
	s := newStickyStore()
	s.evictIdle(1000)
	require.Empty(t, s.snapshot())
}

func TestStickyStore_ConcurrentReadsAndWritesStaySafe(t *testing.T) {
	// Every slot reads once per tick and writes on a switch. A missing
	// lock here is a map-concurrent-write crash of the whole proxy.
	s := newStickyStore()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			k := stickyKey{network: "evm:1", method: string(rune('a' + i%8)), finality: "*"}
			for j := 0; j < 100; j++ {
				s.Set(k, "rpc1", int64(j))
				s.Get(k)
				s.snapshot()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	require.Len(t, s.snapshot(), 8)
}
