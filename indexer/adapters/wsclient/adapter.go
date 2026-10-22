// Package wsclient adapts a single client WebSocket connection into an
// indexer.EventEgress. One adapter per WsConnection. The adapter owns
// per-subscription queues and writer goroutines — the slow-client policy
// lives here rather than in the indexer core because future egresses
// (Kafka, webhook) will want different policies.
//
// The policy: never evict. A subscription's queue grows on demand up to
// the protocol's ceiling (two blocks of the most logs a block can hold),
// and the process shares one byte budget. A subscriber past its cap, or
// the largest one when the budget is spent, has its connection closed so
// the client can see the gap, resubscribe and back-fill. Every
// notification that is not written belongs to a closing connection.
package wsclient

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/erpc/erpc/indexer"
	"github.com/rs/zerolog"
)

// DropReason says why a notification never reached the wire.
type DropReason string

// DropClosing: the connection is closing (or was just closed) because a
// subscriber fell too far behind. The client gets a close, so the gap is
// visible.
const DropClosing DropReason = "closing"

// CloseReason says why the adapter asked for its connection to close.
type CloseReason string

const (
	// CloseSubCap: one subscription queued more than its protocol cap.
	CloseSubCap CloseReason = "sub_cap"
	// CloseHostBudget: the process's byte budget was spent and this
	// connection held the most queued bytes.
	CloseHostBudget CloseReason = "host_budget"
)

// Options sets the queue limits and the callbacks.
type Options struct {
	// MinQueue is a floor on every subscription's cap (the notifyBufferSize
	// setting). It never preallocates.
	MinQueue int
	// QueueCap returns the live protocol cap for a network. The cap is
	// max(MinQueue, QueueCap); when both are 0 there is no cap, and only
	// the budget bounds the queue.
	QueueCap func(networkID string) int
	// Budget is shared by every adapter in the process. It may be nil.
	Budget *ByteBudget
	// OnDrop is called once per notification that does not reach the
	// writer. It may be nil.
	OnDrop func(networkID string, kind indexer.EventKind, reason DropReason)
	// OnClose is called once per adapter, the first time it must close. The
	// callee closes the connection; it must not block. It may be nil.
	OnClose func(reason CloseReason)
	// OnQueue reports every change to a network's queued items and bytes.
	// It may be nil.
	OnQueue func(networkID string, deltaItems int, deltaBytes int64)
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

	// closing is set the first time the adapter asks for a close. From then
	// on, every delivery is a DropClosing.
	closing atomic.Bool
	// queued is the bytes held across this adapter's subscriptions, both
	// queued and popped but not yet written.
	queued atomic.Int64

	unregister     func()
	drained        atomic.Bool
	unregisterOnce sync.Once

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

	q notifyQueue
	// pending counts notifications queued or popped but not yet written;
	// the cap applies to it.
	pending atomic.Int64
	wake    chan struct{}
	done    chan struct{}
	closed  atomic.Bool
}

// New creates an adapter for a single client connection. The writer is
// invoked from the per-subscription goroutine; it must be safe for
// concurrent use (typically an internal sync.Mutex on the WS connection).
func New(connID string, writer NotificationWriter, logger *zerolog.Logger, opts Options) *Adapter {
	lg := logger.With().Str("connId", connID).Logger()
	a := &Adapter{
		connID: connID,
		writer: writer,
		logger: &lg,
		opts:   opts,
		subs:   make(map[string]*clientSub),
		routes: make(map[routeKey]map[string]struct{}),
	}
	if opts.Budget != nil {
		a.unregister = opts.Budget.Register(a)
	}
	return a
}

// Name identifies the adapter in indexer registries and logs.
func (a *Adapter) Name() string { return "ws:client:" + a.connID }

// QueuedBytes returns the bytes this connection holds and has not written.
func (a *Adapter) QueuedBytes() int64 { return a.queued.Load() }

// InterestedIn: O(1) map lookup. Called by the indexer on every event.
func (a *Adapter) InterestedIn(kind indexer.EventKind, networkID, filterHash string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.routes[routeKey{kind: kind, networkID: networkID, filterHash: filterHash}]
	return ok
}

// Deliver enqueues the event on every matching subscription. It never
// blocks the indexer, and it never evicts a queued notification.
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
		wake:       make(chan struct{}, 1),
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

	a.stop(sub)
	return sub.kind, sub.networkID, sub.filterHash, true
}

// Drain stops every sub's writer, releases every queued byte, and leaves
// the budget once a writer's in-flight batch is released too — call on
// connection close before detaching from the indexer.
func (a *Adapter) Drain() {
	// Mark closing without asking for a close: the connection is already
	// going. The budget then treats any bytes still in flight as recovering,
	// not as a reason to evict another connection.
	a.closing.Store(true)
	a.drained.Store(true)
	a.mu.Lock()
	subs := a.subs
	a.subs = make(map[string]*clientSub)
	a.routes = make(map[routeKey]map[string]struct{})
	a.mu.Unlock()

	for _, sub := range subs {
		a.stop(sub)
	}
	a.maybeUnregister()
}

// maybeUnregister leaves the budget once a drained adapter holds no bytes.
func (a *Adapter) maybeUnregister() {
	if a.unregister != nil && a.drained.Load() && a.queued.Load() == 0 {
		a.unregisterOnce.Do(a.unregister)
	}
}

// stop closes a sub's queue, releases what it held, and stops its writer.
// A batch the writer already popped is released by the writer itself.
func (a *Adapter) stop(sub *clientSub) {
	items, released := sub.q.reset()
	a.release(sub, items, released)
	if sub.closed.CompareAndSwap(false, true) {
		close(sub.done)
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
		case <-sub.wake:
		}
		batch, _ := sub.q.popAll()
		for i, payload := range batch {
			stopped := a.closing.Load()
			if !stopped {
				select {
				case <-sub.done:
					stopped = true
				default:
				}
			}
			if stopped {
				// Stopped mid-batch: release what was popped and not written.
				var rest int64
				for _, p := range batch[i:] {
					rest += int64(len(p))
				}
				a.release(sub, len(batch)-i, rest)
				return
			}
			if err := a.writer.WriteSubscriptionNotification(sub.id, payload); err != nil {
				a.logger.Debug().Err(err).Str("clientSubId", sub.id).
					Msg("failed to write subscription notification")
				// Errors are per-sub; the connection-close path will
				// Drain us when the peer is truly gone.
			}
			a.release(sub, 1, int64(len(payload)))
		}
	}
}

// --- enqueue ----------------------------------------------------------

// enqueue queues one payload for sub. It never blocks and never evicts.
func (a *Adapter) enqueue(sub *clientSub, payload json.RawMessage) {
	if a.closing.Load() {
		a.dropped(sub)
		return
	}
	if limit := a.capFor(sub.networkID); limit > 0 && sub.pending.Load()+1 > int64(limit) {
		a.logger.Warn().Str("clientSubId", sub.id).Str("kind", sub.kind.String()).
			Str("networkId", sub.networkID).Int("cap", limit).
			Msg("subscription queue reached its protocol cap; closing slow client connection")
		a.close(CloseSubCap)
		a.dropped(sub)
		return
	}

	n := int64(len(payload))
	if b := a.opts.Budget; b != nil && !b.Reserve(n) {
		victim, fits := b.EvictLargest()
		if !fits && (victim == nil || victim == a) {
			b.Release(n)
			a.logger.Warn().Str("clientSubId", sub.id).Int64("budgetBytes", b.Limit()).
				Int64("queuedBytes", a.QueuedBytes()).
				Msg("notification byte budget spent and this connection holds the most; closing it")
			a.close(CloseHostBudget)
			a.dropped(sub)
			return
		}
		// Otherwise queue this one. Either the overshoot is only bytes a
		// closing connection is about to release, or another open
		// connection holds the most and is closed now.
		if !fits {
			victim.logger.Warn().Int64("budgetBytes", b.Limit()).
				Int64("queuedBytes", victim.QueuedBytes()).
				Msg("notification byte budget spent and this connection holds the most; closing it")
			victim.close(CloseHostBudget)
		}
	}

	if _, _, ok := sub.q.push(payload); !ok {
		// The sub was removed while we were here.
		if a.opts.Budget != nil {
			a.opts.Budget.Release(n)
		}
		return
	}
	sub.pending.Add(1)
	a.queued.Add(n)
	if a.opts.OnQueue != nil {
		a.opts.OnQueue(sub.networkID, 1, n)
	}
	select {
	case sub.wake <- struct{}{}:
	default:
	}
}

// capFor is the live per-subscription cap: max(MinQueue, QueueCap).
func (a *Adapter) capFor(networkID string) int {
	limit := a.opts.MinQueue
	if a.opts.QueueCap != nil {
		if c := a.opts.QueueCap(networkID); c > limit {
			limit = c
		}
	}
	return limit
}

// close asks the caller to close the connection, once. Everything queued is
// released at once: a closing connection writes nothing more, and its bytes
// must not hold the budget while the connection is torn down. A batch a
// writer already took goes when that writer next checks in.
func (a *Adapter) close(reason CloseReason) {
	if !a.closing.CompareAndSwap(false, true) {
		return
	}
	a.mu.RLock()
	subs := make([]*clientSub, 0, len(a.subs))
	for _, sub := range a.subs {
		subs = append(subs, sub)
	}
	a.mu.RUnlock()
	for _, sub := range subs {
		items, released := sub.q.reset()
		a.release(sub, items, released)
	}
	if a.opts.OnClose != nil {
		a.opts.OnClose(reason)
	}
}

// release undoes the accounting for items that left sub's queue, written
// or not.
func (a *Adapter) release(sub *clientSub, items int, bytes int64) {
	if items == 0 {
		return
	}
	sub.pending.Add(-int64(items))
	a.queued.Add(-bytes)
	if a.opts.Budget != nil {
		a.opts.Budget.Release(bytes)
	}
	if a.opts.OnQueue != nil {
		a.opts.OnQueue(sub.networkID, -items, -bytes)
	}
	a.maybeUnregister()
}

func (a *Adapter) dropped(sub *clientSub) {
	if a.opts.OnDrop != nil {
		a.opts.OnDrop(sub.networkID, sub.kind, DropClosing)
	}
}
