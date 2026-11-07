package erpc

import (
	"context"
	"time"

	"github.com/erpc/erpc/common"
)

// startTipFollow makes the served tip follow the network's corroborated head.
// When that head moves up (the newHeads-fed ws siblings of option (b) report a
// block within milliseconds), the tip voters that decide the served tip poll
// at once instead of waiting out their debounce. eth_blockNumber is answered
// from the served tip (#1256) and "latest" is anchored to it, while only
// method-serving upstreams vote on it (#1394), so without this the tip trails
// the chain by up to a poll debounce (bug log 200).
//
// It is called once per network, from startServedTipRefresh.
func (n *Network) startTipFollow() {
	if n.metricsTracker == nil {
		return
	}
	n.metricsTracker.OnNetworkHeadAdvance(func(networkId string, head int64) {
		if networkId != n.networkId || n.appCtx.Err() != nil {
			return
		}
		go n.pollTipVotersAtLeast(head)
	})
}

// pollTipVotersAtLeast asks the voters that decide the network-wide served tip
// to reach head. It walks the same voters as EvmRefreshLatestBlock: the
// best-ranked ones in routing order, only as many as the quorum needs, since
// a fresher head anywhere else cannot move the served value. A voter already
// at head counts toward the quorum; the others poll through
// PollLatestBlockNumberAtLeast, at most once per new block each.
func (n *Network) pollTipVotersAtLeast(head int64) {
	ctx := n.appCtx
	s := n.tipScopeOf(ctx)
	best, voters := -1, 0
	for u := range n.tipVoters(ctx, s) {
		if _, rank, ok := tipVoterRank(u, false, n.metricsTracker); ok {
			switch {
			case best < 0 || rank < best:
				best, voters = rank, 1
			case rank == best:
				voters++
			}
		}
	}
	if voters == 0 {
		return
	}
	var q common.ServedTipQuorum
	if n.cfg != nil && n.cfg.Evm != nil {
		q = n.cfg.Evm.ServedTipQuorum()
	}
	need := q.Of(voters)
	for u := range n.tipVoters(ctx, s) {
		voterHead, rank, ok := tipVoterRank(u, false, n.metricsTracker)
		if !ok || rank != best {
			continue
		}
		if voterHead < head {
			poller := u.(common.EvmUpstream).EvmStatePoller()
			go func() {
				pctx, cancel := context.WithTimeout(n.appCtx, 10*time.Second)
				defer cancel()
				if _, err := poller.PollLatestBlockNumberAtLeast(pctx, head); err != nil {
					n.logger.Debug().Err(err).Str("upstreamId", u.Id()).Int64("networkHead", head).
						Msg("tip voter poll on network head advance failed")
				}
			}()
		}
		if need--; need == 0 {
			return
		}
	}
}
