package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSecretAuthorizer(t *testing.T, cfg *common.AuthStrategyConfig) *Authorizer {
	t.Helper()
	logger := zerolog.Nop()
	az, err := NewAuthorizer(context.Background(), &logger, "test_project", cfg, nil, 0)
	require.NoError(t, err)
	return az
}

// TestAuthorizer_shouldApplyToMethod_AllowBeatsIgnore pins the documented
// precedence: allowMethods overrides ignoreMethods. Operators use the
// ignore-everything-then-allow-one pattern to build a method allowlist, so if
// the precedence flipped, the allowlist would stop applying and every method
// would run unauthenticated.
func TestAuthorizer_shouldApplyToMethod_AllowBeatsIgnore(t *testing.T) {
	cases := []struct {
		name          string
		ignoreMethods []string
		allowMethods  []string
		method        string
		wantApply     bool
	}{
		{
			name:      "no scoping configured applies to every method",
			method:    "eth_call",
			wantApply: true,
		},
		{
			name:          "an exactly ignored method is skipped",
			ignoreMethods: []string{"eth_chainId"},
			method:        "eth_chainId",
			wantApply:     false,
		},
		{
			name:          "a method outside the ignore list still applies",
			ignoreMethods: []string{"eth_chainId"},
			method:        "eth_call",
			wantApply:     true,
		},
		{
			name:          "a wildcard ignore skips the whole family",
			ignoreMethods: []string{"eth_get*"},
			method:        "eth_getLogs",
			wantApply:     false,
		},
		{
			name:          "a wildcard ignore leaves other families alone",
			ignoreMethods: []string{"eth_get*"},
			method:        "eth_sendRawTransaction",
			wantApply:     true,
		},
		{
			name:          "ignore-all plus allow-one is a method allowlist",
			ignoreMethods: []string{"*"},
			allowMethods:  []string{"eth_getLogs"},
			method:        "eth_getLogs",
			wantApply:     true,
		},
		{
			name:          "ignore-all plus allow-one skips everything else",
			ignoreMethods: []string{"*"},
			allowMethods:  []string{"eth_getLogs"},
			method:        "eth_call",
			wantApply:     false,
		},
		{
			name:          "the allow list wins even for an explicitly ignored method",
			ignoreMethods: []string{"eth_call"},
			allowMethods:  []string{"eth_call"},
			method:        "eth_call",
			wantApply:     true,
		},
		{
			name:         "an allow list on its own does not turn into a deny list",
			allowMethods: []string{"eth_getLogs"},
			method:       "eth_call",
			wantApply:    true,
		},
		{
			name:          "a later entry in the ignore list is still checked",
			ignoreMethods: []string{"eth_getLogs", "eth_call", "eth_chainId"},
			method:        "eth_chainId",
			wantApply:     false,
		},
		{
			name:          "a later entry in the allow list is still checked",
			ignoreMethods: []string{"*"},
			allowMethods:  []string{"eth_getLogs", "eth_call", "eth_chainId"},
			method:        "eth_chainId",
			wantApply:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			az := newSecretAuthorizer(t, &common.AuthStrategyConfig{
				Type:          common.AuthTypeSecret,
				IgnoreMethods: tc.ignoreMethods,
				AllowMethods:  tc.allowMethods,
				Secret:        &common.SecretStrategyConfig{Id: "ops", Value: "ops-token"},
			})
			assert.Equal(t, tc.wantApply, az.shouldApplyToMethod(tc.method))
		})
	}
}

// TestAuthRegistry_Authenticate_SkipsAnAuthorizerScopedAwayFromTheMethod proves
// the scope decision reaches the registry loop. It is the operator-visible
// behaviour: a strategy that does not cover the method must not authenticate
// the caller, and when no strategy covers it the registry must say so.
func TestAuthRegistry_Authenticate_SkipsAnAuthorizerScopedAwayFromTheMethod(t *testing.T) {
	registry := newSecretAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{
			{
				Type:          common.AuthTypeSecret,
				IgnoreMethods: []string{"eth_sendRawTransaction"},
				Secret:        &common.SecretStrategyConfig{Id: "reader", Value: "shared-token"},
			},
			{
				Type:         common.AuthTypeSecret,
				AllowMethods: []string{"eth_sendRawTransaction"},
				// The write strategy demands a different, stronger token.
				Secret: &common.SecretStrategyConfig{Id: "writer", Value: "write-token"},
			},
		},
	})

	payload := func(secret string) *AuthPayload {
		return &AuthPayload{Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: secret}}
	}

	t.Run("the read token authenticates a read method", func(t *testing.T) {
		user, err := registry.Authenticate(context.Background(), nil, "eth_call", payload("shared-token"))
		require.NoError(t, err)
		require.NotNil(t, user)
		assert.Equal(t, "reader", user.Id)
	})

	t.Run("the read token cannot authenticate the write method", func(t *testing.T) {
		// The reader strategy ignores eth_sendRawTransaction, so only the
		// writer strategy is consulted and it rejects the read token. A
		// regression here would let a read-only key broadcast transactions.
		user, err := registry.Authenticate(context.Background(), nil, "eth_sendRawTransaction", payload("shared-token"))
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))
		assert.Nil(t, user)
	})

	t.Run("the write token authenticates the write method", func(t *testing.T) {
		user, err := registry.Authenticate(context.Background(), nil, "eth_sendRawTransaction", payload("write-token"))
		require.NoError(t, err)
		require.NotNil(t, user)
		assert.Equal(t, "writer", user.Id)
	})
}

// TestAuthRegistry_Authenticate_RejectsWhenNothingMatches covers the two "no
// verdict" exits. Both must deny; a nil error here would mean an unauthenticated
// request reached the upstreams.
func TestAuthRegistry_Authenticate_RejectsWhenNothingMatches(t *testing.T) {
	registry := newSecretAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{
			{
				Type:   common.AuthTypeSecret,
				Secret: &common.SecretStrategyConfig{Id: "ops", Value: "ops-token"},
			},
		},
	})

	t.Run("a nil payload is rejected", func(t *testing.T) {
		user, err := registry.Authenticate(context.Background(), nil, "eth_call", nil)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))
		assert.Nil(t, user)
	})

	t.Run("a payload type no strategy supports is rejected", func(t *testing.T) {
		user, err := registry.Authenticate(context.Background(), nil, "eth_call", &AuthPayload{
			Type: common.AuthTypeNetwork,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no auth strategy matched")
		assert.Nil(t, user)
	})

	t.Run("every method scoped away is rejected", func(t *testing.T) {
		scoped := newSecretAuthRegistry(t, &common.AuthConfig{
			Strategies: []*common.AuthStrategyConfig{
				{
					Type:          common.AuthTypeSecret,
					IgnoreMethods: []string{"*"},
					Secret:        &common.SecretStrategyConfig{Id: "ops", Value: "ops-token"},
				},
			},
		})
		user, err := scoped.Authenticate(context.Background(), nil, "eth_call", &AuthPayload{
			Type:   common.AuthTypeSecret,
			Secret: &SecretPayload{Value: "ops-token"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no auth strategy matched")
		assert.Nil(t, user)
	})
}

// TestAuthRegistry_Authenticate_JoinsEveryStrategyError makes the multi-error
// path visible. An operator debugging a 401 needs to see why each candidate
// strategy rejected the caller, not just the last one.
func TestAuthRegistry_Authenticate_JoinsEveryStrategyError(t *testing.T) {
	registry := newSecretAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{
			{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Id: "a", Value: "token-a"}},
			{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Id: "b", Value: "token-b"}},
		},
	})

	user, err := registry.Authenticate(context.Background(), nil, "eth_call", &AuthPayload{
		Type:   common.AuthTypeSecret,
		Secret: &SecretPayload{Value: "wrong-token"},
	})
	require.Error(t, err)
	assert.Nil(t, user)

	// Two strategies rejected, so the joined message must carry two reasons.
	assert.Equal(t, 2, strings.Count(err.Error(), "invalid secret"),
		"both failing strategies must be reported, not just the last one")
}

// TestAuthRegistry_FindDatabaseStrategy_ReportsAMissingId keeps the lookup
// honest. A silent nil here would surface later as a nil-pointer panic in the
// admin path rather than a clear configuration error.
func TestAuthRegistry_FindDatabaseStrategy_ReportsAMissingId(t *testing.T) {
	registry := newSecretAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{
			{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Id: "ops", Value: "ops-token"}},
		},
	})

	strategy, err := registry.FindDatabaseStrategy("no-such-connector")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-connector")
	assert.Nil(t, strategy)
}

// TestNewAuthorizer_RejectsIncompleteConfig proves every strategy type demands
// its own config block. Booting with a half-written strategy would leave the
// operator believing authentication is on when it is not.
func TestNewAuthorizer_RejectsIncompleteConfig(t *testing.T) {
	logger := zerolog.Nop()

	cases := []struct {
		name string
		cfg  *common.AuthStrategyConfig
	}{
		{"nil config", nil},
		{"secret without its block", &common.AuthStrategyConfig{Type: common.AuthTypeSecret}},
		{"jwt without its block", &common.AuthStrategyConfig{Type: common.AuthTypeJwt}},
		{"siwe without its block", &common.AuthStrategyConfig{Type: common.AuthTypeSiwe}},
		{"network without its block", &common.AuthStrategyConfig{Type: common.AuthTypeNetwork}},
		{"database without its block", &common.AuthStrategyConfig{Type: common.AuthTypeDatabase}},
		{"unknown type", &common.AuthStrategyConfig{Type: common.AuthType("carrier-pigeon")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			az, err := NewAuthorizer(context.Background(), &logger, "test_project", tc.cfg, nil, 0)
			require.Error(t, err)
			assert.IsType(t, &common.ErrInvalidConfig{}, err,
				"a bad strategy config must fail as invalid config")
			assert.Nil(t, az)
		})
	}

	t.Run("an invalid network allowlist fails the authorizer too", func(t *testing.T) {
		az, err := NewAuthorizer(context.Background(), &logger, "test_project", &common.AuthStrategyConfig{
			Type:    common.AuthTypeNetwork,
			Network: &common.NetworkStrategyConfig{AllowedIPs: []string{"nonsense"}},
		}, nil, 0)
		require.Error(t, err)
		assert.Nil(t, az)
	})

	t.Run("a valid network strategy builds", func(t *testing.T) {
		az, err := NewAuthorizer(context.Background(), &logger, "test_project", &common.AuthStrategyConfig{
			Type:    common.AuthTypeNetwork,
			Network: &common.NetworkStrategyConfig{AllowLocalhost: true},
		}, nil, 0)
		require.NoError(t, err)
		require.NotNil(t, az)
		assert.IsType(t, &NetworkStrategy{}, az.strategy)
	})

	t.Run("a valid siwe strategy builds", func(t *testing.T) {
		az, err := NewAuthorizer(context.Background(), &logger, "test_project", &common.AuthStrategyConfig{
			Type: common.AuthTypeSiwe,
			Siwe: &common.SiweStrategyConfig{AllowedDomains: []string{"app.example.com"}},
		}, nil, 0)
		require.NoError(t, err)
		require.NotNil(t, az)
		assert.IsType(t, &SiweStrategy{}, az.strategy)
	})
}

// TestNewAuthRegistry_RejectsANilConfig keeps the registry from booting into a
// silently open state.
func TestNewAuthRegistry_RejectsANilConfig(t *testing.T) {
	logger := zerolog.Nop()
	r, err := NewAuthRegistry(context.Background(), &logger, "test_project", nil, nil)
	require.Error(t, err)
	assert.Nil(t, r)
}

// TestAuthRegistry_Authenticate_NoStrategiesAllowsEveryone pins the documented
// open-by-default behaviour of an empty strategy list, so nobody mistakes an
// empty list for a deny-all.
func TestAuthRegistry_Authenticate_NoStrategiesAllowsEveryone(t *testing.T) {
	registry := newSecretAuthRegistry(t, &common.AuthConfig{})
	user, err := registry.Authenticate(context.Background(), nil, "eth_call", &AuthPayload{
		Type: common.AuthTypeNetwork,
	})
	require.NoError(t, err)
	assert.Nil(t, user, "an unauthenticated pass-through must not invent a user identity")
}
