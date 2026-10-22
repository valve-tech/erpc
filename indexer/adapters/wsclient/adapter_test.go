package wsclient

import (
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/rs/zerolog"
)

// blockingWriter holds every write until release is closed.
type blockingWriter struct {
	release chan struct{}
	mu      sync.Mutex
	got     []string
	fail    bool
}

func newBlockingWriter() *blockingWriter { return &blockingWriter{release: make(chan struct{})} }

func (w *blockingWriter) WriteSubscriptionNotification(_ string, result json.RawMessage) error {
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, string(result))
	if w.fail {
		return errors.New("peer gone")
	}
	return nil
}

func (w *blockingWriter) written() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.got...)
}

// recorder captures the adapter's callbacks.
type recorder struct {
	mu     sync.Mutex
	drops  map[DropReason]int
	closes []CloseReason
	items  int
	bytes  int64
}

func newRecorder() *recorder { return &recorder{drops: map[DropReason]int{}} }

func (r *recorder) opts(o Options) Options {
	o.OnDrop = func(_ string, _ indexer.EventKind, reason DropReason) {
		r.mu.Lock()
		r.drops[reason]++
		r.mu.Unlock()
	}
	o.OnClose = func(reason CloseReason) {
		r.mu.Lock()
		r.closes = append(r.closes, reason)
		r.mu.Unlock()
	}
	o.OnQueue = func(_ string, di int, db int64) {
		r.mu.Lock()
		r.items += di
		r.bytes += db
		r.mu.Unlock()
	}
	return o
}

func (r *recorder) snapshot() (map[DropReason]int, []CloseReason, int, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := map[DropReason]int{}
	for k, v := range r.drops {
		d[k] = v
	}
	return d, append([]CloseReason(nil), r.closes...), r.items, r.bytes
}

func logEvent(payload string) indexer.IndexedEvent {
	var ev indexer.IndexedEvent
	ev.Kind = indexer.KindLog
	ev.NetworkId = "evm:1"
	ev.FilterHash = "f"
	ev.Payload = json.RawMessage(payload)
	return ev
}

func capOf(n int) func(string) int { return func(string) int { return n } }

var nopLogger = zerolog.Nop()

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// pin subscribes s1 and parks its writer on a first event, so every later
// delivery stays queued. The parked event counts as pending.
func pin(t *testing.T, a *Adapter) {
	t.Helper()
	a.AddSubscription("s1", "evm:1", indexer.KindLog, "f")
	a.Deliver(logEvent(`"pin"`))
	waitFor(t, "writer to take the first event", func() bool { return a.subs["s1"].q.depth() == 0 })
}

func TestDeliverProtocolBlockNoDrop(t *testing.T) {
	w := newBlockingWriter()
	r := newRecorder()
	a := New("c", w, &nopLogger, r.opts(Options{QueueCap: capOf(160_001)}))
	defer a.Drain()
	pin(t, a)

	for i := 0; i < 160_000; i++ {
		a.Deliver(logEvent(strconv.Itoa(i)))
	}
	drops, closes, items, _ := r.snapshot()
	if a.subs["s1"].q.depth() != 160_000 {
		t.Fatalf("depth = %d, want 160000", a.subs["s1"].q.depth())
	}
	if len(drops) != 0 || len(closes) != 0 {
		t.Fatalf("drops=%v closes=%v, want none", drops, closes)
	}
	if items != 160_001 {
		t.Fatalf("queued gauge = %d, want 160001", items)
	}

	// Release the writer: every event arrives, in order, and the gauges
	// return to zero.
	close(w.release)
	waitFor(t, "all writes", func() bool { return len(w.written()) == 160_001 })
	got := w.written()
	for i := 0; i < 160_000; i++ {
		if got[i+1] != strconv.Itoa(i) {
			t.Fatalf("write %d = %s", i+1, got[i+1])
		}
	}
	waitFor(t, "gauges to drain", func() bool {
		_, _, items, bytes := r.snapshot()
		return items == 0 && bytes == 0 && a.QueuedBytes() == 0
	})
}

func TestDeliverOverSubCapCloses4013(t *testing.T) {
	r := newRecorder()
	a := New("c", newBlockingWriter(), &nopLogger, r.opts(Options{QueueCap: capOf(10)}))
	defer a.Drain()
	pin(t, a) // 1 pending

	for i := 0; i < 11; i++ { // 9 fit, the 10th trips the cap, the 11th is closing
		a.Deliver(logEvent(`1`))
	}
	drops, closes, _, _ := r.snapshot()
	if len(closes) != 1 || closes[0] != CloseSubCap {
		t.Fatalf("closes = %v, want [sub_cap]", closes)
	}
	if drops[DropClosing] != 2 || len(drops) != 1 {
		t.Fatalf("drops = %v, want closing:2", drops)
	}
	// Nothing was evicted before the close; the close then released the
	// whole queue at once, leaving only the writer's in-flight pin.
	if a.subs["s1"].q.depth() != 0 || a.QueuedBytes() != int64(len(`"pin"`)) {
		t.Fatalf("depth=%d queued=%d after close, want 0 / %d", a.subs["s1"].q.depth(), a.QueuedBytes(), len(`"pin"`))
	}
}

func TestMinQueueIsFloor(t *testing.T) {
	r := newRecorder()
	a := New("c", newBlockingWriter(), &nopLogger, r.opts(Options{MinQueue: 1024, QueueCap: capOf(10)}))
	defer a.Drain()
	pin(t, a)

	for i := 0; i < 1023; i++ {
		a.Deliver(logEvent(`1`))
	}
	if _, closes, _, _ := r.snapshot(); len(closes) != 0 {
		t.Fatalf("closes = %v, want none under the floor", closes)
	}
	a.Deliver(logEvent(`1`))
	if _, closes, _, _ := r.snapshot(); len(closes) != 1 {
		t.Fatalf("closes = %v, want one past the floor", closes)
	}
}

func TestNoCapWhenUnset(t *testing.T) {
	r := newRecorder()
	a := New("c", newBlockingWriter(), &nopLogger, r.opts(Options{}))
	defer a.Drain()
	pin(t, a)
	for i := 0; i < 5000; i++ {
		a.Deliver(logEvent(`1`))
	}
	if _, closes, _, _ := r.snapshot(); len(closes) != 0 {
		t.Fatalf("closes = %v, want none", closes)
	}
}

func payloadOf(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '1'
	}
	return string(b)
}

func TestBudgetEvictsLargest(t *testing.T) {
	budget := NewByteBudget(1 << 20)
	ra, rb := newRecorder(), newRecorder()
	a := New("a", newBlockingWriter(), &nopLogger, ra.opts(Options{Budget: budget}))
	b := New("b", newBlockingWriter(), &nopLogger, rb.opts(Options{Budget: budget}))
	defer a.Drain()
	defer b.Drain()
	pin(t, a)
	pin(t, b)

	a.Deliver(logEvent(payloadOf(600 << 10)))
	b.Deliver(logEvent(payloadOf(300 << 10)))
	b.Deliver(logEvent(payloadOf(200 << 10)))

	_, aCloses, _, _ := ra.snapshot()
	bDrops, bCloses, _, _ := rb.snapshot()
	if len(aCloses) != 1 || aCloses[0] != CloseHostBudget {
		t.Fatalf("a closes = %v, want [host_budget]", aCloses)
	}
	if len(bCloses) != 0 || len(bDrops) != 0 {
		t.Fatalf("b closes=%v drops=%v, want none", bCloses, bDrops)
	}
	if b.subs["s1"].q.depth() != 2 {
		t.Fatalf("b depth = %d, want 2 (its payload queued)", b.subs["s1"].q.depth())
	}

	// a is closing: its next delivery is a closing drop, and a later
	// overrun picks b, the largest open adapter.
	a.Deliver(logEvent(`1`))
	if aDrops, _, _, _ := ra.snapshot(); aDrops[DropClosing] != 1 {
		t.Fatalf("a drops = %v, want closing:1", aDrops)
	}
}

func TestBudgetEvictsSelfWhenLargest(t *testing.T) {
	budget := NewByteBudget(1 << 20)
	ra, rb := newRecorder(), newRecorder()
	a := New("a", newBlockingWriter(), &nopLogger, ra.opts(Options{Budget: budget}))
	b := New("b", newBlockingWriter(), &nopLogger, rb.opts(Options{Budget: budget}))
	defer a.Drain()
	defer b.Drain()
	pin(t, a)
	pin(t, b)

	a.Deliver(logEvent(payloadOf(300 << 10)))
	b.Deliver(logEvent(payloadOf(600 << 10)))
	b.Deliver(logEvent(payloadOf(200 << 10)))

	bDrops, bCloses, _, _ := rb.snapshot()
	if len(bCloses) != 1 || bCloses[0] != CloseHostBudget {
		t.Fatalf("b closes = %v, want [host_budget]", bCloses)
	}
	if bDrops[DropClosing] != 1 {
		t.Fatalf("b drops = %v, want closing:1", bDrops)
	}
	if _, aCloses, _, _ := ra.snapshot(); len(aCloses) != 0 {
		t.Fatalf("a closes = %v, want none", aCloses)
	}
	// The refused payload is released, and so is b's queue on close: the
	// budget holds exactly what both adapters still hold.
	if got, want := budget.Used(), a.QueuedBytes()+b.QueuedBytes(); got != want {
		t.Fatalf("budget used = %d, want %d", got, want)
	}
}

func TestBudgetTieGoesToNewest(t *testing.T) {
	budget := NewByteBudget(1)
	a := New("a", newBlockingWriter(), &nopLogger, Options{Budget: budget})
	b := New("b", newBlockingWriter(), &nopLogger, Options{Budget: budget})
	defer a.Drain()
	defer b.Drain()
	if victim, fits := budget.EvictLargest(); victim != nil || !fits {
		t.Fatal("under the limit: nobody should be evicted")
	}
	c := New("c", newBlockingWriter(), &nopLogger, Options{Budget: budget})
	c.close(CloseSubCap) // closing with 0 bytes: recovers nothing
	defer c.Drain()
	// Both pin 5 bytes in flight; the 1-byte budget is over by 9 with no
	// closing adapter holding any of it.
	a.AddSubscription("s1", "evm:1", indexer.KindLog, "f")
	b.AddSubscription("s1", "evm:1", indexer.KindLog, "f")
	budget.used.Add(10)
	a.queued.Add(5)
	b.queued.Add(5)
	if victim, fits := budget.EvictLargest(); fits || victim != b {
		t.Fatal("tie should pick the newest adapter")
	}
	budget.used.Add(-10)
	a.queued.Add(-5)
	b.queued.Add(-5)
	if budget.Limit() != 1 {
		t.Fatalf("limit = %d", budget.Limit())
	}
}

// One slow client must not cascade into closing healthy connections while
// its own bytes are still on their way out.
func TestBudgetNoCascadeWhileVictimDrains(t *testing.T) {
	budget := NewByteBudget(1 << 20)
	wSlow := newBlockingWriter()
	rs, rh := newRecorder(), newRecorder()
	slow := New("slow", wSlow, &nopLogger, rs.opts(Options{Budget: budget}))
	healthy := New("healthy", newBlockingWriter(), &nopLogger, rh.opts(Options{Budget: budget}))
	defer slow.Drain()
	defer healthy.Drain()

	// The slow writer takes one big payload and blocks on it: in flight,
	// so close() cannot release it.
	slow.AddSubscription("s1", "evm:1", indexer.KindLog, "f")
	slow.Deliver(logEvent(payloadOf(900 << 10)))
	waitFor(t, "slow writer to take its payload", func() bool { return slow.subs["s1"].q.depth() == 0 })
	slow.Deliver(logEvent(payloadOf(50 << 10))) // queued behind it

	healthy.AddSubscription("s1", "evm:1", indexer.KindLog, "f")
	healthy.Deliver(logEvent(payloadOf(200 << 10))) // over budget: slow is largest
	if _, closes, _, _ := rs.snapshot(); len(closes) != 1 {
		t.Fatalf("slow closes = %v, want one", closes)
	}
	// close() released slow's queued bytes at once; its in-flight batch
	// still holds the budget until the blocked write returns.
	if got := slow.QueuedBytes(); got != 900<<10 {
		t.Fatalf("slow queued = %d, want only the in-flight %d", got, 900<<10)
	}

	// More over-budget deliveries: while the bytes over the limit are only
	// slow's, which are on their way out, healthy is not closed.
	for i := 0; i < 8; i++ {
		healthy.Deliver(logEvent(payloadOf(100 << 10)))
	}
	if _, closes, _, _ := rh.snapshot(); len(closes) != 0 {
		t.Fatalf("healthy closes = %v, want none while slow's bytes drain", closes)
	}
	// The overshoot is bounded: once healthy itself holds more than the
	// budget, it is the largest open connection and is closed.
	healthy.Deliver(logEvent(payloadOf(100 << 10)))
	if _, closes, _, _ := rh.snapshot(); len(closes) != 1 || closes[0] != CloseHostBudget {
		t.Fatalf("healthy closes = %v, want [host_budget] past the bound", closes)
	}
	if over := budget.Used() - slow.QueuedBytes(); over > budget.Limit() {
		t.Fatalf("budget used minus recovering = %d, want <= %d", over, budget.Limit())
	}

	// The blocked write returns: slow's bytes go, and it holds nothing.
	close(wSlow.release)
	waitFor(t, "slow to release its bytes", func() bool { return slow.QueuedBytes() == 0 })
}

// A drained adapter stays in the budget until its in-flight bytes are
// released, so those bytes cannot trigger an eviction of someone else.
func TestDrainedAdapterLeavesBudgetAfterInFlight(t *testing.T) {
	budget := NewByteBudget(1 << 30)
	w := newBlockingWriter()
	a := New("a", w, &nopLogger, Options{Budget: budget})
	pin(t, a)
	a.Drain()
	budget.mu.Lock()
	_, stillRegistered := budget.adapters[a]
	budget.mu.Unlock()
	if !stillRegistered {
		t.Fatal("drained adapter left the budget with bytes still in flight")
	}
	if victim, _ := budget.EvictLargest(); victim != nil {
		t.Fatal("a draining adapter's bytes must not trigger an eviction")
	}
	close(w.release)
	waitFor(t, "unregister", func() bool {
		budget.mu.Lock()
		defer budget.mu.Unlock()
		_, ok := budget.adapters[a]
		return !ok
	})
}

func TestBudgetReleasedOnDrain(t *testing.T) {
	budget := NewByteBudget(1 << 30)
	w := newBlockingWriter()
	r := newRecorder()
	a := New("a", w, &nopLogger, r.opts(Options{Budget: budget}))
	pin(t, a)
	a.AddSubscription("s2", "evm:1", indexer.KindLog, "f")
	for i := 0; i < 100; i++ {
		a.Deliver(logEvent(`12345`))
	}
	if budget.Used() == 0 {
		t.Fatal("budget should hold the queued bytes")
	}
	a.Drain()
	if victim, _ := budget.EvictLargest(); victim != nil {
		t.Fatal("a drained adapter must leave the budget")
	}
	// Queued bytes go at once. A batch a writer already popped is still in
	// memory until its blocked write returns, so it stays charged until the
	// writer sees done.
	close(w.release)
	waitFor(t, "budget to reach 0", func() bool { return budget.Used() == 0 })
	waitFor(t, "gauges to reach 0", func() bool {
		_, _, items, bytes := r.snapshot()
		return items == 0 && bytes == 0
	})
}

func TestWriterReleasesUnwrittenBatchOnStop(t *testing.T) {
	budget := NewByteBudget(1 << 30)
	w := newBlockingWriter()
	r := newRecorder()
	a := New("a", w, &nopLogger, r.opts(Options{Budget: budget}))
	a.AddSubscription("s1", "evm:1", indexer.KindLog, "f")
	// Queue three before the writer wakes, so it pops them as one batch.
	sub := a.subs["s1"]
	for i := 0; i < 3; i++ {
		a.Deliver(logEvent(`12`))
	}
	// The writer takes a batch (all three, or fewer if it woke early) and
	// blocks on its first write.
	waitFor(t, "writer to pop a batch", func() bool { return sub.q.depth() < 3 })
	a.RemoveSubscription("s1")
	close(w.release) // the first write completes, then the writer sees done
	waitFor(t, "budget to drain", func() bool { return budget.Used() == 0 })
	waitFor(t, "gauges to drain", func() bool {
		_, _, items, bytes := r.snapshot()
		return items == 0 && bytes == 0
	})
}

func TestEnqueueAfterRemoveIsRefused(t *testing.T) {
	budget := NewByteBudget(1 << 30)
	a := New("a", newBlockingWriter(), &nopLogger, Options{Budget: budget})
	a.AddSubscription("s1", "evm:1", indexer.KindLog, "f")
	sub := a.subs["s1"]
	a.RemoveSubscription("s1")
	a.enqueue(sub, json.RawMessage(`1`))
	if budget.Used() != 0 || a.QueuedBytes() != 0 {
		t.Fatalf("used=%d queued=%d, want 0", budget.Used(), a.QueuedBytes())
	}
	if _, _, _, ok := a.RemoveSubscription("s1"); ok {
		t.Fatal("second remove should report not existed")
	}
}

func TestWriteErrorKeepsDelivering(t *testing.T) {
	w := newBlockingWriter()
	w.fail = true
	close(w.release)
	a := New("a", w, &nopLogger, Options{})
	defer a.Drain()
	a.AddSubscription("s1", "evm:1", indexer.KindLog, "f")
	a.Deliver(logEvent(`1`))
	a.Deliver(logEvent(`2`))
	waitFor(t, "both writes", func() bool { return len(w.written()) == 2 })
}

func TestIdleSubscriptionsCostNothing(t *testing.T) {
	budget := NewByteBudget(1 << 30)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	adapters := make([]*Adapter, 100)
	for i := range adapters {
		adapters[i] = New("c"+strconv.Itoa(i), newBlockingWriter(), &nopLogger, Options{Budget: budget, QueueCap: capOf(320_000)})
		adapters[i].AddSubscription("s", "evm:1", indexer.KindLog, "f")
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	for _, a := range adapters {
		a.Drain()
	}
	if delta := int64(after.HeapAlloc) - int64(before.HeapAlloc); delta > 1<<20 {
		t.Fatalf("100 idle subscriptions hold %d heap bytes, want < 1 MiB", delta)
	}
}

func TestSubscriptionsAndCount(t *testing.T) {
	a := New("a", newBlockingWriter(), &nopLogger, Options{})
	defer a.Drain()
	a.AddSubscription("s1", "evm:1", indexer.KindLog, "f")
	a.AddSubscription("s2", "evm:1", indexer.KindNewHead, "")
	if a.Count() != 2 || len(a.Subscriptions()) != 2 {
		t.Fatalf("count=%d subs=%d", a.Count(), len(a.Subscriptions()))
	}
	if !a.InterestedIn(indexer.KindLog, "evm:1", "f") || a.InterestedIn(indexer.KindLog, "evm:1", "g") {
		t.Fatal("InterestedIn mismatch")
	}
	if a.Name() != "ws:client:a" {
		t.Fatal(a.Name())
	}
	var ev indexer.IndexedEvent
	ev.Kind, ev.NetworkId, ev.FilterHash = indexer.KindLog, "evm:1", "g"
	a.Deliver(ev) // no route: a no-op
}

// Closing once calls OnClose once, however many deliveries race in.
func TestCloseFiresOnce(t *testing.T) {
	var n atomic.Int32
	a := New("a", newBlockingWriter(), &nopLogger, Options{OnClose: func(CloseReason) { n.Add(1) }})
	a.close(CloseSubCap)
	a.close(CloseHostBudget)
	if n.Load() != 1 {
		t.Fatalf("OnClose calls = %d, want 1", n.Load())
	}
}
