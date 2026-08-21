package common

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ExecState feeds the X-ERPC-Attempts / X-ERPC-Retries / X-ERPC-Hedges headers
// and the execution.* span attributes. An operator uses those numbers to answer
// "how hard did eRPC work for this request", so double-counting or dropping a
// scope makes a healthy request look pathological (or the reverse).

func TestExecState_SnapshotDerivesTotalsFromTheScopes(t *testing.T) {
	s := &ExecState{StartedAt: time.Now()}

	s.UpstreamAttempts.Store(5)
	s.UpstreamRetries.Store(2)
	s.UpstreamHedges.Store(1)
	s.NetworkAttempts.Store(3)
	s.NetworkRetries.Store(2)
	s.NetworkHedges.Store(1)
	s.CacheAttempts.Store(4)
	s.CacheRetries.Store(1)
	s.CacheHedges.Store(1)
	s.ConsensusSlots.Store(3)
	s.ConsensusDisputes.Store(1)
	s.ConsensusLowParticipants.Store(1)

	snap := s.Snapshot()

	// NetworkAttempts counts rotations, and each rotation already produced an
	// upstream attempt. Summing it in would report 12 physical calls where 9
	// happened.
	require.Equal(t, 9, snap.Attempts, "total attempts must exclude the rotation count")
	require.Equal(t, 5, snap.Retries, "retries are distinct events per scope and DO sum")
	require.Equal(t, 3, snap.Hedges)

	// Every per-scope counter must survive the snapshot untouched.
	require.Equal(t, 5, snap.UpstreamAttempts)
	require.Equal(t, 2, snap.UpstreamRetries)
	require.Equal(t, 1, snap.UpstreamHedges)
	require.Equal(t, 3, snap.NetworkAttempts)
	require.Equal(t, 2, snap.NetworkRetries)
	require.Equal(t, 1, snap.NetworkHedges)
	require.Equal(t, 4, snap.CacheAttempts)
	require.Equal(t, 1, snap.CacheRetries)
	require.Equal(t, 1, snap.CacheHedges)
	require.Equal(t, 3, snap.ConsensusSlots)
	require.Equal(t, 1, snap.ConsensusDisputes)
	require.Equal(t, 1, snap.ConsensusLowParticipants)
	require.Equal(t, s.StartedAt, snap.StartedAt)
}

// A nil ExecState is the normal state for a request nobody instrumented. Every
// accessor must answer zero rather than panic in the header writer.
func TestExecState_NilStateAnswersZero(t *testing.T) {
	var s *ExecState

	require.Equal(t, ExecStateSnapshot{}, s.Snapshot())
	require.Nil(t, s.UpstreamAttemptLog())
	require.Equal(t, int64(0), s.CreditUnitsTotal())
	require.Nil(t, s.CreditUnitsByVendor())

	require.NotPanics(t, func() {
		s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "up1"})
		s.MarkUpstreamAttemptWon("up1")
		s.Apply(nil)
	})
}

func TestExecState_AttemptLogIsACopyInOrder(t *testing.T) {
	s := &ExecState{StartedAt: time.Now()}

	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "up1", Reason: SelectionReasonPrimary, Outcome: UpstreamOutcomeServerError})
	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "up2", Reason: SelectionReasonRetry, Outcome: UpstreamOutcomeSuccess})

	log := s.UpstreamAttemptLog()
	require.Len(t, log, 2)
	require.Equal(t, "up1", log[0].UpstreamId, "the log must keep the order attempts started in")
	require.Equal(t, "up2", log[1].UpstreamId)

	// The caller gets a copy: mutating it must not rewrite the request's own
	// record, which several goroutines still read.
	log[0].UpstreamId = "tampered"
	require.Equal(t, "up1", s.UpstreamAttemptLog()[0].UpstreamId)
}

// MarkUpstreamAttemptWon flags the MOST RECENT attempt on that upstream. A
// retried upstream has several entries; flagging the wrong one would credit a
// failed attempt with producing the response.
func TestExecState_MarkWonPicksTheLatestAttemptForThatUpstream(t *testing.T) {
	s := &ExecState{StartedAt: time.Now()}

	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "up1", AttemptIdx: 0, Outcome: UpstreamOutcomeServerError})
	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "up2", AttemptIdx: 0, Outcome: UpstreamOutcomeTimeout})
	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "up1", AttemptIdx: 1, Outcome: UpstreamOutcomeSuccess})

	s.MarkUpstreamAttemptWon("up1")

	log := s.UpstreamAttemptLog()
	require.False(t, log[0].Won, "the earlier failed attempt must not be credited")
	require.False(t, log[1].Won)
	require.True(t, log[2].Won)

	// An unknown or empty id is a no-op, not a panic and not a wrong flag.
	s.MarkUpstreamAttemptWon("up-does-not-exist")
	s.MarkUpstreamAttemptWon("")
	log = s.UpstreamAttemptLog()
	require.False(t, log[0].Won)
	require.True(t, log[2].Won)
}

// Credit units are what an operator bills against. An attempt with no vendor is
// self-hosted and costs nothing, so counting it would inflate the invoice.
func TestExecState_CreditUnitsExcludeVendorlessAttempts(t *testing.T) {
	s := &ExecState{StartedAt: time.Now()}

	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "a1", VendorName: "alchemy", CreditUnits: 10})
	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "a2", VendorName: "alchemy", CreditUnits: 5})
	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "d1", VendorName: "drpc", CreditUnits: 7})
	// Self-hosted: no vendor, so no cost even though a number is present.
	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "self", CreditUnits: 99})
	// Never dialed: a vendor but zero cost.
	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "a3", VendorName: "alchemy", CreditUnits: 0})

	require.Equal(t, int64(22), s.CreditUnitsTotal())

	byVendor := s.CreditUnitsByVendor()
	require.Equal(t, map[string]int64{"alchemy": 15, "drpc": 7}, byVendor)

	// The total must always equal the sum of the per-vendor buckets, or the
	// header and the breakdown disagree.
	var sum int64
	for _, v := range byVendor {
		sum += v
	}
	require.Equal(t, s.CreditUnitsTotal(), sum)
}

// With nothing to charge the breakdown must be nil, not an empty map: the
// header writer omits the field on nil and would emit "{}" otherwise.
func TestExecState_CreditUnitsByVendorIsNilWhenNothingWasCharged(t *testing.T) {
	s := &ExecState{StartedAt: time.Now()}
	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "self", CreditUnits: 99})
	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "a1", VendorName: "alchemy", CreditUnits: 0})

	require.Equal(t, int64(0), s.CreditUnitsTotal())
	require.Nil(t, s.CreditUnitsByVendor())
}

// Consensus participants record concurrently. A lost append would understate
// the participation log an operator reads to explain a dispute.
func TestExecState_ConcurrentRecordingKeepsEveryAttempt(t *testing.T) {
	s := &ExecState{StartedAt: time.Now()}

	const writers = 20
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "up", VendorName: "alchemy", CreditUnits: 2})
		}()
	}
	wg.Wait()

	require.Len(t, s.UpstreamAttemptLog(), writers)
	require.Equal(t, int64(2*writers), s.CreditUnitsTotal())
}

// The holder allocates once and hands the same state to every caller. A second
// allocation would split one request's counters across two objects.
func TestExecStateHolder_AllocatesExactlyOnce(t *testing.T) {
	var h execStateHolder

	first := h.get()
	require.NotNil(t, first)
	require.False(t, first.StartedAt.IsZero(), "the start time must be set on creation")

	require.Same(t, first, h.get())

	// Under concurrency too — the executors race to touch the state.
	var wg sync.WaitGroup
	results := make([]*ExecState, 16)
	wg.Add(len(results))
	for i := range results {
		go func(i int) {
			defer wg.Done()
			results[i] = h.get()
		}(i)
	}
	wg.Wait()
	for i := range results {
		require.Same(t, first, results[i], "goroutine %d got a different ExecState", i)
	}
}
