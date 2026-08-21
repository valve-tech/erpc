package util

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestInitializer_StopDoesNotDeadlockAgainstTheAutoRetryLoop pins the ordering
// inside Stop.
//
// Stop used to take tasksMu and then, still holding it, wait for the auto-retry
// goroutine. That goroutine calls attemptRemainingTasks, which takes tasksMu
// itself, so it could never finish and never decrement autoRetryWg. Stop then
// waited forever with the mutex held. Nothing breaks the cycle: that Wait has
// no timeout.
//
// `go test -race ./util/ -count=6` found it. Stop sat on the Wait for 39
// minutes. In production the same cycle hangs shutdown until an orchestrator
// kills the process.
//
// The task below always fails, so hasPendingWork stays true and the loop keeps
// re-entering attemptRemainingTasks. The retry delay is tiny so the loop is
// almost always in or near that critical section when Stop runs.
//
// The assertion runs Stop in a goroutine and races it against a deadline, so a
// regression FAILS this test instead of hanging the whole package for its full
// timeout — which is exactly how the defect hid.
func TestInitializer_StopDoesNotDeadlockAgainstTheAutoRetryLoop(t *testing.T) {
	// The window is narrow: the loop must pass its context check and then reach
	// attemptRemainingTasks' Lock while Stop holds the mutex. A single Stop
	// almost always misses it — the original defect needed 39 minutes of
	// `-race -count=6` to surface. So this drives many short-lived
	// initializers and fails on the FIRST one whose Stop does not return.
	const rounds = 400

	for round := 0; round < rounds; round++ {
		appCtx, cancel := context.WithCancel(context.Background())

		init := setupInitializer(t, appCtx, &InitializerConfig{
			TaskTimeout:   50 * time.Millisecond,
			AutoRetry:     true,
			RetryMinDelay: time.Microsecond,
			RetryMaxDelay: time.Microsecond,
		})

		// The task always fails, so hasPendingWork stays true and the loop
		// keeps re-entering the critical section Stop competes for.
		_ = init.ExecuteTasks(appCtx, NewBootstrapTask("never-succeeds",
			func(ctx context.Context) error { return errors.New("keeps the retry loop busy") }))

		stopped := make(chan struct{})
		go func() {
			_ = init.Stop(nil)
			close(stopped)
		}()

		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			cancel()
			t.Fatalf("round %d: Stop deadlocked. It waits for the auto-retry "+
				"goroutine while holding the mutex that goroutine needs to finish.", round)
		}
		cancel()
	}
}
