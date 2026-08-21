package common

import (
	"testing"
	"time"

	"github.com/grafana/sobek"
	"github.com/stretchr/testify/require"
)

// compiledEvalFunc stands in for what SetDefaults produces: a selection policy
// whose JS source already compiled.
func compiledEvalFunc(t *testing.T) *sobek.Program {
	t.Helper()
	prg, err := sobek.Compile("selectionPolicy", "(function(u){return u})", true)
	require.NoError(t, err)
	return prg
}

// Failsafe validation decides whether a retry, hedge, breaker or consensus
// policy is coherent. A policy that validates but is nonsense (a breaker that
// can never open, a consensus quota that can never be met) fails silently at
// request time: the operator sees traffic behaving as if the policy were
// absent, with nothing in the logs to say why.

func TestFailsafeConfig_Validate_MatchMethodAndRequestKind(t *testing.T) {
	// An empty matchMethod would match nothing, so the policy would never run.
	// '*' is the documented way to say "any method".
	require.ErrorContains(t, (&FailsafeConfig{}).Validate(),
		"failsafe.matchMethod cannot be empty, use '*'")

	for _, kind := range []string{"", "*", "user", "internal"} {
		require.NoError(t, (&FailsafeConfig{MatchMethod: "*", MatchRequestKind: kind}).Validate(),
			"matchRequestKind %q", kind)
	}

	err := (&FailsafeConfig{MatchMethod: "*", MatchRequestKind: "system"}).Validate()
	require.ErrorContains(t, err, "matchRequestKind 'system' is invalid")
}

// Each sub-policy's error must reach the top. A failsafe block that swallowed
// one would boot with a half-configured policy.
func TestFailsafeConfig_Validate_PropagatesEverySubPolicyError(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*FailsafeConfig)
		wantSub string
	}{
		{"timeout", func(f *FailsafeConfig) { f.Timeout = &TimeoutPolicyConfig{} }, "timeout.duration is required"},
		{"retry", func(f *FailsafeConfig) { f.Retry = &RetryPolicyConfig{} }, "backoffFactor must be greater than 0"},
		{"hedge", func(f *FailsafeConfig) { f.Hedge = &HedgePolicyConfig{} }, "hedge.delay is required"},
		{"circuitBreaker", func(f *FailsafeConfig) { f.CircuitBreaker = &CircuitBreakerPolicyConfig{} }, "halfOpenAfter is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &FailsafeConfig{MatchMethod: "*"}
			tc.mutate(f)
			require.ErrorContains(t, f.Validate(), tc.wantSub)
		})
	}
}

func TestTimeoutPolicyConfig_Validate_DurationIsRequiredAndChecked(t *testing.T) {
	require.ErrorContains(t, (&TimeoutPolicyConfig{}).Validate(), "timeout.duration is required")

	require.NoError(t,
		(&TimeoutPolicyConfig{Duration: &AdaptiveDuration{Base: Duration(5 * time.Second)}}).Validate())

	// The adaptive form's own rules must reach the top through the policy.
	require.ErrorContains(t,
		(&TimeoutPolicyConfig{Duration: &AdaptiveDuration{Quantile: 2}}).Validate(),
		"quantile must be between 0 and 1")
	require.ErrorContains(t,
		(&TimeoutPolicyConfig{Duration: &AdaptiveDuration{}}).Validate(),
		"must specify at least one of base/quantile/min/max")
}

func TestRetryPolicyConfig_Validate_BackoffFieldsMustBeSet(t *testing.T) {
	// A zero or negative backoff factor makes every retry fire immediately,
	// turning one slow upstream into a self-inflicted burst.
	require.ErrorContains(t, (&RetryPolicyConfig{}).Validate(), "backoffFactor must be greater than 0")
	require.ErrorContains(t, (&RetryPolicyConfig{BackoffFactor: -1}).Validate(), "backoffFactor must be greater than 0")

	require.ErrorContains(t,
		(&RetryPolicyConfig{BackoffFactor: 2}).Validate(),
		"backoffMaxDelay is required")

	require.NoError(t,
		(&RetryPolicyConfig{BackoffFactor: 2, BackoffMaxDelay: Duration(time.Second)}).Validate())
}

func TestHedgePolicyConfig_Validate_DelayIsRequired(t *testing.T) {
	require.ErrorContains(t, (&HedgePolicyConfig{}).Validate(), "hedge.delay is required")
	// An all-zero AdaptiveDuration is "not configured", not "zero delay". A
	// zero-delay hedge would double every request the moment it is sent.
	require.ErrorContains(t,
		(&HedgePolicyConfig{Delay: &AdaptiveDuration{}}).Validate(), "hedge.delay is required")

	require.NoError(t,
		(&HedgePolicyConfig{Delay: &AdaptiveDuration{Base: Duration(50 * time.Millisecond)}}).Validate())

	require.ErrorContains(t,
		(&HedgePolicyConfig{Delay: &AdaptiveDuration{Quantile: 1.5}}).Validate(),
		"quantile must be between 0 and 1")
}

func TestCircuitBreakerPolicyConfig_Validate_ThresholdsMustFitTheirCapacity(t *testing.T) {
	base := func() *CircuitBreakerPolicyConfig {
		return &CircuitBreakerPolicyConfig{
			HalfOpenAfter:            Duration(30 * time.Second),
			FailureThresholdCount:    5,
			FailureThresholdCapacity: 10,
			SuccessThresholdCount:    2,
			SuccessThresholdCapacity: 4,
		}
	}
	require.NoError(t, base().Validate())

	cases := []struct {
		name    string
		mutate  func(*CircuitBreakerPolicyConfig)
		wantSub string
	}{
		{"halfOpenAfter", func(c *CircuitBreakerPolicyConfig) { c.HalfOpenAfter = 0 }, "halfOpenAfter is required"},
		{"failure capacity", func(c *CircuitBreakerPolicyConfig) { c.FailureThresholdCapacity = 0 }, "failureThresholdCapacity must be greater than 0"},
		{"failure count", func(c *CircuitBreakerPolicyConfig) { c.FailureThresholdCount = 0 }, "failureThresholdCount must be greater than 0"},
		// A count larger than its capacity can never be reached, so the
		// breaker would never open however bad the upstream got.
		{"failure count over capacity", func(c *CircuitBreakerPolicyConfig) { c.FailureThresholdCount = 11 }, "failureThresholdCount must be less than or equal to failureThresholdCapacity"},
		{"success count", func(c *CircuitBreakerPolicyConfig) { c.SuccessThresholdCount = 0 }, "successThresholdCount must be greater than 0"},
		{"success capacity", func(c *CircuitBreakerPolicyConfig) { c.SuccessThresholdCapacity = 0 }, "successThresholdCapacity must be greater than 0"},
		// The mirror case: a breaker that can never close stays open forever
		// and permanently removes the upstream.
		{"success count over capacity", func(c *CircuitBreakerPolicyConfig) { c.SuccessThresholdCount = 5 }, "successThresholdCount must be less than or equal to"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			require.ErrorContains(t, c.Validate(), tc.wantSub)
		})
	}
}

func TestConsensusPolicyConfig_Validate_ParticipantArithmetic(t *testing.T) {
	base := func() *ConsensusPolicyConfig {
		return &ConsensusPolicyConfig{MaxParticipants: 3, AgreementThreshold: 2}
	}
	require.NoError(t, base().Validate())

	require.ErrorContains(t,
		(&ConsensusPolicyConfig{AgreementThreshold: 2}).Validate(),
		"maxParticipants must be greater than 0")
	require.ErrorContains(t,
		(&ConsensusPolicyConfig{MaxParticipants: 3}).Validate(),
		"agreementThreshold must be greater than 0")
	// Asking for more agreement than participants makes consensus unreachable:
	// every request would end in a dispute the operator cannot resolve.
	require.ErrorContains(t,
		(&ConsensusPolicyConfig{MaxParticipants: 2, AgreementThreshold: 3}).Validate(),
		"maxParticipants must be greater than or equal to agreementThreshold")

	t.Run("punishMisbehavior errors propagate", func(t *testing.T) {
		c := base()
		c.PunishMisbehavior = &PunishMisbehaviorConfig{}
		require.ErrorContains(t, c.Validate(), "disputeThreshold must be greater than 0")
	})

	t.Run("misbehaviorsDestination errors are wrapped", func(t *testing.T) {
		c := base()
		c.MisbehaviorsDestination = &MisbehaviorsDestinationConfig{Type: MisbehaviorsDestinationTypeFile, Path: "relative/path"}
		err := c.Validate()
		require.ErrorContains(t, err, "misbehaviorsDestination is invalid")
		require.ErrorContains(t, err, "must be an absolute path")
	})
}

// requiredParticipants is a composition quota. A quota that cannot be satisfied
// turns every response into a composition dispute, so the arithmetic has to be
// checked at load rather than discovered in production.
func TestConsensusPolicyConfig_Validate_RequiredParticipantQuotas(t *testing.T) {
	withRP := func(rps ...*ConsensusRequiredParticipant) *ConsensusPolicyConfig {
		return &ConsensusPolicyConfig{MaxParticipants: 3, AgreementThreshold: 2, RequiredParticipants: rps}
	}

	require.NoError(t, withRP(&ConsensusRequiredParticipant{
		Tag: "region:us-east", MinParticipants: 2, MinAgreement: 1,
	}).Validate())

	cases := []struct {
		name    string
		rp      *ConsensusRequiredParticipant
		wantSub string
	}{
		{"nil entry", nil, "must not be null"},
		{"blank tag", &ConsensusRequiredParticipant{Tag: "  ", MinParticipants: 1}, "tag is required"},
		{"zero minParticipants", &ConsensusRequiredParticipant{Tag: "t"}, "minParticipants must be greater than 0"},
		{"minParticipants over max", &ConsensusRequiredParticipant{Tag: "t", MinParticipants: 9}, "cannot exceed maxParticipants"},
		{"negative minAgreement", &ConsensusRequiredParticipant{Tag: "t", MinParticipants: 2, MinAgreement: -1}, "minAgreement must not be negative"},
		{"minAgreement over minParticipants", &ConsensusRequiredParticipant{Tag: "t", MinParticipants: 2, MinAgreement: 3}, "cannot exceed minParticipants"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := withRP(tc.rp).Validate()
			require.ErrorContains(t, err, tc.wantSub)
			require.Contains(t, err.Error(), "requiredParticipants[0]",
				"the message must name the offending entry so an operator can find it")
		})
	}
}

func TestPunishMisbehaviorConfig_Validate_AllThreeFieldsArePositive(t *testing.T) {
	good := &PunishMisbehaviorConfig{
		DisputeThreshold: 3,
		DisputeWindow:    Duration(time.Minute),
		SitOutPenalty:    Duration(5 * time.Minute),
	}
	require.NoError(t, good.Validate())

	// A zero sit-out penalty would punish an upstream for no time at all — the
	// policy would appear active and change nothing.
	p := *good
	p.SitOutPenalty = 0
	require.ErrorContains(t, p.Validate(), "sitOutPenalty must be greater than 0")

	p = *good
	p.DisputeWindow = 0
	require.ErrorContains(t, p.Validate(), "disputeWindow must be greater than 0")

	p = *good
	p.DisputeThreshold = 0
	require.ErrorContains(t, p.Validate(), "disputeThreshold must be greater than 0")
}

func TestMisbehaviorsDestinationConfig_Validate_FileAndS3Shapes(t *testing.T) {
	t.Run("file is the default type", func(t *testing.T) {
		require.NoError(t, (&MisbehaviorsDestinationConfig{Path: "/var/log/erpc"}).Validate())
		require.NoError(t, (&MisbehaviorsDestinationConfig{
			Type: MisbehaviorsDestinationTypeFile, Path: "/var/log/erpc",
		}).Validate())

		require.ErrorContains(t, (&MisbehaviorsDestinationConfig{Path: "  "}).Validate(),
			"path is required for file destination")
		// A relative path resolves against whatever the process working
		// directory happens to be, which in a container is rarely writable.
		require.ErrorContains(t, (&MisbehaviorsDestinationConfig{Path: "logs/erpc"}).Validate(),
			"must be an absolute path")
	})

	t.Run("s3 needs a bucket uri and a settings block", func(t *testing.T) {
		require.ErrorContains(t,
			(&MisbehaviorsDestinationConfig{Type: MisbehaviorsDestinationTypeS3}).Validate(),
			"path is required for s3 destination")
		require.ErrorContains(t,
			(&MisbehaviorsDestinationConfig{Type: MisbehaviorsDestinationTypeS3, Path: "/var/log"}).Validate(),
			"must start with s3://")
		require.ErrorContains(t,
			(&MisbehaviorsDestinationConfig{Type: MisbehaviorsDestinationTypeS3, Path: "s3://bucket/prefix"}).Validate(),
			"misbehaviorsDestination.s3 is required")

		require.NoError(t, (&MisbehaviorsDestinationConfig{
			Type: MisbehaviorsDestinationTypeS3, Path: "S3://bucket/prefix", S3: &S3FlushConfig{},
		}).Validate(), "the s3:// prefix is matched case-insensitively")

		// The S3 block's own error must be wrapped, not replaced.
		err := (&MisbehaviorsDestinationConfig{
			Type: MisbehaviorsDestinationTypeS3, Path: "s3://bucket", S3: &S3FlushConfig{MaxRecords: -1},
		}).Validate()
		require.ErrorContains(t, err, "misbehaviorsDestination.s3 is invalid")
		require.ErrorContains(t, err, "maxRecords must be >= 0")
	})

	t.Run("unknown type", func(t *testing.T) {
		require.ErrorContains(t,
			(&MisbehaviorsDestinationConfig{Type: "gcs", Path: "/tmp"}).Validate(),
			"type must be 'file' or 's3'")
	})
}

func TestS3FlushConfig_Validate_BuffersAndCredentials(t *testing.T) {
	require.NoError(t, (&S3FlushConfig{}).Validate())

	require.ErrorContains(t, (&S3FlushConfig{MaxRecords: -1}).Validate(), "maxRecords must be >= 0")
	require.ErrorContains(t, (&S3FlushConfig{MaxSize: -1}).Validate(), "maxSize must be >= 0")
	require.ErrorContains(t, (&S3FlushConfig{FlushInterval: Duration(-time.Second)}).Validate(), "flushInterval must be >= 0")

	t.Run("credential modes", func(t *testing.T) {
		// An empty mode means the default AWS credential chain and must stay
		// legal — most deployments rely on an instance role.
		require.NoError(t, (&S3FlushConfig{Credentials: &AwsAuthConfig{}}).Validate())
		require.NoError(t, (&S3FlushConfig{Credentials: &AwsAuthConfig{Mode: "ENV"}}).Validate())

		require.ErrorContains(t,
			(&S3FlushConfig{Credentials: &AwsAuthConfig{Mode: "file"}}).Validate(),
			"credentialsFile is required when mode is 'file'")
		require.ErrorContains(t,
			(&S3FlushConfig{Credentials: &AwsAuthConfig{Mode: "file", CredentialsFile: "/creds"}}).Validate(),
			"profile is required when mode is 'file'")
		require.NoError(t,
			(&S3FlushConfig{Credentials: &AwsAuthConfig{Mode: "file", CredentialsFile: "/creds", Profile: "p"}}).Validate())

		require.ErrorContains(t,
			(&S3FlushConfig{Credentials: &AwsAuthConfig{Mode: "secret", AccessKeyID: "AK"}}).Validate(),
			"accessKeyID and secretAccessKey are required")
		require.NoError(t,
			(&S3FlushConfig{Credentials: &AwsAuthConfig{Mode: "secret", AccessKeyID: "AK", SecretAccessKey: "SK"}}).Validate())

		require.ErrorContains(t,
			(&S3FlushConfig{Credentials: &AwsAuthConfig{Mode: "imds"}}).Validate(),
			"credentials.mode must be one of: env, file, secret")
	})
}

func TestRateLimitAutoTuneConfig_Validate_FactorsAndThresholds(t *testing.T) {
	on, off := true, false
	base := func() *RateLimitAutoTuneConfig {
		return &RateLimitAutoTuneConfig{
			Enabled:            &on,
			AdjustmentPeriod:   Duration(time.Minute),
			ErrorRateThreshold: 0.1,
			IncreaseFactor:     1.05,
			DecreaseFactor:     0.9,
		}
	}
	require.NoError(t, base().Validate())

	// A disabled auto-tune block must not be validated at all: an operator who
	// turned it off should not have to keep its numbers coherent.
	require.NoError(t, (&RateLimitAutoTuneConfig{Enabled: &off, IncreaseFactor: 0}).Validate())
	require.NoError(t, (&RateLimitAutoTuneConfig{}).Validate())

	cases := []struct {
		name    string
		mutate  func(*RateLimitAutoTuneConfig)
		wantSub string
	}{
		{"adjustmentPeriod", func(r *RateLimitAutoTuneConfig) { r.AdjustmentPeriod = 0 }, "adjustmentPeriod is required"},
		{"errorRateThreshold zero", func(r *RateLimitAutoTuneConfig) { r.ErrorRateThreshold = 0 }, "errorRateThreshold must be greater than 0"},
		{"errorRateThreshold over one", func(r *RateLimitAutoTuneConfig) { r.ErrorRateThreshold = 1.5 }, "errorRateThreshold must be greater than 0"},
		// An increase factor of 1 or less would shrink the budget while
		// pretending to grow it.
		{"increaseFactor", func(r *RateLimitAutoTuneConfig) { r.IncreaseFactor = 1 }, "increaseFactor must be greater than 1"},
		{"decreaseFactor zero", func(r *RateLimitAutoTuneConfig) { r.DecreaseFactor = 0 }, "decreaseFactor must be greater than 0"},
		{"decreaseFactor one", func(r *RateLimitAutoTuneConfig) { r.DecreaseFactor = 1 }, "decreaseFactor must be greater than 0"},
		{"minBudget", func(r *RateLimitAutoTuneConfig) { r.MinBudget = -1 }, "minBudget must be greater than or equal to 0"},
		{"maxBudget", func(r *RateLimitAutoTuneConfig) { r.MaxBudget = -1 }, "maxBudget must be greater than or equal to 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.mutate(r)
			require.ErrorContains(t, r.Validate(), tc.wantSub)
		})
	}
}

func TestJsonRpcUpstreamConfig_Validate_BatchingAndProxyPool(t *testing.T) {
	on, off := true, false
	cfg := &Config{ProxyPools: []*ProxyPoolConfig{{ID: "pool-a", Urls: []string{"http://p:1"}}}}

	require.NoError(t, (&JsonRpcUpstreamConfig{}).Validate(cfg))

	// Batching without a wait or a size would either never flush or flush one
	// request at a time — both defeat the feature silently.
	require.ErrorContains(t,
		(&JsonRpcUpstreamConfig{SupportsBatch: &on}).Validate(cfg),
		"batchMaxWait is required")
	require.ErrorContains(t,
		(&JsonRpcUpstreamConfig{SupportsBatch: &on, BatchMaxWait: Duration(10 * time.Millisecond)}).Validate(cfg),
		"batchMaxSize must be greater than 0")
	require.NoError(t,
		(&JsonRpcUpstreamConfig{SupportsBatch: &on, BatchMaxWait: Duration(10 * time.Millisecond), BatchMaxSize: 10}).Validate(cfg))

	// With batching off the same empty values are fine.
	require.NoError(t, (&JsonRpcUpstreamConfig{SupportsBatch: &off}).Validate(cfg))

	t.Run("proxyPool must exist", func(t *testing.T) {
		require.NoError(t, (&JsonRpcUpstreamConfig{ProxyPool: "pool-a"}).Validate(cfg))

		err := (&JsonRpcUpstreamConfig{ProxyPool: "pool-z"}).Validate(cfg)
		require.ErrorContains(t, err, "does not exist in configured proxyPools")
		require.Contains(t, err.Error(), "pool-a", "the message must list the pools that do exist")
	})
}

func TestSelectionPolicyConfig_Validate_IntervalsAndEvalFunc(t *testing.T) {
	base := func() *SelectionPolicyConfig {
		return &SelectionPolicyConfig{
			EvalInterval:    Duration(10 * time.Second),
			EvalTimeout:     Duration(5 * time.Second),
			EvalFunc:        "(u) => u",
			CompiledProgram: compiledEvalFunc(t),
		}
	}

	require.NoError(t, base().Validate())

	require.ErrorContains(t,
		(&SelectionPolicyConfig{EvalTimeout: Duration(time.Second)}).Validate(),
		"evalInterval must be greater than 0")
	require.ErrorContains(t,
		(&SelectionPolicyConfig{EvalInterval: Duration(time.Second)}).Validate(),
		"evalTimeout must be greater than 0")

	// A timeout at or above the interval means a slow eval never finishes
	// before the next tick, so the policy silently stops updating.
	c := base()
	c.EvalTimeout = c.EvalInterval
	require.ErrorContains(t, c.Validate(), "must be less than evalInterval")

	c = base()
	c.EvalFunc = ""
	require.ErrorContains(t, c.Validate(), "evalFunc is required")

	// A missing compiled program means the source never compiled. Booting on it
	// would leave every tick throwing inside the JS runtime.
	c = base()
	c.CompiledProgram = nil
	require.ErrorContains(t, c.Validate(), "failed to compile")

	// A TypeScript config carries a sentinel instead of a compiled program:
	// the function lives on the user-script runtime and is resolved at eval
	// time, so the nil program is expected there.
	c = base()
	c.CompiledProgram = nil
	c.EvalFunc = "__ts_fn__:7"
	require.NoError(t, c.Validate())
}
