package upstream

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
)

// Upstream bootstrap for the fork's pluggable chain families.
//
// # WHAT detectFeatures ACTUALLY DOES, AND WHAT THE EQUIVALENT IS
//
// detectFeatures (upstream/upstream.go) is not validation. For EVM it asks the
// node for its chain id, refuses the upstream when the answer contradicts the
// config, stores the derived networkId — which is the ONLY thing that binds an
// upstream to a network — and arms chain-identity enforcement on clients that
// support it. For SVM it checks the cluster is named and stores the networkId;
// the identity check (the genesis hash) runs a step later in Bootstrap.
//
// Two things are therefore common to both: the upstream learns its networkId,
// and eRPC refuses to route to a node it has not confirmed is the right one.
//
// For a chain family the equivalent is this file. The networkId comes from
// `type` + `chain` (there is no chain id to ask for), and the confirmation is
// ONE ChainFamily.Probe — the same call that later drives routing. A node that
// does not answer the probe never joins the pool; the initializer retries it, so
// a node that was merely restarting joins as soon as it answers.
//
// A SYNCING node DOES join. It is cordoned instead, and the poller lifts the
// cordon when it catches up. Refusing it at bootstrap would look the same on day
// one and hide the node from operators for as long as it is behind — the tip it
// publishes is exactly what an operator watches to know whether it is catching
// up or stuck.

// defaultChainProbeInterval is the poll cadence when the upstream names none.
//
// Ten seconds is well under any of these chains' block times, so a stalled node
// is noticed long before its lag matters, and it is cheap: one getblockchaininfo
// per upstream per tick. Operators tune it via `chainProbeInterval`.
const defaultChainProbeInterval = 10 * time.Second

// maxChainProbeTimeout bounds one probe call. A probe that outlives its tick
// would let slow nodes accumulate in-flight requests until the pool is empty.
const maxChainProbeTimeout = 10 * time.Second

// chainProbeHttpClient is the probe's transport. Deliberately NOT the upstream's
// request-path client: that one carries failsafe policies, rate-limit budgets,
// cordoning and metrics, none of which a health check may trip. A probe that
// consumed the upstream's rate-limit budget or opened its breaker would turn the
// health check into the outage it exists to detect.
var chainProbeHttpClient = &http.Client{
	Timeout: maxChainProbeTimeout,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	},
}

// chainProbePollers holds the live poller for each upstream that has one.
//
// Keyed by *Upstream because the poller is fork-owned state and Upstream is an
// upstream-owned struct: adding a field there would be a merge conflict on every
// release for no behavioural gain. Bootstrap is retried on the SAME Upstream, so
// the LoadOrStore is what keeps a retry from leaking a second ticker goroutine.
var chainProbePollers sync.Map // *Upstream -> *chainProbePoller

// detectChainFamilyFeatures is detectFeatures for every upstream type that is
// not evm or svm. See the file comment for what it is the equivalent of.
func (u *Upstream) detectChainFamilyFeatures(ctx context.Context, cfg *common.UpstreamConfig) error {
	family, known := common.LookupChainFamilyForUpstreamType(cfg.Type)
	if !known {
		// Same wording the hardcoded gate used, so an operator who typo'd
		// `type:` reads the same message as before.
		return common.NewTaskFatal(fmt.Errorf("upstream type not supported: %s", cfg.Type))
	}

	// The chain name is config, so a bad one will never fix itself. Fail fatally
	// rather than let the initializer retry a typo forever.
	if cfg.Chain == "" {
		return common.NewTaskFatal(common.NewErrUpstreamClientInitialization(
			fmt.Errorf("upstream %q of type %q is missing `chain` (e.g. chain: mainnet)", cfg.Id, cfg.Type), u,
		))
	}
	if !family.ValidateNetworkId(cfg.Chain) {
		return common.NewTaskFatal(common.NewErrUpstreamClientInitialization(
			fmt.Errorf("upstream %q has chain %q, which the %s family rejects as a network name",
				cfg.Id, cfg.Chain, family.Family()), u,
		))
	}

	caller, hasTransport := common.NewFamilyProbeCaller(family, cfg.Endpoint, chainProbeHttpClient)
	if !hasTransport {
		// A missing probe transport is a gap in the family, not in the operator's
		// config, and no retry will fill it.
		return common.NewTaskFatal(common.NewErrUpstreamClientInitialization(
			fmt.Errorf("chain family %s provides no probe transport, so eRPC cannot tell "+
				"whether upstream %q is synced; it will not be routed to", family.Family(), cfg.Id), u,
		))
	}

	// One probe, before the upstream is routable. It proves the endpoint speaks
	// this chain's RPC and seeds the tip, so the first request already sees a
	// ranked pool instead of an unmeasured one.
	probeCtx, cancel := context.WithTimeout(ctx, maxChainProbeTimeout)
	probe := family.Probe(probeCtx, caller)
	cancel()
	if probe.Liveness == common.ChainDown {
		// Transient by assumption: an unreachable node is a restart, a firewall
		// blip or a node still loading its index. Leave the error retryable so
		// the initializer keeps trying and the upstream self-heals.
		return common.NewErrUpstreamClientInitialization(
			&common.BaseError{Code: "ErrUpstreamChainProbeFailed", Cause: probe.Err}, u,
		)
	}

	// The node's own answer about which chain it is on, checked before the
	// upstream becomes routable. This is the chain-family half of what
	// detectFeatures does for EVM when it refuses a node whose chain id
	// contradicts the config: without it a testnet bitcoind joins a mainnet
	// pool and serves testnet blocks to mainnet clients, and nothing anywhere
	// says so.
	//
	// A node that reported NO chain is accepted. An older or unusual client may
	// omit the field, and eRPC has then observed nothing to contradict — taking
	// a working upstream out of service over a missing string would be a
	// regression, and it would punish the operator for their client's version.
	// The reconciliation itself belongs to the family: "main" means "mainnet"
	// to bitcoind and nothing at all to an EVM node.
	if probe.Chain != "" && !family.MatchesConfiguredChain(cfg.Chain, probe.Chain) {
		// Fatal, like the EVM chain-id mismatch it mirrors. The config will not
		// change by itself and neither will the node's chain, so retrying only
		// hides the message an operator has to read.
		return common.NewTaskFatal(common.NewErrUpstreamClientInitialization(
			&common.BaseError{
				Code: "ErrUpstreamChainMismatch",
				Cause: fmt.Errorf("upstream %q is configured for chain %q but the node reports chain %q",
					cfg.Id, cfg.Chain, probe.Chain),
			}, u,
		))
	}

	u.networkId.Store(string(cfg.Type) + ":" + cfg.Chain)

	poller := u.chainProbePoller(family, caller)
	poller.apply(probe)
	poller.start()
	return nil
}

// chainProbePoller returns this upstream's poller, creating it once.
func (u *Upstream) chainProbePoller(family common.ChainFamily, caller common.ProbeCaller) *chainProbePoller {
	interval := u.Config().ChainProbeInterval.Duration()
	if interval <= 0 {
		interval = defaultChainProbeInterval
	}
	fresh := &chainProbePoller{
		upstream: u,
		family:   family,
		caller:   caller,
		interval: interval,
	}
	existing, loaded := chainProbePollers.LoadOrStore(u, fresh)
	if loaded {
		return existing.(*chainProbePoller)
	}
	return fresh
}

// chainProbePoller keeps one upstream's tip and rotation state current by
// re-running ChainFamily.Probe on a ticker.
//
// It is the chain-family counterpart of EvmStatePoller and SvmStatePoller, and
// it is much smaller than either because everything it feeds is already
// chain-agnostic: health.Tracker stores a plain int64 height, and the cordon
// flag the selection policy reads (`removeCordoned`) knows nothing about chains.
// Those two writes are the whole integration.
type chainProbePoller struct {
	upstream *Upstream
	family   common.ChainFamily
	caller   common.ProbeCaller
	interval time.Duration

	// started guards the ticker goroutine. Bootstrap runs again on every
	// initializer retry, and an unguarded `go loop()` leaks a goroutine per
	// retry that only appCtx cancellation can stop.
	started atomic.Bool

	// cordonedByProbe records whether THIS poller placed the cordon that now
	// stands. It gates the UNCORDON only: a recovery lifts a cordon the poller
	// placed, and never an operator's manual one.
	//
	// It does NOT gate the cordon. Whether to cordon is a question about the
	// upstream's live state, not about this poller's history, so apply() asks
	// the upstream. A latch there would let one manual uncordon disarm the
	// probe for good, and the node would serve stale reads unannounced.
	cordonedByProbe atomic.Bool
}

func (p *chainProbePoller) start() {
	if !p.started.CompareAndSwap(false, true) {
		return
	}
	go p.loop()
}

func (p *chainProbePoller) loop() {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	defer chainProbePollers.Delete(p.upstream)
	for {
		select {
		case <-p.upstream.appCtx.Done():
			return
		case <-ticker.C:
			p.poll()
		}
	}
}

func (p *chainProbePoller) poll() {
	ctx, cancel := context.WithTimeout(p.upstream.appCtx, maxChainProbeTimeout)
	defer cancel()
	p.apply(p.family.Probe(ctx, p.caller))
}

// apply turns one probe result into the two chain-agnostic signals the rest of
// eRPC already runs on: the upstream's tip, and whether it may take traffic.
func (p *chainProbePoller) apply(probe common.ChainProbe) {
	u := p.upstream

	// Bootstrap checked the chain once. An endpoint outlives the node behind
	// it — a DNS name or a load balancer gets repointed, and eRPC never
	// restarts — so the same answer is re-read on every tick.
	//
	// The tip is DROPPED along with the verdict, and that is the point. Head lag
	// is derived from the highest tip in the network, so one testnet height
	// among mainnet upstreams would make every correct node look millions of
	// blocks behind and empty the pool. A height from another chain is not
	// comparable with this one's, so it is not published at all.
	if configured := u.Config().Chain; probe.Chain != "" &&
		!p.family.MatchesConfiguredChain(configured, probe.Chain) {
		// Err as well as Detail: ChainDown always carries a cause, and the
		// cordon log prints it. A node that answers promptly and looks healthy
		// leaves an operator nothing else to go on.
		mismatch := fmt.Errorf("node reports chain %q, but this upstream is configured for %q",
			probe.Chain, configured)
		probe = common.ChainProbe{
			Liveness: common.ChainDown,
			Chain:    probe.Chain,
			Detail:   mismatch.Error(),
			Err:      mismatch,
		}
	}

	// The tip is published even for a node that is behind or syncing. The
	// tracker derives every upstream's lag from the highest tip in the network,
	// so a withheld height would read as "furthest behind" rather than
	// "unknown", and an operator would lose the number that says whether the
	// node is catching up.
	if probe.Tip > 0 && u.metricsTracker != nil {
		// Timestamp 0: these chains' probes carry no block timestamp, and
		// inventing one would poison the tracker's block-time EMA.
		u.metricsTracker.SetLatestBlockNumber(u, probe.Tip, 0)
	}

	if probe.Liveness.Serving() {
		if p.cordonedByProbe.CompareAndSwap(true, false) {
			u.logger.Info().
				Int64("tip", probe.Tip).
				Str("detail", probe.Detail).
				Msg("chain probe: upstream caught up; uncordoning")
			u.Uncordon("*", "chain probe: "+probe.Detail)
		}
		return
	}
	// The node is not serving, so it must be out of rotation. Ask the upstream
	// whether it already is, rather than trusting a latch: an operator (or
	// anything else) can uncordon between two ticks, and the next probe must
	// take the node out again. A cordon that already stands is left exactly as
	// it is — restating it every tick would churn the cordon gauge's reason
	// label and overwrite an operator's annotation.
	if _, cordoned := u.CordonedReason("*"); cordoned {
		return
	}
	p.cordonedByProbe.Store(true)
	reason := fmt.Sprintf("chain probe: %s (%s)", probe.Liveness, probe.Detail)
	u.logger.Warn().
		Int64("tip", probe.Tip).
		Err(probe.Err).
		Msg("chain probe: upstream is not serving; cordoning out of rotation")
	u.Cordon("*", reason)
}
