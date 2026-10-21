package wsclient

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/rs/zerolog"
)

// blockingWriter holds every write until release is closed, so the
// sub's buffer fills deterministically.
type blockingWriter struct {
	release chan struct{}
	mu      sync.Mutex
	got     []string
}

func (w *blockingWriter) WriteSubscriptionNotification(_ string, result json.RawMessage) error {
	<-w.release
	w.mu.Lock()
	w.got = append(w.got, string(result))
	w.mu.Unlock()
	return nil
}

type dropCounts struct {
	mu sync.Mutex
	by map[DropReason]int
}

func (d *dropCounts) onDrop(_ string, _ indexer.EventKind, r DropReason) {
	d.mu.Lock()
	d.by[r]++
	d.mu.Unlock()
}

func (d *dropCounts) get(r DropReason) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.by[r]
}

func logEvent(i int) indexer.IndexedEvent {
	var ev indexer.IndexedEvent
	ev.Kind = indexer.KindLog
	ev.NetworkId = "evm:1"
	ev.FilterHash = "f"
	ev.Payload = json.RawMessage([]byte{'"', byte('a' + i%26), '"'})
	return ev
}

// fill subscribes one logs sub, lets its writer take one payload and
// block on it, then delivers n more.
func fill(t *testing.T, a *Adapter, n int) {
	t.Helper()
	a.AddSubscription("s1", "evm:1", indexer.KindLog, "f")
	a.Deliver(logEvent(0))
	// Wait for the writer to pull event 0 and block in the writer.
	deadline := time.Now().Add(2 * time.Second)
	for len(a.subs["s1"].notify) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("writer never picked up the first event")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 1; i <= n; i++ {
		a.Deliver(logEvent(i))
	}
}

func TestDefaultBufferSize(t *testing.T) {
	a := New("c", &blockingWriter{release: make(chan struct{})}, &zerolog.Logger{}, Options{})
	a.AddSubscription("s1", "evm:1", indexer.KindLog, "f")
	defer a.Drain()
	if got := cap(a.subs["s1"].notify); got != 256 {
		t.Fatalf("default buffer = %d, want 256", got)
	}
	b := New("c", &blockingWriter{release: make(chan struct{})}, &zerolog.Logger{}, Options{BufferSize: 3})
	b.AddSubscription("s1", "evm:1", indexer.KindLog, "f")
	defer b.Drain()
	if got := cap(b.subs["s1"].notify); got != 3 {
		t.Fatalf("configured buffer = %d, want 3", got)
	}
}

func TestBufferFull_EvictsOldestAndCountsEach(t *testing.T) {
	w := &blockingWriter{release: make(chan struct{})}
	d := &dropCounts{by: map[DropReason]int{}}
	a := New("c", w, &zerolog.Logger{}, Options{BufferSize: 4, OnDrop: d.onDrop})
	defer a.Drain()

	fill(t, a, 10) // 4 fit, 6 evict

	if got := d.get(DropBufferFull); got != 6 {
		t.Fatalf("buffer_full drops = %d, want 6", got)
	}
	if got := d.get(DropSlowClose); got != 0 {
		t.Fatalf("slow_close drops = %d, want 0", got)
	}
	if got := len(a.subs["s1"].notify); got != 4 {
		t.Fatalf("buffered = %d, want 4", got)
	}
}

func TestBufferFull_ClosePolicyEvictsNothing(t *testing.T) {
	w := &blockingWriter{release: make(chan struct{})}
	d := &dropCounts{by: map[DropReason]int{}}
	var slowCalls atomic.Int32
	a := New("c", w, &zerolog.Logger{}, Options{
		BufferSize: 4,
		OnDrop:     d.onDrop,
		OnSlow:     func() { slowCalls.Add(1) },
	})
	defer a.Drain()

	fill(t, a, 10) // 4 fit, the 5th trips the close, 6..10 are dropped after it

	if got := slowCalls.Load(); got != 1 {
		t.Fatalf("OnSlow calls = %d, want 1", got)
	}
	if got := d.get(DropBufferFull); got != 0 {
		t.Fatalf("buffer_full drops = %d, want 0 (nothing evicted)", got)
	}
	if got := d.get(DropSlowClose); got != 6 {
		t.Fatalf("slow_close drops = %d, want 6", got)
	}

	// The four buffered events are the first four, in order: nothing
	// queued before the close was evicted.
	close(w.release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		w.mu.Lock()
		n := len(w.got)
		w.mu.Unlock()
		if n == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("writer wrote %d, want 5", n)
		}
		time.Sleep(time.Millisecond)
	}
	want := []string{`"a"`, `"b"`, `"c"`, `"d"`, `"e"`}
	for i, g := range w.got {
		if g != want[i] {
			t.Fatalf("write %d = %s, want %s", i, g, want[i])
		}
	}
}
