package wsclient

import (
	"encoding/json"
	"sync"
)

// notifyQueue is one subscription's pending notifications, in order. It
// allocates only for what it holds: an idle subscription costs a nil slice,
// and a 160,000-log burst does not pin its array once drained.
type notifyQueue struct {
	mu     sync.Mutex
	items  []json.RawMessage
	bytes  int64
	closed bool
}

// push appends p and returns the queue's new depth and bytes. It refuses
// (ok=false) once reset has closed the queue, so nothing is queued, or
// charged, after cleanup.
func (q *notifyQueue) push(p json.RawMessage) (depth int, bytes int64, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return len(q.items), q.bytes, false
	}
	q.items = append(q.items, p)
	q.bytes += int64(len(p))
	return len(q.items), q.bytes, true
}

// depth returns the number of queued items.
func (q *notifyQueue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// popAll takes every queued item, in order, and returns their total bytes.
// The caller owns the returned array, and the queue starts again from nil,
// so a drained burst pins nothing.
func (q *notifyQueue) popAll() ([]json.RawMessage, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out, n := q.items, q.bytes
	q.items, q.bytes = nil, 0
	return out, n
}

// reset closes the queue, discards every queued item, and returns what it
// released.
func (q *notifyQueue) reset() (items int, released int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	items, released = len(q.items), q.bytes
	q.items, q.bytes, q.closed = nil, 0, true
	return items, released
}
