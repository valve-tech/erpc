package wsupstream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Filter bookkeeping decides which client subscriptions survive an upstream
// reconnect. A filter the adapter forgets goes permanently silent for the
// client that asked for it; a filter it never forgets keeps an upstream
// subscription alive forever and leaks it on every unsubscribe.

// filterHarness gives each test a live WS client, a stubbed subscribe RPC
// and a sink to observe. The subscribe RPC is stubbed because the real one
// rides upstream.Forward — that is the failsafe pipeline, not this layer.
type filterHarness struct {
	adapter *Adapter
	server  *notifyServer
	conn    *notifyConn
	sink    *fakeSink

	mu           sync.Mutex
	subscribed   []string // params[0] of every eth_subscribe seen
	unsubscribed []string // subscription IDs passed to eth_unsubscribe
	nextSubID    int
	subscribeErr error
}

func newFilterHarness(t *testing.T) *filterHarness {
	t.Helper()
	server := newNotifyServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.ErrorLevel)
	up := common.NewFakeUpstream("test-ws-upstream")
	ci, err := clients.NewWsJsonRpcClient(ctx, &logger, "test-project", up, server.wsURL(t), nil, nil)
	require.NoError(t, err)
	wsc := ci.(*clients.WsJsonRpcClient)

	select {
	case conn := <-server.newConn:
		h := &filterHarness{server: server, conn: conn, sink: &fakeSink{events: make(chan indexer.StreamEvent, 32)}}
		h.adapter = &Adapter{
			upstreamID: "test-ws-upstream",
			networkID:  "evm:1",
			wsClient:   wsc,
			logger:     &logger,
			filters:    make(map[string]*filterSub),
			sink:       h.sink,
			forward:    h.fakeForward,
		}
		require.True(t, wsc.IsConnected(), "the harness needs a connected client")
		return h
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the client connection")
		return nil
	}
}

// fakeForward answers eth_subscribe with a fresh ID and records
// eth_unsubscribe calls, so a test can assert the adapter really released
// the upstream subscription.
func (h *filterHarness) fakeForward(_ context.Context, nq *common.NormalizedRequest, _ bool) (*common.NormalizedResponse, error) {
	jr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch jr.Method {
	case methodEthSubscribe:
		if h.subscribeErr != nil {
			return nil, h.subscribeErr
		}
		first, _ := jr.Params[0].(string)
		h.subscribed = append(h.subscribed, first)
		h.nextSubID++
		return syntheticSubscribeResponse(fmt.Sprintf("0xsub%d", h.nextSubID)), nil
	case methodEthUnsubscribe:
		id, _ := jr.Params[0].(string)
		h.unsubscribed = append(h.unsubscribed, id)
		return syntheticSubscribeResponse("true"), nil
	}
	return nil, fmt.Errorf("unexpected method %q", jr.Method)
}

func (h *filterHarness) unsubscribes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.unsubscribed...)
}

func (h *filterHarness) subscribes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.subscribed...)
}

func (h *filterHarness) trackedKeys() []string {
	h.adapter.subsMu.Lock()
	defer h.adapter.subsMu.Unlock()
	out := make([]string, 0, len(h.adapter.filters))
	for k := range h.adapter.filters {
		out = append(out, k)
	}
	return out
}

func (h *filterHarness) upstreamSubFor(key string) string {
	h.adapter.subsMu.Lock()
	defer h.adapter.subsMu.Unlock()
	if s, ok := h.adapter.filters[key]; ok {
		return s.upstreamSub
	}
	return ""
}

// sendRawNotification pushes an eth_subscription frame whose `result` is
// the given raw JSON, so a test controls exactly what handleFilter parses.
func (nc *notifyConn) sendRawNotification(subID, rawResult string) {
	frame := fmt.Sprintf(
		`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":%q,"result":%s}}`,
		subID, rawResult)
	nc.writeMu.Lock()
	defer nc.writeMu.Unlock()
	_ = nc.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = nc.conn.WriteMessage(websocket.TextMessage, []byte(frame))
}

func logsParams(topic string) []interface{} {
	return []interface{}{"logs", map[string]interface{}{"topics": []interface{}{topic}}}
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

func TestEnsureFilter_SubscribesUpstreamAndTracksTheFilter(t *testing.T) {
	h := newFilterHarness(t)
	require.NoError(t, h.adapter.EnsureFilter(context.Background(), "logs", "h1", logsParams("0x1")))

	require.Equal(t, []string{"logs:h1"}, h.trackedKeys())
	require.Equal(t, []string{"logs"}, h.subscribes(),
		"the upstream subscribe must carry the sub type as its first param")
	require.Equal(t, "0xsub1", h.upstreamSubFor("logs:h1"))
}

func TestEnsureFilter_RepeatCallKeepsOneEntryAndReleasesTheOldUpstreamSub(t *testing.T) {
	// The indexer calls EnsureFilter again after a reconnect. A second
	// tracked entry would double every future resubscribe, and an
	// un-released old subscription would keep delivering duplicates.
	h := newFilterHarness(t)
	ctx := context.Background()
	require.NoError(t, h.adapter.EnsureFilter(ctx, "logs", "h1", logsParams("0x1")))
	require.NoError(t, h.adapter.EnsureFilter(ctx, "logs", "h1", logsParams("0x1")))

	require.Len(t, h.trackedKeys(), 1, "one filter must stay one tracked entry")
	require.Equal(t, "0xsub2", h.upstreamSubFor("logs:h1"),
		"the newest upstream subscription id must replace the previous one")
}

func TestEnsureFilter_TracksDistinctFiltersSeparately(t *testing.T) {
	h := newFilterHarness(t)
	ctx := context.Background()
	require.NoError(t, h.adapter.EnsureFilter(ctx, "logs", "h1", logsParams("0x1")))
	require.NoError(t, h.adapter.EnsureFilter(ctx, "logs", "h2", logsParams("0x2")))
	require.NoError(t, h.adapter.EnsureFilter(ctx, "newPendingTransactions", "h1", []interface{}{"newPendingTransactions"}))

	require.ElementsMatch(t, []string{"logs:h1", "logs:h2", "newPendingTransactions:h1"}, h.trackedKeys())
}

func TestEnsureFilter_KeepsTrackingTheFilterWhenTheUpstreamSubscribeFails(t *testing.T) {
	// A failed subscribe (open circuit breaker, upstream 5xx) must not
	// lose the filter. The reconnect retry loop iterates the tracked set,
	// so forgetting it here is the difference between "recovers on its
	// own" and "silent until the client resubscribes".
	h := newFilterHarness(t)
	h.mu.Lock()
	h.subscribeErr = errors.New("circuit breaker is open on upstream-level")
	h.mu.Unlock()

	err := h.adapter.EnsureFilter(context.Background(), "logs", "h1", logsParams("0x1"))
	require.Error(t, err, "the caller must learn the subscribe failed")
	require.Equal(t, []string{"logs:h1"}, h.trackedKeys(),
		"the filter must stay tracked so the reconnect loop retries it")
	require.Equal(t, "", h.upstreamSubFor("logs:h1"))
}

func TestEnsureFilter_DeliversNotificationsTaggedWithTheFilterHash(t *testing.T) {
	// The indexer fans an event out by FilterHash. A wrong or empty hash
	// sends one client's logs to every other client, or to nobody.
	h := newFilterHarness(t)
	require.NoError(t, h.adapter.EnsureFilter(context.Background(), "logs", "h1", logsParams("0x1")))

	h.conn.sendRawNotification("0xsub1", `{"blockNumber":"0x64","blockHash":"0xabc","address":"0xdead"}`)

	select {
	case ev := <-h.sink.events:
		require.Equal(t, indexer.KindLog, ev.Kind)
		require.Equal(t, "h1", ev.FilterHash)
		require.Equal(t, "evm:1", ev.NetworkId)
		require.Equal(t, "ws:test-ws-upstream", ev.SourceId)
		require.Equal(t, int64(100), ev.Block.Number,
			"the block ref must be extracted so the indexer can lifecycle-tag the log")
		require.Equal(t, "0xabc", ev.Block.Hash)
	case <-time.After(5 * time.Second):
		t.Fatal("the filter notification never reached the sink")
	}
}

func TestEnsureFilter_PendingTxNotificationsCarryTheirOwnKind(t *testing.T) {
	// Pending transactions have no block. Tagging them as logs would push
	// them through the canonical-chain tracker and fabricate reorgs.
	h := newFilterHarness(t)
	require.NoError(t, h.adapter.EnsureFilter(context.Background(),
		indexer.SubTypeNewPendingTransactions, "h9", []interface{}{indexer.SubTypeNewPendingTransactions}))

	h.conn.sendRawNotification("0xsub1", `"0xdeadbeef"`)

	select {
	case ev := <-h.sink.events:
		require.Equal(t, indexer.KindPendingTx, ev.Kind)
		require.Equal(t, "h9", ev.FilterHash)
		require.True(t, ev.Block.Zero(), "a pending tx carries no block reference")
	case <-time.After(5 * time.Second):
		t.Fatal("the pending-tx notification never reached the sink")
	}
}

func TestRemoveFilter_StopsTrackingAndReleasesTheUpstreamSubscription(t *testing.T) {
	// Without the upstream unsubscribe the node keeps streaming a filter
	// nobody consumes — the operator pays for it and the adapter drops it.
	h := newFilterHarness(t)
	ctx := context.Background()
	require.NoError(t, h.adapter.EnsureFilter(ctx, "logs", "h1", logsParams("0x1")))
	require.NoError(t, h.adapter.EnsureFilter(ctx, "logs", "h2", logsParams("0x2")))

	require.NoError(t, h.adapter.RemoveFilter(ctx, "logs", "h1"))

	require.Equal(t, []string{"logs:h2"}, h.trackedKeys())
	require.Equal(t, []string{"0xsub1"}, h.unsubscribes(),
		"only the removed filter's upstream subscription may be released")
}

func TestRemoveFilter_UnknownKeyIsANoOp(t *testing.T) {
	// Teardown races make this call routine. It must not error and must
	// not unsubscribe somebody else's subscription.
	h := newFilterHarness(t)
	ctx := context.Background()
	require.NoError(t, h.adapter.EnsureFilter(ctx, "logs", "h1", logsParams("0x1")))

	require.NoError(t, h.adapter.RemoveFilter(ctx, "logs", "never-subscribed"))
	require.NoError(t, h.adapter.RemoveFilter(ctx, "newPendingTransactions", "h1"))
	require.Equal(t, []string{"logs:h1"}, h.trackedKeys())
	require.Empty(t, h.unsubscribes())
}

func TestRemoveFilter_IsIdempotent(t *testing.T) {
	h := newFilterHarness(t)
	ctx := context.Background()
	require.NoError(t, h.adapter.EnsureFilter(ctx, "logs", "h1", logsParams("0x1")))

	require.NoError(t, h.adapter.RemoveFilter(ctx, "logs", "h1"))
	require.NoError(t, h.adapter.RemoveFilter(ctx, "logs", "h1"))
	require.Equal(t, []string{"0xsub1"}, h.unsubscribes(),
		"a second removal must not send a second unsubscribe")
}

func TestRemoveFilter_NeverSubscribedFilterSendsNoUnsubscribe(t *testing.T) {
	// A filter tracked but never subscribed upstream (breaker was open)
	// has no upstream ID. Sending eth_unsubscribe with an empty ID would
	// be a wasted RPC and an upstream error in the log.
	h := newFilterHarness(t)
	h.mu.Lock()
	h.subscribeErr = errors.New("breaker open")
	h.mu.Unlock()
	ctx := context.Background()
	_ = h.adapter.EnsureFilter(ctx, "logs", "h1", logsParams("0x1"))

	require.NoError(t, h.adapter.RemoveFilter(ctx, "logs", "h1"))
	require.Empty(t, h.trackedKeys())
	require.Empty(t, h.unsubscribes())
}

func TestStop_ReleasesEveryFilterAndTheHeadSubscription(t *testing.T) {
	h := newFilterHarness(t)
	ctx := context.Background()
	require.NoError(t, h.adapter.EnsureFilter(ctx, "logs", "h1", logsParams("0x1")))
	require.NoError(t, h.adapter.EnsureFilter(ctx, "logs", "h2", logsParams("0x2")))
	h.adapter.subsMu.Lock()
	h.adapter.newHeadsSubID = "0xheads"
	h.adapter.subsMu.Unlock()

	require.NoError(t, h.adapter.Stop(ctx))

	require.Empty(t, h.trackedKeys(), "Stop must release the adapter's filter state")
	require.ElementsMatch(t, []string{"0xheads", "0xsub1", "0xsub2"}, h.unsubscribes())
	require.False(t, h.adapter.Healthy(), "a stopped adapter must not report itself healthy")
}

func TestStop_PreventsAnyFurtherResubscribeEpoch(t *testing.T) {
	// A reconnect callback that fires after Stop would resurrect the
	// adapter and start subscribing against a torn-down indexer.
	h := newFilterHarness(t)
	require.NoError(t, h.adapter.Stop(context.Background()))

	h.adapter.startResubscribe()

	h.adapter.resubMu.Lock()
	stopped, cancel := h.adapter.stopped, h.adapter.resubCancel
	h.adapter.resubMu.Unlock()
	require.True(t, stopped)
	require.Nil(t, cancel, "no new retry epoch may start after Stop")
}

func TestStop_OnAnAdapterWithNoSubscriptionsIsSafe(t *testing.T) {
	h := newFilterHarness(t)
	require.NoError(t, h.adapter.Stop(context.Background()))
	require.NoError(t, h.adapter.Stop(context.Background()), "Stop must stay idempotent")
	require.Empty(t, h.unsubscribes())
}

func TestHandleFilter_MalformedNotificationIsDroppedNotPanicked(t *testing.T) {
	// One upstream sending a malformed frame must not take down the
	// process or stall the other filters on the same connection.
	h := newFilterHarness(t)
	h.adapter.handleFilter("logs", "h1", []byte(`not json`))

	select {
	case ev := <-h.sink.events:
		t.Fatalf("a malformed notification produced an event: %v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	// A well-formed one still gets through afterwards.
	h.adapter.handleFilter("logs", "h1", []byte(`{"subscription":"0xs","result":{"blockNumber":"0x1"}}`))
	select {
	case ev := <-h.sink.events:
		require.Equal(t, "h1", ev.FilterHash)
	case <-time.After(5 * time.Second):
		t.Fatal("a valid notification was dropped after a malformed one")
	}
}

func TestHandleFilter_LogWithAnUnparseableBlockNumberStillReachesTheSink(t *testing.T) {
	// The block ref is opportunistic. Dropping the whole log because its
	// block number did not parse would lose real client data.
	h := newFilterHarness(t)
	h.adapter.handleFilter("logs", "h1", []byte(`{"subscription":"0xs","result":{"blockNumber":"pending","address":"0x1"}}`))

	select {
	case ev := <-h.sink.events:
		require.Equal(t, indexer.KindLog, ev.Kind)
		require.Equal(t, "h1", ev.FilterHash)
		require.True(t, ev.Block.Zero(), "an unparseable block number must leave the ref empty")
	case <-time.After(5 * time.Second):
		t.Fatal("the log never reached the sink")
	}
}
