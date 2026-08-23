package erpc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These cover the routing decision and the bookkeeping behind passthrough
// subscriptions — every `<ns>_subscribe` that is not eth's. Before this path
// existed such a call fell through to project.Forward as a one-shot: the
// upstream opened a real subscription and returned a real id, eRPC handed it
// back, and nothing ever registered a handler for it. The client held an id
// that could never deliver.

// The single most important assertion in this file: eth_subscribe must NOT
// take the passthrough path. It keeps its indexer backing, which gives it
// filter dedup and fan-out across upstreams; a passthrough subscription is
// pinned to one socket and has neither. Sending eth down here would be the
// regression that matters most.
func TestPassthroughRouting_LeavesTheEthPairOnTheIndexerPath(t *testing.T) {
	for _, method := range []string{MethodEthSubscribe, MethodEthUnsubscribe} {
		assert.False(t, IsPassthroughSubscriptionMethod(method),
			"%s must stay on the indexer-backed path", method)
		assert.True(t, IsSubscriptionMethod(method),
			"%s must still be recognised by the eth path", method)
	}
}

// A namespace this code has never heard of still routes. eRPC cannot hold a
// list of namespaces — reth serves msgboard today and the set is open-ended —
// so the shape of the method name is the whole test.
func TestPassthroughRouting_ClaimsEverySubscribeThatIsNotEths(t *testing.T) {
	for _, method := range []string{
		"msgboard_subscribe",
		"msgboard_unsubscribe",
		"debug_subscribe",
		"some_futureNamespaceInvention_subscribe",
	} {
		assert.True(t, IsPassthroughSubscriptionMethod(method),
			"%s must reach the passthrough path, not fall through to Forward", method)
		assert.False(t, IsSubscriptionMethod(method),
			"%s must not be mistaken for eth's", method)
	}
}

// The counterweight. An ordinary request must not be dragged onto the
// subscription path just because its name contains the word — this is what
// keeps the suffix test from becoming a substring test.
func TestPassthroughRouting_IgnoresEveryOtherMethod(t *testing.T) {
	for _, method := range []string{
		"eth_getBlockByNumber",
		"msgboard_addMessage",
		"msgboard_status",
		"eth_subscribeSomethingElse",
		"subscribe",
		"_subscribe_not_at_the_end",
		"",
	} {
		assert.False(t, IsPassthroughSubscriptionMethod(method),
			"%s is an ordinary request and must be forwarded normally", method)
	}
}

// The subscribe and unsubscribe halves must not claim each other's methods.
// A subscribe routed to the teardown branch would tear down nothing and
// answer the client with a success it never earned.
func TestPassthroughRouting_KeepsTheTwoHalvesApart(t *testing.T) {
	assert.True(t, IsGenericSubscribeMethod("msgboard_subscribe"))
	assert.False(t, IsGenericUnsubscribeMethod("msgboard_subscribe"))

	assert.True(t, IsGenericUnsubscribeMethod("msgboard_unsubscribe"))
	assert.False(t, IsGenericSubscribeMethod("msgboard_unsubscribe"),
		`"msgboard_unsubscribe" ends in "subscribe" as a substring; only the full suffix may match`)
}

// --- the per-connection registry --------------------------------------

// take() must hand a subscription over exactly once. Both teardown paths —
// an explicit unsubscribe and an upstream disconnect — call it, and they can
// race; a second caller unregistering the same handler would be harmless, but
// a second caller CLOSING the connection would not be.
func TestPassthroughRegistry_HandsASubscriptionOverExactlyOnce(t *testing.T) {
	reg := newPassthroughSubscriptions()
	reg.add(&passthroughSub{subID: "0xaaa", upstreamID: "up1"})
	require.Equal(t, 1, reg.len())

	sub, ok := reg.take("0xaaa")
	require.True(t, ok)
	require.Equal(t, "up1", sub.upstreamID)
	assert.Equal(t, 0, reg.len(), "take must remove what it returns")

	_, ok = reg.take("0xaaa")
	assert.False(t, ok, "a second take must not hand out the same subscription again")
}

// An id this connection does not hold is not an error — it is simply not ours.
// The unsubscribe path forwards such a request to an upstream rather than
// answering for it, so a miss here must be quiet.
func TestPassthroughRegistry_DoesNotClaimAnIdItNeverHeld(t *testing.T) {
	reg := newPassthroughSubscriptions()
	reg.add(&passthroughSub{subID: "0xaaa"})

	_, ok := reg.take("0xsomeoneelses")
	assert.False(t, ok)
	assert.Equal(t, 1, reg.len(), "a miss must not disturb what the connection does hold")
}

// drain is what runs on client-connection close. The handlers it releases live
// on the UPSTREAM's ws client, which outlives this connection and serves every
// other one, so anything left behind leaks for the life of the process.
func TestPassthroughRegistry_DrainReleasesEverythingExactlyOnce(t *testing.T) {
	reg := newPassthroughSubscriptions()
	for _, id := range []string{"0xaaa", "0xbbb", "0xccc"} {
		reg.add(&passthroughSub{subID: id})
	}

	drained := reg.drain()
	assert.Len(t, drained, 3, "every live subscription must be handed back for teardown")
	assert.Equal(t, 0, reg.len(), "the registry must be empty afterwards")

	seen := map[string]int{}
	for _, sub := range drained {
		seen[sub.subID]++
	}
	for id, n := range seen {
		assert.Equal(t, 1, n, "%s was handed back %d times", id, n)
	}

	assert.Empty(t, reg.drain(), "a second drain must release nothing")
}

// A re-subscribe under an id the connection already holds must replace it
// rather than accumulate. Two entries for one id would leave one handler
// registered forever after teardown.
func TestPassthroughRegistry_ReplacesRatherThanAccumulates(t *testing.T) {
	reg := newPassthroughSubscriptions()
	reg.add(&passthroughSub{subID: "0xaaa", upstreamID: "up1"})
	reg.add(&passthroughSub{subID: "0xaaa", upstreamID: "up2"})

	require.Equal(t, 1, reg.len())
	sub, ok := reg.take("0xaaa")
	require.True(t, ok)
	assert.Equal(t, "up2", sub.upstreamID, "the newer registration wins")
}
