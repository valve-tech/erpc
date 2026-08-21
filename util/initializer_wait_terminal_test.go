package util

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

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
