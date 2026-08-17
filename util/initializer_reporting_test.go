package util

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTaskState_String pins the task labels. They are the words an operator
// reads in /healthcheck and in the status JSON, so a reordered or renamed
// label mislabels every task at once. String() indexes a slice by the enum
// value, which is exactly the kind of mapping a reorder breaks silently.
func TestTaskState_String(t *testing.T) {
	assert.Equal(t, "pending", TaskPending.String())
	assert.Equal(t, "running", TaskRunning.String())
	assert.Equal(t, "succeeded", TaskSucceeded.String())
	assert.Equal(t, "timedOut", TaskTimedOut.String())
	assert.Equal(t, "failed", TaskFailed.String())
	assert.Equal(t, "fatal", TaskFatal.String())
}

// TestInitializer_ExecuteNoTasksIsANoOp: callers pass whatever task list they
// assembled, sometimes empty. An empty call must not register anything or
// start the auto-retry goroutine, or every such call leaks a loop.
func TestInitializer_ExecuteNoTasksIsANoOp(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, nil)

	require.NoError(t, init.ExecuteTasks(appCtx))
	assert.False(t, init.autoRetryActive.Load(), "no tasks means no retry loop")
	assert.Empty(t, init.Status().Tasks)
}

// TestBootstrapTask_WaitOnANeverStartedTask: Wait must honour the caller's
// deadline even when the task has never had an attempt. A Wait that ignores
// the context here parks a request goroutine forever.
func TestBootstrapTask_WaitOnANeverStartedTask(t *testing.T) {
	task := NewBootstrapTask("never-started", func(ctx context.Context) error { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := task.Wait(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second, "Wait must return on the deadline, not hang")
	assert.Equal(t, TaskPending, TaskState(task.state.Load()))
}

// TestInitializer_CancelledTaskIsReportedWithoutItsReason pins the behaviour a
// task gets when its function returns context.Canceled.
//
// The initializer stores wrappedError{nil} before each attempt, so the
// cancellation branch's CompareAndSwap(nil, ...) can never succeed and the real
// reason is dropped. Wait then notices the empty error and substitutes "task
// failed without specific error". The task IS correctly reported as failed, and
// the aggregate error still counts it — only the reason is lost. This test
// pins today's behaviour so a fix is a visible change, not a silent one.
func TestInitializer_CancelledTaskIsReportedWithoutItsReason(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: 5 * time.Second,
		AutoRetry:   false,
	})

	release := make(chan struct{})
	task := NewBootstrapTask("cancelled", func(ctx context.Context) error {
		<-release
		return context.Canceled
	})

	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()

	err := init.ExecuteTasks(appCtx, task)
	defer func() { _ = init.Stop(nil) }()

	require.Error(t, err, "a cancelled task must still count as a failed task")
	assert.Contains(t, err.Error(), "1/1 tasks failed")
	assert.Equal(t, TaskFailed, TaskState(task.state.Load()))

	require.NotNil(t, task.Error())
	assert.Equal(t, "task failed without specific error", task.Error().Err.Error(),
		"today the cancellation reason is dropped; see initializer.go:376")
}

// TestInitializer_RetryBackoff covers the schedule that paces re-attempts.
// Both the request path and the background loop use it, so a wrong delay here
// either hammers a broken upstream or strands a recoverable one.
func TestInitializer_RetryBackoff(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Run("the delay grows by the factor and stops at the maximum", func(t *testing.T) {
		init := setupInitializer(t, appCtx, &InitializerConfig{
			RetryMinDelay: 1 * time.Second,
			RetryMaxDelay: 4 * time.Second,
			RetryFactor:   2,
		})

		assert.Equal(t, 1*time.Second, init.retryBackoff(1))
		assert.Equal(t, 2*time.Second, init.retryBackoff(2))
		assert.Equal(t, 4*time.Second, init.retryBackoff(3))
		assert.Equal(t, 4*time.Second, init.retryBackoff(9), "the cap must hold")
	})

	t.Run("a task with no attempt yet waits the minimum", func(t *testing.T) {
		init := setupInitializer(t, appCtx, &InitializerConfig{
			RetryMinDelay: 2 * time.Second,
			RetryMaxDelay: 9 * time.Second,
			RetryFactor:   2,
		})

		assert.Equal(t, 2*time.Second, init.retryBackoff(0))
		assert.Equal(t, 2*time.Second, init.retryBackoff(-3))
	})

	t.Run("an unset configuration falls back to safe defaults", func(t *testing.T) {
		// A zero factor would make every delay collapse to the minimum and a
		// zero maximum would cap every delay at zero, turning the backoff
		// into a busy loop against a failing dependency.
		init := setupInitializer(t, appCtx, &InitializerConfig{})

		assert.Equal(t, 3*time.Second, init.retryBackoff(1))
		assert.Equal(t, 4500*time.Millisecond, init.retryBackoff(2))
		assert.Equal(t, 130*time.Second, init.retryBackoff(20))
	})

	t.Run("a negative configuration falls back to the same defaults", func(t *testing.T) {
		init := setupInitializer(t, appCtx, &InitializerConfig{
			RetryMinDelay: -1 * time.Second,
			RetryMaxDelay: -1 * time.Second,
			RetryFactor:   0.5,
		})

		assert.Equal(t, 3*time.Second, init.retryBackoff(1))
		assert.Equal(t, 130*time.Second, init.retryBackoff(20))
	})
}

// TestInitializer_TaskReadyForRetry: a task that never ran must be allowed to
// start at once. Gating it behind a backoff would delay every first attempt.
func TestInitializer_TaskReadyForRetry(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		RetryMinDelay: time.Hour,
		RetryMaxDelay: time.Hour,
		RetryFactor:   2,
	})

	fresh := NewBootstrapTask("fresh", func(ctx context.Context) error { return nil })
	assert.True(t, init.taskReadyForRetry(fresh), "a task with no attempt is always ready")

	fresh.lastAttempt.Store(time.Time{})
	assert.True(t, init.taskReadyForRetry(fresh), "a zero attempt time counts as never run")

	fresh.lastAttempt.Store(time.Now())
	fresh.attempts.Store(1)
	assert.False(t, init.taskReadyForRetry(fresh), "a just-attempted task must wait out its backoff")
}

// TestInitializer_AppContextAlreadyCancelledWedgesTheInitializer pins a live
// defect, it does not endorse it.
//
// When a task starts after the app context is already cancelled, the launched
// goroutine takes the early-return branch at initializer.go:337 and never calls
// wg.Done(). attemptRemainingTasks then blocks on wg.Wait() at
// initializer.go:397 forever, while still holding i.tasksMu through its
// deferred unlock. Every later ExecuteTasks, attemptRemainingTasks and Stop on
// that initializer blocks on the same mutex, so one late task wedges the whole
// initializer — including shutdown.
//
// Every registry calls ExecuteTasks on the request path, so any request that
// races process shutdown reaches this. The task state itself is still recorded
// correctly; only the caller never returns.
//
// This test deliberately leaks the two parked goroutines. When the bug is
// fixed, it fails loudly and tells you to rewrite it.
func TestInitializer_AppContextAlreadyCancelledWedgesTheInitializer(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	cancel()

	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: time.Second,
		AutoRetry:   false,
	})

	var ran atomic.Bool
	task := NewBootstrapTask("after-shutdown", func(ctx context.Context) error {
		ran.Store(true)
		return nil
	})

	executed := make(chan error, 1)
	go func() { executed <- init.ExecuteTasks(context.Background(), task) }()

	select {
	case err := <-executed:
		t.Fatalf("ExecuteTasks returned (%v) — initializer.go:337 now calls wg.Done() "+
			"on the cancelled-context path. The defect is fixed; rewrite this test to "+
			"assert the task fails with context.Canceled and the call returns.", err)
	case <-time.After(500 * time.Millisecond):
		// Still parked, as the defect predicts.
	}

	assert.False(t, ran.Load(), "the task function must not run once the app context is dead")
	assert.Equal(t, TaskFailed, TaskState(task.state.Load()),
		"the task itself is recorded correctly; only the caller is stranded")
	require.NotNil(t, task.Error())
	assert.ErrorIs(t, task.Error().Err, context.Canceled)

	// Blast radius: the held mutex strands shutdown too.
	stopped := make(chan struct{})
	go func() { _ = init.Stop(nil); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned — the mutex is no longer stranded; rewrite this test")
	case <-time.After(500 * time.Millisecond):
	}
}

// wrappedFatalErr is a fatal task error that also wraps a cause, matching the
// shape common.TaskFatalError has in production.
type wrappedFatalErr struct{ cause error }

func (e *wrappedFatalErr) Error() string     { return "fatal: " + e.cause.Error() }
func (e *wrappedFatalErr) IsTaskFatal() bool { return true }
func (e *wrappedFatalErr) Unwrap() error     { return e.cause }

// TestInitializer_FatalErrorReportsTheUnderlyingCause: the fatal wrapper is
// plumbing. An operator needs the reason inside it — "chainId mismatch", not
// "fatal". Reporting the wrapper hides the only actionable part.
func TestInitializer_FatalErrorReportsTheUnderlyingCause(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: time.Second,
		AutoRetry:   false,
	})

	cause := errors.New("chainId mismatch: expected 1, got 137")
	task := NewBootstrapTask("fatal-wrapped", func(ctx context.Context) error {
		return &wrappedFatalErr{cause: cause}
	})

	_ = init.ExecuteTasks(appCtx, task)
	defer func() { _ = init.Stop(nil) }()

	assert.Equal(t, TaskFatal, TaskState(task.state.Load()))
	require.NotNil(t, task.Error())
	assert.Same(t, cause, task.Error().Err, "the wrapper must be unwrapped, not reported")
	assert.Equal(t, StateFatal, init.State())
}

// TestInitializer_ErrorsJoinsEveryTaskFailure: Errors() is what a caller reads
// to find out what went wrong overall. Dropping any task's error leaves an
// operator debugging a failure that is not mentioned anywhere.
func TestInitializer_ErrorsJoinsEveryTaskFailure(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: time.Second,
		AutoRetry:   false,
	})

	errA := errors.New("upstream-a unreachable")
	errB := errors.New("upstream-b bad chainId")
	_ = init.ExecuteTasks(appCtx,
		NewBootstrapTask("a", func(ctx context.Context) error { return errA }),
		NewBootstrapTask("b", func(ctx context.Context) error { return errB }),
		NewBootstrapTask("ok", func(ctx context.Context) error { return nil }),
	)
	defer func() { _ = init.Stop(nil) }()

	joined := init.Errors()
	require.Error(t, joined)
	assert.ErrorIs(t, joined, errA)
	assert.ErrorIs(t, joined, errB)
}

// TestInitializer_ErrorsIsNilWhenEverythingSucceeded stops Errors() from
// reporting a phantom failure on a healthy initializer.
func TestInitializer_ErrorsIsNilWhenEverythingSucceeded(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, nil)

	require.NoError(t, init.ExecuteTasks(appCtx,
		NewBootstrapTask("ok", func(ctx context.Context) error { return nil })))
	defer func() { _ = init.Stop(nil) }()

	assert.NoError(t, init.Errors())
}

// TestInitializer_MarkTaskAsFailedIgnoresUnknownNames: the name comes from a
// caller, so a typo must leave every registered task alone rather than fail
// the first one it walks past.
func TestInitializer_MarkTaskAsFailedIgnoresUnknownNames(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: time.Second,
		AutoRetry:   false,
	})

	a := NewBootstrapTask("alpha", func(ctx context.Context) error { return nil })
	b := NewBootstrapTask("beta", func(ctx context.Context) error { return nil })
	require.NoError(t, init.ExecuteTasks(appCtx, a, b))
	defer func() { _ = init.Stop(nil) }()

	init.MarkTaskAsFailed("gamma", errors.New("boom"))

	assert.Equal(t, TaskSucceeded, TaskState(a.state.Load()))
	assert.Equal(t, TaskSucceeded, TaskState(b.state.Load()))
	assert.NoError(t, init.Errors())
	assert.Equal(t, StateReady, init.State())
}

// TestInitializerStatus_MarshalJSON pins the shape of the status document.
// The healthcheck endpoint serves it, so a renamed key or a state rendered as
// a number breaks whatever the operator's monitoring parses.
func TestInitializerStatus_MarshalJSON(t *testing.T) {
	lastAttempt := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	status := &InitializerStatus{
		State:     StatePartial,
		LastError: nil,
		Tasks: []TaskStatus{{
			Name:        "upstream-a",
			State:       TaskFailed,
			Err:         nil,
			LastAttempt: lastAttempt,
			Attempts:    3,
		}},
	}

	raw, err := status.MarshalJSON()
	require.NoError(t, err)

	var got map[string]interface{}
	require.NoError(t, sonic.Unmarshal(raw, &got))
	assert.Equal(t, "partial", got["state"], "the state must render as its label, not its number")
	require.Contains(t, got, "tasks")

	tasks, ok := got["tasks"].([]interface{})
	require.True(t, ok)
	require.Len(t, tasks, 1)
	task, ok := tasks[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "upstream-a", task["name"])
	assert.Equal(t, "failed", task["state"])
	assert.EqualValues(t, 3, task["attempts"])
	assert.Contains(t, task, "lastAttempt")
}

// TestTaskStatus_MarshalJSON covers a task status on its own, including a
// recorded error.
func TestTaskStatus_MarshalJSON(t *testing.T) {
	s := &TaskStatus{
		Name:        "upstream-b",
		State:       TaskFatal,
		Err:         errors.New("chainId mismatch"),
		LastAttempt: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC),
		Attempts:    1,
	}

	raw, err := s.MarshalJSON()
	require.NoError(t, err)

	var got map[string]interface{}
	require.NoError(t, sonic.Unmarshal(raw, &got))
	assert.Equal(t, "upstream-b", got["name"])
	assert.Equal(t, "fatal", got["state"])
	assert.EqualValues(t, 1, got["attempts"])
}

// TestInitializer_StateRetryingAfterRepeatedAttempts: while a task is still
// being retried the initializer must say "retrying", not "initializing".
// Operators use that word to tell a slow first boot from a stuck one.
func TestInitializer_StateRetryingAfterRepeatedAttempts(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: 5 * time.Second,
		AutoRetry:   false,
	})

	block := make(chan struct{})
	defer close(block)

	task := NewBootstrapTask("slow", func(ctx context.Context) error {
		<-block
		return nil
	})
	init.tasks.Store(task.Name, task)

	assert.Equal(t, StateInitializing, init.State(), "a pending task alone is still initializing")

	init.attempts.Store(2)
	init.attemptRemainingTasks(false)

	assert.Equal(t, StateRetrying, init.State())
}
