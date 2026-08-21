package auth

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRateLimitedAuthorizer builds an Authorizer backed by a real
// upstream.RateLimitersRegistry. Only the registry is real; the strategy is a
// plain secret strategy because acquireRateLimitPermit never consults it.
func newRateLimitedAuthorizer(t *testing.T, budgets []*common.RateLimitBudgetConfig, strategyBudget string) *Authorizer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	logger := zerolog.New(zerolog.NewTestWriter(t))
	registry, err := upstream.NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{
		Store:   &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: budgets,
	}, &logger)
	require.NoError(t, err)

	az, err := NewAuthorizer(ctx, &logger, "rl_project", &common.AuthStrategyConfig{
		Type:            common.AuthTypeSecret,
		Secret:          &common.SecretStrategyConfig{Value: "s3cr3t"},
		RateLimitBudget: strategyBudget,
	}, registry, 2)
	require.NoError(t, err)
	return az
}

func rateLimitedRequest(t *testing.T, userId, userBudget, clientIP string) *common.NormalizedRequest {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))
	if userId != "" || userBudget != "" {
		req.SetUser(&common.User{Id: userId, RateLimitBudget: userBudget})
	}
	if clientIP != "" {
		req.SetClientIP(clientIP)
	}
	return req
}

// TestAcquireRateLimitPermit_NoBudgetIsAlwaysAllowed pins that an authorizer
// with no budget — on the strategy or on the user — never refuses a request,
// no matter how many arrive. The registry it holds does have budgets, so a
// refusal here could only come from the empty-budget path.
func TestAcquireRateLimitPermit_NoBudgetIsAlwaysAllowed(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	az := newRateLimitedAuthorizer(t, []*common.RateLimitBudgetConfig{{
		Id:    "some-budget",
		Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 1, Period: common.RateLimitPeriodMinute}},
	}}, "" /* no strategy budget */)

	req := rateLimitedRequest(t, "u1", "", "10.0.0.1")
	for i := 0; i < 10; i++ {
		require.NoError(t, az.acquireRateLimitPermit(ctx, req, "eth_call"),
			"request %d must pass: neither the strategy nor the user has a budget", i+1)
	}
	require.NoError(t, az.acquireRateLimitPermit(ctx, nil, "eth_call"))

	// The registry really can refuse — the same authorizer with a user that
	// names the configured budget is limited after one request.
	budgeted := rateLimitedRequest(t, "u2", "some-budget", "10.0.0.2")
	require.NoError(t, az.acquireRateLimitPermit(ctx, budgeted, "eth_call"))
	require.Error(t, az.acquireRateLimitPermit(ctx, budgeted, "eth_call"),
		"the configured budget must still be enforceable through this authorizer")
}

// TestAcquireRateLimitPermit_NilRegistryIsSafeWithoutBudget documents a
// current-behaviour dependency: an authorizer built without a rate limiters
// registry must not crash when it has no budget to look up.
func TestAcquireRateLimitPermit_NilRegistryIsSafeWithoutBudget(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := zerolog.New(zerolog.NewTestWriter(t))
	az, err := NewAuthorizer(ctx, &logger, "rl_project", &common.AuthStrategyConfig{
		Type:   common.AuthTypeSecret,
		Secret: &common.SecretStrategyConfig{Value: "s3cr3t"},
	}, nil, 0)
	require.NoError(t, err)

	require.NotPanics(t, func() {
		assert.NoError(t, az.acquireRateLimitPermit(ctx, rateLimitedRequest(t, "u1", "", ""), "eth_call"))
		assert.NoError(t, az.acquireRateLimitPermit(ctx, nil, "eth_call"))
	}, "an unbudgeted authorizer with no registry must not panic")
}

// TestAcquireRateLimitPermit_EnforcesStrategyBudget pins the plain path: the
// budget on the auth strategy is enforced, and the request past the ceiling
// gets an ErrAuthRateLimitRuleExceeded carrying every field an operator needs
// to identify the caller.
func TestAcquireRateLimitPermit_EnforcesStrategyBudget(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	az := newRateLimitedAuthorizer(t, []*common.RateLimitBudgetConfig{{
		Id: "strategy-budget",
		Rules: []*common.RateLimitRuleConfig{
			{Method: "*", MaxCount: 2, Period: common.RateLimitPeriodMinute},
		},
	}}, "strategy-budget")

	req := rateLimitedRequest(t, "user-7", "", "10.1.2.3")

	require.NoError(t, az.acquireRateLimitPermit(ctx, req, "eth_call"), "request 1 is inside the ceiling")
	require.NoError(t, az.acquireRateLimitPermit(ctx, req, "eth_call"), "request 2 is inside the ceiling")

	err := az.acquireRateLimitPermit(ctx, req, "eth_call")
	require.Error(t, err, "request 3 must exceed the ceiling of 2")
	require.True(t, common.HasErrorCode(err, common.ErrCodeAuthRateLimitRuleExceeded))

	// The refusal must name the project, strategy, budget, rule, user and IP.
	// Assert each value is PRESENT and correct — an empty field would slip past
	// a check that only looks for the absence of a wrong value.
	se, ok := err.(common.StandardError)
	require.True(t, ok)
	details := se.Base().Details
	assert.Equal(t, "rl_project", details["projectId"])
	assert.Equal(t, string(common.AuthTypeSecret), details["strategy"])
	assert.Equal(t, "strategy-budget", details["budget"])
	assert.Equal(t, "method:eth_call", details["rule"])
	assert.Equal(t, "user-7", details["userId"])
	assert.Equal(t, "10.1.2.3", details["clientIp"])
}

// TestAcquireRateLimitPermit_UserBudgetOverridesStrategyBudget pins the
// per-user override. The strategy budget is deliberately loose and the user
// budget tight, so only the user budget can produce the refusal below.
func TestAcquireRateLimitPermit_UserBudgetOverridesStrategyBudget(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	az := newRateLimitedAuthorizer(t, []*common.RateLimitBudgetConfig{
		{
			Id:    "loose-strategy-budget",
			Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 1000, Period: common.RateLimitPeriodMinute}},
		},
		{
			Id:    "tight-user-budget",
			Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 1, Period: common.RateLimitPeriodMinute}},
		},
	}, "loose-strategy-budget")

	req := rateLimitedRequest(t, "user-9", "tight-user-budget", "10.9.9.9")

	require.NoError(t, az.acquireRateLimitPermit(ctx, req, "eth_call"))

	err := az.acquireRateLimitPermit(ctx, req, "eth_call")
	require.Error(t, err, "the user's own budget of 1 must be the one enforced")
	se, ok := err.(common.StandardError)
	require.True(t, ok)
	assert.Equal(t, "tight-user-budget", se.Base().Details["budget"],
		"the refusal must name the user's budget, not the strategy's")

	// A different user on the same authorizer, with no budget of its own,
	// falls back to the loose strategy budget and is still allowed. This
	// proves the tight limit above came from the user override.
	other := rateLimitedRequest(t, "user-10", "", "10.9.9.10")
	assert.NoError(t, az.acquireRateLimitPermit(ctx, other, "eth_call"))
}

// TestAcquireRateLimitPermit_UserWithoutBudgetKeepsStrategyBudget pins the
// fallback: a user record with an empty RateLimitBudget must not blank out the
// strategy budget.
func TestAcquireRateLimitPermit_UserWithoutBudgetKeepsStrategyBudget(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	az := newRateLimitedAuthorizer(t, []*common.RateLimitBudgetConfig{{
		Id:    "strategy-budget",
		Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 1, Period: common.RateLimitPeriodMinute}},
	}}, "strategy-budget")

	req := rateLimitedRequest(t, "user-11", "", "10.11.11.11")

	require.NoError(t, az.acquireRateLimitPermit(ctx, req, "eth_call"))

	err := az.acquireRateLimitPermit(ctx, req, "eth_call")
	require.Error(t, err, "the strategy budget must still apply to a user with no budget of its own")
	se, ok := err.(common.StandardError)
	require.True(t, ok)
	assert.Equal(t, "strategy-budget", se.Base().Details["budget"])
}

// TestAcquireRateLimitPermit_UnknownBudgetIsReported pins that a budget id
// with no matching config surfaces as an error instead of silently allowing
// the request. A typo in the operator's YAML must be loud.
func TestAcquireRateLimitPermit_UnknownBudgetIsReported(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	az := newRateLimitedAuthorizer(t, []*common.RateLimitBudgetConfig{{
		Id:    "configured-budget",
		Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 10, Period: common.RateLimitPeriodMinute}},
	}}, "typo-budget")

	err := az.acquireRateLimitPermit(ctx, rateLimitedRequest(t, "user-12", "", ""), "eth_call")
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrorCode("ErrRateLimitBudgetNotFound")))
	assert.Contains(t, err.Error(), "typo-budget", "the error must name the budget that was not found")

	// The same authorizer with a user budget that DOES exist succeeds, so the
	// error above is the missing budget and not a broken registry.
	ok := rateLimitedRequest(t, "user-13", "configured-budget", "")
	assert.NoError(t, az.acquireRateLimitPermit(ctx, ok, "eth_call"))
}

// TestAcquireRateLimitPermit_PerMethodRule pins that the method reaches the
// rule matcher: a rule scoped to one method must not limit another.
func TestAcquireRateLimitPermit_PerMethodRule(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	az := newRateLimitedAuthorizer(t, []*common.RateLimitBudgetConfig{{
		Id:    "per-method",
		Rules: []*common.RateLimitRuleConfig{{Method: "eth_call", MaxCount: 1, Period: common.RateLimitPeriodMinute}},
	}}, "per-method")

	req := rateLimitedRequest(t, "user-14", "", "10.14.14.14")

	require.NoError(t, az.acquireRateLimitPermit(ctx, req, "eth_call"))
	require.Error(t, az.acquireRateLimitPermit(ctx, req, "eth_call"),
		"the second eth_call must exceed the per-method ceiling of 1")

	// An unmatched method has no rule and must pass freely.
	for i := 0; i < 5; i++ {
		assert.NoError(t, az.acquireRateLimitPermit(ctx, req, "eth_blockNumber"),
			"eth_blockNumber has no rule and must not be limited")
	}
}

// TestAcquireRateLimitPermit_NilRequestUsesStrategyBudget pins the nil-request
// path used by callers that have not parsed a request yet: the strategy budget
// still applies and the refusal falls back to "n/a" identifiers.
func TestAcquireRateLimitPermit_NilRequestUsesStrategyBudget(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	az := newRateLimitedAuthorizer(t, []*common.RateLimitBudgetConfig{{
		Id:    "strategy-budget",
		Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 1, Period: common.RateLimitPeriodMinute}},
	}}, "strategy-budget")

	require.NoError(t, az.acquireRateLimitPermit(ctx, nil, "eth_call"))

	err := az.acquireRateLimitPermit(ctx, nil, "eth_call")
	require.Error(t, err)
	se, ok := err.(common.StandardError)
	require.True(t, ok)
	assert.Equal(t, "n/a", se.Base().Details["userId"], "a nil request must report a placeholder user id")
	assert.Equal(t, "n/a", se.Base().Details["clientIp"], "a nil request must report a placeholder client ip")
	assert.Equal(t, "strategy-budget", se.Base().Details["budget"])
}
