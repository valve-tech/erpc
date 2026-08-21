package upstream

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The rate-limiter registry sits between the operator's budget config and the
// hot path. Two rules matter here: a Redis that is not there must fail OPEN
// rather than take the process down, and the auto-tuner's budget changes must
// reach the rules the hot path reads.

func newBudgetRegistry(t *testing.T, budgets ...*common.RateLimitBudgetConfig) *RateLimitersRegistry {
	t.Helper()
	lg := zerolog.Nop()
	r, err := NewRateLimitersRegistry(t.Context(), &common.RateLimiterConfig{Budgets: budgets}, &lg)
	require.NoError(t, err)
	return r
}

func TestRateLimitersRegistry_GetBudgetsReturnsTheConfiguredBudgets(t *testing.T) {
	cfg := []*common.RateLimitBudgetConfig{
		{Id: "b1", Rules: []*common.RateLimitRuleConfig{
			{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodSecond, WaitTime: common.Duration(0)},
		}},
		{Id: "b2"},
	}
	r := newBudgetRegistry(t, cfg...)

	// The admin surface enumerates budgets through this. An empty answer makes
	// every configured limit look absent.
	got := r.GetBudgets()
	require.Len(t, got, 2)
	assert.Equal(t, "b1", got[0].Id)
	assert.Equal(t, "b2", got[1].Id)
}

func TestRateLimitersRegistry_AdjustBudgetReachesTheRulesTheHotPathReads(t *testing.T) {
	r := newBudgetRegistry(t, &common.RateLimitBudgetConfig{
		Id: "b1", Rules: []*common.RateLimitRuleConfig{
			{Method: "eth_*", MaxCount: 100, Period: common.RateLimitPeriodSecond},
			{Method: "trace_*", MaxCount: 50, Period: common.RateLimitPeriodSecond},
		},
	})

	require.NoError(t, r.AdjustBudget("b1", "eth_call", 250))

	budget, err := r.GetBudget("b1")
	require.NoError(t, err)

	rules, err := budget.GetRulesByMethod("eth_call")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	// The auto-tuner writes through this path. If the change did not land on
	// the rule the limiter reads, the tuner would log an increase that never
	// happened and the upstream would stay throttled.
	assert.EqualValues(t, 250, rules[0].Config.MaxCount)

	// A rule the method does not match must be untouched.
	others, err := budget.GetRulesByMethod("trace_block")
	require.NoError(t, err)
	require.Len(t, others, 1)
	assert.EqualValues(t, 50, others[0].Config.MaxCount)
}

func TestRateLimitersRegistry_AdjustBudgetIgnoresEmptyArgumentsAndUnknownBudgets(t *testing.T) {
	r := newBudgetRegistry(t, &common.RateLimitBudgetConfig{
		Id: "b1", Rules: []*common.RateLimitRuleConfig{
			{Method: "*", MaxCount: 10, Period: common.RateLimitPeriodSecond},
		},
	})

	// An upstream with no budget configured calls this with an empty id every
	// tune cycle; erroring would flood the log.
	assert.NoError(t, r.AdjustBudget("", "eth_call", 5))
	assert.NoError(t, r.AdjustBudget("b1", "", 5))

	// An unknown budget IS an operator error and must surface.
	err := r.AdjustBudget("nosuch", "eth_call", 5)
	require.Error(t, err)
	var notFound *common.ErrRateLimitBudgetNotFound
	require.ErrorAs(t, err, &notFound, "got: %v", err)
	assert.Contains(t, err.Error(), "nosuch", "the error must name the budget the operator mistyped")

	budget, err := r.GetBudget("b1")
	require.NoError(t, err)
	rules, err := budget.GetRulesByMethod("eth_call")
	require.NoError(t, err)
	assert.EqualValues(t, 10, rules[0].Config.MaxCount, "a no-op adjust must not change anything")
}

func TestRemoteAdmissionCap_NeverDropsBelowTheFloor(t *testing.T) {
	// This cap decides when a budget load-sheds instead of queueing. Queueing
	// here was the root cause of the 2026-05-07 receipts death-spiral, and a
	// cap of ~0 would push every request into the shed path on normal traffic.
	for _, tc := range []struct {
		poolSize int
		want     int
	}{
		{poolSize: 0, want: 256},
		{poolSize: -5, want: 256},
		{poolSize: 1, want: 256},
		{poolSize: 8, want: 256},
		{poolSize: 9, want: 288},
		{poolSize: 100, want: 3200},
	} {
		assert.Equal(t, tc.want, remoteAdmissionCap(tc.poolSize), "connPoolSize=%d", tc.poolSize)
	}
}

func TestRateLimiterDefaults_OnlyOverrideWhenTheOperatorSetSomethingUsable(t *testing.T) {
	// The near-limit ratio is a fraction; 0 or 1 would make every check either
	// always or never "near limit".
	assert.Equal(t, float32(0.8), defaultNearLimitRatio(0))
	assert.Equal(t, float32(0.8), defaultNearLimitRatio(1))
	assert.Equal(t, float32(0.8), defaultNearLimitRatio(-0.5))
	assert.Equal(t, float32(0.5), defaultNearLimitRatio(0.5))

	// The key prefix namespaces this deployment's counters in a shared Redis.
	// An empty one would let two projects share each other's limits.
	assert.Equal(t, "erpc_rl_", defaultCacheKeyPrefix(""))
	assert.Equal(t, "mine_", defaultCacheKeyPrefix("mine_"))
}

func TestRateLimitersRegistry_MemoryStoreGetsACacheUpFront(t *testing.T) {
	lg := zerolog.Nop()
	r, err := NewRateLimitersRegistry(t.Context(), &common.RateLimiterConfig{
		Store:   &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{Id: "b1"}},
	}, &lg)
	require.NoError(t, err)
	assert.NotNil(t, r.GetCache(), "a memory store must have a cache before the first request")
}

func TestRateLimitersRegistry_UnreachableRedisFailsOpenInsteadOfCrashing(t *testing.T) {
	// The envoyproxy/ratelimit client panics on a bad connection. eRPC recovers
	// it so a Redis outage degrades rate limiting to fail-open instead of
	// killing the process — the whole point of the recover in connectRedisTask.
	lg := zerolog.Nop()
	r := &RateLimitersRegistry{
		appCtx: t.Context(),
		logger: &lg,
		cfg: &common.RateLimiterConfig{
			Store: &common.RateLimitStoreConfig{
				Driver: "redis",
				// Port 1 refuses immediately on every platform we run on.
				Redis: &common.RedisConnectorConfig{URI: "redis://127.0.0.1:1", ConnPoolSize: 1},
			},
		},
	}

	done := make(chan error, 1)
	go func() { done <- r.connectRedisTask(context.Background()) }()

	select {
	case err := <-done:
		require.Error(t, err, "an unreachable Redis must be reported, not silently accepted")
		// Cache stays nil: the budget path reads GetCache() and fails open.
		assert.Nil(t, r.GetCache(),
			"a failed connect must not install a half-built cache the hot path would use")
	case <-time.After(30 * time.Second):
		t.Fatal("connectRedisTask never returned; a Redis outage would block startup")
	}
}
