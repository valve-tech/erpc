package wsclient

import (
	"encoding/json"
	"sync"
	"sync/atomic"
)

// ByteBudget bounds the notification bytes queued across every adapter in
// the process. It charges a payload once, however many subscriptions hold
// it: the indexer hands every subscriber the same json.RawMessage, so N
// all-logs subscribers on a full block hold one block of memory, not N.
type ByteBudget struct {
	limit int64
	used  atomic.Int64

	refMu sync.Mutex
	refs  map[*byte]int32 // payload's first byte -> subscriptions holding it

	mu       sync.Mutex
	adapters map[*Adapter]uint64 // -> registration order; higher is newer
	seq      uint64
}

// NewByteBudget returns a budget of limit bytes.
func NewByteBudget(limit int64) *ByteBudget {
	return &ByteBudget{limit: limit, adapters: make(map[*Adapter]uint64), refs: make(map[*byte]int32)}
}

// Register makes a eligible for EvictLargest until unregister is called.
func (b *ByteBudget) Register(a *Adapter) (unregister func()) {
	b.mu.Lock()
	b.seq++
	b.adapters[a] = b.seq
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.adapters, a)
		b.mu.Unlock()
	}
}

// Reserve takes one reference to p, charging its bytes only for the first,
// and reports whether the budget still holds. The reference stands either
// way; the caller releases it if it does not queue.
func (b *ByteBudget) Reserve(p json.RawMessage) bool {
	if len(p) == 0 {
		return b.used.Load() <= b.limit
	}
	b.refMu.Lock()
	b.refs[&p[0]]++
	first := b.refs[&p[0]] == 1
	b.refMu.Unlock()
	if first {
		return b.used.Add(int64(len(p))) <= b.limit
	}
	return b.used.Load() <= b.limit
}

// Release drops one reference to p, returning its bytes with the last.
func (b *ByteBudget) Release(p json.RawMessage) {
	if len(p) == 0 {
		return
	}
	b.refMu.Lock()
	b.refs[&p[0]]--
	last := b.refs[&p[0]] <= 0
	if last {
		delete(b.refs, &p[0])
	}
	b.refMu.Unlock()
	if last {
		b.used.Add(-int64(len(p)))
	}
}

// Used returns the bytes currently charged.
func (b *ByteBudget) Used() int64 { return b.used.Load() }

// Limit returns the budget's size in bytes.
func (b *ByteBudget) Limit() int64 { return b.limit }

// EvictLargest decides what to do when a Reserve fails. It returns
// fits=true when the bytes over the limit are all held by closing adapters:
// those bytes go when their blocked writers return, within the write
// deadline, so the caller queues and closes nobody. That bounds the
// overshoot to bytes already on their way out.
//
// Adapters count their bytes per subscription, so with shared payloads the
// "recovering" sum can overstate what a close frees. A hard ceiling of
// twice the limit bounds that: past it, fits is never true.
//
// Otherwise it returns the open adapter holding the most bytes (ties go to
// the newest), never one that holds 0 bytes. A nil victim with fits=false
// means no open adapter holds bytes; the caller refuses its own payload.
//
// Largest, not oldest: the relay keeps one long-lived connection per client,
// so the oldest connections are healthy bridges. A subscriber that keeps up
// holds no bytes and is never chosen; the one furthest behind frees the most
// memory with one close.
func (b *ByteBudget) EvictLargest() (victim *Adapter, fits bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var recovering, vBytes int64
	var vSeq uint64
	for a, seq := range b.adapters {
		n := a.QueuedBytes()
		if a.closing.Load() {
			recovering += n
			continue
		}
		if n > 0 && (victim == nil || n > vBytes || (n == vBytes && seq > vSeq)) {
			victim, vBytes, vSeq = a, n, seq
		}
	}
	if used := b.used.Load(); used-recovering <= b.limit && used <= 2*b.limit {
		return nil, true
	}
	return victim, false
}
