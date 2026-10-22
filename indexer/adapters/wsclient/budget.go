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

// EvictLargest returns the open adapter holding the most queued bytes; ties
// go to the newest. It returns nil when no open adapter is registered.
//
// Largest, not oldest: the relay keeps one long-lived connection per client,
// so the oldest connections are healthy bridges. A subscriber that keeps up
// holds no bytes and is never chosen; the one furthest behind frees the most
// memory with one close.
func (b *ByteBudget) EvictLargest() *Adapter {
	b.mu.Lock()
	defer b.mu.Unlock()
	var victim *Adapter
	var vBytes int64
	var vSeq uint64
	for a, seq := range b.adapters {
		if a.closing.Load() {
			continue
		}
		n := a.QueuedBytes()
		if victim == nil || n > vBytes || (n == vBytes && seq > vSeq) {
			victim, vBytes, vSeq = a, n, seq
		}
	}
	return victim
}
