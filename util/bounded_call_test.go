package util

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// BoundedCall is the defense against an outbound connection that wedges
// while ignoring its context. The contract an operator depends on: the
// CALLER always returns inside the deadline, and the error it returns is
// the context cause, not the generic deadline error.

var errCause = errors.New("dynamic timeout exceeded")

func TestBoundedCallT_ReturnsTheFunctionResultOnTheHappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := BoundedCallT(ctx, func(context.Context) (string, error) {
		return "ok", nil
	})
	require.NoError(t, err)
	require.Equal(t, "ok", got)
}

func TestBoundedCallT_PropagatesTheFunctionError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	inner := errors.New("upstream refused")
	_, err := BoundedCallT(ctx, func(context.Context) (int, error) {
		return 0, inner
	})
	require.ErrorIs(t, err, inner, "the caller must see the real upstream error, not a wrapper")
}

func TestBoundedCallT_ReturnsWhileTheWedgedCallStillRuns(t *testing.T) {
	// This is the whole point: fn never honours ctx, yet the caller
	// returns. Without the select, one wedged connection would pin a
	// request goroutine forever.
	ctx, cancel := context.WithTimeoutCause(context.Background(), 30*time.Millisecond, errCause)
	defer cancel()

	release := make(chan struct{})
	defer close(release)

	done := make(chan error, 1)
	go func() {
		_, err := BoundedCallT(ctx, func(context.Context) (int, error) {
			<-release // deliberately ignores ctx
			return 1, nil
		})
		done <- err
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, errCause,
			"the caller must surface the context cause, not context.DeadlineExceeded")
	case <-time.After(5 * time.Second):
		t.Fatal("BoundedCallT did not return while fn was wedged")
	}
}

func TestBoundedCallT_AlreadyDeadContextSkipsTheCallEntirely(t *testing.T) {
	// The fast path must not spawn a goroutine that then runs real work
	// against a dead context — a retry loop would otherwise fire one
	// pointless upstream request per exhausted attempt.
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errCause)

	called := make(chan struct{}, 1)
	got, err := BoundedCallT(ctx, func(context.Context) (int, error) {
		called <- struct{}{}
		return 7, nil
	})
	require.ErrorIs(t, err, errCause)
	require.Equal(t, 0, got, "a dead context must yield the zero value")
	select {
	case <-called:
		t.Fatal("fn ran even though the context was already dead")
	case <-time.After(50 * time.Millisecond):
	}
}

// lateContext reports an error while its Done channel stays unready. It
// forces BoundedCallT down the "fn returned AND the context is dead" arm
// of the select, which a real cancelled context can only reach by winning
// a race. Done() returns a nil channel — a receive on nil blocks forever.
type lateContext struct {
	context.Context
	dead *atomic.Bool
}

func (c lateContext) Done() <-chan struct{} { return nil }
func (c lateContext) Err() error {
	if c.dead.Load() {
		return errCause
	}
	return nil
}

func TestBoundedCallT_ContextCauseWinsWhenFnFinishesAtTheDeadline(t *testing.T) {
	// fn succeeds, but its own context died first. The deadline is the
	// proximate cause, so the caller must see it — otherwise a timed-out
	// request would be recorded as a success and the upstream that blew
	// the deadline would look healthy in the tracker.
	var dead atomic.Bool
	ctx := lateContext{Context: context.Background(), dead: &dead}

	done := make(chan struct{})
	go func() {
		defer close(done)
		got, err := BoundedCallT[int](ctx, func(context.Context) (int, error) {
			dead.Store(true)
			return 42, nil
		})
		require.ErrorIs(t, err, errCause, "fn's result must not mask a dead context")
		require.Equal(t, 0, got, "a dead context must yield the zero value, not fn's value")
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("BoundedCallT never returned")
	}
}

func TestBoundedCallT_TurnsAPanicIntoAnErrorInsteadOfKillingTheProcess(t *testing.T) {
	// A vendor client that panics must degrade one request, not the whole
	// proxy. The goroutine is not the caller's, so nothing else can
	// recover it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := BoundedCallT(ctx, func(context.Context) (int, error) {
		panic("vendor client exploded")
	})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "vendor client exploded"),
		"the panic value must reach the operator, got %v", err)
	require.True(t, strings.Contains(err.Error(), "bounded-call goroutine panic"),
		"the error must name its origin, got %v", err)
}

func TestBoundedCall_UntypedVariantMirrorsTheTypedOne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, BoundedCall(ctx, func(context.Context) error { return nil }))

	inner := errors.New("drain failed")
	require.ErrorIs(t, BoundedCall(ctx, func(context.Context) error { return inner }), inner)

	dead, deadCancel := context.WithCancelCause(context.Background())
	deadCancel(errCause)
	require.ErrorIs(t, BoundedCall(dead, func(context.Context) error { return nil }), errCause)
}

func TestBoundedCallT_PassesTheSameContextThroughToFn(t *testing.T) {
	// fn must see the caller's context so a well-behaved client can still
	// cancel itself early; passing a detached context would defeat that.
	type ctxKey struct{}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), ctxKey{}, "v"), 5*time.Second)
	defer cancel()

	got, err := BoundedCallT(ctx, func(inner context.Context) (string, error) {
		v, _ := inner.Value(ctxKey{}).(string)
		return v, nil
	})
	require.NoError(t, err)
	require.Equal(t, "v", got)
}
