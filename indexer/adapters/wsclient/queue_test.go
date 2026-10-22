package wsclient

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestQueueStartsUnallocated(t *testing.T) {
	var q notifyQueue
	if cap(q.items) != 0 {
		t.Fatalf("cap = %d, want 0", cap(q.items))
	}
}

func TestQueuePreservesOrderAcrossGrowth(t *testing.T) {
	var q notifyQueue
	const n = 200_000
	var want int64
	for i := 0; i < n; i++ {
		p := json.RawMessage(strconv.Itoa(i))
		want += int64(len(p))
		if d, _, _ := q.push(p); d != i+1 {
			t.Fatalf("depth after push %d = %d", i, d)
		}
	}
	if q.depth() != n {
		t.Fatalf("depth = %d, want %d", q.depth(), n)
	}
	got, bytes := q.popAll()
	if len(got) != n || bytes != want {
		t.Fatalf("popAll = %d items / %d bytes, want %d / %d", len(got), bytes, n, want)
	}
	for i, p := range got {
		if string(p) != strconv.Itoa(i) {
			t.Fatalf("item %d = %s", i, p)
		}
	}
}

func TestQueueShrinksAfterDrain(t *testing.T) {
	var q notifyQueue
	for i := 0; i < 160_000; i++ {
		q.push(json.RawMessage(`1`))
	}
	q.popAll()
	q.push(json.RawMessage(`1`))
	if cap(q.items) > 4096 {
		t.Fatalf("cap after drain = %d, want <= 4096", cap(q.items))
	}
}

func TestQueueReset(t *testing.T) {
	var q notifyQueue
	q.push(json.RawMessage(`12`))
	q.push(json.RawMessage(`345`))
	if out := q.reset(); len(out) != 2 || string(out[0]) != "12" || string(out[1]) != "345" {
		t.Fatalf("reset = %q, want [12 345]", out)
	}
	if q.depth() != 0 || q.bytes != 0 {
		t.Fatalf("after reset depth=%d bytes=%d", q.depth(), q.bytes)
	}
	if _, _, ok := q.push(json.RawMessage(`6`)); ok || q.depth() != 0 {
		t.Fatal("push after reset was accepted")
	}
}
