package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestAuthStrategyConfig_SetDefaults_InfersTheTypeFromTheBlockAnOperatorWrote.
// An operator who writes `secret: {...}` and forgets `type: secret` must still
// get a working strategy; leaving Type empty would silently authenticate nobody
// and every request would be rejected.
func TestAuthStrategyConfig_SetDefaults_InfersTheTypeFromTheBlockAnOperatorWrote(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  *AuthStrategyConfig
		want AuthType
	}{
		{"secret block", &AuthStrategyConfig{Secret: &SecretStrategyConfig{Id: "s", Value: "v"}}, AuthTypeSecret},
		{"database block", &AuthStrategyConfig{Database: &DatabaseStrategyConfig{}}, AuthTypeDatabase},
		{"jwt block", &AuthStrategyConfig{Jwt: &JwtStrategyConfig{}}, AuthTypeJwt},
		{"siwe block", &AuthStrategyConfig{Siwe: &SiweStrategyConfig{}}, AuthTypeSiwe},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, tc.cfg.SetDefaults())
			require.Equal(t, tc.want, tc.cfg.Type)
		})
	}
}

// TestAuthStrategyConfig_SetDefaults_MaterializesTheBlockNamedByTheType is the
// other direction: `type: jwt` with no jwt block must still produce a usable
// (defaulted) block rather than a nil dereference at auth time.
func TestAuthStrategyConfig_SetDefaults_MaterializesTheBlockNamedByTheType(t *testing.T) {
	t.Parallel()

	jwt := &AuthStrategyConfig{Type: AuthTypeJwt}
	require.NoError(t, jwt.SetDefaults())
	require.NotNil(t, jwt.Jwt)
	require.Equal(t, "rlm", jwt.Jwt.RateLimitBudgetClaimName)

	network := &AuthStrategyConfig{Type: AuthTypeNetwork}
	require.NoError(t, network.SetDefaults())
	require.NotNil(t, network.Network)

	secret := &AuthStrategyConfig{Type: AuthTypeSecret}
	require.NoError(t, secret.SetDefaults())
	require.NotNil(t, secret.Secret)

	siwe := &AuthStrategyConfig{Type: AuthTypeSiwe}
	require.NoError(t, siwe.SetDefaults())
	require.NotNil(t, siwe.Siwe)

	database := &AuthStrategyConfig{Type: AuthTypeDatabase}
	require.NoError(t, database.SetDefaults())
	require.NotNil(t, database.Database)
	require.NotNil(t, database.Database.Connector)
}

// TestAuthStrategyConfig_SetDefaults_TheLastBlockWinsWhenSeveralArePresent
// pins the defaulting half of a two-part contract. SetDefaults infers `type`
// from block presence, so with several blocks the LAST inference in source
// order wins and the losing block stays in the config looking active. That is
// a config mistake — docs/pages/config/auth.mdx gotcha 11 says "Never set
// multiple sub-config blocks in one strategy entry" — so SetDefaults does not
// have to reach a sensible answer here. Validate is what rejects the shape;
// see TestAuthStrategyConfig_Validate_RejectsSeveralBlocksInOneStrategy.
// SetDefaults stays permissive because it also runs on configs that are never
// validated (tests, TS export), and because the single-block inference it
// exists for must not change.
func TestAuthStrategyConfig_SetDefaults_TheLastBlockWinsWhenSeveralArePresent(t *testing.T) {
	t.Parallel()

	s := &AuthStrategyConfig{
		Type:   AuthTypeNetwork,
		Secret: &SecretStrategyConfig{Id: "s", Value: "v"},
		Jwt:    &JwtStrategyConfig{},
	}
	require.NoError(t, s.SetDefaults())

	require.Equal(t, AuthTypeJwt, s.Type,
		"the declared type is overwritten without a word to the operator")
	require.NotNil(t, s.Secret, "the ignored block is still present and looks active")

	// The operator never gets to run on this config: Validate stops it, and
	// stops it for the ambiguity itself rather than for some incidental
	// complaint about the block that happened to win.
	err := s.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "exactly one")
}

// TestAuthStrategyConfig_Validate_RejectsSeveralBlocksInOneStrategy is the
// enforcing half. An operator who leaves an old `secret:` block above a new
// `jwt:` block would otherwise silently swap who authenticates, so the config
// is rejected at startup instead of being resolved by source order.
func TestAuthStrategyConfig_Validate_RejectsSeveralBlocksInOneStrategy(t *testing.T) {
	t.Parallel()

	// The shape is reachable from plain YAML — both keys are ordinary fields.
	var s AuthStrategyConfig
	require.NoError(t, yaml.Unmarshal([]byte(`
type: jwt
secret:
  id: leftover
  value: old-token
jwt:
  verificationKeys:
    kid: pem
`), &s))
	require.NotNil(t, s.Secret)
	require.NotNil(t, s.Jwt)

	require.NoError(t, s.SetDefaults())
	err := s.Validate()
	require.Error(t, err, "two blocks in one strategy must not be silently resolved")
	require.Contains(t, err.Error(), "exactly one")
	require.Contains(t, err.Error(), "secret")
	require.Contains(t, err.Error(), "jwt")
}

// TestAuthStrategyConfig_Validate_AcceptsEverySingleBlockShape guards the other
// side: the rejection must not fire on any config an operator would really
// write. SetDefaults materializes the block named by `type`, so a well-formed
// strategy has exactly one block by the time Validate sees it.
func TestAuthStrategyConfig_Validate_AcceptsEverySingleBlockShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  *AuthStrategyConfig
	}{
		{"type only, block materialized", &AuthStrategyConfig{Type: AuthTypeNetwork}},
		{"block only, type inferred", &AuthStrategyConfig{Secret: &SecretStrategyConfig{Id: "s", Value: "v"}}},
		{"type and matching block", &AuthStrategyConfig{
			Type:   AuthTypeSecret,
			Secret: &SecretStrategyConfig{Id: "s", Value: "v"},
		}},
		{"jwt with keys", &AuthStrategyConfig{
			Type: AuthTypeJwt,
			Jwt:  &JwtStrategyConfig{VerificationKeys: map[string]string{"kid": "pem"}},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, tc.cfg.SetDefaults())
			require.NoError(t, tc.cfg.Validate())
		})
	}
}

// TestAuthConfig_SetDefaults_ReachesEveryStrategy — a defaulting pass that
// stopped at the first strategy would leave later ones unusable.
func TestAuthConfig_SetDefaults_ReachesEveryStrategy(t *testing.T) {
	t.Parallel()

	a := &AuthConfig{Strategies: []*AuthStrategyConfig{
		{Type: AuthTypeSecret},
		{Type: AuthTypeJwt},
		{Type: AuthTypeDatabase},
	}}
	require.NoError(t, a.SetDefaults())

	require.NotNil(t, a.Strategies[0].Secret)
	require.Equal(t, "rlm", a.Strategies[1].Jwt.RateLimitBudgetClaimName)
	require.Equal(t, 3, a.Strategies[2].Database.Retry.MaxAttempts)
}

// TestJwtStrategyConfig_SetDefaults_OnlySetsTheRefreshIntervalWithAJwksUrl.
// A refresh interval on a strategy with static keys would start a background
// fetch loop against nothing.
func TestJwtStrategyConfig_SetDefaults_OnlySetsTheRefreshIntervalWithAJwksUrl(t *testing.T) {
	t.Parallel()

	static := &JwtStrategyConfig{VerificationKeys: map[string]string{"kid": "pem"}}
	require.NoError(t, static.SetDefaults())
	require.Equal(t, Duration(0), static.VerificationJwksRefreshInterval)
	require.Equal(t, "rlm", static.RateLimitBudgetClaimName)

	remote := &JwtStrategyConfig{VerificationJwksUrl: "https://issuer.example/.well-known/jwks.json"}
	require.NoError(t, remote.SetDefaults())
	require.Equal(t, Duration(time.Hour), remote.VerificationJwksRefreshInterval)

	explicit := &JwtStrategyConfig{
		VerificationJwksUrl:             "https://issuer.example/.well-known/jwks.json",
		VerificationJwksRefreshInterval: Duration(5 * time.Minute),
	}
	require.NoError(t, explicit.SetDefaults())
	require.Equal(t, Duration(5*time.Minute), explicit.VerificationJwksRefreshInterval)

	custom := &JwtStrategyConfig{RateLimitBudgetClaimName: "budget"}
	require.NoError(t, custom.SetDefaults())
	require.Equal(t, "budget", custom.RateLimitBudgetClaimName)
}

// TestDatabaseStrategyConfig_SetDefaults_ShippedFallbacks. Every one of these
// bounds a live auth lookup: the cache sizes bound memory, the retry bounds how
// long a request waits on a slow auth database, and maxWait bounds the whole
// lookup.
func TestDatabaseStrategyConfig_SetDefaults_ShippedFallbacks(t *testing.T) {
	t.Parallel()

	d := &DatabaseStrategyConfig{}
	require.NoError(t, d.SetDefaults())

	require.NotNil(t, d.Connector)
	require.NotNil(t, d.Cache)
	require.Equal(t, time.Hour, *d.Cache.TTL)
	require.Equal(t, int64(10000), *d.Cache.MaxSize)
	require.Equal(t, int64(1<<30), *d.Cache.MaxCost)
	require.Equal(t, int64(100000), *d.Cache.NumCounters)

	require.Equal(t, 3, d.Retry.MaxAttempts)
	require.Equal(t, Duration(100*time.Millisecond), d.Retry.BaseBackoff)

	require.False(t, d.FailOpen.Enabled, "auth must fail CLOSED unless an operator opts out")
	require.Equal(t, "emergency-failopen", d.FailOpen.UserId)

	require.Equal(t, Duration(time.Second), d.MaxWait)
}

// TestDatabaseStrategyConfig_SetDefaults_KeepsExplicitValues — an operator who
// tuned the auth cache must not have it reset on every reload.
func TestDatabaseStrategyConfig_SetDefaults_KeepsExplicitValues(t *testing.T) {
	t.Parallel()

	ttl := 5 * time.Minute
	size := int64(7)
	d := &DatabaseStrategyConfig{
		Cache:    &DatabaseStrategyCacheConfig{TTL: &ttl, MaxSize: &size},
		Retry:    &DatabaseRetryConfig{MaxAttempts: 9, BaseBackoff: Duration(2 * time.Second)},
		FailOpen: &DatabaseFailOpenConfig{Enabled: true, UserId: "ops"},
		MaxWait:  Duration(4 * time.Second),
	}
	require.NoError(t, d.SetDefaults())

	require.Equal(t, 5*time.Minute, *d.Cache.TTL)
	require.Equal(t, int64(7), *d.Cache.MaxSize)
	require.Equal(t, 9, d.Retry.MaxAttempts)
	require.Equal(t, Duration(2*time.Second), d.Retry.BaseBackoff)
	require.True(t, d.FailOpen.Enabled)
	require.Equal(t, "ops", d.FailOpen.UserId)
	require.Equal(t, Duration(4*time.Second), d.MaxWait)
}

// TestDatabaseRetryConfig_SetDefaults_RejectsANonPositiveAttemptCount. Zero or
// negative attempts would mean the auth lookup never runs and every caller is
// rejected.
func TestDatabaseRetryConfig_SetDefaults_RejectsANonPositiveAttemptCount(t *testing.T) {
	t.Parallel()

	for _, in := range []int{0, -1} {
		r := &DatabaseRetryConfig{MaxAttempts: in}
		require.NoError(t, r.SetDefaults())
		require.Equal(t, 3, r.MaxAttempts)
	}
}

// TestRateLimiterConfig_SetDefaults_ReachesEveryBudgetAndRule. A rule that
// never gets defaulted has period zero-value and method "", which matches
// nothing — so the operator's budget silently stops limiting.
func TestRateLimiterConfig_SetDefaults_ReachesEveryBudgetAndRule(t *testing.T) {
	t.Parallel()

	r := &RateLimiterConfig{
		Budgets: []*RateLimitBudgetConfig{
			{Id: "free", Rules: []*RateLimitRuleConfig{{MaxCount: 10}, {MaxCount: 20}}},
			{Id: "paid", Rules: []*RateLimitRuleConfig{{MaxCount: 100}}},
		},
	}
	require.NoError(t, r.SetDefaults())

	require.NotNil(t, r.Store)
	require.Equal(t, "memory", r.Store.Driver, "an unconfigured store must be local, not remote")

	for _, b := range r.Budgets {
		for _, rule := range b.Rules {
			require.Equal(t, "*", rule.Method)
			require.Equal(t, RateLimitPeriodSecond, rule.Period)
		}
	}
}

// TestRateLimitStoreConfig_SetDefaults_OnlyBuildsARedisBlockForRedis. A redis
// block materialized for the memory driver would make erpc try to dial an empty
// address on start-up.
func TestRateLimitStoreConfig_SetDefaults_OnlyBuildsARedisBlockForRedis(t *testing.T) {
	t.Parallel()

	mem := &RateLimitStoreConfig{}
	require.NoError(t, mem.SetDefaults())
	require.Equal(t, "memory", mem.Driver)
	require.Nil(t, mem.Redis)

	red := &RateLimitStoreConfig{Driver: "redis"}
	require.NoError(t, red.SetDefaults())
	require.Equal(t, "redis", red.Driver)
	require.NotNil(t, red.Redis, "the redis driver needs a defaulted connector block")
}

// TestRateLimitRuleConfig_SetDefaults_FallsBackToTheTightestPeriod. An
// unrecognised period must become "second" — the tightest window — so a typo
// makes the limit stricter rather than effectively unlimited.
func TestRateLimitRuleConfig_SetDefaults_FallsBackToTheTightestPeriod(t *testing.T) {
	t.Parallel()

	bogus := &RateLimitRuleConfig{Period: RateLimitPeriod(99)}
	require.NoError(t, bogus.SetDefaults())
	require.Equal(t, RateLimitPeriodSecond, bogus.Period)
	require.Equal(t, "*", bogus.Method)

	// Every valid period must survive untouched, otherwise an hourly budget
	// silently becomes a per-second one.
	for _, p := range []RateLimitPeriod{
		RateLimitPeriodSecond, RateLimitPeriodMinute, RateLimitPeriodHour,
		RateLimitPeriodDay, RateLimitPeriodWeek, RateLimitPeriodMonth, RateLimitPeriodYear,
	} {
		rule := &RateLimitRuleConfig{Period: p, Method: "eth_call"}
		require.NoError(t, rule.SetDefaults())
		require.Equal(t, p, rule.Period, "period %s must survive defaulting", p)
		require.Equal(t, "eth_call", rule.Method)
	}
}

// TestRateLimitPeriod_UnmarshalYAML_AcceptsEveryDocumentedSpelling is the
// back-compat surface: older configs wrote Go durations, newer ones write the
// enum name. Reading "1m" as a second-long window would tighten an operator's
// budget sixtyfold without any error.
func TestRateLimitPeriod_UnmarshalYAML_AcceptsEveryDocumentedSpelling(t *testing.T) {
	t.Parallel()

	cases := map[string]RateLimitPeriod{
		"second": RateLimitPeriodSecond,
		"1s":     RateLimitPeriodSecond,
		"minute": RateLimitPeriodMinute,
		"1m":     RateLimitPeriodMinute,
		"60s":    RateLimitPeriodMinute,
		"hour":   RateLimitPeriodHour,
		"1h":     RateLimitPeriodHour,
		"3600s":  RateLimitPeriodHour,
		"day":    RateLimitPeriodDay,
		"24h":    RateLimitPeriodDay,
		"week":   RateLimitPeriodWeek,
		"168h":   RateLimitPeriodWeek,
		"month":  RateLimitPeriodMonth,
		"720h":   RateLimitPeriodMonth,
		"year":   RateLimitPeriodYear,
		"8760h":  RateLimitPeriodYear,
		"MINUTE": RateLimitPeriodMinute,
		" hour ": RateLimitPeriodHour,

		// Spellings no lookup entry covers, resolved by parsing the duration.
		// These are the only inputs that reach that branch, so without them the
		// whole duration-matching table is untested.
		"1000ms":    RateLimitPeriodSecond,
		"60000ms":   RateLimitPeriodMinute,
		"3600000ms": RateLimitPeriodHour,
		"1440m":     RateLimitPeriodDay,
		"10080m":    RateLimitPeriodWeek,
		"43200m":    RateLimitPeriodMonth,
		"525600m":   RateLimitPeriodYear,
	}

	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			var p RateLimitPeriod
			require.NoError(t, yaml.Unmarshal([]byte(`"`+in+`"`), &p))
			require.Equal(t, want, p, "%q must mean %s", in, want)
		})
	}
}

// TestRateLimitPeriod_UnmarshalYAML_AcceptsTheIntegerEnum keeps generated and
// programmatic configs loading.
func TestRateLimitPeriod_UnmarshalYAML_AcceptsTheIntegerEnum(t *testing.T) {
	t.Parallel()

	var p RateLimitPeriod
	require.NoError(t, yaml.Unmarshal([]byte(`3`), &p))
	require.Equal(t, RateLimitPeriodDay, p)
}

// TestRateLimitPeriod_UnmarshalYAML_RejectsAnythingElse. A period erpc cannot
// interpret must stop the config load, not quietly become "second" — the
// operator has to be told, because either answer changes the budget.
func TestRateLimitPeriod_UnmarshalYAML_RejectsAnythingElse(t *testing.T) {
	t.Parallel()

	for _, in := range []string{`"fortnight"`, `"90s"`, `"0s"`, `99`, `-1`} {
		t.Run(in, func(t *testing.T) {
			var p RateLimitPeriod
			err := yaml.Unmarshal([]byte(in), &p)
			require.Error(t, err)
			require.Contains(t, err.Error(), "must be one of")
		})
	}
}

// TestRateLimitPeriod_RoundTripsThroughYamlAndJson — the marshalled form is
// what an operator sees in the admin config dump, so it must be the readable
// name and it must read back as the same period.
func TestRateLimitPeriod_RoundTripsThroughYamlAndJson(t *testing.T) {
	t.Parallel()

	for _, p := range []RateLimitPeriod{
		RateLimitPeriodSecond, RateLimitPeriodMinute, RateLimitPeriodHour,
		RateLimitPeriodDay, RateLimitPeriodWeek, RateLimitPeriodMonth, RateLimitPeriodYear,
	} {
		out, err := p.MarshalYAML()
		require.NoError(t, err)
		require.Equal(t, p.String(), out)

		var back RateLimitPeriod
		require.NoError(t, yaml.Unmarshal([]byte(`"`+p.String()+`"`), &back))
		require.Equal(t, p, back)

		j, err := p.MarshalJSON()
		require.NoError(t, err)
		require.Equal(t, `"`+p.String()+`"`, string(j))
	}

	require.Equal(t, "unknown", RateLimitPeriod(99).String())
}

// TestRateLimitRuleConfig_ScopeString_ListsScopesInAFixedOrder. The scope
// string ends up in the rate-limit key, so an unstable order would split one
// budget across several counters and let a caller exceed it.
func TestRateLimitRuleConfig_ScopeString_ListsScopesInAFixedOrder(t *testing.T) {
	t.Parallel()

	all := &RateLimitRuleConfig{PerUser: true, PerNetwork: true, PerIP: true}
	require.Equal(t, "user,network,ip", all.ScopeString())

	require.Equal(t, "", (&RateLimitRuleConfig{}).ScopeString())
	require.Equal(t, "user", (&RateLimitRuleConfig{PerUser: true}).ScopeString())
	require.Equal(t, "network,ip", (&RateLimitRuleConfig{PerNetwork: true, PerIP: true}).ScopeString())
}

// TestCORSConfig_SetDefaults_ShippedFallbacks. Credentials default to OFF: an
// allow-all origin combined with credentials would let any web page on the
// internet make authenticated calls through the operator's proxy.
func TestCORSConfig_SetDefaults_ShippedFallbacks(t *testing.T) {
	t.Parallel()

	c := &CORSConfig{}
	require.NoError(t, c.SetDefaults())

	require.Equal(t, []string{"*"}, c.AllowedOrigins)
	require.Equal(t, []string{"GET", "POST", "OPTIONS"}, c.AllowedMethods)
	require.Contains(t, c.AllowedHeaders, "content-type")
	require.Contains(t, c.AllowedHeaders, "authorization")
	require.Contains(t, c.AllowedHeaders, "x-erpc-secret-token")
	require.NotNil(t, c.AllowCredentials)
	require.False(t, *c.AllowCredentials, "credentials must be off unless an operator asks")
	require.Equal(t, 3600, c.MaxAge)
}

// TestCORSConfig_SetDefaults_KeepsAnExplicitEmptyList. An operator who writes
// `allowedOrigins: []` means "nobody"; turning that into "*" would open the
// proxy to every origin.
func TestCORSConfig_SetDefaults_KeepsAnExplicitEmptyList(t *testing.T) {
	t.Parallel()

	yes := true
	c := &CORSConfig{
		AllowedOrigins:   []string{},
		AllowedMethods:   []string{"POST"},
		AllowedHeaders:   []string{"x-custom"},
		AllowCredentials: &yes,
		MaxAge:           60,
	}
	require.NoError(t, c.SetDefaults())

	require.Equal(t, []string{}, c.AllowedOrigins)
	require.Equal(t, []string{"POST"}, c.AllowedMethods)
	require.Equal(t, []string{"x-custom"}, c.AllowedHeaders)
	require.True(t, *c.AllowCredentials)
	require.Equal(t, 60, c.MaxAge)
}
