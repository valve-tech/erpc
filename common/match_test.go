package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// SelectExecutor decides which failsafe policy a request runs under. An
// operator who writes a method-specific policy next to a catch-all expects the
// specific one to win; these tests pin that ordering, the wildcard forms that
// count as "unspecific", and the no-match answer.

type testExec struct {
	name     string
	method   string
	finality []DataFinalityState
}

func selectTestExec(execs []testExec, method string, finality DataFinalityState) (string, bool) {
	e, ok := SelectExecutor(
		execs, method, finality,
		func(e testExec) string { return e.method },
		func(e testExec) []DataFinalityState { return e.finality },
	)
	return e.name, ok
}

func TestSelectExecutor_PrefersTheMostSpecificMatch(t *testing.T) {
	finalized := []DataFinalityState{DataFinalityStateFinalized}

	catchAll := testExec{name: "catch-all", method: "*"}
	emptyCatchAll := testExec{name: "empty-catch-all", method: ""}
	finalityOnly := testExec{name: "finality-only", method: "*", finality: finalized}
	methodOnly := testExec{name: "method-only", method: "eth_getLogs"}
	methodAndFinality := testExec{name: "method+finality", method: "eth_getLogs", finality: finalized}

	tests := []struct {
		name     string
		execs    []testExec
		method   string
		finality DataFinalityState
		want     string
		wantOk   bool
	}{
		{
			name:     "method+finality beats every less specific entry",
			execs:    []testExec{catchAll, finalityOnly, methodOnly, methodAndFinality},
			method:   "eth_getLogs",
			finality: DataFinalityStateFinalized,
			want:     "method+finality",
			wantOk:   true,
		},
		{
			name:     "method-only beats finality-only and catch-all",
			execs:    []testExec{catchAll, finalityOnly, methodOnly},
			method:   "eth_getLogs",
			finality: DataFinalityStateFinalized,
			want:     "method-only",
			wantOk:   true,
		},
		{
			name:     "finality-only beats catch-all",
			execs:    []testExec{catchAll, finalityOnly},
			method:   "eth_getLogs",
			finality: DataFinalityStateFinalized,
			want:     "finality-only",
			wantOk:   true,
		},
		{
			name:     "catch-all is the last resort",
			execs:    []testExec{catchAll},
			method:   "eth_getLogs",
			finality: DataFinalityStateRealtime,
			want:     "catch-all",
			wantOk:   true,
		},
		{
			name:     "an empty matchMethod is a catch-all too",
			execs:    []testExec{emptyCatchAll},
			method:   "eth_call",
			finality: DataFinalityStateUnknown,
			want:     "empty-catch-all",
			wantOk:   true,
		},
		{
			name:     "a finality the entry does not list is skipped",
			execs:    []testExec{methodAndFinality},
			method:   "eth_getLogs",
			finality: DataFinalityStateRealtime,
			want:     "",
			wantOk:   false,
		},
		{
			name:     "a method the entry does not name is skipped",
			execs:    []testExec{methodOnly},
			method:   "eth_call",
			finality: DataFinalityStateFinalized,
			want:     "",
			wantOk:   false,
		},
		{
			name:     "an empty executor list matches nothing",
			execs:    nil,
			method:   "eth_call",
			finality: DataFinalityStateFinalized,
			want:     "",
			wantOk:   false,
		},
		{
			name: "a wildcard pattern counts as a specific method match",
			execs: []testExec{
				catchAll,
				{name: "glob", method: "eth_*"},
			},
			method:   "eth_chainId",
			finality: DataFinalityStateFinalized,
			want:     "glob",
			wantOk:   true,
		},
		{
			name: "a wildcard pattern that does not match falls through",
			execs: []testExec{
				catchAll,
				{name: "glob", method: "debug_*"},
			},
			method:   "eth_chainId",
			finality: DataFinalityStateFinalized,
			want:     "catch-all",
			wantOk:   true,
		},
		{
			name: "an entry listing several finalities matches any of them",
			execs: []testExec{
				{name: "multi", method: "eth_getLogs", finality: []DataFinalityState{DataFinalityStateFinalized, DataFinalityStateRealtime}},
			},
			method:   "eth_getLogs",
			finality: DataFinalityStateRealtime,
			want:     "multi",
			wantOk:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := selectTestExec(tt.execs, tt.method, tt.finality)
			require.Equal(t, tt.wantOk, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestSelectExecutor_KeepsTheFirstEntryOfATier pins the tie-break: within one
// specificity tier the earliest entry wins, so an operator can order two
// equally specific policies and get the one they wrote first.
func TestSelectExecutor_KeepsTheFirstEntryOfATier(t *testing.T) {
	finalized := []DataFinalityState{DataFinalityStateFinalized}

	cases := []struct {
		name  string
		execs []testExec
		want  string
	}{
		{
			name: "two method+finality entries",
			execs: []testExec{
				{name: "first", method: "eth_getLogs", finality: finalized},
				{name: "second", method: "eth_*", finality: finalized},
			},
			want: "first",
		},
		{
			name: "two method-only entries",
			execs: []testExec{
				{name: "first", method: "eth_*"},
				{name: "second", method: "eth_getLogs"},
			},
			want: "first",
		},
		{
			name: "two finality-only entries",
			execs: []testExec{
				{name: "first", method: "*", finality: finalized},
				{name: "second", method: "", finality: finalized},
			},
			want: "first",
		},
		{
			name: "two catch-all entries",
			execs: []testExec{
				{name: "first", method: "*"},
				{name: "second", method: ""},
			},
			want: "first",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := selectTestExec(tc.execs, "eth_getLogs", DataFinalityStateFinalized)
			require.True(t, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestSelectExecutor_ReturnsTheStoredValueNotAnAlias proves the selected value
// is the element the caller passed in, so a caller can compare identities.
func TestSelectExecutor_ReturnsPointerElementsIntact(t *testing.T) {
	a := &testExec{name: "a", method: "eth_getLogs"}
	b := &testExec{name: "b", method: "*"}

	got, ok := SelectExecutor(
		[]*testExec{b, a}, "eth_getLogs", DataFinalityStateFinalized,
		func(e *testExec) string { return e.method },
		func(e *testExec) []DataFinalityState { return e.finality },
	)

	require.True(t, ok)
	require.Same(t, a, got, "the specific entry must be returned by identity")
}

// TestSelectExecutor_NoMatchReturnsTheZeroValue pins what a caller sees when
// nothing matches: the zero value and false, never a stale earlier candidate.
func TestSelectExecutor_NoMatchReturnsTheZeroValue(t *testing.T) {
	got, ok := SelectExecutor(
		[]*testExec{{name: "a", method: "debug_traceCall"}},
		"eth_getLogs", DataFinalityStateFinalized,
		func(e *testExec) string { return e.method },
		func(e *testExec) []DataFinalityState { return e.finality },
	)

	require.False(t, ok)
	require.Nil(t, got)
}
