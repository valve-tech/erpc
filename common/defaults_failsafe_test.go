package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFailsafeConfig_SetDefaults_MatchesEveryMethodWhenUnset. An empty
// matchMethod that stayed empty would match NOTHING, so every request would run
// with no retry, no timeout and no breaker at all.
func TestFailsafeConfig_SetDefaults_MatchesEveryMethodWhenUnset(t *testing.T) {
	t.Parallel()

	f := &FailsafeConfig{}
	require.NoError(t, f.SetDefaults(nil))
	require.Equal(t, "*", f.MatchMethod)
}

// TestFailsafeConfig_SetDefaults_InheritsTheMatchMethodFromDefaults lets an
// operator scope a whole defaults block to one method family.
func TestFailsafeConfig_SetDefaults_InheritsTheMatchMethodFromDefaults(t *testing.T) {
	t.Parallel()

	f := &FailsafeConfig{}
	require.NoError(t, f.SetDefaults(&FailsafeConfig{MatchMethod: "eth_get*"}))
	require.Equal(t, "eth_get*", f.MatchMethod)

	// An explicit value must win over the default.
	own := &FailsafeConfig{MatchMethod: "eth_call"}
	require.NoError(t, own.SetDefaults(&FailsafeConfig{MatchMethod: "eth_get*"}))
	require.Equal(t, "eth_call", own.MatchMethod)
}

// TestFailsafeConfig_SetDefaults_MaterializesEveryPolicyPresentInTheDefaults is
// the inheritance an operator relies on: a per-method override that mentions
// only `retry` must still get the timeout, hedge and breaker from the defaults
// block. Without it, naming one policy silently disables the other three.
func TestFailsafeConfig_SetDefaults_MaterializesEveryPolicyPresentInTheDefaults(t *testing.T) {
	t.Parallel()

	defaults := &FailsafeConfig{
		Timeout:        &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Base: Duration(7 * time.Second)}},
		Retry:          &RetryPolicyConfig{MaxAttempts: 9},
		Hedge:          &HedgePolicyConfig{MaxCount: 4, Delay: &AdaptiveDuration{Base: Duration(250 * time.Millisecond)}},
		CircuitBreaker: &CircuitBreakerPolicyConfig{FailureThresholdCount: 42},
	}

	f := &FailsafeConfig{Retry: &RetryPolicyConfig{MaxAttempts: 2}}
	require.NoError(t, f.SetDefaults(defaults))

	require.Equal(t, 2, f.Retry.MaxAttempts, "the explicit override must win")

	require.NotNil(t, f.Timeout, "an unmentioned timeout must be materialized")
	require.Equal(t, Duration(7*time.Second), f.Timeout.Duration.Base)

	require.NotNil(t, f.Hedge, "an unmentioned hedge must be materialized")
	require.Equal(t, 4, f.Hedge.MaxCount)
	require.Equal(t, Duration(250*time.Millisecond), f.Hedge.Delay.Base)

	require.NotNil(t, f.CircuitBreaker, "an unmentioned breaker must be materialized")
	require.Equal(t, uint(42), f.CircuitBreaker.FailureThresholdCount)
}

// TestFailsafeConfig_SetDefaults_DoesNotInventPoliciesTheDefaultsDoNotHave. A
// hedge nobody configured would double the request volume against every
// upstream and double the bill.
func TestFailsafeConfig_SetDefaults_DoesNotInventPoliciesTheDefaultsDoNotHave(t *testing.T) {
	t.Parallel()

	f := &FailsafeConfig{}
	require.NoError(t, f.SetDefaults(&FailsafeConfig{Retry: &RetryPolicyConfig{MaxAttempts: 3}}))

	require.NotNil(t, f.Retry)
	require.Nil(t, f.Hedge, "a hedge must never appear out of nowhere")
	require.Nil(t, f.Timeout)
	require.Nil(t, f.CircuitBreaker)
	require.Nil(t, f.Consensus)
}

// TestFailsafeConfig_SetDefaults_FillsAPartialPolicyFromTheDefaults covers the
// merge of a policy the override DOES mention: the fields it omits come from
// the defaults, not from the hard-coded fallbacks.
func TestFailsafeConfig_SetDefaults_FillsAPartialPolicyFromTheDefaults(t *testing.T) {
	t.Parallel()

	defaults := &FailsafeConfig{
		Retry: &RetryPolicyConfig{
			MaxAttempts:            9,
			BackoffFactor:          2.5,
			BackoffMaxDelay:        Duration(30 * time.Second),
			Delay:                  Duration(400 * time.Millisecond),
			Jitter:                 Duration(50 * time.Millisecond),
			EmptyResultMaxAttempts: 7,
			EmptyResultDelay:       Duration(2 * time.Second),
			EmptyResultAccept:      []string{"eth_getLogs"},
		},
		CircuitBreaker: &CircuitBreakerPolicyConfig{
			FailureThresholdCount:    11,
			FailureThresholdCapacity: 22,
			HalfOpenAfter:            Duration(90 * time.Second),
			SuccessThresholdCount:    3,
			SuccessThresholdCapacity: 4,
		},
	}

	f := &FailsafeConfig{
		Retry:          &RetryPolicyConfig{MaxAttempts: 2},
		CircuitBreaker: &CircuitBreakerPolicyConfig{FailureThresholdCount: 5},
	}
	require.NoError(t, f.SetDefaults(defaults))

	require.Equal(t, 2, f.Retry.MaxAttempts)
	require.Equal(t, float32(2.5), f.Retry.BackoffFactor)
	require.Equal(t, Duration(30*time.Second), f.Retry.BackoffMaxDelay)
	require.Equal(t, Duration(400*time.Millisecond), f.Retry.Delay)
	require.Equal(t, Duration(50*time.Millisecond), f.Retry.Jitter)
	require.Equal(t, 7, f.Retry.EmptyResultMaxAttempts)
	require.Equal(t, Duration(2*time.Second), f.Retry.EmptyResultDelay)
	require.Equal(t, []string{"eth_getLogs"}, f.Retry.EmptyResultAccept)

	require.Equal(t, uint(5), f.CircuitBreaker.FailureThresholdCount)
	require.Equal(t, uint(22), f.CircuitBreaker.FailureThresholdCapacity)
	require.Equal(t, Duration(90*time.Second), f.CircuitBreaker.HalfOpenAfter)
	require.Equal(t, uint(3), f.CircuitBreaker.SuccessThresholdCount)
	require.Equal(t, uint(4), f.CircuitBreaker.SuccessThresholdCapacity)
}

// TestRetryPolicyConfig_SetDefaults_ShippedFallbacks pins the numbers an
// operator gets with an empty `retry: {}`. Each one is a production knob:
// attempts decide failover breadth, backoff decides how hard a struggling
// upstream is hit.
func TestRetryPolicyConfig_SetDefaults_ShippedFallbacks(t *testing.T) {
	t.Parallel()

	r := &RetryPolicyConfig{}
	require.NoError(t, r.SetDefaults(nil))

	require.Equal(t, 3, r.MaxAttempts)
	require.Equal(t, float32(1.2), r.BackoffFactor)
	require.Equal(t, Duration(3*time.Second), r.BackoffMaxDelay)
	require.Equal(t, Duration(0), r.Delay)
	require.Equal(t, Duration(0), r.Jitter)
	require.Equal(t, DefaultEmptyResultMaxAttempts, r.EmptyResultMaxAttempts)
	require.Equal(t, DefaultEmptyResultAccept(), r.EmptyResultAccept)
	require.Equal(t, Duration(0), r.EmptyResultDelay,
		"there is deliberately no fixed empty-result delay fallback")
}

// TestRetryPolicyConfig_SetDefaults_MigratesTheDeprecatedEmptyResultIgnore.
// An operator upgrading erpc must not silently lose the method list that stops
// eth_getLogs from being retried on every legitimately empty answer.
func TestRetryPolicyConfig_SetDefaults_MigratesTheDeprecatedEmptyResultIgnore(t *testing.T) {
	t.Parallel()

	t.Run("from the policy itself", func(t *testing.T) {
		r := &RetryPolicyConfig{EmptyResultIgnore: []string{"eth_call"}}
		require.NoError(t, r.SetDefaults(nil))
		require.Equal(t, []string{"eth_call"}, r.EmptyResultAccept)
	})

	t.Run("from the defaults block", func(t *testing.T) {
		r := &RetryPolicyConfig{}
		require.NoError(t, r.SetDefaults(&RetryPolicyConfig{EmptyResultIgnore: []string{"eth_getLogs"}}))
		require.Equal(t, []string{"eth_getLogs"}, r.EmptyResultAccept)
	})

	t.Run("the new field wins over the deprecated one", func(t *testing.T) {
		r := &RetryPolicyConfig{
			EmptyResultAccept: []string{"new"},
			EmptyResultIgnore: []string{"old"},
		}
		require.NoError(t, r.SetDefaults(nil))
		require.Equal(t, []string{"new"}, r.EmptyResultAccept)
	})

	t.Run("the new field in the defaults wins over the deprecated one", func(t *testing.T) {
		r := &RetryPolicyConfig{}
		require.NoError(t, r.SetDefaults(&RetryPolicyConfig{
			EmptyResultAccept: []string{"new"},
			EmptyResultIgnore: []string{"old"},
		}))
		require.Equal(t, []string{"new"}, r.EmptyResultAccept)
	})
}

// TestRetryPolicyConfig_SetDefaults_MigratesBlockUnavailableDelay. The old key
// must keep working, and must be cleared afterwards so nothing downstream reads
// two different values for the same delay.
func TestRetryPolicyConfig_SetDefaults_MigratesBlockUnavailableDelay(t *testing.T) {
	t.Parallel()

	t.Run("moves into an unset emptyResultDelay", func(t *testing.T) {
		r := &RetryPolicyConfig{BlockUnavailableDelay: Duration(1500 * time.Millisecond)}
		require.NoError(t, r.SetDefaults(nil))
		require.Equal(t, Duration(1500*time.Millisecond), r.EmptyResultDelay)
		require.Equal(t, Duration(0), r.BlockUnavailableDelay,
			"the legacy field must be cleared so it cannot be read by mistake")
	})

	t.Run("does not overwrite an explicit emptyResultDelay", func(t *testing.T) {
		r := &RetryPolicyConfig{
			BlockUnavailableDelay: Duration(1500 * time.Millisecond),
			EmptyResultDelay:      Duration(600 * time.Millisecond),
		}
		require.NoError(t, r.SetDefaults(nil))
		require.Equal(t, Duration(600*time.Millisecond), r.EmptyResultDelay)
		require.Equal(t, Duration(0), r.BlockUnavailableDelay)
	})
}

// TestHedgePolicyConfig_SetDefaults_FloorsTheDelay. A hedge with no floor fires
// immediately and doubles the load on every upstream, which is exactly the
// failure mode hedging is supposed to relieve.
func TestHedgePolicyConfig_SetDefaults_FloorsTheDelay(t *testing.T) {
	t.Parallel()

	h := &HedgePolicyConfig{}
	require.NoError(t, h.SetDefaults(nil))

	require.Equal(t, Duration(defaultHedgeMinDelay), h.Delay.Min)
	require.Equal(t, Duration(defaultHedgeMaxDelay), h.Delay.Max)
	require.Equal(t, 1, h.MaxCount)
}

// TestHedgePolicyConfig_SetDefaults_InheritsTheAdaptiveDelayFieldByField covers
// the per-field merge inside the adaptive duration: an override that sets only
// the quantile must keep the inherited floor and ceiling.
func TestHedgePolicyConfig_SetDefaults_InheritsTheAdaptiveDelayFieldByField(t *testing.T) {
	t.Parallel()

	defaults := &HedgePolicyConfig{
		MaxCount: 3,
		Delay: &AdaptiveDuration{
			Base:     Duration(200 * time.Millisecond),
			Quantile: 0.90,
			Min:      Duration(50 * time.Millisecond),
			Max:      Duration(5 * time.Second),
		},
	}

	h := &HedgePolicyConfig{Delay: &AdaptiveDuration{Quantile: 0.99}}
	require.NoError(t, h.SetDefaults(defaults))

	require.Equal(t, 0.99, h.Delay.Quantile, "the explicit quantile must win")
	require.Equal(t, Duration(200*time.Millisecond), h.Delay.Base)
	require.Equal(t, Duration(50*time.Millisecond), h.Delay.Min)
	require.Equal(t, Duration(5*time.Second), h.Delay.Max)
	require.Equal(t, 3, h.MaxCount)
}

// TestCircuitBreakerPolicyConfig_SetDefaults_ShippedFallbacks. The
// success-threshold pair is the one an earlier duplicated default block got
// wrong: a 8-of-200 close rule let a half-open breaker absorb 192 failures.
func TestCircuitBreakerPolicyConfig_SetDefaults_ShippedFallbacks(t *testing.T) {
	t.Parallel()

	c := &CircuitBreakerPolicyConfig{}
	require.NoError(t, c.SetDefaults(nil))

	require.Equal(t, uint(20), c.FailureThresholdCount)
	require.Equal(t, uint(80), c.FailureThresholdCapacity)
	require.Equal(t, Duration(5*time.Minute), c.HalfOpenAfter)
	require.Equal(t, uint(8), c.SuccessThresholdCount)
	require.Equal(t, uint(10), c.SuccessThresholdCapacity)
	require.LessOrEqual(t, c.SuccessThresholdCount, c.SuccessThresholdCapacity,
		"a count above the capacity can never close the breaker")
}

// TestTimeoutPolicyConfig_SetDefaults_OnlyInheritsFromANonZeroDefault. A
// zero-valued defaults block must not overwrite an explicit timeout with
// nothing, which would mean no timeout at all.
func TestTimeoutPolicyConfig_SetDefaults_OnlyInheritsFromANonZeroDefault(t *testing.T) {
	t.Parallel()

	t.Run("a nil default leaves the policy alone", func(t *testing.T) {
		tp := &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Base: Duration(2 * time.Second)}}
		require.NoError(t, tp.SetDefaults(nil))
		require.Equal(t, Duration(2*time.Second), tp.Duration.Base)
	})

	t.Run("an empty default leaves the policy alone", func(t *testing.T) {
		tp := &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Base: Duration(2 * time.Second)}}
		require.NoError(t, tp.SetDefaults(&TimeoutPolicyConfig{Duration: &AdaptiveDuration{}}))
		require.Equal(t, Duration(2*time.Second), tp.Duration.Base)
	})

	t.Run("an unset duration is copied wholesale", func(t *testing.T) {
		src := &AdaptiveDuration{Base: Duration(9 * time.Second), Quantile: 0.95}
		tp := &TimeoutPolicyConfig{}
		require.NoError(t, tp.SetDefaults(&TimeoutPolicyConfig{Duration: src}))
		require.Equal(t, Duration(9*time.Second), tp.Duration.Base)
		require.Equal(t, 0.95, tp.Duration.Quantile)

		// It must be a copy: mutating the defaults later must not reach here.
		src.Base = Duration(time.Hour)
		require.Equal(t, Duration(9*time.Second), tp.Duration.Base,
			"a shared pointer would let one policy rewrite another")
	})

	t.Run("a partially set duration merges field by field", func(t *testing.T) {
		tp := &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Quantile: 0.99}}
		require.NoError(t, tp.SetDefaults(&TimeoutPolicyConfig{
			Duration: &AdaptiveDuration{Base: Duration(4 * time.Second), Quantile: 0.5, Max: Duration(time.Minute)},
		}))
		require.Equal(t, 0.99, tp.Duration.Quantile)
		require.Equal(t, Duration(4*time.Second), tp.Duration.Base)
		require.Equal(t, Duration(time.Minute), tp.Duration.Max)
	})
}

// TestConsensusPolicyConfig_SetDefaults_ShippedFallbacks. The agreement
// threshold and the dispute behaviour together decide whether erpc serves a
// disputed answer or refuses; both must have a safe default.
func TestConsensusPolicyConfig_SetDefaults_ShippedFallbacks(t *testing.T) {
	t.Parallel()

	c := &ConsensusPolicyConfig{}
	require.NoError(t, c.SetDefaults())

	require.Equal(t, 5, c.MaxParticipants)
	require.Equal(t, 2, c.AgreementThreshold)
	require.Equal(t, ConsensusDisputeBehaviorReturnError, c.DisputeBehavior)
	require.Equal(t, ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult, c.LowParticipantsBehavior)
	require.Equal(t, "warn", c.DisputeLogLevel)

	require.Contains(t, c.IgnoreFields, "eth_getLogs")
	require.Contains(t, c.IgnoreFields["eth_getTransactionReceipt"], "logs.*.blockTimestamp",
		"receipt log timestamps differ between vendors and must not count as a dispute")
}

// TestConsensusPolicyConfig_SetDefaults_KeepsAnExplicitIgnoreList. An operator
// who narrows the list must not have the shipped list merged back on top.
func TestConsensusPolicyConfig_SetDefaults_KeepsAnExplicitIgnoreList(t *testing.T) {
	t.Parallel()

	c := &ConsensusPolicyConfig{IgnoreFields: map[string][]string{"eth_call": {"x"}}}
	require.NoError(t, c.SetDefaults())

	require.Equal(t, map[string][]string{"eth_call": {"x"}}, c.IgnoreFields)
	require.NotContains(t, c.IgnoreFields, "eth_getLogs")
}

// TestFailsafeConfig_SetDefaults_AppliesConsensusDefaults — the consensus block
// is the one policy that has no inheritance, so it must at least be defaulted.
func TestFailsafeConfig_SetDefaults_AppliesConsensusDefaults(t *testing.T) {
	t.Parallel()

	f := &FailsafeConfig{Consensus: &ConsensusPolicyConfig{}}
	require.NoError(t, f.SetDefaults(nil))

	require.Equal(t, 5, f.Consensus.MaxParticipants)
	require.Equal(t, 2, f.Consensus.AgreementThreshold)
}

// TestEvmIntegrityConfig_SetDefaults_TurnsEveryCheckOn is a safety default: an
// unset integrity check must mean "on", so an operator who never wrote the
// block runs with the guards, not without them.
func TestEvmIntegrityConfig_SetDefaults_TurnsEveryCheckOn(t *testing.T) {
	t.Parallel()

	i := &EvmIntegrityConfig{}
	require.NoError(t, i.SetDefaults())

	require.True(t, *i.EnforceHighestBlock)
	require.True(t, *i.EnforceGetLogsBlockRange)
	require.True(t, *i.EnforceNonNullTaggedBlocks)
}

// TestEvmIntegrityConfig_SetDefaults_RespectsAnExplicitFalse. An operator who
// deliberately turns a check off must keep it off; re-enabling it would make
// erpc reject responses the operator chose to accept.
func TestEvmIntegrityConfig_SetDefaults_RespectsAnExplicitFalse(t *testing.T) {
	t.Parallel()

	no := false
	i := &EvmIntegrityConfig{
		EnforceHighestBlock:        &no,
		EnforceGetLogsBlockRange:   &no,
		EnforceNonNullTaggedBlocks: &no,
	}
	require.NoError(t, i.SetDefaults())

	require.False(t, *i.EnforceHighestBlock)
	require.False(t, *i.EnforceGetLogsBlockRange)
	require.False(t, *i.EnforceNonNullTaggedBlocks)
}
