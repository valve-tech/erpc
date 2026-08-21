package upstream

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// These tests pin the per-upstream failsafe decisions: which errors earn a
// retry, which attempts are allowed to move the circuit breaker, and whether
// the original cause survives the retry wrapper. Each rule is one operator
// outcome — a retry on a write method double-sends a transaction, and a hedge
// counted as a breaker failure cordons a healthy upstream.

func newTestExecutor(t *testing.T, cfg *common.UpstreamFailsafeConfig) *upstreamExecutor {
	t.Helper()
	lg := zerolog.Nop()
	e, err := NewUpstreamExecutor(cfg, &lg)
	require.NoError(t, err)
	return e
}

// requestFor builds a NormalizedRequest carrying a real JSON-RPC body, so
// req.Method() resolves the way it does in production rather than through a
// convenient stub.
func requestFor(t *testing.T, method string) *common.NormalizedRequest {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`))
	m, err := req.Method()
	require.NoError(t, err)
	require.Equal(t, method, m)
	return req
}

func TestNewUpstreamExecutor_NilConfigIsANoOpExecutor(t *testing.T) {
	// Upstreams without a failsafe block must still run requests. A nil
	// config becomes a pass-through with no policies attached.
	e := newTestExecutor(t, nil)
	require.Equal(t, "*", e.MatchMethod())
	require.Nil(t, e.MatchFinality())
	require.Nil(t, e.Timeout())
	require.Nil(t, e.Breaker())
}

func TestNewUpstreamExecutor_RejectsConsensusAtTheUpstreamScope(t *testing.T) {
	// Consensus compares answers ACROSS upstreams. Accepting it on a single
	// upstream would silently do nothing while the operator believes their
	// requests are being cross-checked. The error must say so.
	lg := zerolog.Nop()
	_, err := NewUpstreamExecutor(&common.UpstreamFailsafeConfig{
		Consensus: &common.ConsensusPolicyConfig{},
	}, &lg)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, "ErrFailsafeConfiguration"),
		"got %v, want a failsafe-configuration error", err)
}

func TestNewUpstreamExecutor_EmptyMatchMethodBecomesWildcard(t *testing.T) {
	// An unset matchMethod means "all methods". Leaving it empty would make
	// the executor match nothing and silently disable the policy.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{})
	require.Equal(t, "*", e.MatchMethod())
}

func TestUpstreamExecutor_EmptyResultAcceptFallsBackToTheDefaultList(t *testing.T) {
	// The fork's rotation rule reads this list. If an unset config produced an
	// EMPTY list rather than the default, an eth_call returning a zero word
	// would again be treated as missing data and re-sent to every upstream.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{Retry: &common.RetryPolicyConfig{MaxAttempts: 2}})
	require.True(t, e.shouldSkipForEmptyResultAccept("eth_call"),
		"eth_call is in the built-in accept list and must survive the fallback")
	require.False(t, e.shouldSkipForEmptyResultAccept("eth_getBlockByNumber"))
}

func TestUpstreamExecutor_ExplicitEmptyResultAcceptOverridesTheDefault(t *testing.T) {
	// An operator who names their own list must get exactly that list, not
	// the union with the built-in one.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{
		Retry: &common.RetryPolicyConfig{EmptyResultAccept: []string{"eth_getBlockByNumber"}},
	})
	require.True(t, e.shouldSkipForEmptyResultAccept("eth_getBlockByNumber"))
	require.False(t, e.shouldSkipForEmptyResultAccept("eth_call"),
		"an explicit list must replace the default, not extend it")
}

func TestShouldRetry_SuccessIsNeverRetried(t *testing.T) {
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{Retry: &common.RetryPolicyConfig{MaxAttempts: 3}})
	require.False(t, e.shouldRetry(requestFor(t, "eth_blockNumber"), nil, nil, 0))
}

func TestShouldRetry_ReadMethodsRetryOnATransportFailure(t *testing.T) {
	// A dropped connection says nothing about whether the request was valid,
	// so a read must be re-sent. Which WRITE methods are exempt is decided by
	// the per-family method tables under architecture/, and this test
	// deliberately does not pin their contents.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{Retry: &common.RetryPolicyConfig{MaxAttempts: 3}})
	transport := common.NewErrEndpointTransportFailure(&url.URL{Host: "node"}, errors.New("connection reset"))
	require.True(t, e.shouldRetry(requestFor(t, "eth_getBalance"), nil, transport, 0))
}

func TestShouldRetry_CompositeRequestsAreNotRetriedAtTheUpstreamScope(t *testing.T) {
	// A composite request is already assembled from several sub-calls. Retrying
	// the whole thing here would re-issue every sub-call and multiply the load.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{Retry: &common.RetryPolicyConfig{MaxAttempts: 3}})
	req := requestFor(t, "eth_getLogs")
	transport := common.NewErrEndpointTransportFailure(&url.URL{Host: "node"}, errors.New("connection reset"))
	require.True(t, e.shouldRetry(req, nil, transport, 0), "sanity: the same error retries when not composite")

	req.SetCompositeType(common.CompositeTypeLogsSplitProactive)
	require.False(t, e.shouldRetry(req, nil, transport, 0))
}

func TestShouldRetry_ExecutionExceptionsNeedAnExplicitRetryableFlag(t *testing.T) {
	// An execution revert is a deterministic answer from the chain: retrying
	// burns upstream budget and returns the identical revert. Only an error
	// explicitly tagged retryableTowardNetwork may be re-sent.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{Retry: &common.RetryPolicyConfig{MaxAttempts: 3}})
	req := requestFor(t, "eth_call")

	plainRevert := common.NewErrEndpointExecutionException(errors.New("execution reverted"))
	require.False(t, e.shouldRetry(req, nil, plainRevert, 0))

	// The tag lives on the error details and is found by a deep search, so it
	// still applies when the error has been wrapped on the way up.
	tagged := &common.BaseError{
		Code:    common.ErrCodeEndpointExecutionException,
		Message: "execution exception",
		Details: map[string]interface{}{"retryableTowardNetwork": true},
	}
	require.True(t, e.shouldRetry(req, nil, tagged, 0),
		"an explicitly retryable execution exception must be retried")
}

func TestShouldRetry_MissingDataHonoursTheRetryEmptyDirective(t *testing.T) {
	// retryEmpty=false is the client saying "an empty answer is my answer".
	// Ignoring it makes every empty eth_getLogs cost N upstream calls.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{Retry: &common.RetryPolicyConfig{MaxAttempts: 3}})
	missing := common.NewErrEndpointMissingData(errors.New("no data"), nil)

	req := requestFor(t, "eth_getLogs")
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: false})
	require.False(t, e.shouldRetry(req, nil, missing, 0),
		"retryEmpty=false must stop the retry")

	req2 := requestFor(t, "eth_getLogs")
	req2.SetDirectives(&common.RequestDirectives{RetryEmpty: true})
	require.True(t, e.shouldRetry(req2, nil, missing, 0),
		"retryEmpty=true must let the missing-data retry through")
}

func TestShouldRetry_CircuitBreakerOpenIsNotRetried(t *testing.T) {
	// The breaker already decided this upstream is out. Retrying against it
	// just spends the request's deadline before the network layer can move on
	// to a healthy upstream.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{Retry: &common.RetryPolicyConfig{MaxAttempts: 3}})
	now := time.Now()
	cbOpen := common.NewErrFailsafeCircuitBreakerOpen(common.ScopeUpstream, failsafe.ErrCircuitOpen, &now)
	require.False(t, e.shouldRetry(requestFor(t, "eth_getBalance"), nil, cbOpen, 0))
}

func TestComputeDelay_UsesTheConfiguredRetryBackoff(t *testing.T) {
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts:   4,
			Delay:         common.Duration(50 * time.Millisecond),
			BackoffFactor: 2,
		},
	})
	require.Equal(t, 50*time.Millisecond, e.computeDelay(nil, nil, nil, 0))
	require.Equal(t, 200*time.Millisecond, e.computeDelay(nil, nil, nil, 2))
}

func TestComputeDelay_NoRetryPolicyMeansNoSleep(t *testing.T) {
	// Without a retry policy the retry loop runs once; a non-zero delay here
	// would add latency to a path that never retries.
	require.Equal(t, time.Duration(0), newTestExecutor(t, nil).computeDelay(nil, nil, nil, 3))
	require.Equal(t, time.Duration(0),
		newTestExecutor(t, &common.UpstreamFailsafeConfig{}).computeDelay(nil, nil, nil, 3))
}

func TestUpstreamBreakerEligible_HedgesAndProbesDoNotMoveTheBreaker(t *testing.T) {
	// A hedge is a deliberate duplicate; the state poller's probes are not
	// client traffic. Letting either count would cordon an upstream on the
	// strength of requests no user ever made.
	require.False(t, upstreamBreakerEligible(nil, true), "a hedge attempt must never count")

	internal := requestFor(t, "eth_blockNumber")
	internal.SetDirectives(&common.RequestDirectives{IsInternal: true})
	require.False(t, upstreamBreakerEligible(internal, false), "an internal probe must never count")

	composite := requestFor(t, "eth_getLogs")
	composite.SetCompositeType(common.CompositeTypeLogsSplitProactive)
	require.False(t, upstreamBreakerEligible(composite, false), "a composite sub-call must never count")

	require.True(t, upstreamBreakerEligible(requestFor(t, "eth_getBalance"), false),
		"ordinary client traffic must count")
	require.True(t, upstreamBreakerEligible(nil, false),
		"a call with no request attached is treated as real traffic")
}

func TestUpstreamBreakerOutcome_ServerAndTransportErrorsOpenTheBreaker(t *testing.T) {
	// These are the errors that mean "this endpoint is broken", as opposed to
	// "this request was bad". Only they may take the upstream out of rotation.
	cases := map[string]error{
		"server-side 5xx": common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500),
		"transport":       common.NewErrEndpointTransportFailure(&url.URL{Host: "node"}, errors.New("dial tcp")),
		"unauthorized":    common.NewErrEndpointUnauthorized(errors.New("bad api key")),
		"billing":         common.NewErrEndpointBillingIssue(errors.New("out of credits")),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, failsafe.OutcomeFailure, upstreamBreakerOutcome(nil, err))
		})
	}
}

func TestUpstreamBreakerOutcome_SoftErrorsAreIgnoredNotCountedAsFailures(t *testing.T) {
	// A cancelled request says nothing about the upstream's health — the
	// client hung up. Counting cancellations would cordon every upstream
	// during a client-side timeout storm, exactly when capacity matters most.
	require.Equal(t, failsafe.OutcomeIgnore,
		upstreamBreakerOutcome(nil, common.NewErrEndpointRequestCanceled(context.Canceled)))
	require.Equal(t, failsafe.OutcomeIgnore,
		upstreamBreakerOutcome(nil, common.NewErrUpstreamRequestSkipped(errors.New("no method"), "ups1")))
	require.Equal(t, failsafe.OutcomeIgnore,
		upstreamBreakerOutcome(nil, common.NewErrEndpointExecutionException(errors.New("execution reverted"))),
		"a chain-level revert is a valid answer, not an upstream fault")
}

func TestUpstreamBreakerOutcome_CleanResponseIsASuccess(t *testing.T) {
	require.Equal(t, failsafe.OutcomeSuccess, upstreamBreakerOutcome(nil, nil))
}

func TestUpstreamExecutor_RetryExceededKeepsTheOriginalCauseReachable(t *testing.T) {
	// The recurring failure mode here is a layer that swallows the real error
	// and returns its own tidy one. An operator reading only
	// "retry exceeded" cannot tell a dead upstream from a bad API key.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{
		Retry: &common.RetryPolicyConfig{MaxAttempts: 3, Delay: common.Duration(0)},
	})
	cause := errors.New("upstream returned 502 from the load balancer")
	serverSide := common.NewErrEndpointServerSideException(cause, nil, 502)

	attempts := 0
	_, err := e.Run(context.Background(), requestFor(t, "eth_getBalance"),
		func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error) {
			attempts++
			return nil, serverSide
		})

	require.Equal(t, 3, attempts, "maxAttempts must be honoured exactly")
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeRetryExceeded),
		"the retry wrapper must classify the failure, got %v", err)
	require.True(t, errors.Is(err, cause),
		"the ORIGINAL cause must survive the retry wrapper, got %v", err)
}

func TestUpstreamExecutor_InternalRequestsBypassRetryEntirely(t *testing.T) {
	// The state poller runs on its own cadence. Retrying its probes multiplies
	// background load and, worse, makes the poller's own error rate look like
	// client traffic in the metrics.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{
		Retry: &common.RetryPolicyConfig{MaxAttempts: 5, Delay: common.Duration(0)},
	})
	req := requestFor(t, "eth_blockNumber")
	req.SetDirectives(&common.RequestDirectives{IsInternal: true})

	attempts := 0
	boom := common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500)
	_, err := e.Run(context.Background(), req,
		func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error) {
			attempts++
			return nil, boom
		})
	require.Equal(t, 1, attempts, "an internal request must be attempted exactly once")
	require.True(t, errors.Is(err, boom), "and its error must be returned unwrapped, got %v", err)
}

func TestUpstreamExecutor_OpenBreakerRefusesTheCallBeforeReachingTheUpstream(t *testing.T) {
	// The point of the breaker is that a cordoned upstream receives NO
	// traffic. If inner still ran, the "circuit open" error would be cosmetic.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{
		CircuitBreaker: &common.CircuitBreakerPolicyConfig{
			FailureThresholdCount:    1,
			FailureThresholdCapacity: 1,
			SuccessThresholdCount:    1,
			SuccessThresholdCapacity: 1,
			HalfOpenAfter:            common.Duration(time.Hour),
		},
	})
	require.NotNil(t, e.Breaker())

	calls := 0
	inner := func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error) {
		calls++
		return nil, common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500)
	}
	req := requestFor(t, "eth_getBalance")

	_, err := e.Run(context.Background(), req, inner)
	require.Error(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, failsafe.StateOpen, e.Breaker().State(), "one 5xx must open a 1-of-1 breaker")

	_, err = e.Run(context.Background(), req, inner)
	require.Equal(t, 1, calls, "an open breaker must not reach the upstream at all")
	require.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen),
		"got %v, want a circuit-breaker-open error", err)
	require.True(t, errors.Is(err, failsafe.ErrCircuitOpen),
		"the breaker sentinel must stay reachable through the wrapper, got %v", err)
}

func TestUpstreamExecutor_InternalProbesStillReachAnOpenBreakersUpstream(t *testing.T) {
	// Recovery depends on this. The state poller must keep probing a cordoned
	// upstream; if the breaker blocked its probes, nothing would ever observe
	// the upstream coming back.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{
		CircuitBreaker: &common.CircuitBreakerPolicyConfig{
			FailureThresholdCount:    1,
			FailureThresholdCapacity: 1,
			SuccessThresholdCount:    1,
			SuccessThresholdCapacity: 1,
			HalfOpenAfter:            common.Duration(time.Hour),
		},
	})
	boom := common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500)
	calls := 0
	inner := func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error) {
		calls++
		return nil, boom
	}
	_, _ = e.Run(context.Background(), requestFor(t, "eth_getBalance"), inner)
	require.Equal(t, failsafe.StateOpen, e.Breaker().State())

	probe := requestFor(t, "eth_blockNumber")
	probe.SetDirectives(&common.RequestDirectives{IsInternal: true})
	_, err := e.Run(context.Background(), probe, inner)
	require.Equal(t, 2, calls, "the internal probe must still reach the upstream")
	require.True(t, errors.Is(err, boom))
}

func TestUpstreamExecutor_TimeoutIsClassifiedAsAFailsafeTimeout(t *testing.T) {
	// The per-attempt timeout must surface as a typed failsafe timeout so the
	// network layer can tell it apart from a client cancellation and move on
	// to another upstream instead of returning the client's own deadline.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{
		Timeout: &common.TimeoutPolicyConfig{Duration: &common.AdaptiveDuration{Base: common.Duration(20 * time.Millisecond)}},
	})
	require.NotNil(t, e.Timeout())

	_, err := e.Run(context.Background(), requestFor(t, "eth_getBalance"),
		func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error) {
			<-ctx.Done()
			return nil, context.Cause(ctx)
		})
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded),
		"got %v, want a failsafe timeout classification", err)
	require.True(t, errors.Is(err, common.ErrDynamicTimeoutExceeded),
		"the timeout sentinel must remain reachable, got %v", err)
}

func TestUpstreamExecutor_NonTimeoutErrorsAreNotRelabelledAsTimeouts(t *testing.T) {
	// A timeout policy must not turn every failure into "timed out". That
	// misdiagnosis sends operators looking for latency instead of the real
	// unauthorized/5xx cause.
	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{
		Timeout: &common.TimeoutPolicyConfig{Duration: &common.AdaptiveDuration{Base: common.Duration(10 * time.Second)}},
	})
	boom := common.NewErrEndpointUnauthorized(errors.New("bad api key"))
	_, err := e.Run(context.Background(), requestFor(t, "eth_getBalance"),
		func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error) {
			return nil, boom
		})
	require.True(t, errors.Is(err, boom))
	require.False(t, common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded))
}

func TestHedgeFromCtx_OnlyTrueForATaggedContext(t *testing.T) {
	// The tag is what stops a hedge attempt from being counted against the
	// breaker. A wrong-typed or absent value must read as "not a hedge".
	require.False(t, hedgeFromCtx(context.Background()))
	require.True(t, hedgeFromCtx(context.WithValue(context.Background(), hedgeCtxKey{}, true)))
	require.False(t, hedgeFromCtx(context.WithValue(context.Background(), hedgeCtxKey{}, false)))
	require.False(t, hedgeFromCtx(context.WithValue(context.Background(), hedgeCtxKey{}, "yes")),
		"a non-bool value must not be read as a hedge")
}

func TestUpstreamExecutor_StringDescribesTheConfiguredPolicies(t *testing.T) {
	// This string is what an operator sees in the logs when they ask which
	// policy matched a request. It has to name the real configuration.
	require.Equal(t, "upstreamExecutor(nil)", (*upstreamExecutor)(nil).String())

	e := newTestExecutor(t, &common.UpstreamFailsafeConfig{
		MatchMethod:    "eth_get*",
		MatchFinality:  []common.DataFinalityState{common.DataFinalityStateFinalized},
		Retry:          &common.RetryPolicyConfig{MaxAttempts: 3},
		CircuitBreaker: &common.CircuitBreakerPolicyConfig{FailureThresholdCount: 1},
		Hedge:          &common.HedgePolicyConfig{MaxCount: 2},
	})
	require.Equal(t,
		"upstreamExecutor{method=eth_get*,finalities=[finalized],retry=3,cb=true,hedge=2}",
		e.String())
}
