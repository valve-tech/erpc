// Where the `tier:fallback` rule lives for block-number aggregation.
//
// A fallback-tier upstream often runs ahead of the primaries (a third-party
// provider against your own nodes). If its head defines the network's served
// `latest`/`finalized`, tag translation asks for blocks the primaries cannot
// serve yet, and the request churns on "block not found". The fork once solved
// that with a tier check inside the aggregation function itself. That check is
// gone, and it is not coming back: two upstream mechanisms now cover the same
// ground, and this file pins both.
//
//  1. THE SELECTION POLICY DECIDES WHO VOTES. Every tip accessor sources its
//     candidate set from `Network.tipCandidateUpstreams`, which reads
//     `policyEngine.GetOrdered`. The default program's
//     `preferTag('!tier:fallback', { minHealthy: 1, fallback: 'tier:fallback' })`
//     is a hard filter, so one healthy primary removes every fallback from that
//     list — before the ballot is built, in BOTH the max mode and the
//     served-tip mode. The tier concept therefore stays in the one place that
//     already owns tiering, and Go keeps no second copy of it.
//
//  2. THE MAJORITY PICKER REFUSES TO FOLLOW A LONE LEADER. Should a fallback
//     reach the ballot anyway, `evm.PickServedTip` returns the block a strict
//     majority already has, so one upstream out ahead — of any tier — cannot
//     move the tip.
//
// The two are independent, which is the point: (1) is configuration and can be
// switched off, (2) is arithmetic and cannot.
package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNetwork_FallbackTierDoesNotDefineTheServedTip is the replacement for the
// fork's `EvmHighestFinalizedBlockNumber_FallbackExcludedWhenPrimariesUp`. That
// test passed `nil` for the policy engine, which switches off mechanism (1)
// entirely, and then asserted mechanism (1)'s outcome from the aggregation
// function — so it could only ever pass while a Go-level tier check existed.
func TestNetwork_FallbackTierDoesNotDefineTheServedTip(t *testing.T) {
	// The selection policy keeps a healthy fallback out of the candidate set,
	// so the fallback's head never reaches the ballot the tip is picked from.
	// The gap is deliberately under the default policy's
	// `blockNumberLagAbove(16)`: a wider one would exclude the PRIMARY for lag
	// and the assertion would pass for the wrong reason.
	t.Run("PolicyKeepsFallbackOutOfTheBallot", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Pin what a poll will WRITE to match what the Suggest calls below SET.
		//
		// setupSelectionPolicyNetwork seeds every upstream to latest=1000 /
		// finalized=900, and ups.Bootstrap starts a poll that can land after
		// this test's own seeding. When the poll disagreed it wrote finalized
		// back to 900, and SuggestFinalizedBlock could not re-raise it — see
		// statePollerHeads. That cost this test roughly one run in ten under the
		// load of six concurrent test-fast shards.
		statePollerHeads["http://primary.localhost"] = [2]int64{1000, 1000}
		statePollerHeads["http://fallback.localhost"] = [2]int64{1010, 1010}
		t.Cleanup(func() { statePollerHeads = map[string][2]int64{} })

		network := setupSelectionPolicyNetwork(t, ctx, []*common.UpstreamConfig{
			{
				Type:     common.UpstreamTypeEvm,
				Id:       "primary-node",
				Endpoint: "http://primary.localhost",
				Evm:      &common.EvmUpstreamConfig{ChainId: 123},
			},
			{
				Type:     common.UpstreamTypeEvm,
				Id:       "fallback-node",
				Endpoint: "http://fallback.localhost",
				Tags:     []string{common.TagTierFallback},
				Evm:      &common.EvmUpstreamConfig{ChainId: 123},
			},
		})

		primaryUp := upstreamByID(t, network, "primary-node").(*upstream.Upstream)
		fallbackUp := upstreamByID(t, network, "fallback-node").(*upstream.Upstream)

		primaryUp.EvmStatePoller().SuggestLatestBlock(1000)
		primaryUp.EvmStatePoller().SuggestFinalizedBlock(1000)
		fallbackUp.EvmStatePoller().SuggestLatestBlock(1010)
		fallbackUp.EvmStatePoller().SuggestFinalizedBlock(1010)

		require.Eventually(t, func() bool {
			return primaryUp.EvmEffectiveFinalizedBlock() == 1000 &&
				fallbackUp.EvmEffectiveFinalizedBlock() == 1010
		}, 20*time.Second, 10*time.Millisecond,
			"seeded heads should settle before the tip is read")

		policy.TickForTest(network.policyEngine, network.networkId, "*")

		require.Equal(t, []string{"primary-node"},
			idsFromUpstreams(network.policyEngine.GetOrdered(network.networkId, "*", "*")),
			"preferTag('!tier:fallback') must drop the fallback while the primary is healthy")

		assert.Equal(t, int64(1000), network.EvmHighestFinalizedBlockNumber(ctx),
			"the tip is picked over the policy-eligible set, which excludes the fallback")
		assert.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx),
			"the tip is picked over the policy-eligible set, which excludes the fallback")
	})

	// Second line of defence, with mechanism (1) removed on purpose: no policy
	// engine, so the ballot is the raw registry and the ahead fallback DOES
	// vote. The majority picker still refuses to advertise a block only one
	// upstream claims — the fork's scenario, decided by arithmetic instead of
	// by a tag.
	t.Run("MajorityPickIgnoresALoneAheadFallback", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		primary := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "primary-node",
			Endpoint: "http://primary.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		}
		fallback := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "fallback-node",
			Endpoint: "http://fallback.localhost",
			Tags:     []string{common.TagTierFallback},
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		}

		for _, host := range []string{"http://primary.localhost", "http://fallback.localhost"} {
			gock.New(host).Post("").Persist().
				Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), `eth_chainId`) }).
				Reply(200).JSON([]byte(`{"result":"0x7b"}`))
		}

		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
		require.NoError(t, err)
		metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)

		vr := thirdparty.NewVendorsRegistry()
		pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
		require.NoError(t, err)

		ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
			},
		})
		require.NoError(t, err)

		upstreamsRegistry := upstream.NewUpstreamsRegistry(
			ctx, &log.Logger, "test",
			[]*common.UpstreamConfig{primary, fallback}, ssr, rateLimitersRegistry, vr, pr, nil,
			metricsTracker, nil,
		)

		networkConfig := &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
				ServedTip: &common.EvmServedTipConfig{
					EnabledFor: []string{"latest", "finalized"},
					// The trajectory referee needs ten minutes of recorded
					// history to participate at all, so it is already inert
					// here; switching it off states that rather than relying
					// on it. Nothing else in this test is history-dependent.
					TrajectoryWindow: new(common.Duration),
				},
			},
		}
		network, err := NewNetwork(ctx, &log.Logger, "test", networkConfig,
			rateLimitersRegistry, upstreamsRegistry, metricsTracker, nil)
		require.NoError(t, err)

		upstreamsRegistry.Bootstrap(ctx)
		time.Sleep(200 * time.Millisecond)
		require.NoError(t, upstreamsRegistry.GetInitializer().WaitForTasks(ctx))
		require.NoError(t, network.Bootstrap(ctx))
		time.Sleep(250 * time.Millisecond)

		upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
		require.Len(t, upsList, 2)

		var primaryUp, fallbackUp *upstream.Upstream
		for _, ups := range upsList {
			switch ups.Id() {
			case "primary-node":
				primaryUp = ups
			case "fallback-node":
				fallbackUp = ups
			}
		}
		require.NotNil(t, primaryUp)
		require.NotNil(t, fallbackUp)

		primaryUp.EvmStatePoller().SuggestLatestBlock(1000)
		primaryUp.EvmStatePoller().SuggestFinalizedBlock(1000)
		fallbackUp.EvmStatePoller().SuggestLatestBlock(1050)
		fallbackUp.EvmStatePoller().SuggestFinalizedBlock(1050)

		require.Eventually(t, func() bool {
			return primaryUp.EvmEffectiveFinalizedBlock() == 1000 &&
				fallbackUp.EvmEffectiveFinalizedBlock() == 1050
		}, 20*time.Second, 10*time.Millisecond,
			"seeded heads should settle before the tip is read")

		assert.Equal(t, int64(1000), network.EvmHighestFinalizedBlockNumber(ctx),
			"N=2 majority is the lower head: one upstream out ahead never defines the tip")
		assert.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx),
			"N=2 majority is the lower head: one upstream out ahead never defines the tip")
	})
}
