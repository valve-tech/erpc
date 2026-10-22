package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

func logPayload(block int64, idx int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"blockHash":"0x%064x","blockNumber":"0x%x","transactionHash":"0x%064x","logIndex":"0x%x","removed":false}`,
		block, block, idx/4, idx))
}

// newLogsIndexer registers evm:1 with two ingresses and one logs filter, and
// attaches an egress for it.
func newLogsIndexer(t *testing.T, opts Options) (*Indexer, *fakeEgress, string) {
	t.Helper()
	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.WarnLevel)
	idx := New(&logger, opts)
	idx.RegisterNetwork(newFakeNetwork("evm:1", 0))
	for _, name := range []string{"ws:a", "ws:b"} {
		if err := idx.AddIngress(context.Background(), "evm:1", &fakeIngress{name: name}); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := idx.EnsureFilter(context.Background(), "evm:1", "logs",
		[]interface{}{"logs", map[string]interface{}{}})
	if err != nil {
		t.Fatal(err)
	}
	eg := &fakeEgress{name: "eg", filters: map[string]struct{}{hash: {}}}
	idx.Attach(eg)
	return idx, eg, hash
}

func ingestLog(idx *Indexer, hash, source string, block int64, i int) {
	idx.Ingest(StreamEvent{
		Kind: KindLog, NetworkId: "evm:1", SourceId: source, FilterHash: hash,
		Payload: logPayload(block, i),
	})
}

// The second copy arrives more than 8,192 logs after the first: the old
// fixed window had forgotten it and delivered both.
func TestDedupCopiesFarApartDeliverOnce(t *testing.T) {
	idx, eg, hash := newLogsIndexer(t, Options{})
	const n = 20_000
	for i := 0; i < n; i++ {
		ingestLog(idx, hash, "ws:a", 100, i)
	}
	for i := 0; i < n; i++ {
		ingestLog(idx, hash, "ws:b", 100, i)
	}
	eg.mu.Lock()
	got := len(eg.received)
	eg.mu.Unlock()
	if got != n {
		t.Fatalf("delivered %d, want %d (one copy each)", got, n)
	}
}

// A full block at the protocol maximum, from two upstreams a whole block
// apart, reaches the subscriber exactly once per log.
func TestDedupFullProtocolBlockFromTwoUpstreams(t *testing.T) {
	var late atomic.Int32
	idx, eg, hash := newLogsIndexer(t, Options{OnDedupLate: func(string, EventKind) { late.Add(1) }})
	idx.GasLimits().Observe("evm:1", json.RawMessage(`{"gasLimit":"0x3938700"}`)) // 60,000,000
	n := MaxLogsPerBlock(60_000_000)                                              // 160,000
	for i := 0; i < n; i++ {
		ingestLog(idx, hash, "ws:a", 200, i)
	}
	for i := 0; i < n; i++ {
		ingestLog(idx, hash, "ws:b", 200, i)
	}
	eg.mu.Lock()
	got := len(eg.received)
	eg.mu.Unlock()
	if got != n {
		t.Fatalf("delivered %d, want %d (one copy each)", got, n)
	}
	if late.Load() != 0 {
		t.Fatalf("late = %d, want 0 inside the window", late.Load())
	}
}

// Past two full blocks of lag the window cannot vouch for a log any more;
// the late hook makes that visible instead of silent.
func TestDedupLateBeyondWindowIsCounted(t *testing.T) {
	w := NewProtocolDedupWindow(1, func() int { return 3 })
	for i := 0; i < 3; i++ {
		if fresh, late := w.MarkAt(fmt.Sprintf("b10:%d", i), 10); !fresh || late {
			t.Fatalf("block 10 key %d: fresh=%v late=%v", i, fresh, late)
		}
	}
	for i := 0; i < 3; i++ {
		w.MarkAt(fmt.Sprintf("b11:%d", i), 11) // evicts all of block 10
	}
	if fresh, late := w.MarkAt("b10:0", 10); !fresh || !late {
		t.Fatalf("re-sent block-10 key: fresh=%v late=%v, want true/true", fresh, late)
	}
	if fresh, late := w.MarkAt("b12:0", 12); !fresh || late {
		t.Fatalf("new block: fresh=%v late=%v, want true/false", fresh, late)
	}
	if fresh, _ := w.MarkAt("b12:0", 12); fresh {
		t.Fatal("a key still in the window must dedupe")
	}
	if !w.Mark("nokey-block") || w.Mark("nokey-block") {
		t.Fatal("Mark without a block must still dedupe")
	}
}

func TestIngestReportsLateDedup(t *testing.T) {
	var late atomic.Int32
	idx, eg, hash := newLogsIndexer(t, Options{
		DedupWindowSize: 1,
		OnDedupLate:     func(string, EventKind) { late.Add(1) },
	})
	idx.GasLimits().Observe("evm:1", json.RawMessage(`{"gasLimit":"0x177"}`)) // 375: 1 log/block, window 2
	ingestLog(idx, hash, "ws:a", 10, 0)
	ingestLog(idx, hash, "ws:a", 11, 1)
	ingestLog(idx, hash, "ws:a", 11, 2) // evicts block 10
	ingestLog(idx, hash, "ws:b", 10, 0) // its copy, too late to recognise
	if late.Load() != 1 {
		t.Fatalf("late = %d, want 1", late.Load())
	}
	eg.mu.Lock()
	defer eg.mu.Unlock()
	if len(eg.received) != 4 {
		t.Fatalf("delivered %d, want 4 (the late copy goes through, counted)", len(eg.received))
	}
}

// Keys more than dedupAgeBlocks behind the newest block leave the window,
// so a wide filter holds recent logs, not the protocol cap, in normal
// traffic.
func TestDedupAgesOutOldBlocks(t *testing.T) {
	w := NewProtocolDedupWindow(1, func() int { return 1_000_000 })
	for b := int64(1); b <= 100; b++ {
		w.MarkAt(fmt.Sprintf("b%d", b), b)
	}
	w.mu.Lock()
	n := len(w.order)
	w.mu.Unlock()
	if n != dedupAgeBlocks+1 {
		t.Fatalf("window holds %d keys, want %d (blocks 36..100)", n, dedupAgeBlocks+1)
	}
	if fresh, late := w.MarkAt("b10", 10); !fresh || !late {
		t.Fatalf("aged-out block: fresh=%v late=%v, want true/true", fresh, late)
	}
	if fresh, _ := w.MarkAt("b50", 50); fresh {
		t.Fatal("block within the age must still dedupe")
	}
}

// Only logs windows follow the protocol cap; pending-tx windows keep the
// configured size.
func TestPendingTxWindowKeepsConfiguredSize(t *testing.T) {
	idx, _, _ := newLogsIndexer(t, Options{})
	h, err := idx.EnsureFilter(context.Background(), "evm:1", SubTypeNewPendingTransactions,
		[]interface{}{SubTypeNewPendingTransactions})
	if err != nil {
		t.Fatal(err)
	}
	nsRaw, _ := idx.networks.Load("evm:1")
	ns := nsRaw.(*networkState)
	ns.filterMu.RLock()
	w := ns.filterDedup[h]
	ns.filterMu.RUnlock()
	if w.sizeFn != nil || w.capacity() != DefaultDedupWindowSize {
		t.Fatalf("pending-tx window capacity = %d, sizeFn set = %v", w.capacity(), w.sizeFn != nil)
	}
}
