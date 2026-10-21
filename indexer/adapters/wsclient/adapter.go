// Package wsclient adapts a single client WebSocket connection into an
// indexer.EventEgress. One adapter per WsConnection. The adapter owns
// per-subscription write buffers and writer goroutines — the slow-client
// policy (oldest-drop, or close) lives here rather than in the indexer
// core because future egresses (Kafka, webhook) will want different
// policies.
package wsclient

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/erpc/erpc/indexer"
	"github.com/rs/zerolog"
)

// DefaultNotifyBufferSize is the per-subscription buffer depth when the
// caller does not set one.
//
// Retention scales with depth, not throughput: each buffered
// notification pins the upstream-allocated json.RawMessage until the
// writer drains it, so total GC-reachable heap from this path is roughly
// N_subs × depth × payload_size. 256 × a 10 KB header is about 2.5 MB per
// subscription at worst. A depth of 8 was too small: one busy block can
// emit dozens of matching logs, and a slow peer stalls the writer for up
// to the 10 s write deadline.
const DefaultNotifyBufferSize = 256

// DropReason says why a notification never reached the wire.
type DropReason string

const (
	// DropBufferFull: the buffer was full and the oldest queued
	// notification was evicted to make room.
	DropBufferFull DropReason = "buffer_full"
	// DropSlowClose: the buffer was full, the close policy is on, and the
	// connection is being closed instead. The client sees the gap.
	DropSlowClose DropReason = "slow_close"
)

// Options tunes the slow-client policy.
type Options struct {
	// BufferSize is the per-subscription depth. Zero or less means
	// DefaultNotifyBufferSize.
	BufferSize int
	// OnDrop is called once per notification that does not reach the
	// writer. It may be nil.
	OnDrop func(networkID string, kind indexer.EventKind, reason DropReason)
	// OnSlow, when set, turns on the close policy: a full buffer evicts
	// nothing and OnSlow is called once for the connection. The callee
	// closes the connection. It must not block.
	OnSlow func()
}

// NotificationWriter is what the adapter uses to push a delivered event
// onto the wire. The indirection lets the adapter stay unaware of
// gorilla/websocket specifically — any transport that can deliver a
// (clientSubId, result) pair to a single consumer works.
type NotificationWriter interface {
	WriteSubscriptionNotification(clientSubId string, result json.RawMessage) error
}

// Adapter is the per-connection EventEgress. Subscribe/Unsubscribe calls
// on the underlying connection translate into AddSubscription /
// RemoveSubscription here; the indexer calls InterestedIn on every event
// and Deliver for matches.
type Adapter struct {
	connID string
	writer NotificationWriter
	logger *zerolog.Logger
	opts   Options
	// slow is set once the close policy has fired; later deliveries are
	// dropped as DropSlowClose without calling OnSlow again.
	slow atomic.Bool

	mu sync.RWMutex
	// subs keyed by clientSubId. A single map supports both newHeads
	// and filter subs.
	subs map[string]*clientSub
	// routes is an interest-index: (kind, networkId, filterHash) -> set
	// of clientSubIds. Kept in sync with subs for O(1) InterestedIn.
	routes map[routeKey]map[string]struct{}
}

type routeKey struct {
	kind       indexer.EventKind
	networkID  string
	filterHash string
}

type clientSub struct {
	id        string
	kind      indexer.EventKind
	networkID string
	// filterHash is "" for newHeads.
	filterHash string

	notify chan json.RawMessage
	done   chan struct{}
	closed atomic.Bool
}

// New creates an adapter for a single client connection. The writer is
// invoked from the per-subscription goroutine; it must be safe for
// concurrent use (typically an internal sync.Mutex on the WS connection).
func New(connID string, writer NotificationWriter, logger *zerolog.Logger, opts Options) *Adapter {
	lg := logger.With().Str("connId", connID).Logger()
	if opts.BufferSize <= 0 {
		opts.BufferSize = DefaultNotifyBufferSize
	}
	return &Adapter{
		connID: connID,
		writer: writer,
		logger: &lg,
		opts:   opts,
		subs:   make(map[string]*clientSub),
		routes: make(map[routeKey]map[string]struct{}),
	}
}

// Name identifies the adapter in indexer registries and logs.
func (a *Adapter) Name() string { return "ws:client:" + a.connID }

// InterestedIn: O(1) map lookup. Called by the indexer on every event.
func (a *Adapter) InterestedIn(kind indexer.EventKind, networkID, filterHash string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.routes[routeKey{kind: kind, networkID: networkID, filterHash: filterHash}]
	return ok
}

// Deliver enqueues the event on every matching per-sub channel. It never
// blocks the indexer. If a sub's buffer is full, the adapter either
// evicts the oldest queued notification, or (close policy) asks the
// caller to close the connection. Every lost notification goes to OnDrop.
func (a *Adapter) Deliver(ev indexer.IndexedEvent) {
	a.mu.RLock()
	routes, ok := a.routes[routeKey{kind: ev.Kind, networkID: ev.NetworkId, filterHash: ev.FilterHash}]
	if !ok || len(routes) == 0 {
		a.mu.RUnlock()
		return
	}
	ids := make([]string, 0, len(routes))
	for id := range routes {
		ids = append(ids, id)
	}
	a.mu.RUnlock()

	for _, id := range ids {
		a.mu.RLock()
		sub, ok := a.subs[id]
		a.mu.RUnlock()
		if !ok {
			continue
		}
		a.enqueue(sub, ev.Payload)
	}
}

// AddSubscription registers a client subscription on this connection and
// starts its writer goroutine. clientSubId is the erpc-generated opaque
// ID the caller already returned to the client. filterHash is "" for
// newHeads.
func (a *Adapter) AddSubscription(clientSubID, networkID string, kind indexer.EventKind, filterHash string) {
	sub := &clientSub{
		id:         clientSubID,
		kind:       kind,
		networkID:  networkID,
		filterHash: filterHash,
		notify:     make(chan json.RawMessage, a.opts.BufferSize),
		done:       make(chan struct{}),
	}

	a.mu.Lock()
	a.subs[clientSubID] = sub
	key := routeKey{kind: kind, networkID: networkID, filterHash: filterHash}
	set, ok := a.routes[key]
	if !ok {
		set = make(map[string]struct{})
		a.routes[key] = set
	}
	set[clientSubID] = struct{}{}
	a.mu.Unlock()

	go a.runWriter(sub)
}

// RemoveSubscription deregisters a subscription and stops its writer. It
// returns the (kind, filterHash) of the removed sub so the caller can
// decide whether to call indexer.ReleaseFilter (for filter subs once the
// refcount drops to zero on its side).
func (a *Adapter) RemoveSubscription(clientSubID string) (kind indexer.EventKind, networkID, filterHash string, existed bool) {
	a.mu.Lock()
	sub, ok := a.subs[clientSubID]
	if !ok {
		a.mu.Unlock()
		return 0, "", "", false
	}
	delete(a.subs, clientSubID)
	key := routeKey{kind: sub.kind, networkID: sub.networkID, filterHash: sub.filterHash}
	if set, ok := a.routes[key]; ok {
		delete(set, clientSubID)
		if len(set) == 0 {
			delete(a.routes, key)
		}
	}
	a.mu.Unlock()

	if sub.closed.CompareAndSwap(false, true) {
		close(sub.done)
	}
	return sub.kind, sub.networkID, sub.filterHash, true
}

// Drain stops every sub's writer — call on connection close before
// detaching from the indexer.
func (a *Adapter) Drain() {
	a.mu.Lock()
	subs := a.subs
	a.subs = make(map[string]*clientSub)
	a.routes = make(map[routeKey]map[string]struct{})
	a.mu.Unlock()

	for _, sub := range subs {
		if sub.closed.CompareAndSwap(false, true) {
			close(sub.done)
		}
	}
}

// Subscriptions returns a snapshot of the active subscriptions. Used by
// connection cleanup paths to iterate subs without racing with Deliver.
func (a *Adapter) Subscriptions() []Subscription {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Subscription, 0, len(a.subs))
	for _, sub := range a.subs {
		out = append(out, Subscription{
			ClientSubID: sub.id,
			Kind:        sub.kind,
			NetworkID:   sub.networkID,
			FilterHash:  sub.filterHash,
		})
	}
	return out
}

// Subscription is a caller-visible snapshot of one active subscription.
type Subscription struct {
	ClientSubID string
	Kind        indexer.EventKind
	NetworkID   string
	FilterHash  string
}

// Count returns the number of active subscriptions — used by the
// client-facing manager to enforce per-connection limits.
func (a *Adapter) Count() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.subs)
}

// --- writer goroutine -------------------------------------------------

func (a *Adapter) runWriter(sub *clientSub) {
	for {
		select {
		case <-sub.done:
			return
		case payload, ok := <-sub.notify:
			if !ok {
				return
			}
			if err := a.writer.WriteSubscriptionNotification(sub.id, payload); err != nil {
				a.logger.Debug().Err(err).Str("clientSubId", sub.id).
					Msg("failed to write subscription notification")
				// Errors are per-sub; the connection-close path will
				// Drain us when the peer is truly gone.
			}
		}
	}
}

// enqueue pushes a payload onto the sub's buffer. Never blocks.
func (a *Adapter) enqueue(sub *clientSub, payload json.RawMessage) {
	if a.slow.Load() {
		a.dropped(sub, DropSlowClose)
		return
	}
	for {
		select {
		case sub.notify <- payload:
			return
		default:
		}
		if a.opts.OnSlow != nil {
			a.dropped(sub, DropSlowClose)
			if a.slow.CompareAndSwap(false, true) {
				a.logger.Warn().Str("clientSubId", sub.id).Str("kind", sub.kind.String()).
					Int("bufferSize", a.opts.BufferSize).
					Msg("subscription buffer full; closing slow client connection")
				a.opts.OnSlow()
			}
			return
		}
		// Buffer full; evict the oldest and retry. If the writer drained
		// the buffer between the two selects, the receive finds nothing
		// and the retry succeeds — the new payload is never lost to that race.
		select {
		case <-sub.notify:
			a.dropped(sub, DropBufferFull)
		default:
		}
	}
}

func (a *Adapter) dropped(sub *clientSub, reason DropReason) {
	if a.opts.OnDrop != nil {
		a.opts.OnDrop(sub.networkID, sub.kind, reason)
	}
}
