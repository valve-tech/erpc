package util

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Bug 157 pin, route one: the task is ALREADY terminal when Wait is called.
//
// Wait's terminal branch returned lastErr if it held one and nil otherwise. A
// failed task that recorded no reason therefore answered nil — success. The
// cancellation path is one producer of exactly that state: it drops its reason,
// as TestInitializer_CancelledTaskIsReportedWithoutItsReason pins.
//
// waitForTasks then read task.Error() == nil, recorded nothing, and
// ExecuteTasks told the caller the bootstrap was ready.
func TestBootstrapTask_WaitOnATerminalFailureWithNoRecordedReasonStillFails(t *testing.T) {
	t.Parallel()

	for _, state := range []TaskState{TaskFailed, TaskTimedOut, TaskFatal} {
		t.Run(state.String(), func(t *testing.T) {
			task := NewBootstrapTask("silent", func(context.Context) error { return nil })
			task.state.Store(int32(state))
			// lastErr deliberately never set.

			err := task.Wait(context.Background())
			require.Error(t, err,
				"bug 157: a terminal failure must never report success, reason or no reason")
		})
	}
}

// A success must still answer nil — the guard above must not turn every
// terminal state into a failure.
func TestBootstrapTask_WaitOnASuccessAnswersNil(t *testing.T) {
	t.Parallel()

	task := NewBootstrapTask("fine", func(context.Context) error { return nil })
	task.state.Store(int32(TaskSucceeded))
	require.NoError(t, task.Wait(context.Background()))
}

// Bug 157 pin, route two: the attempt ends while the retry loop is claiming the
// next one.
//
// Wait used to return as soon as the attempt's done channel closed, then read
// the state on the NEXT line to decide what to return. Between those two the
// auto-retry loop could swap the task back to TaskRunning, so the `== TaskFailed`
// test was false and Wait returned nil for a task that had failed every attempt
// and was failing again.
//
// This drives the sequence directly rather than betting on a real race: close
// the attempt's channel with the task set to Running, exactly as the window
// leaves it. Wait must NOT treat that as the task finishing.
func TestBootstrapTask_WaitDoesNotMistakeAnEndedAttemptForAnEndedTask(t *testing.T) {
	t.Parallel()

	task := NewBootstrapTask("retrying", func(context.Context) error { return nil })
	task.state.Store(int32(TaskRunning))
	ch := task.createNewDoneChannel()

	var returned atomic.Value
	done := make(chan struct{})
	go func() {
		defer close(done)
		returned.Store(wrappedError{err: task.Wait(context.Background())})
	}()

	// The attempt ends, but the task is still Running: the retry loop has
	// claimed the next attempt and has not published its channel yet.
	close(ch)

	select {
	case <-done:
		t.Fatalf("bug 157: Wait returned %v for a task that is still running",
			returned.Load().(wrappedError).err)
	case <-time.After(250 * time.Millisecond):
		// Correct: still waiting for a TERMINAL state.
	}

	// Now the task really does fail, with a reason.
	boom := errors.New("every attempt failed")
	task.lastErr.Store(wrappedError{err: boom})
	task.state.Store(int32(TaskFailed))

	select {
	case <-done:
		require.ErrorIs(t, returned.Load().(wrappedError).err, boom,
			"Wait must report the failure it finally reached")
	case <-time.After(10 * time.Second):
		t.Fatal("Wait never noticed the terminal state")
	}
}

// Bug 109 pin, first half. Stop gives in-flight tasks one task-timeout to end.
// When they do not, it used to log a warning and return only destroyFn's error,
// so a caller could not tell a clean stop from an abandoned one — it freed
// whatever the initializer was guarding while task goroutines were still
// running and still holding the logger it lent them.
//
// The task below IGNORES its context on purpose. That is the only way to reach
// the abandoned branch: a well-behaved Fn always ends within TaskTimeout, and
// Stop waits TaskTimeout+100ms. A misbehaving Fn is exactly what bug 109 is
// about — "a task goroutine whose Fn is still running when Stop gives up".
//
// ExecuteTasks therefore runs on its own goroutine. Called inline it would
// never return, because Wait now waits for a TERMINAL state (bug 157) and this
// task never reaches one until the test releases it.
func TestInitializer_StopReportsThatItAbandonedRunningTasks(t *testing.T) {
	t.Parallel()

	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// io.Discard, NOT zerolog.NewTestWriter(t). This test deliberately leaves a
	// task goroutine running past Stop, and that goroutine keeps the logger it
	// was lent. A TestWriter would then be written to after tRunner finished,
	// which the race detector flags — and it is right to: that IS bug 109's
	// second half, still open. Pinning the first half must not import the
	// second into every run of this package.
	logger := zerolog.New(io.Discard)
	init := NewInitializer(appCtx, &logger, &InitializerConfig{
		TaskTimeout: 50 * time.Millisecond,
		AutoRetry:   false,
	})

	started := make(chan struct{})
	block := make(chan struct{})
	task := NewBootstrapTask("ignores-its-context", func(ctx context.Context) error {
		close(started)
		<-block
		return nil
	})

	executed := make(chan struct{})
	go func() { defer close(executed); _ = init.ExecuteTasks(appCtx, task) }()
	<-started

	err := init.Stop(nil)
	require.Error(t, err, "bug 109: an abandoned stop must not look like a clean one")
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"the caller must be able to see WHY the stop was abandoned")

	close(block)
	<-executed
}

// The control: a stop that really did wait for its tasks reports no error, so
// the check above cannot pass by making every Stop fail.
func TestInitializer_StopReportsNoErrorWhenTasksActuallyFinished(t *testing.T) {
	t.Parallel()

	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.New(zerolog.NewTestWriter(t))
	init := NewInitializer(appCtx, &logger, &InitializerConfig{
		TaskTimeout: 5 * time.Second,
		AutoRetry:   false,
	})
	_ = init.ExecuteTasks(appCtx, NewBootstrapTask("quick", func(context.Context) error { return nil }))

	require.NoError(t, init.Stop(nil))
}
