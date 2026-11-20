package common

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type NetworkArchitecture string

const (
	ArchitectureEvm NetworkArchitecture = "evm"
	ArchitectureSvm NetworkArchitecture = "svm"
)

type Network interface {
	Id() string
	Label() string
	ProjectId() string
	Architecture() NetworkArchitecture
	Config() *NetworkConfig
	Logger() *zerolog.Logger
	GetMethodMetrics(method string) TrackedMetrics
	Forward(ctx context.Context, nq *NormalizedRequest) (*NormalizedResponse, error)
	GetFinality(ctx context.Context, req *NormalizedRequest, resp *NormalizedResponse) DataFinalityState
}

// EvmNetwork is the EVM-specific view of a Network. Callers read the head
// through EvmHeadOf, which type-asserts and degrades to the zero Head.
type EvmNetwork interface {
	Network
	// EvmHead is the head a request serves (spec §6.4, §7): req's, taken
	// once per request from its lane, else the request bound to ctx, else
	// the network-wide lane's. It never computes a ballot for a lane.
	EvmHead(ctx context.Context, req *NormalizedRequest) EvmHead
	// EvmLaneHead is the published Head of the lane keyed groupKey, ""
	// being the network-wide lane, materialized from selector on first use:
	// for readers bound to a lane, not to a request (spec §6.4, §6.5).
	EvmLaneHead(groupKey, selector string) EvmHead
}

// EvmHead is one lane's head (spec §6.4, §6.5), published as one value. The
// zero Head is an unknown one. Latest, LatestHash and Firm are the lane's
// one view, the chain tracker's; the rest is the head loop's number facts.
type EvmHead struct {
	// Latest is the view's Head: the highest pin one counted hash vote
	// holds, undisputed, within every guarantee, sticky until the pins move.
	Latest int64
	// LatestHash is Latest's hash, the hash every transport sends there.
	LatestHash [32]byte
	// Firm is the highest block at or below Latest that q voters hold;
	// routing and the refusal rule read it, nothing serves it alone.
	Firm int64
	// Finalized is the q-th highest voter finalized number, at most Latest.
	Finalized int64
	// Leader is the highest voter within Latest + maxRetryableBlockDistance.
	Leader Upstream
	// Ceiling is the raw max report of the lane's counted upstreams, at
	// least Latest, which exists; 0 while any of them has none.
	Ceiling int64
	// OldestReportAt is when the oldest counted report's evidence was
	// taken: a poll's send time, a push's receive time; zero while one has
	// none.
	OldestReportAt time.Time
	// Reach and ReachAt are the counted report that puts the chain
	// furthest once the blocks it made since each report are counted:
	// Reach its head, ReachAt when its evidence was taken; zero ReachAt
	// while a report has none. The refusal rule's near future reads them
	// (ReachNow): no counted upstream can hold a block far above it.
	Reach   int64
	ReachAt time.Time
	// CeilingPeak is the highest Ceiling this instance published for the
	// lane, and CeilingFellAt when Ceiling last fell below the one before
	// it: a cursor between the two heights names a block no upstream
	// reports any more (spec §10.1, §19 C16).
	CeilingPeak   int64
	CeilingFellAt time.Time
	// LatestAt is when the lane's view first stood at Latest; zero when
	// Latest is not the view's Head (a limit, a guarantee cap, the cache
	// head's anchor lowered it) or unknown.
	LatestAt time.Time
	// Anchored is set when the read follows the cache's fresh head
	// (evm.servedTip.followCacheHead): Latest is at most that head, and a
	// read of latest is served from the cache or by number, never sent to
	// an upstream as the tag. Unset with no cache head, or a stale one.
	Anchored bool
	// NextAt is the earliest time the lane's next block may come: LatestAt
	// plus the shorter of half a block and the shortest recent step of the
	// view; zero while LatestAt is, or too few steps or no block time are
	// known.
	NextAt time.Time
}

// Settled reports whether no block can reach an upstream within d of now:
// Latest stood at least d before now, and Latest+1 is due more than d
// after it. A lookup asked again d later can only find what a block that
// reached an upstream within d brought: one that just came (an upstream
// slower than this instance's view) or one about to come. Either time
// unknown: not settled.
func (h EvmHead) Settled(now time.Time, d time.Duration) bool {
	return !h.LatestAt.IsZero() && !h.NextAt.IsZero() && !now.Before(h.LatestAt.Add(d)) && h.NextAt.Sub(now) > d
}

// HeadRule is the refusal rule's row for block N on a request (spec §6.4).
type HeadRule uint8

const (
	// HeadServe: N ≤ Firm, or Latest or Firm unknown (tags and numbers
	// reach upstreams unchanged).
	HeadServe HeadRule = iota
	// HeadThin: Firm < N ≤ Latest: fewer than q voters hold N yet.
	HeadThin
	// HeadBand: Latest < N ≤ ceiling, or the ceiling is unknown.
	HeadBand
	// HeadFuture: above the ceiling, every counted report younger than
	// 1 bt: no counted upstream has N yet.
	HeadFuture
	// HeadStale: above the ceiling, a counted report 1 bt old or older:
	// those reports are Asked before N is decided again. Never a request's
	// verdict.
	HeadStale
)

// Rule is the refusal rule's row for block n at now, bt the block time (> 0).
func (h EvmHead) Rule(n int64, bt time.Duration, now time.Time) HeadRule {
	switch {
	case h.Latest <= 0 && (h.Ceiling <= 0 || n <= h.Ceiling):
		// No view yet (cold start): numbers reach upstreams unchanged,
		// unless every counted upstream reported one below n.
		return HeadServe
	case h.Latest <= 0:
	case n <= h.Latest:
		if h.Firm <= 0 || n <= h.Firm {
			return HeadServe
		}
		return HeadThin
	case h.Ceiling <= 0 || n <= h.Ceiling:
		return HeadBand
	}
	switch {
	case h.OldestReportAt.IsZero() || now.Sub(h.OldestReportAt) >= bt:
		return HeadStale
	}
	return HeadFuture
}

// HeadRefusal is the refusal rule's verdict for one request's block, taken
// once (spec §6.4), and the retryable misses its legs got since.
type HeadRefusal struct {
	Rule  HeadRule
	Block int64
	// Latest is the Latest Block was decided against, 0 while the head
	// was unknown: then no block was promised.
	Latest int64
	// Ceiling is the ceiling Block was decided against: the one the
	// near-future wait saw when Waited.
	Ceiling int64
	// Waited: N, near the ceiling, waited up to W for it (spec §6.4 near
	// future).
	Waited bool
	// Held: Block is pinned in the lane's view, so it exists; only who
	// holds it is open. Nothing Asks an upstream for it: each attempt
	// waits for the upstream that most likely holds it soonest (spec §6.4
	// held).
	Held bool
	// missed: a leg of the request took its retryable miss.
	missed atomic.Bool
	// answered: a leg took its answer after the miss.
	answered atomic.Bool
}

// FutureRule is the refusal counter's rule for a future verdict:
// future_after_wait when it waited for the ceiling.
func (r *HeadRefusal) FutureRule() string {
	if r.Waited {
		return "future_after_wait"
	}
	return "future"
}

// RefusalLeg is one execution of a request: a consensus slot, a hedge
// leg, or a retry round's. One goroutine runs it.
type RefusalLeg struct{ missed bool }

// MissVerdict is what an empty or missing answer above Firm is to the leg
// that got it.
type MissVerdict uint8

const (
	// MissRetry: the leg's one retryable miss; the leg retries.
	MissRetry MissVerdict = iota
	// MissFinal: the answer after the leg's miss, sent as the upstream
	// gave it.
	MissFinal
)

// AboveFirm reports whether the request's block is above its Firm: in the
// thin region, the band or the future, where a miss is retried once.
func (r *HeadRefusal) AboveFirm() bool {
	return r != nil && r.Rule != HeadServe
}

// Final reports whether an empty answer now may be the request's answer:
// a leg took its miss.
func (r *HeadRefusal) Final() bool {
	return r.AboveFirm() && r.missed.Load()
}

// Spent reports whether a leg took its answer after its miss, so an empty
// or missing answer may no longer be retried.
func (r *HeadRefusal) Spent() bool {
	return r != nil && r.answered.Load()
}

// Miss records an empty or missing answer leg got for a block above Firm:
// each leg's first is its retryable miss, so a miss in one leg never stops
// another (spec §6.4: every leg gets the one retry); the leg's next is
// final.
func (r *HeadRefusal) Miss(leg *RefusalLeg) MissVerdict {
	r.missed.Store(true)
	if !leg.missed {
		leg.missed = true
		return MissRetry
	}
	r.answered.Store(true)
	return MissFinal
}

// Retried reports whether a leg of the request got its retryable miss.
func (r *HeadRefusal) Retried() bool {
	return r != nil && r.missed.Load()
}

// Refuse records that the request's null is its answer: decided with no
// dispatch (a future block under shortCircuitFutureBlocks), or a numbered
// eth_getBlockByNumber above Latest that no upstream served (spec §6.4).
func (r *HeadRefusal) Refuse() {
	r.missed.Store(true)
	r.answered.Store(true)
}

// SvmNetwork is the SVM-specific view of a Network. Production Network
// implementations satisfy this automatically when the underlying network is
// SVM; EVM networks correctly fail the assertion.
type SvmNetwork interface {
	Network
	SvmHighestLatestSlot(ctx context.Context) int64
	SvmHighestFinalizedSlot(ctx context.Context) int64
	// SvmHighestFinalizedSlotMax is the finalized root of the MOST-ADVANCED
	// upstream, where SvmHighestFinalizedSlot is the majority tip. Rejection
	// gates (the getBlock availability guard) need this one: they must not null
	// out a slot some upstream can already serve. Advertising a head to clients
	// needs the majority one, so a lone rogue upstream cannot inflate it.
	SvmHighestFinalizedSlotMax(ctx context.Context) int64
	// SvmHighestLatestSlotMax is the latest (processed) slot of the
	// MOST-ADVANCED upstream, harvested answers included. The availability
	// guards raise the indexed frontier with it: an upstream that answered at
	// a slot holds it, even before its next shred-insert poll.
	SvmHighestLatestSlotMax(ctx context.Context) int64
	SvmHighestIndexedSlot(ctx context.Context) int64
	SvmEnforceBlockAvailability() bool
}

// EvmHeadOf returns n.EvmHead(ctx, req), or the zero Head when n is not an
// EVM network.
func EvmHeadOf(n Network, ctx context.Context, req *NormalizedRequest) EvmHead {
	if e, ok := n.(EvmNetwork); ok {
		return e.EvmHead(ctx, req)
	}
	return EvmHead{}
}

// RunsConsensus reports whether n votes req through a consensus policy;
// false for a network that cannot say.
func RunsConsensus(n Network, ctx context.Context, req *NormalizedRequest) bool {
	c, ok := n.(interface {
		RunsConsensus(context.Context, *NormalizedRequest) bool
	})
	return ok && c.RunsConsensus(ctx, req)
}

// IsValidArchitecture reports whether eRPC serves this architecture. It is the
// URL gate: /<project>/<architecture>/<chain> is rejected here before any
// network is looked up.
//
// The answer comes from the chain-family registry (chain_family.go), not from
// a list of names. Every architecture eRPC serves registers a family in its
// package init(), so adding a chain does not touch this function — and a chain
// that is registered can never be probeable and unroutable at the same time.
func IsValidArchitecture(architecture string) bool {
	_, ok := LookupChainFamily(NetworkArchitecture(architecture))
	return ok
}

// IsValidNetwork reports whether a config may name this network.
//
// Two questions live here, and they are not the same question:
//
//  1. IS THE ID WELL FORMED? That belongs to the chain family, so this asks
//     the registry through util.IsValidNetworkId. A family registers its own
//     rule at init, so adding a chain needs no edit here — the same property
//     IsValidArchitecture above already has. This function used to match
//     "evm:" and then "svm:" and then return false, which meant a config
//     naming btc:mainnet under providers[].onlyNetworks failed to load even
//     though every other gate in the request path accepted it.
//
//  2. DOES CONFIG ACCEPT IT? That is policy, and it stays here. EVM is the
//     one family with a rule of this kind: a chain id must be positive.
//     util.IsEvmNetworkIdBody deliberately accepts a negative integer (see its
//     comment) because it answers question 1 only. Delegating outright would
//     therefore start accepting "evm:0" and "evm:-1" in config, so the check
//     stays, right next to the other thing config decides.
func IsValidNetwork(network string) bool {
	if !util.IsValidNetworkId(network) {
		return false
	}
	if body, ok := strings.CutPrefix(network, "evm:"); ok {
		chainId, err := strconv.ParseInt(body, 10, 64)
		return err == nil && chainId > 0
	}
	return true
}

type QuantileTracker interface {
	Add(value float64)
	GetQuantile(qtile float64) time.Duration
	Reset()
}

type TrackedMetrics interface {
	ErrorRate() float64
	GetResponseQuantiles() QuantileTracker
}
