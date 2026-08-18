package util

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two branches below exist only for the window between a task's state
// change and the publication of its done channel. Both are real: nothing in
// the Initializer holds a lock across that window, and MarkTaskAsFailed writes
// task state without taking tasksMu at all.

// TestBootstrapTask_WaitBusySpinsAndIgnoresItsContextBeforeTheDoneChannelExists
// pins Wait's running-but-no-channel branch.
//
// attemptRemainingTasks swaps a task to Running and only then stores the
// channel that signals the attempt's end. A Wait that lands between the two
// finds a running task with no channel to wait on, and the branch it takes is
// a bare `continue`: no context check, no sleep, no yield. Wait therefore burns
// a whole core and cannot be cancelled until another goroutine publishes the
// channel or a terminal state. Logged as upstream bug 60.
//
// The setup is deterministic, not a race: with the state Running and no done
// channel, Wait has no path that returns, so the observation window below can
// only end one way.
func TestBootstrapTask_WaitBusySpinsAndIgnoresItsContextBeforeTheDoneChannelExists(t *testing.T) {
	t.Parallel()

	task := NewBootstrapTask("windowed", func(context.Context) error { return nil })
	// Running, with no done channel yet — the exact window.
	task.state.Store(int32(TaskRunning))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Wait even starts

	waited := make(chan error, 1)
	go func() { waited <- task.Wait(ctx) }()

	// Bug 60: a cancelled context must end the wait, and it does not. Any
	// return here would mean the branch honoured cancellation.
	select {
	case err := <-waited:
		t.Fatalf("Wait returned %v; bug 60 is fixed, update this test", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Only another goroutine moving the task out of Running can release it —
	// which is what MarkTaskAsFailed does. Use a distinct error so the
	// returned value proves which path produced it.
	task.lastErr.Store(wrappedError{err: assert.AnError})
	task.state.Store(int32(TaskFailed))

	select {
	case err := <-waited:
		require.ErrorIs(t, err, assert.AnError,
			"Wait must report the attempt's own failure, not the context it ignored")
	case <-time.After(20 * time.Second):
		t.Fatal("Wait never left the window after the task left the Running state")
	}
}

// TestInitializer_ALostStateRaceStillReleasesTheStartBarrier proves that a
// task whose state changes under attemptRemainingTasks does not strand the
// caller.
//
// attemptRemainingTasks counts every task into one WaitGroup and then waits for
// all of them to start. When the compare-and-swap to Running loses to a
// concurrent MarkTaskAsFailed, no goroutine is launched for that task, so the
// skip path owns the wg.Done(). Miss it and wg.Wait() never returns — and
// because tasksMu is held across the whole function, every later ExecuteTasks
// and Stop on that Initializer blocks too. That is the same failure shape as
// the shutdown-race wedge already logged as bug 56.
//
// The race is driven, not hoped for: one goroutine rewrites the task's state
// continuously while the scheduler walks many registrations of it.
func TestInitializer_ALostStateRaceStillReleasesTheStartBarrier(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lg := zerolog.New(io.Discard)
	i := NewInitializer(ctx, &lg, &InitializerConfig{
		TaskTimeout:   10 * time.Second,
		AutoRetry:     false,
		RetryFactor:   1.5,
		RetryMinDelay: time.Millisecond,
		RetryMaxDelay: time.Millisecond,
	})

	var runs sync.WaitGroup
	task := NewBootstrapTask("racy", func(context.Context) error { return nil })
	// One task under many keys: the scheduler visits it once per key, so a
	// single writer racing those visits gets many chances to land inside the
	// load-then-swap window.
	const registrations = 2000
	for n := 0; n < registrations; n++ {
		i.tasks.Store(fmt.Sprintf("racy-%d", n), task)
	}

	stop := make(chan struct{})
	const writers = 4
	runs.Add(writers)
	for w := 0; w < writers; w++ {
		go func() {
			defer runs.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				task.state.Store(int32(TaskFailed))
				task.state.Store(int32(TaskPending))
			}
		}()
	}

	scheduled := make(chan struct{})
	go func() {
		defer close(scheduled)
		i.attemptRemainingTasks(false)
	}()

	select {
	case <-scheduled:
	case <-time.After(30 * time.Second):
		close(stop)
		runs.Wait()
		t.Fatal("attemptRemainingTasks never released its start barrier; tasksMu is now held forever")
	}
	close(stop)
	runs.Wait()

	// The Initializer must still be usable: tasksMu was released, so a second
	// scheduling pass returns too.
	second := make(chan struct{})
	go func() {
		defer close(second)
		i.attemptRemainingTasks(false)
	}()
	select {
	case <-second:
	case <-time.After(30 * time.Second):
		t.Fatal("the Initializer is wedged: a later scheduling pass cannot take tasksMu")
	}

	assert.NotZero(t, task.attempts.Load(), "at least one visit must have started the task")
}
