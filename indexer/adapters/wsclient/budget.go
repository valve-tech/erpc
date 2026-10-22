package wsclient

import (
	"sync"
	"sync/atomic"
)

// ByteBudget bounds the notification bytes queued across every adapter in
// the process. A payload shared by several subscriptions counts once per
// subscription, which overstates memory: the safe direction.
type ByteBudget struct {
	limit int64
	used  atomic.Int64

	mu       sync.Mutex
	adapters map[*Adapter]uint64 // -> registration order; higher is newer
	seq      uint64
}

// NewByteBudget returns a budget of limit bytes.
func NewByteBudget(limit int64) *ByteBudget {
	return &ByteBudget{limit: limit, adapters: make(map[*Adapter]uint64)}
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

// Reserve charges n bytes and reports whether the budget still holds. The
// charge stands either way; the caller releases it if it does not queue.
func (b *ByteBudget) Reserve(n int64) bool {
	return b.used.Add(n) <= b.limit
}

// Release returns n bytes to the budget.
func (b *ByteBudget) Release(n int64) { b.used.Add(-n) }

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
	if b.used.Load()-recovering <= b.limit {
		return nil, true
	}
	return victim, false
}
