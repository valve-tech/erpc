package upstream

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/stretchr/testify/require"
)

// These tests drive the hedge race that upstreamExecutor.runHedge builds on top
// of failsafe.RunHedged. The generic primitive is covered in
// failsafe/hedge_race_test.go; what is pinned HERE is the executor's own wiring:
// which requests may be hedged at all, which attempt carries the hedge tag,
// which attempt is allowed to move the circuit breaker, and whether a losing
// attempt's response buffer goes back to the pool.
//
// Ordering is decided by channels, never by sleeps. Each attempt parks on its
// own gate until the test releases it, so "the hedge answered first" is a fact
// the test creates rather than a race it hopes to win.

// hedgeReply is the answer one scripted attempt hands back to the executor.
type hedgeReply struct {
	resp *common.NormalizedResponse
	err  error
}

// hedgeAttempt is one scripted trip through the executor's inner function.
type hedgeAttempt struct {
	started  chan struct{}
	finished chan struct{}
	canceled chan struct{}
	reply    chan hedgeReply

	// onCancel is returned when the attempt's context is canceled before the
	// test posted a reply. A zero onCancel yields (nil, context.Cause(ctx)) —
	// the shape of a client that gave up. A non-nil resp models a client that
	// was already mid-flight and still produces a buffer someone must release.
	onCancel hedgeReply

	isHedge atomic.Bool
	tagged  atomic.Bool
}

// hedgeScript hands the executor's attempts out in order, one gate each.
//
// Reuse it for any upstream-scope concurrency fixture: build one with the
// number of attempts you expect, pass script.inner to upstreamExecutor.Run,
// and drive the order with attempt(i).started / release(i, ...). An attempt
// beyond the scripted count is recorded as an overrun instead of blocking,
// so a policy that fires too often fails the test rather than hanging it.
type hedgeScript struct {
	attempts []*hedgeAttempt
	next     atomic.Int32
	overrun  atomic.Int32
}

func newHedgeScript(n int) *hedgeScript {
	s := &hedgeScript{attempts: make([]*hedgeAttempt, n)}
	for i := range s.attempts {
		s.attempts[i] = &hedgeAttempt{
			started:  make(chan struct{}),
			finished: make(chan struct{}),
			canceled: make(chan struct{}),
			reply:    make(chan hedgeReply, 1),
		}
	}
	return s
}

func (s *hedgeScript) attempt(i int) *hedgeAttempt { return s.attempts[i] }

// count reports how many attempts the executor has made so far.
func (s *hedgeScript) count() int { return int(s.next.Load()) }

// release posts the answer for attempt i.
func (s *hedgeScript) release(i int, resp *common.NormalizedResponse, err error) {
	s.attempts[i].reply <- hedgeReply{resp: resp, err: err}
}

func (s *hedgeScript) inner(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error) {
	i := int(s.next.Add(1)) - 1
	if i >= len(s.attempts) {
		s.overrun.Add(1)
		return nil, errors.New("unscripted attempt")
	}
	a := s.attempts[i]
	a.isHedge.Store(isHedge)
	a.tagged.Store(hedgeFromCtx(ctx))
	defer close(a.finished)
	close(a.started)
	select {
	case r := <-a.reply:
		return r.resp, r.err
	case <-ctx.Done():
		close(a.canceled)
		if a.onCancel.resp == nil && a.onCancel.err == nil {
			return nil, context.Cause(ctx)
		}
		return a.onCancel.resp, a.onCancel.err
	}
}

// hedgeTestDeadline bounds every wait in this file. A mutation to the hedge
// race is at least as likely to deadlock as to return a wrong answer, and a
// deadlocked test that only dies at the binary timeout reads like a pass.
const hedgeTestDeadline = 5 * time.Second

type runResult struct {
	resp *common.NormalizedResponse
	err  error
}

// runAsync starts e.Run on its own goroutine so the test goroutine stays free
// to drive the script and to time the whole race out.
func runAsync(e *upstreamExecutor, req *common.NormalizedRequest, inner func(context.Context, bool) (*common.NormalizedResponse, error)) <-chan runResult {
	out := make(chan runResult, 1)
	go func() {
		resp, err := e.Run(context.Background(), req, inner)
		out <- runResult{resp: resp, err: err}
	}()
	return out
}

func awaitRun(t *testing.T, ch <-chan runResult) runResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(hedgeTestDeadline):
		t.Fatal("upstreamExecutor.Run never returned: the hedge race deadlocked")
		return runResult{}
	}
}

func awaitClose(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(hedgeTestDeadline):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// hedgeResponse builds an already-parsed response. Parsing up front matters:
// Release() clears the parsed body, so a nil JsonRpcResponse afterwards is
// proof the hedger handed the buffer back to the pool.
func hedgeResponse(t *testing.T, result string) *common.NormalizedResponse {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"result":"` + result + `"}`
	r := common.NewNormalizedResponse().
		WithBody(io.NopCloser(strings.NewReader(body))).
		WithExpectedSize(len(body))
	jrr, err := r.JsonRpcResponse()
	require.NoError(t, err)
	require.NotNil(t, jrr)
	return r
}

// wasReleased reports whether Release() has cleared the response's parsed body.
func wasReleased(r *common.NormalizedResponse) bool {
	jrr, _ := r.JsonRpcResponse()
	return jrr == nil
}

// hedgingExecutor builds an executor that hedges once after `delay`.
func hedgingExecutor(t *testing.T, delay time.Duration, cb *common.CircuitBreakerPolicyConfig) *upstreamExecutor {
	t.Helper()
	cfg := &common.UpstreamFailsafeConfig{
		Hedge: &common.HedgePolicyConfig{
			MaxCount: 1,
			Delay:    &common.AdaptiveDuration{Base: common.Duration(delay)},
		},
		CircuitBreaker: cb,
	}
	return newTestExecutor(t, cfg)
}

// neverFires is a hedge delay no test will ever wait out. It makes "the primary
// won before the hedge was due" a deterministic fact instead of a timing race.
const neverFires = time.Hour

func TestRunHedge_NoHedgePolicyRunsThePrimaryAlone(t *testing.T) {
	// An upstream with hedging switched off must issue exactly one call.
	// A stray second attempt doubles the load — and the bill — of every
	// operator who never asked for hedging.
	for name, cfg := range map[string]*common.UpstreamFailsafeConfig{
		"no hedge block":   {},
		"maxCount zero":    {Hedge: &common.HedgePolicyConfig{MaxCount: 0}},
		"negative count":   {Hedge: &common.HedgePolicyConfig{MaxCount: -3}},
		"nil whole config": nil,
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestExecutor(t, cfg)
			s := newHedgeScript(2)
			want := hedgeResponse(t, "0x1")

			ch := runAsync(e, requestFor(t, "eth_getBalance"), s.inner)
			awaitClose(t, s.attempt(0).started, "the primary attempt to start")
			s.release(0, want, nil)

			got := awaitRun(t, ch)
			require.NoError(t, got.err)
			require.Same(t, want, got.resp)
			require.Equal(t, 1, s.count(), "hedging is off, so exactly one attempt may run")
			require.False(t, s.attempt(0).isHedge.Load(), "the only attempt is not a hedge")
		})
	}
}

func TestRunHedge_CompositeRequestsAreNeverHedged(t *testing.T) {
	// A composite request is already fanned out into sub-calls. Hedging it
	// duplicates the whole fan-out, so one client request becomes 2N upstream
	// calls against a node that is usually already the slow one.
	e := hedgingExecutor(t, 0, nil)
	req := requestFor(t, "eth_getLogs")
	req.SetCompositeType(common.CompositeTypeLogsSplitProactive)
	s := newHedgeScript(2)
	want := hedgeResponse(t, "0x1")

	ch := runAsync(e, req, s.inner)
	awaitClose(t, s.attempt(0).started, "the primary attempt to start")
	s.release(0, want, nil)

	got := awaitRun(t, ch)
	require.NoError(t, got.err)
	require.Same(t, want, got.resp)
	require.Equal(t, 1, s.count(), "a composite request must not be hedged")
	require.Zero(t, req.ExecState().UpstreamHedges.Load())
}

func TestRunHedge_WriteMethodsAreNeverHedged(t *testing.T) {
	// Hedging a write sends the same transaction twice. Even with a zero
	// hedge delay the second copy must never leave the process.
	e := hedgingExecutor(t, 0, nil)
	s := newHedgeScript(2)
	want := hedgeResponse(t, "0xhash")

	ch := runAsync(e, requestFor(t, "eth_sendTransaction"), s.inner)
	awaitClose(t, s.attempt(0).started, "the primary attempt to start")
	s.release(0, want, nil)

	got := awaitRun(t, ch)
	require.NoError(t, got.err)
	require.Same(t, want, got.resp)
	require.Equal(t, 1, s.count(), "a write method must be sent exactly once")
}

func TestRunHedge_PrimaryWinsBeforeTheHedgeIsDue(t *testing.T) {
	// The common case: the upstream answers inside the hedge delay, so no
	// extra call is made. The hedge counter must stay at zero, otherwise
	// every request looks hedged and a real hedge-rate regression hides.
	e := hedgingExecutor(t, neverFires, nil)
	req := requestFor(t, "eth_getBalance")
	s := newHedgeScript(2)
	want := hedgeResponse(t, "0x2a")

	ch := runAsync(e, req, s.inner)
	awaitClose(t, s.attempt(0).started, "the primary attempt to start")
	s.release(0, want, nil)

	got := awaitRun(t, ch)
	require.NoError(t, got.err)
	require.Same(t, want, got.resp)
	require.Equal(t, 1, s.count(), "the hedge was not due yet")
	require.Zero(t, req.ExecState().UpstreamHedges.Load())
	require.Equal(t, int32(1), req.ExecState().UpstreamAttempts.Load())
}

func TestRunHedge_TheHedgeWinsAndCancelsThePrimary(t *testing.T) {
	// A hedge exists to rescue a stalled primary. When it answers first its
	// result must be the one returned, and the stalled sibling must be
	// canceled — otherwise the hedged request keeps burning the upstream's
	// capacity and rate-limit budget on an answer nobody will read.
	e := hedgingExecutor(t, 0, nil)
	req := requestFor(t, "eth_getBalance")
	s := newHedgeScript(2)
	want := hedgeResponse(t, "0xhedge")

	ch := runAsync(e, req, s.inner)
	awaitClose(t, s.attempt(0).started, "the primary attempt to start")
	awaitClose(t, s.attempt(1).started, "the hedge attempt to start")
	s.release(1, want, nil)

	got := awaitRun(t, ch)
	require.NoError(t, got.err)
	require.Same(t, want, got.resp, "the hedge's answer must win")
	awaitClose(t, s.attempt(0).canceled, "the stalled primary to be canceled")

	st := req.ExecState()
	require.Equal(t, int32(1), st.UpstreamHedges.Load(), "exactly one extra attempt is a hedge")
	require.Equal(t, int32(2), st.UpstreamAttempts.Load(), "the hedge counts as an attempt too")
	require.Zero(t, st.UpstreamRetries.Load(), "a hedge is not a retry")
}

func TestRunHedge_OnlyTheExtraAttemptCarriesTheHedgeTag(t *testing.T) {
	// The tag is what keeps a hedge off the breaker and out of the primary's
	// metrics. Tagging the primary would exempt ordinary traffic from the
	// breaker; tagging nothing would let duplicates cordon a healthy upstream.
	e := hedgingExecutor(t, 0, nil)
	s := newHedgeScript(2)
	want := hedgeResponse(t, "0xhedge")

	ch := runAsync(e, requestFor(t, "eth_getBalance"), s.inner)
	awaitClose(t, s.attempt(0).started, "the primary attempt to start")
	awaitClose(t, s.attempt(1).started, "the hedge attempt to start")
	s.release(1, want, nil)
	require.NoError(t, awaitRun(t, ch).err)

	require.False(t, s.attempt(0).isHedge.Load(), "the primary must not be flagged as a hedge")
	require.False(t, s.attempt(0).tagged.Load(), "the primary's context must not carry the hedge tag")
	require.True(t, s.attempt(1).isHedge.Load(), "the extra attempt must be flagged as a hedge")
	require.True(t, s.attempt(1).tagged.Load(), "the hedge's context must carry the hedge tag")
}

func TestRunHedge_ALateArrivingLoserIsReleasedNotLeaked(t *testing.T) {
	// The primary is already mid-flight when the hedge wins, so it still
	// produces a response buffer. eRPC pools those buffers: a missed release
	// is a slow leak that only appears under sustained hedged traffic.
	e := hedgingExecutor(t, 0, nil)
	s := newHedgeScript(2)
	loser := hedgeResponse(t, "0xlate")
	winner := hedgeResponse(t, "0xhedge")
	s.attempt(0).onCancel = hedgeReply{resp: loser}

	ch := runAsync(e, requestFor(t, "eth_getBalance"), s.inner)
	awaitClose(t, s.attempt(0).started, "the primary attempt to start")
	awaitClose(t, s.attempt(1).started, "the hedge attempt to start")
	s.release(1, winner, nil)

	got := awaitRun(t, ch)
	require.NoError(t, got.err)
	require.Same(t, winner, got.resp)

	require.True(t, wasReleased(loser),
		"the late-arriving primary's buffer must be released back to the pool")
	require.False(t, wasReleased(winner),
		"the winner belongs to the caller and must never be released by the hedger")
}

func TestRunHedge_ARetryableFailureFromThePrimaryLetsTheHedgeWin(t *testing.T) {
	// A transport failure says nothing about the request, only about that
	// connection. Accepting it as the race's answer would return an error to
	// the client while a healthy hedge was already in flight.
	e := hedgingExecutor(t, 0, nil)
	s := newHedgeScript(2)
	winner := hedgeResponse(t, "0xhedge")
	transport := common.NewErrEndpointTransportFailure(&url.URL{Host: "node"}, errors.New("connection reset"))

	ch := runAsync(e, requestFor(t, "eth_getBalance"), s.inner)
	awaitClose(t, s.attempt(0).started, "the primary attempt to start")
	awaitClose(t, s.attempt(1).started, "the hedge attempt to start")
	s.release(0, nil, transport)
	s.release(1, winner, nil)

	got := awaitRun(t, ch)
	require.NoError(t, got.err, "the hedge's success must survive the primary's failure")
	require.Same(t, winner, got.resp)
}

func TestRunHedge_ATerminalFailureFromThePrimaryEndsTheRace(t *testing.T) {
	// An execution revert is the chain's real answer. Racing on it would send
	// a second identical call whose reply is guaranteed to be the same revert,
	// so the race must stop and the revert must reach the caller intact.
	e := hedgingExecutor(t, neverFires, nil)
	s := newHedgeScript(2)
	cause := errors.New("execution reverted")
	revert := common.NewErrEndpointExecutionException(cause)

	ch := runAsync(e, requestFor(t, "eth_call"), s.inner)
	awaitClose(t, s.attempt(0).started, "the primary attempt to start")
	s.release(0, nil, revert)

	got := awaitRun(t, ch)
	require.Error(t, got.err)
	require.True(t, errors.Is(got.err, cause), "the revert must reach the caller, got %v", got.err)
	require.Equal(t, 1, s.count(), "a terminal answer must not be raced")
}

func TestRunHedge_AHedgeFailureDoesNotOpenTheBreaker(t *testing.T) {
	// A hedge is a duplicate this process chose to send. Counting its failure
	// would let eRPC cordon an upstream on the strength of traffic no client
	// ever asked for — and hedging is heaviest exactly when the fleet is
	// already under pressure.
	cb := &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(time.Hour),
	}
	e := hedgingExecutor(t, 0, cb)
	require.NotNil(t, e.Breaker())
	s := newHedgeScript(2)
	winner := hedgeResponse(t, "0xok")

	ch := runAsync(e, requestFor(t, "eth_getBalance"), s.inner)
	awaitClose(t, s.attempt(0).started, "the primary attempt to start")
	awaitClose(t, s.attempt(1).started, "the hedge attempt to start")
	s.release(1, nil, common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500))
	awaitClose(t, s.attempt(1).finished, "the hedge attempt to return")
	s.release(0, winner, nil)

	got := awaitRun(t, ch)
	require.NoError(t, got.err)
	require.Same(t, winner, got.resp)
	require.Equal(t, failsafe.StateClosed, e.Breaker().State(),
		"a hedge's 5xx must not move a 1-of-1 breaker")
}

func TestRunHedge_ThePrimarysFailureStillOpensTheBreaker(t *testing.T) {
	// The mirror image of the rule above. A hedge rescuing the request must
	// not hide the primary's 5xx from the breaker, or a broken upstream stays
	// in rotation for as long as hedging keeps papering over it.
	cb := &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(time.Hour),
	}
	e := hedgingExecutor(t, 0, cb)
	s := newHedgeScript(2)
	winner := hedgeResponse(t, "0xok")

	ch := runAsync(e, requestFor(t, "eth_getBalance"), s.inner)
	awaitClose(t, s.attempt(0).started, "the primary attempt to start")
	awaitClose(t, s.attempt(1).started, "the hedge attempt to start")
	s.release(0, nil, common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500))
	awaitClose(t, s.attempt(0).finished, "the primary attempt to return")
	s.release(1, winner, nil)

	got := awaitRun(t, ch)
	require.NoError(t, got.err, "the hedge still answers the client")
	require.Same(t, winner, got.resp)
	require.Equal(t, failsafe.StateOpen, e.Breaker().State(),
		"the primary's 5xx must still open a 1-of-1 breaker")
}

func TestRunHedge_AnOpenBreakerStopsThePrimaryButNotTheHedge(t *testing.T) {
	// Hedges skip the breaker's permit check as well as its counters. That is
	// what lets a hedge probe a cordoned upstream, and it is the reason the
	// eligibility rule has to be one decision used on both sides.
	cb := &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(time.Hour),
	}
	e := hedgingExecutor(t, 0, cb)

	// Open the breaker with one ordinary failing request.
	opener := newHedgeScript(2)
	ch := runAsync(e, requestFor(t, "eth_getBalance"), opener.inner)
	awaitClose(t, opener.attempt(0).started, "the opening attempt to start")
	awaitClose(t, opener.attempt(1).started, "the opening hedge to start")
	opener.release(0, nil, common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500))
	opener.release(1, nil, common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500))
	require.Error(t, awaitRun(t, ch).err)
	require.Equal(t, failsafe.StateOpen, e.Breaker().State())

	// Now the primary is refused before it reaches the upstream, while the
	// hedge is still allowed through and answers the client.
	s := newHedgeScript(1)
	winner := hedgeResponse(t, "0xrescued")
	ch = runAsync(e, requestFor(t, "eth_getBalance"), s.inner)
	awaitClose(t, s.attempt(0).started, "the hedge attempt to reach the upstream")
	require.True(t, s.attempt(0).isHedge.Load(),
		"the only attempt that reaches the upstream must be the hedge")
	s.release(0, winner, nil)

	got := awaitRun(t, ch)
	require.NoError(t, got.err)
	require.Same(t, winner, got.resp)
	require.Zero(t, s.overrun.Load(), "the primary must never reach the upstream")
}
