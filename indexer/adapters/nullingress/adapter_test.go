package nullingress_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/nullingress"
	"github.com/stretchr/testify/require"
)

// nullingress is the forcing function for indexer.EventIngress: if the
// interface ever grows a WebSocket-shaped requirement, this adapter stops
// compiling. These tests pin the lifecycle contract every ingress must
// honour, so a future Kafka or gRPC adapter has an executable spec.

var _ indexer.EventIngress = (*nullingress.Adapter)(nil)

// captureSink records what the ingress pushed. It is the whole downstream
// world as far as an ingress is concerned.
type captureSink struct {
	got chan indexer.StreamEvent
}

func newCaptureSink() *captureSink {
	return &captureSink{got: make(chan indexer.StreamEvent, 16)}
}

func (s *captureSink) Ingest(ev indexer.StreamEvent) { s.got <- ev }

func (s *captureSink) next(t *testing.T) indexer.StreamEvent {
	t.Helper()
	select {
	case ev := <-s.got:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event reached the sink")
		return indexer.StreamEvent{}
	}
}

func headEvent(n int64, hash string) indexer.StreamEvent {
	return indexer.StreamEvent{
		Kind:       indexer.KindNewHead,
		NetworkId:  "evm:1",
		SourceId:   "null:test",
		Block:      indexer.BlockRef{Number: n, Hash: hash},
		Payload:    json.RawMessage(`{"number":"0x1"}`),
		ObservedAt: time.Unix(0, 0),
	}
}

func TestAdapter_NameIsStableAndMatchesConstruction(t *testing.T) {
	// The name keys per-source bookkeeping inside the indexer. A name that
	// changed between calls would split one source's dedup state in two.
	a := nullingress.New("null:mock1")
	require.Equal(t, "null:mock1", a.Name())
	require.Equal(t, "null:mock1", a.Name())
}

func TestAdapter_StartNeedsNoNetworkState_ProvingTheInterfaceIsTransportFree(t *testing.T) {
	// Start is handed a nil NetworkHandle. A working ingress proves the
	// interface does not secretly require WebSocket- or EVM-shaped network
	// state to begin pumping.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a := nullingress.New("null:test")
	sink := newCaptureSink()
	require.NoError(t, a.Start(ctx, nil, sink))
	defer func() { require.NoError(t, a.Stop(context.Background())) }()

	a.Push(headEvent(100, "0xA"))
	got := sink.next(t)
	require.Equal(t, int64(100), got.Block.Number)
	require.Equal(t, "0xA", got.Block.Hash)
	require.Equal(t, json.RawMessage(`{"number":"0x1"}`), got.Payload,
		"the payload must reach the sink verbatim — re-marshalling would lose upstream formatting")
}

func TestAdapter_PreservesPushOrder(t *testing.T) {
	// Head ordering drives the indexer's reorg tracker. An ingress that
	// reorders its own events would fabricate reorgs downstream.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a := nullingress.New("null:test")
	sink := newCaptureSink()
	require.NoError(t, a.Start(ctx, nil, sink))
	defer func() { require.NoError(t, a.Stop(context.Background())) }()

	for i := int64(1); i <= 5; i++ {
		a.Push(headEvent(i, "0x0"))
	}
	for i := int64(1); i <= 5; i++ {
		require.Equal(t, i, sink.next(t).Block.Number)
	}
}

func TestAdapter_SecondStartIsANoOpAndKeepsTheFirstSink(t *testing.T) {
	// The indexer may re-Start an ingress after a network re-registers.
	// A second pump would double-deliver every event, and the dedup window
	// downstream is time-bounded, so the duplicate can escape.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a := nullingress.New("null:test")
	first, second := newCaptureSink(), newCaptureSink()
	require.NoError(t, a.Start(ctx, nil, first))
	require.NoError(t, a.Start(ctx, nil, second), "a repeat Start must not error")
	defer func() { require.NoError(t, a.Stop(context.Background())) }()

	a.Push(headEvent(1, "0xA"))
	require.Equal(t, int64(1), first.next(t).Block.Number)

	select {
	case ev := <-second.got:
		t.Fatalf("the second Start swapped the sink and delivered %v", ev)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case ev := <-first.got:
		t.Fatalf("the event was delivered twice: %v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAdapter_StopEndsDelivery(t *testing.T) {
	// After Stop returns, the indexer assumes no further events arrive.
	// A late delivery would land on a torn-down egress set.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a := nullingress.New("null:test")
	sink := newCaptureSink()
	require.NoError(t, a.Start(ctx, nil, sink))

	a.Push(headEvent(1, "0xA"))
	require.Equal(t, int64(1), sink.next(t).Block.Number)

	require.NoError(t, a.Stop(context.Background()))
	a.Push(headEvent(2, "0xB"))
	select {
	case ev := <-sink.got:
		t.Fatalf("an event arrived after Stop: %v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAdapter_StopBeforeStartIsSafe(t *testing.T) {
	// Boot can fail between construction and Start. Stop must not panic on
	// a nil cancel func, or one bad upstream takes the process down.
	a := nullingress.New("null:test")
	require.NoError(t, a.Stop(context.Background()))
	require.NoError(t, a.Stop(context.Background()), "Stop must stay idempotent")
}

func TestAdapter_RestartsAfterStop(t *testing.T) {
	// Stop clears the started flag, so a reconnect can re-Start the same
	// adapter. Without that, a reconnected ingress would go permanently
	// silent while still reporting itself as registered.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a := nullingress.New("null:test")
	first := newCaptureSink()
	require.NoError(t, a.Start(ctx, nil, first))
	require.NoError(t, a.Stop(context.Background()))

	second := newCaptureSink()
	require.NoError(t, a.Start(ctx, nil, second))
	defer func() { require.NoError(t, a.Stop(context.Background())) }()

	a.Push(headEvent(7, "0xC"))
	require.Equal(t, int64(7), second.next(t).Block.Number)
}

func TestAdapter_ContextCancellationEndsDeliveryWithoutStop(t *testing.T) {
	// The indexer may cancel the whole tree instead of calling Stop. The
	// pump must honour the context or it outlives its owner.
	ctx, cancel := context.WithCancel(context.Background())
	a := nullingress.New("null:test")
	sink := newCaptureSink()
	require.NoError(t, a.Start(ctx, nil, sink))

	a.Push(headEvent(1, "0xA"))
	require.Equal(t, int64(1), sink.next(t).Block.Number)

	cancel()
	a.Push(headEvent(2, "0xB"))
	select {
	case ev := <-sink.got:
		t.Fatalf("an event arrived after the context was cancelled: %v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAdapter_EnsureFilterIsIdempotentPerKey(t *testing.T) {
	// The indexer refcounts filters and calls EnsureFilter on every new
	// subscriber. An adapter that treated a repeat call as a new
	// subscription would open one upstream subscription per client.
	ctx := context.Background()
	a := nullingress.New("null:test")

	require.NoError(t, a.EnsureFilter(ctx, "logs", "h1", nil))
	require.NoError(t, a.EnsureFilter(ctx, "logs", "h1", nil))
	require.Equal(t, []string{"logs:h1"}, a.ActiveFilters())
}

func TestAdapter_FilterKeyCombinesSubTypeAndParamsHash(t *testing.T) {
	// Two different filters must never collapse into one key, or one
	// client's unsubscribe silently kills another client's stream.
	ctx := context.Background()
	a := nullingress.New("null:test")

	require.NoError(t, a.EnsureFilter(ctx, "logs", "h1", nil))
	require.NoError(t, a.EnsureFilter(ctx, "logs", "h2", nil))
	require.NoError(t, a.EnsureFilter(ctx, "newPendingTransactions", "h1", nil))

	got := a.ActiveFilters()
	sort.Strings(got)
	require.Equal(t, []string{"logs:h1", "logs:h2", "newPendingTransactions:h1"}, got)
}

func TestAdapter_RemoveFilterDropsOnlyItsOwnKey(t *testing.T) {
	ctx := context.Background()
	a := nullingress.New("null:test")
	require.NoError(t, a.EnsureFilter(ctx, "logs", "h1", nil))
	require.NoError(t, a.EnsureFilter(ctx, "logs", "h2", nil))

	require.NoError(t, a.RemoveFilter(ctx, "logs", "h1"))
	require.Equal(t, []string{"logs:h2"}, a.ActiveFilters())
}

func TestAdapter_RemoveFilterForAnUnknownKeyIsANoOp(t *testing.T) {
	// The interface promises RemoveFilter is safe for a filter that was
	// never subscribed — teardown races make that call routine.
	ctx := context.Background()
	a := nullingress.New("null:test")
	require.NoError(t, a.EnsureFilter(ctx, "logs", "h1", nil))

	require.NoError(t, a.RemoveFilter(ctx, "logs", "never-subscribed"))
	require.NoError(t, a.RemoveFilter(ctx, "logs", "h1"))
	require.NoError(t, a.RemoveFilter(ctx, "logs", "h1"))
	require.Empty(t, a.ActiveFilters())
}

func TestAdapter_ActiveFiltersReturnsAnIndependentSnapshot(t *testing.T) {
	// Callers hold the slice while the indexer keeps subscribing. A shared
	// backing array would make the snapshot mutate under them.
	ctx := context.Background()
	a := nullingress.New("null:test")
	require.NoError(t, a.EnsureFilter(ctx, "logs", "h1", nil))

	snap := a.ActiveFilters()
	require.NoError(t, a.EnsureFilter(ctx, "logs", "h2", nil))
	require.Equal(t, []string{"logs:h1"}, snap, "the earlier snapshot must not grow")
	require.Len(t, a.ActiveFilters(), 2)
}

func TestAdapter_FreshAdapterHasNoFilters(t *testing.T) {
	require.Empty(t, nullingress.New("null:test").ActiveFilters())
}
