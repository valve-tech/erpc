package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bootstrapOnlyNetwork builds the smallest Network that Bootstrap can run
// against: no upstreams, no request path, just the config and the engine. An
// empty upstreams registry answers every lookup with an empty slice, which is
// all the engine's first tick needs.
func bootstrapOnlyNetwork(t *testing.T, ctx context.Context, engine *policy.Engine, id string, cfg *common.NetworkConfig) *Network {
	t.Helper()
	lg := zerolog.Nop()
	return &Network{
		networkId:         id,
		logger:            &lg,
		appCtx:            ctx,
		cfg:               cfg,
		upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, &lg, "test", nil, nil, nil, nil, nil, nil, nil, nil),
		policyEngine:      engine,
	}
}

// TestNetworkBootstrap_DoesNotWriteTheConfigItWasGiven pins the ownership rule
// behind entries 96, 97 and 131: Bootstrap derives a per-network selection
// policy and must leave the operator's config object exactly as it found it.
// Two networks can reach Bootstrap holding one config pointer, and the first
// write in Bootstrap runs outside the lock entry 96 added.
func TestNetworkBootstrap_DoesNotWriteTheConfigItWasGiven(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lg := zerolog.Nop()
	tracker := health.NewTracker(&lg, "test", 0)
	engine := policy.NewEngine(ctx, &lg, "test", tracker, nil, nil)
	defer engine.Stop()

	shared := &common.SelectionPolicyConfig{
		EvalFunc:             "(ups, _ctx) => ups",
		DisableTickerForTest: true,
	}
	require.NoError(t, shared.SetDefaults())

	on, off := true, false
	withFailover := &common.NetworkConfig{
		Architecture:    common.ArchitectureEvm,
		Evm:             &common.EvmNetworkConfig{ChainId: 1},
		Failover:        &common.FailoverConfig{OnDefaultsExhausted: &on},
		SelectionPolicy: shared,
	}
	withoutFailover := &common.NetworkConfig{
		Architecture:    common.ArchitectureEvm,
		Evm:             &common.EvmNetworkConfig{ChainId: 2},
		Failover:        &common.FailoverConfig{OnDefaultsExhausted: &off},
		SelectionPolicy: shared,
	}

	require.NoError(t, bootstrapOnlyNetwork(t, ctx, engine, "evm:1", withFailover).Bootstrap(ctx))
	require.NoError(t, bootstrapOnlyNetwork(t, ctx, engine, "evm:2", withoutFailover).Bootstrap(ctx))

	assert.False(t, shared.FailoverOnDefaultsExhausted,
		"Bootstrap must not write a derived flag into the config the operator owns")

	one := policy.RegisteredPolicyConfigForTest(engine, "evm:1")
	two := policy.RegisteredPolicyConfigForTest(engine, "evm:2")
	require.NotNil(t, one)
	require.NotNil(t, two)
	assert.NotSame(t, one, two,
		"each network must register its own policy config, not one shared struct")

	// The discriminating pair: with one shared struct, the second Bootstrap
	// overwrites the first network's failover flag and nothing logs it.
	assert.True(t, one.FailoverOnDefaultsExhausted,
		"evm:1 has failover.onDefaultsExhausted on and must keep it")
	assert.False(t, two.FailoverOnDefaultsExhausted,
		"evm:2 has failover.onDefaultsExhausted off and must keep it")
}

// TestNetworkBootstrap_CompilesADefaultPolicyWithoutTouchingTheSource covers
// the second unguarded write entry 131 names: SetDefaults fills EvalFunc,
// EvalFuncOriginal and CompiledProgram — the same three fields entry 96
// protects — on a config Bootstrap does not own.
func TestNetworkBootstrap_CompilesADefaultPolicyWithoutTouchingTheSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lg := zerolog.Nop()
	tracker := health.NewTracker(&lg, "test", 0)
	engine := policy.NewEngine(ctx, &lg, "test", tracker, nil, nil)
	defer engine.Stop()

	// A config built as a Go struct literal, exactly as a test or a
	// programmatic caller would: no defaults applied, so no compiled program.
	shared := &common.SelectionPolicyConfig{DisableTickerForTest: true}

	nw := bootstrapOnlyNetwork(t, ctx, engine, "evm:1", &common.NetworkConfig{
		Architecture:    common.ArchitectureEvm,
		Evm:             &common.EvmNetworkConfig{ChainId: 1},
		SelectionPolicy: shared,
	})
	require.NoError(t, nw.Bootstrap(ctx))

	assert.Nil(t, shared.CompiledProgram,
		"the operator's config must not gain a compiled program it never asked for")
	assert.Empty(t, shared.EvalFunc,
		"the operator's config must not gain an eval function it never wrote")
	assert.Empty(t, shared.EvalFuncOriginal)

	registered := policy.RegisteredPolicyConfigForTest(engine, "evm:1")
	require.NotNil(t, registered)
	assert.NotNil(t, registered.CompiledProgram,
		"the engine must still receive a compiled program")
}
