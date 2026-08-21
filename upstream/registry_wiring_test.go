package upstream

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The registry is the layer that decides WHICH upstreams a network may use.
// Every read below is on the request path, so a wrong answer here is not a
// reporting bug — it routes traffic somewhere the operator did not ask for.

// registerFor builds a bare upstream bound to `networkId` and puts it through
// the registry's real registration path. It needs no client: registration only
// reads the config and the network id.
func registerFor(t *testing.T, reg *UpstreamsRegistry, networkId string, cfg *common.UpstreamConfig) *Upstream {
	t.Helper()
	lg := zerolog.Nop()
	ups := &Upstream{ProjectId: "test", config: cfg, logger: &lg, appCtx: t.Context()}
	ups.networkId.Store(networkId)
	reg.doRegisterBootstrappedUpstream(ups)
	return ups
}

func TestRegistry_ShadowUpstreamsNeverJoinTheServingPool(t *testing.T) {
	reg, _ := newBootstrapTestRegistry(t)
	ctx := t.Context()

	serving := registerFor(t, reg, "evm:123", &common.UpstreamConfig{Id: "serving", Endpoint: "http://a.localhost"})
	shadow := registerFor(t, reg, "evm:123", &common.UpstreamConfig{
		Id:       "shadow",
		Endpoint: "http://b.localhost",
		Shadow:   &common.ShadowUpstreamConfig{Enabled: true},
	})

	// A shadow upstream exists to be compared against, never to answer a
	// client. Leaking it into the serving list sends real traffic to a canary.
	pool := reg.GetNetworkUpstreams(ctx, "evm:123")
	require.Len(t, pool, 1, "the serving pool must hold only the non-shadow upstream, got %v", pool)
	assert.Same(t, serving, pool[0])

	shadows := reg.GetNetworkShadowUpstreams("evm:123")
	require.Len(t, shadows, 1)
	assert.Same(t, shadow, shadows[0])

	// The health surface is the operator's view and must show both, or a
	// misbehaving shadow node is invisible until someone promotes it.
	health, err := reg.GetUpstreamsHealth()
	require.NoError(t, err)
	assert.Len(t, health.Upstreams, 2, "GetUpstreamsHealth must list the shadow upstream too")
}

func TestRegistry_GetSortedUpstreamsSaysWhenANetworkHasNone(t *testing.T) {
	reg, _ := newBootstrapTestRegistry(t)
	ctx := t.Context()

	_, err := reg.GetSortedUpstreams(ctx, "evm:999", "eth_call")
	require.Error(t, err, "an empty network must not return an empty list that reads as success")
	var notFound *common.ErrNoUpstreamsFound
	require.ErrorAs(t, err, &notFound, "got: %v", err)
	// The operator has to read WHICH network is empty out of the message.
	assert.Contains(t, err.Error(), "evm:999")
	assert.Contains(t, err.Error(), "test")

	first := registerFor(t, reg, "evm:123", &common.UpstreamConfig{Id: "a", Endpoint: "http://a.localhost"})
	second := registerFor(t, reg, "evm:123", &common.UpstreamConfig{Id: "b", Endpoint: "http://b.localhost"})

	got, err := reg.GetSortedUpstreams(ctx, "evm:123", "eth_call")
	require.NoError(t, err)
	require.Len(t, got, 2)
	// Registration order, and every element must be the live instance — a copy
	// would give the caller a stale view of cordon and breaker state.
	assert.Same(t, first, got[0].(*Upstream))
	assert.Same(t, second, got[1].(*Upstream))
}

func TestRegistry_GetWsUpstreamsKeepsOnlyWebsocketEndpoints(t *testing.T) {
	reg, _ := newBootstrapTestRegistry(t)
	ctx := t.Context()

	registerFor(t, reg, "evm:123", &common.UpstreamConfig{Id: "http", Endpoint: "http://a.localhost"})
	registerFor(t, reg, "evm:123", &common.UpstreamConfig{Id: "https", Endpoint: "https://b.localhost"})
	registerFor(t, reg, "evm:123", &common.UpstreamConfig{Id: "ws", Endpoint: "ws://c.localhost"})
	registerFor(t, reg, "evm:123", &common.UpstreamConfig{Id: "wss", Endpoint: "wss://d.localhost"})
	// A URL the parser rejects must be skipped, not panic and not counted.
	registerFor(t, reg, "evm:123", &common.UpstreamConfig{Id: "broken", Endpoint: "ws://%zz"})

	var ids []string
	for _, up := range reg.GetWsUpstreams(ctx, "evm:123") {
		ids = append(ids, up.Id())
	}
	assert.ElementsMatch(t, []string{"ws", "wss"}, ids,
		"only ws:// and wss:// endpoints can carry a subscription")

	assert.Empty(t, reg.GetWsUpstreams(ctx, "evm:404"), "an unknown network has no ws upstreams")
}

func TestRegistry_ReRegisteringTheSameUpstreamDoesNotDuplicateIt(t *testing.T) {
	reg, _ := newBootstrapTestRegistry(t)
	ctx := t.Context()

	cfg := &common.UpstreamConfig{Id: "a", Endpoint: "http://a.localhost"}
	ups := registerFor(t, reg, "evm:123", cfg)

	// Bootstrap tasks are retried against an already-registered upstream. A
	// duplicate entry would double that node's share of every round-robin.
	reg.doRegisterBootstrappedUpstream(ups)
	assert.Len(t, reg.GetAllUpstreams(), 1)
	assert.Len(t, reg.GetNetworkUpstreams(ctx, "evm:123"), 1)

	// A DIFFERENT instance carrying the same id is the same node; it may join
	// allUpstreams but must not appear twice in the network's serving pool.
	registerFor(t, reg, "evm:123", &common.UpstreamConfig{Id: "a", Endpoint: "http://a.localhost"})
	assert.Len(t, reg.GetNetworkUpstreams(ctx, "evm:123"), 1,
		"two instances of the same upstream id must not both serve the network")

	// The lock-free snapshot must track writes, or a newly bootstrapped
	// upstream stays invisible to the request path forever.
	registerFor(t, reg, "evm:123", &common.UpstreamConfig{Id: "b", Endpoint: "http://b.localhost"})
	assert.Len(t, reg.GetNetworkUpstreams(ctx, "evm:123"), 2,
		"the atomic snapshot did not pick up the newly registered upstream")
}

func TestRegistry_OverrideOrderForTestPinsTheServingOrder(t *testing.T) {
	reg, _ := newBootstrapTestRegistry(t)
	ctx := t.Context()

	for _, id := range []string{"c", "a", "b"} {
		registerFor(t, reg, "evm:123", &common.UpstreamConfig{Id: id, Endpoint: "http://" + id + ".localhost"})
	}

	idsOf := func() []string {
		var out []string
		for _, up := range reg.GetNetworkUpstreams(ctx, "evm:123") {
			out = append(out, up.Id())
		}
		return out
	}
	require.Equal(t, []string{"c", "a", "b"}, idsOf(), "registration order is the starting point")

	// Explicit order wins, and an id nobody registered is dropped rather than
	// inserted as a nil that the request path would dereference.
	reg.OverrideOrderForTest("evm:123", "b", "nosuch", "c", "a")
	assert.Equal(t, []string{"b", "c", "a"}, idsOf())

	// No ids means deterministic ascending order.
	reg.OverrideOrderForTest("evm:123")
	assert.Equal(t, []string{"a", "b", "c"}, idsOf())

	// An unknown network is a no-op, not a panic and not an empty pool.
	reg.OverrideOrderForTest("evm:404", "a")
	assert.Empty(t, reg.GetNetworkUpstreams(ctx, "evm:404"))
	assert.Equal(t, []string{"a", "b", "c"}, idsOf())
}

func TestRegistry_ExposesTheCollaboratorsItWasBuiltWith(t *testing.T) {
	reg, _ := newBootstrapTestRegistry(t)

	// Everything downstream (networks, the admin surface, the policy engine)
	// reaches these through the registry. A nil here is a nil-pointer panic on
	// the first request, not a degraded mode.
	require.NotNil(t, reg.GetInitializer())
	require.NotNil(t, reg.SharedStateRegistry())
	require.NotNil(t, reg.GetProvidersRegistry())
	assert.Same(t, reg.metricsTracker, reg.GetMetricsTracker())
	assert.Same(t, reg.initializer, reg.GetInitializer())
	assert.Same(t, reg.providersRegistry, reg.GetProvidersRegistry())
	assert.Equal(t, reg.sharedStateRegistry, reg.SharedStateRegistry())

	// Retained as a no-op until the policy engine owns scoring; callers still
	// check the error.
	assert.NoError(t, reg.RefreshUpstreamNetworkMethodScores())

	// The exported read lock must pair, or every later write deadlocks.
	reg.RLockUpstreams()
	reg.RUnlockUpstreams()
}

// ---------------------------------------------------------------------------
// Provider bootstrap
// ---------------------------------------------------------------------------

// fakeVendor is a common.Vendor whose answers are canned, so a provider task
// can be driven without any network.
type fakeVendor struct {
	supports    bool
	supportsErr error
	cfgs        []*common.UpstreamConfig
	genErr      error
}

func (v *fakeVendor) Name() string                             { return "fakevendor" }
func (v *fakeVendor) OwnsUpstream(*common.UpstreamConfig) bool { return false }
func (v *fakeVendor) GetVendorSpecificErrorIfAny(*common.NormalizedRequest, *http.Response, interface{}, map[string]interface{}) error {
	return nil
}
func (v *fakeVendor) SupportsNetwork(context.Context, *zerolog.Logger, common.VendorSettings, string) (bool, error) {
	return v.supports, v.supportsErr
}
func (v *fakeVendor) GenerateConfigs(_ context.Context, _ *zerolog.Logger, _ *common.UpstreamConfig, _ common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if v.genErr != nil {
		return nil, v.genErr
	}
	return v.cfgs, nil
}

func newFakeProvider(t *testing.T, v common.Vendor) *thirdparty.Provider {
	t.Helper()
	lg := zerolog.Nop()
	return thirdparty.NewProvider(&lg, &common.ProviderConfig{
		Id:                 "p1",
		Vendor:             "fakevendor",
		Settings:           common.VendorSettings{},
		UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
	}, v, nil)
}

func TestProviderBootstrapTask_SkipsANetworkTheProviderDoesNotServe(t *testing.T) {
	reg, _ := newBootstrapTestRegistry(t)
	task := reg.buildProviderBootstrapTask(newFakeProvider(t, &fakeVendor{supports: false}), "evm:123")

	// Not an error: most providers serve a handful of chains, and treating
	// "not mine" as a failure would keep the initializer retrying forever.
	require.NoError(t, task.Fn(t.Context()))
	assert.Empty(t, reg.GetAllUpstreams(), "a provider that does not serve the network must create nothing")
}

func TestProviderBootstrapTask_PropagatesTheProviderError(t *testing.T) {
	boom := errors.New("provider api is down")

	t.Run("support lookup fails", func(t *testing.T) {
		reg, _ := newBootstrapTestRegistry(t)
		task := reg.buildProviderBootstrapTask(newFakeProvider(t, &fakeVendor{supportsErr: boom}), "evm:123")

		err := task.Fn(t.Context())
		require.Error(t, err)
		// The initializer decides whether to retry from this error. A tidy
		// replacement error would hide the cause from every operator log.
		assert.ErrorIs(t, err, boom)
	})

	t.Run("config generation fails", func(t *testing.T) {
		reg, _ := newBootstrapTestRegistry(t)
		task := reg.buildProviderBootstrapTask(
			newFakeProvider(t, &fakeVendor{supports: true, genErr: boom}), "evm:123")

		err := task.Fn(t.Context())
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)
	})
}

func TestProviderBootstrapTask_RegistersTheUpstreamsItGenerates(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	mockUpstreamHealthy()

	reg, _ := newBootstrapTestRegistry(t)
	vendor := &fakeVendor{supports: true, cfgs: []*common.UpstreamConfig{{
		Id:       "from-provider",
		Type:     common.UpstreamTypeEvm,
		Endpoint: bootstrapTestEndpoint,
		Evm:      &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(time.Hour)},
	}}}
	task := reg.buildProviderBootstrapTask(newFakeProvider(t, vendor), "evm:123")

	require.NoError(t, task.Fn(t.Context()))

	// A provider-discovered upstream has to end up in the same serving pool a
	// statically configured one does, or lazy-loaded networks never get one.
	pool := reg.GetNetworkUpstreams(t.Context(), "evm:123")
	require.Len(t, pool, 1)
	assert.Equal(t, "from-provider", pool[0].Id())
}
