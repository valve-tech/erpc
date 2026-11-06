package wsupstream

import (
	"testing"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// The fork's EnsureFilter/RemoveFilter/Stop tests drove the adapter through a
// stubbed forward that returned a synthetic subscribe reply. Upstream's
// adapter takes the subscription id from the WS client (WithWsSubscription),
// so a stubbed forward can no longer produce one, and Stop no longer exists.
// Upstream's own suite (adapter_reconnect_test.go) covers subscribe, repeat
// subscribe, remove during subscribe, retry, release and reconnect against a
// real WS server. What follows are the fork's checks that need no harness.

func newHandleFilterAdapter(t *testing.T) (*Adapter, *fakeSink) {
	t.Helper()
	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.ErrorLevel)
	sink := &fakeSink{events: make(chan indexer.StreamEvent, 8)}
	return &Adapter{
		upstreamID: "test-ws-upstream",
		networkID:  "evm:1",
		logger:     &logger,
		filters:    make(map[string]*filterSub),
		sink:       sink,
	}, sink
}

func TestFilterKey_CombinesSubTypeAndParamsHash(t *testing.T) {
	// The key is what the reconnect loop iterates. Two filters that
	// collapse into one key mean the second client's subscription is
	// silently served by the first client's upstream subscription.
	require.Equal(t, "logs:h1", filterKey("logs", "h1"))
	require.NotEqual(t, filterKey("logs", "h1"), filterKey("logs", "h2"))
	require.NotEqual(t, filterKey("logs", "h1"), filterKey("newPendingTransactions", "h1"))

	// Observed limitation, pinned rather than asserted away: the ":" join
	// is ambiguous if a sub type ever contains a colon. Today it cannot —
	// the subscription manager only reaches this adapter with "logs" or
	// "newPendingTransactions" — so nothing forces a stricter encoding.
	// This line is the tripwire if that ever changes.
	require.Equal(t, filterKey("a:b", "c"), filterKey("a", "b:c"))
}

func TestHandleFilter_PendingTxNotificationsCarryTheirOwnKind(t *testing.T) {
	a, sink := newHandleFilterAdapter(t)
	a.handleFilter(indexer.SubTypeNewPendingTransactions, "h1", []byte(`{"subscription":"0xs","result":"0xabc"}`))
	select {
	case ev := <-sink.events:
		require.Equal(t, indexer.KindPendingTx, ev.Kind)
		require.Equal(t, "h1", ev.FilterHash)
	case <-time.After(5 * time.Second):
		t.Fatal("the pending tx never reached the sink")
	}
}

func TestHandleFilter_MalformedNotificationIsDroppedNotPanicked(t *testing.T) {
	// One upstream sending a malformed frame must not take down the
	// process or stall the other filters on the same connection.
	a, sink := newHandleFilterAdapter(t)
	a.handleFilter("logs", "h1", []byte(`not json`))

	select {
	case ev := <-sink.events:
		t.Fatalf("a malformed notification produced an event: %v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	// A well-formed one still gets through afterwards.
	a.handleFilter("logs", "h1", []byte(`{"subscription":"0xs","result":{"blockNumber":"0x1"}}`))
	select {
	case ev := <-sink.events:
		require.Equal(t, "h1", ev.FilterHash)
	case <-time.After(5 * time.Second):
		t.Fatal("a valid notification was dropped after a malformed one")
	}
}

func TestHandleFilter_LogWithAnUnparseableBlockNumberStillReachesTheSink(t *testing.T) {
	// The block ref is opportunistic. Dropping the whole log because its
	// block number did not parse would lose real client data.
	a, sink := newHandleFilterAdapter(t)
	a.handleFilter("logs", "h1", []byte(`{"subscription":"0xs","result":{"blockNumber":"pending","address":"0x1"}}`))

	select {
	case ev := <-sink.events:
		require.Equal(t, indexer.KindLog, ev.Kind)
		require.Equal(t, "h1", ev.FilterHash)
		require.True(t, ev.Block.Zero(), "an unparseable block number must leave the ref empty")
	case <-time.After(5 * time.Second):
		t.Fatal("the log never reached the sink")
	}
}
