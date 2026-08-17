package common

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enableZerologOutput lifts the global level that init_test.go pins to
// Disabled, so a test can read back what an error actually writes to the log.
// Callers must not run in parallel: the level is a package global in zerolog.
func enableZerologOutput(t *testing.T) {
	t.Helper()

	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })
}

// ---------------------------------------------------------------------------
// BaseError.WithRetryableTowardNetwork
// ---------------------------------------------------------------------------

// The promoted method must return the receiver itself. Callers chain it and
// keep the result, so returning anything else would drop the concrete type and
// with it the ErrorStatusCode and the errors.As identity.
func TestBaseError_WithRetryableTowardNetwork_ReturnsReceiver(t *testing.T) {
	t.Parallel()

	be := &BaseError{Code: "ErrSomething", Message: "boom"}
	got := be.WithRetryableTowardNetwork(false)

	assert.Same(t, be, got, "the method must give back the same error, not a copy")
	assert.Equal(t, false, be.Details["retryableTowardNetwork"])

	// Calling it again must flip the flag, not append a second one.
	got = be.WithRetryableTowardNetwork(true)
	assert.Same(t, be, got)
	assert.Equal(t, true, be.Details["retryableTowardNetwork"])
	assert.Len(t, be.Details, 1)
}

// WithPermanentMissingData must be safe on a nil receiver, because call sites
// chain it onto a constructor result.
func TestBaseError_WithPermanentMissingData_NilReceiver(t *testing.T) {
	t.Parallel()

	var be *BaseError
	assert.NotPanics(t, func() { assert.Nil(t, be.WithPermanentMissingData(true)) })

	live := &BaseError{Code: "ErrEndpointMissingData"}
	assert.Same(t, live, live.WithPermanentMissingData(true))
	assert.Equal(t, true, live.Details["permanentMissingData"])
}

// ---------------------------------------------------------------------------
// TaskFatalError
// ---------------------------------------------------------------------------

// The initializer logs this error's text. An empty or panicking Error() would
// leave the operator with no reason why startup stopped.
func TestTaskFatalError_Error(t *testing.T) {
	t.Parallel()

	var nilErr *TaskFatalError
	assert.Equal(t, "fatal task error", nilErr.Error(), "a nil receiver must still describe itself")
	assert.Equal(t, "fatal task error", (&TaskFatalError{}).Error(), "a nil cause must still describe itself")

	inner := errors.New("chain id mismatch")
	wrapped := NewTaskFatal(inner)
	assert.Equal(t, "chain id mismatch", wrapped.Error())
	assert.Equal(t, inner, errors.Unwrap(wrapped))
	assert.True(t, errors.Is(wrapped, inner), "errors.Is must see through the fatal marker")

	var tf interface{ IsTaskFatal() bool }
	require.True(t, errors.As(wrapped, &tf))
	assert.True(t, tf.IsTaskFatal())
}

// ---------------------------------------------------------------------------
// ErrUpstreamRequest response metadata
// ---------------------------------------------------------------------------

// The HTTP layer reads these four accessors to write the X-ERPC-* headers on an
// error response. Each must survive a details map that is nil or that holds the
// wrong type, and each must read its own key.
func TestErrUpstreamRequest_MetadataAccessors(t *testing.T) {
	t.Parallel()

	t.Run("populated", func(t *testing.T) {
		t.Parallel()

		err := NewErrUpstreamRequest(errors.New("boom"), NewFakeUpstream("rpc-alpha"),
			"evm:1", "eth_call", 250*time.Millisecond, 5, 3, 1)
		ure, ok := err.(*ErrUpstreamRequest)
		require.True(t, ok)

		assert.Equal(t, "rpc-alpha", ure.UpstreamId())
		assert.Equal(t, 5, ure.Attempts())
		assert.Equal(t, 3, ure.Retries())
		assert.Equal(t, 1, ure.Hedges())
		assert.False(t, ure.FromCache(), "an upstream error never came from cache")
		assert.False(t, ure.IsObjectNull())
	})

	t.Run("nil details", func(t *testing.T) {
		t.Parallel()

		ure := &ErrUpstreamRequest{BaseError: BaseError{Code: ErrCodeUpstreamRequest}}
		assert.Equal(t, "", ure.UpstreamId())
		assert.Equal(t, 0, ure.Attempts())
		assert.Equal(t, 0, ure.Retries())
		assert.Equal(t, 0, ure.Hedges())
	})

	t.Run("wrong types in details", func(t *testing.T) {
		t.Parallel()

		ure := &ErrUpstreamRequest{BaseError: BaseError{
			Code: ErrCodeUpstreamRequest,
			Details: map[string]interface{}{
				"upstreamId": 42,
				"attempts":   "five",
				"retries":    1.5,
				"hedges":     nil,
			},
		}}
		assert.Equal(t, "", ure.UpstreamId())
		assert.Equal(t, 0, ure.Attempts())
		assert.Equal(t, 0, ure.Retries())
		assert.Equal(t, 0, ure.Hedges())
	})

	t.Run("a nameless upstream leaves upstreamId out", func(t *testing.T) {
		t.Parallel()

		err := NewErrUpstreamRequest(errors.New("boom"), nil, "evm:1", "eth_call", time.Second, 1, 0, 0)
		ure := err.(*ErrUpstreamRequest)
		assert.Equal(t, "", ure.UpstreamId())
		_, present := ure.Details["upstreamId"]
		assert.False(t, present, "a nil upstream must not add an empty upstreamId key")
	})

	t.Run("IsObjectNull", func(t *testing.T) {
		t.Parallel()

		var nilErr *ErrUpstreamRequest
		assert.True(t, nilErr.IsObjectNull())
		assert.True(t, (&ErrUpstreamRequest{}).IsObjectNull(), "a codeless error is null")
	})
}

// ---------------------------------------------------------------------------
// ErrUpstreamsExhausted ordering and metadata
// ---------------------------------------------------------------------------

// causeSortKey is the tie-breaker that keeps the exhausted bundle in one order
// across runs. Every key shape it accepts is checked, because a fallthrough to
// the map key's address would reorder the bundle on every request.
func TestCauseSortKey(t *testing.T) {
	t.Parallel()

	plain := errors.New("plain failure")

	t.Run("an Upstream key gives its id", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "rpc-alpha", causeSortKey(NewFakeUpstream("rpc-alpha"), plain))
	})

	t.Run("a nil Upstream key falls through", func(t *testing.T) {
		t.Parallel()
		var up Upstream
		assert.Equal(t, "plain failure", causeSortKey(up, plain))
	})

	t.Run("a string key is used verbatim", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "rpc-beta", causeSortKey("rpc-beta", plain))
	})

	t.Run("an unusable key falls back to the error's upstream", func(t *testing.T) {
		t.Parallel()
		err := NewErrUpstreamRequest(plain, NewFakeUpstream("rpc-gamma"), "evm:1", "eth_call", 0, 1, 0, 0)
		assert.Equal(t, "rpc-gamma", causeSortKey(12345, err))
	})

	t.Run("an unusable key and no upstream falls back to the text", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "plain failure", causeSortKey(12345, plain))
	})
}

// orderCauses is the single source of determinism for the exhausted bundle. A
// retryable cause must sort ahead of a terminal one, so the representative the
// client sees is "not yet" rather than "gone forever".
func TestOrderCauses_RetryableFirstThenById(t *testing.T) {
	t.Parallel()

	assert.Nil(t, orderCauses(nil), "a nil map yields no causes")

	// An invalid request is rejected the same way by every upstream, so it is
	// marked non-retryable toward the network.
	terminalZ := NewErrInvalidRequest(errors.New("z terminal"))
	terminalA := NewErrInvalidRequest(errors.New("a terminal"))
	retryable := NewErrEndpointServerSideException(errors.New("retry me"), nil, 500)

	require.False(t, IsRetryableTowardNetwork(terminalZ), "fixture must be terminal")
	require.True(t, IsRetryableTowardNetwork(retryable), "fixture must be retryable")

	m := &sync.Map{}
	m.Store("upstream-z", terminalZ)
	m.Store("upstream-a", terminalA)
	m.Store("upstream-m", retryable)
	m.Store("upstream-nil", nil)          // dropped: not an error
	m.Store("upstream-str", "not an err") // dropped: not an error

	ordered := orderCauses(m)
	require.Len(t, ordered, 3, "non-error values must be dropped")
	assert.Same(t, retryable, ordered[0], "the retryable cause must come first")
	assert.Same(t, terminalA, ordered[1], "terminal causes then sort by upstream id")
	assert.Same(t, terminalZ, ordered[2])

	// Same input, same order — run it again to prove the ordering is not
	// inheriting sync.Map iteration order.
	for i := 0; i < 20; i++ {
		again := orderCauses(m)
		require.Equal(t, ordered, again, "orderCauses must be stable across runs")
	}
}

// Two causes that yield the SAME sort key must still get one canonical order,
// decided by their text. Two distinct Upstream values sharing an id is the
// shape that produces the tie in practice.
//
// The fixture carries no Details map on purpose. BaseError.Error() renders
// Details through Sonic with SortMapKeys disabled, so an error with two or more
// details produces a different string on each call and this tie-break stops
// being deterministic. See the upstream bug note.
func TestOrderCauses_TiedKeysFallBackToText(t *testing.T) {
	t.Parallel()

	// Six causes, all under the same upstream id, stored in reverse text order.
	// Without the text tie-break the result is sync.Map iteration order, which
	// matches the wanted order only 1 time in 720 — and never twice running.
	m := &sync.Map{}
	var want []error
	for _, msg := range []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff"} {
		want = append(want, &BaseError{Code: "ErrTied", Message: msg})
	}
	for i := len(want) - 1; i >= 0; i-- {
		up := NewFakeUpstream("rpc-alpha")
		require.Equal(t, "rpc-alpha", causeSortKey(up, want[i]), "the fixture must tie on the key")
		m.Store(up, want[i])
	}

	for i := 0; i < 20; i++ {
		assert.Equal(t, want, orderCauses(m),
			"tied keys must break on the error text, the same way every time")
	}
}

// Upstreams() lifts the upstream out of every cause that carries one, and skips
// the causes that do not.
func TestErrUpstreamsExhausted_Upstreams(t *testing.T) {
	t.Parallel()

	var nilErr *ErrUpstreamsExhausted
	assert.Nil(t, nilErr.Upstreams())

	m := &sync.Map{}
	m.Store("rpc-alpha", NewErrUpstreamRequest(errors.New("a"), NewFakeUpstream("rpc-alpha"), "evm:1", "eth_call", 0, 1, 0, 0))
	m.Store("rpc-beta", errors.New("no upstream on this one"))

	err := NewErrUpstreamsExhausted(nil, m, "prj", "evm:1", "eth_call", time.Second, 2, 1, 0, 2)
	exh := err.(*ErrUpstreamsExhausted)

	ups := exh.Upstreams()
	require.Len(t, ups, 1, "only causes that carry an upstream may contribute")
	assert.Equal(t, "rpc-alpha", ups[0].Id())
}

// The exhausted bundle exposes the same metadata the HTTP layer needs, and it
// keeps a handle on the originating request.
func TestErrUpstreamsExhausted_MetadataAccessors(t *testing.T) {
	t.Parallel()

	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))
	m := &sync.Map{}
	m.Store("rpc-alpha", NewErrUpstreamRequest(errors.New("a"), NewFakeUpstream("rpc-alpha"), "evm:1", "eth_call", 0, 1, 0, 0))

	err := NewErrUpstreamsExhausted(req, m, "prj", "evm:1", "eth_call", time.Second, 7, 6, 5, 2)
	exh := err.(*ErrUpstreamsExhausted)

	assert.Same(t, req, exh.Request())
	assert.Equal(t, 7, exh.Attempts())
	assert.Equal(t, 6, exh.Retries())
	assert.Equal(t, 5, exh.Hedges())
	assert.False(t, exh.FromCache())
	assert.False(t, exh.IsObjectNull())
	assert.Equal(t, "rpc-alpha", exh.UpstreamId(), "UpstreamId must dig the id out of a nested cause")

	empty := &ErrUpstreamsExhausted{}
	assert.Equal(t, 0, empty.Attempts())
	assert.Equal(t, 0, empty.Retries())
	assert.Equal(t, 0, empty.Hedges())
	assert.Equal(t, "", empty.UpstreamId())
	assert.True(t, empty.IsObjectNull())

	// A details map that survived a JSON round trip holds float64, not int.
	// The accessors must report zero rather than a wrong number.
	roundTripped := &ErrUpstreamsExhausted{BaseError: BaseError{
		Code: ErrCodeUpstreamsExhausted,
		Details: map[string]interface{}{
			"attempts": 7.0, "retries": 6.0, "hedges": 5.0, "upstreamId": 42,
		},
	}}
	assert.Equal(t, 0, roundTripped.Attempts())
	assert.Equal(t, 0, roundTripped.Retries())
	assert.Equal(t, 0, roundTripped.Hedges())
	assert.Equal(t, "", roundTripped.UpstreamId())

	var nilExh *ErrUpstreamsExhausted
	assert.True(t, nilExh.IsObjectNull())
	assert.Nil(t, nilExh.Errors())
}

// Errors() must return the joined children, and nothing when the cause is not a
// join.
func TestErrUpstreamsExhausted_Errors(t *testing.T) {
	t.Parallel()

	single := NewErrUpstreamsExhaustedWithCause(errors.New("just one")).(*ErrUpstreamsExhausted)
	assert.Nil(t, single.Errors(), "a non-joined cause has no children to list")

	joined := &ErrUpstreamsExhausted{BaseError: BaseError{
		Code:  ErrCodeUpstreamsExhausted,
		Cause: errors.Join(errors.New("a"), errors.New("b")),
	}}
	assert.Len(t, joined.Errors(), 2)

	assert.Nil(t, (&ErrUpstreamsExhausted{}).Errors(), "a causeless bundle has no children")
}

// DeepestMessage falls through to the single child when the causes do not
// summarize. The operator reads this string in the log line.
func TestErrUpstreamsExhausted_DeepestMessage(t *testing.T) {
	t.Parallel()

	t.Run("no cause returns the message", func(t *testing.T) {
		t.Parallel()
		e := &ErrUpstreamsExhausted{BaseError: BaseError{Code: ErrCodeUpstreamsExhausted, Message: "all upstream attempts failed"}}
		assert.Equal(t, "all upstream attempts failed", e.DeepestMessage())
	})

	t.Run("a recognised cause summarizes", func(t *testing.T) {
		t.Parallel()
		e := &ErrUpstreamsExhausted{BaseError: BaseError{
			Code:  ErrCodeUpstreamsExhausted,
			Cause: errors.Join(NewErrEndpointUnsupported(errors.New("nope"))),
		}}
		assert.Equal(t, e.SummarizeCauses(), e.DeepestMessage())
		assert.NotEmpty(t, e.DeepestMessage())
	})

	// SummarizeCauses buckets an unrecognised child as "unknown", so ANY joined
	// cause summarizes to a non-empty string. DeepestMessage therefore always
	// returns the summary and never reaches its own unwrap branch. These two
	// cases pin that: they are the shapes a reader would expect to unwrap.
	t.Run("one unrecognised StandardError child still summarizes", func(t *testing.T) {
		t.Parallel()
		child := &BaseError{Code: "ErrSomethingOdd", Message: "the deepest note"}
		e := &ErrUpstreamsExhausted{BaseError: BaseError{
			Code:  ErrCodeUpstreamsExhausted,
			Cause: errors.Join(child),
		}}
		assert.Equal(t, "1 upstream unknown errors", e.DeepestMessage())
		assert.NotEqual(t, "the deepest note", e.DeepestMessage(),
			"the child's own message never reaches the operator")
	})

	t.Run("several unrecognised children summarize as a count", func(t *testing.T) {
		t.Parallel()
		e := &ErrUpstreamsExhausted{BaseError: BaseError{
			Code:  ErrCodeUpstreamsExhausted,
			Cause: errors.Join(errors.New("a"), errors.New("b")),
		}}
		assert.Equal(t, "2 upstream unknown errors", e.DeepestMessage())
	})

	t.Run("a non-joined cause gives nothing", func(t *testing.T) {
		t.Parallel()
		e := NewErrUpstreamsExhaustedWithCause(errors.New("solo")).(*ErrUpstreamsExhausted)
		require.Equal(t, "", e.SummarizeCauses(), "a non-joined cause cannot be summarized")
		assert.Equal(t, "", e.DeepestMessage(),
			"a non-joined cause leaves the operator with an empty deepest message")
	})
}

// CodeChain on the exhausted bundle returns the bare code on every path — it
// never appends the cause codes the way BaseError.CodeChain does. This test
// pins today's behaviour; see the upstream bug note.
func TestErrUpstreamsExhausted_CodeChain(t *testing.T) {
	t.Parallel()

	bare := &ErrUpstreamsExhausted{BaseError: BaseError{Code: ErrCodeUpstreamsExhausted}}
	assert.Equal(t, string(ErrCodeUpstreamsExhausted), bare.CodeChain())

	summarizable := &ErrUpstreamsExhausted{BaseError: BaseError{
		Code:  ErrCodeUpstreamsExhausted,
		Cause: errors.Join(NewErrEndpointUnsupported(errors.New("nope"))),
	}}
	require.NotEmpty(t, summarizable.SummarizeCauses())
	assert.Equal(t, string(ErrCodeUpstreamsExhausted), summarizable.CodeChain())

	unsummarizable := NewErrUpstreamsExhaustedWithCause(
		NewErrEndpointUnsupported(errors.New("nope"))).(*ErrUpstreamsExhausted)
	require.Empty(t, unsummarizable.SummarizeCauses(), "a non-joined cause cannot be summarized")
	assert.Equal(t, string(ErrCodeUpstreamsExhausted), unsummarizable.CodeChain(),
		"today every branch returns the bare code; the cause codes are dropped")

	// BaseError.CodeChain on the same cause DOES append the cause code, which
	// is the behaviour this override silently drops.
	assert.Equal(t, string(ErrCodeUpstreamsExhausted)+" <- "+ErrCodeEndpointUnsupported,
		unsummarizable.BaseError.CodeChain())
}

// ---------------------------------------------------------------------------
// Failsafe DeepestMessage
// ---------------------------------------------------------------------------

// The three failsafe wrappers each prefix their own message onto the cause, so
// the operator sees both "the timeout fired" and "why the attempt failed".
func TestFailsafeErrors_DeepestMessage(t *testing.T) {
	t.Parallel()

	start := time.Now()
	standardCause := NewErrEndpointServerSideException(errors.New("upstream 503"), nil, 503)
	plainCause := errors.New("dial tcp: connection refused")

	for _, tc := range []struct {
		name string
		make func(cause error) error
	}{
		{"timeout exceeded", func(cause error) error {
			return NewErrFailsafeTimeoutExceeded(ScopeUpstream, cause, &start)
		}},
		{"retry exceeded", func(cause error) error {
			return NewErrFailsafeRetryExceeded(ScopeUpstream, cause, &start)
		}},
		{"circuit breaker open", func(cause error) error {
			return NewErrFailsafeCircuitBreakerOpen(ScopeUpstream, cause, &start)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			withStandard := tc.make(standardCause).(StandardError)
			own := withStandard.Base().Message
			assert.Equal(t,
				fmt.Sprintf("%s: %s", own, standardCause.(StandardError).DeepestMessage()),
				withStandard.DeepestMessage(),
				"a StandardError cause must be unwrapped to its own deepest message")

			withPlain := tc.make(plainCause).(StandardError)
			assert.Contains(t, withPlain.DeepestMessage(), withPlain.Base().Message)
			assert.Contains(t, withPlain.DeepestMessage(), "dial tcp: connection refused")

			noCause := tc.make(nil).(StandardError)
			assert.Equal(t, noCause.Base().Message, noCause.DeepestMessage(),
				"with no cause the wrapper's own message is the deepest one")
		})
	}
}

// A nil start time must not fabricate a duration in the message.
func TestFailsafeErrors_NilStartTimeOmitsDuration(t *testing.T) {
	t.Parallel()

	for name, err := range map[string]error{
		"timeout":         NewErrFailsafeTimeoutExceeded(ScopeNetwork, nil, nil),
		"retry":           NewErrFailsafeRetryExceeded(ScopeNetwork, nil, nil),
		"circuit breaker": NewErrFailsafeCircuitBreakerOpen(ScopeNetwork, nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			se := err.(StandardError)
			assert.NotContains(t, se.Base().Message, " after ",
				"no start time means no duration in the message")
		})
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC exceptions
// ---------------------------------------------------------------------------

// The internal exception surfaces the upstream's own numeric code. It arrives
// as an int in-process and as a float64 after a JSON round trip, so both must
// read back as the same int.
func TestErrJsonRpcExceptionInternal_OriginalCode(t *testing.T) {
	t.Parallel()

	fromInt := NewErrJsonRpcExceptionInternal(-32000, JsonRpcErrorServerSideException, "boom", nil, nil)
	assert.Equal(t, -32000, fromInt.OriginalCode())
	assert.Equal(t, JsonRpcErrorServerSideException, fromInt.NormalizedCode())

	fromFloat := &ErrJsonRpcExceptionInternal{BaseError{
		Code:    ErrCodeJsonRpcExceptionInternal,
		Details: map[string]interface{}{"originalCode": float64(-32602)},
	}}
	assert.Equal(t, -32602, fromFloat.OriginalCode(),
		"a JSON round trip turns the code into a float64 and must still read back")

	fromJunk := &ErrJsonRpcExceptionInternal{BaseError{
		Code:    ErrCodeJsonRpcExceptionInternal,
		Details: map[string]interface{}{"originalCode": "-32000"},
	}}
	assert.Equal(t, 0, fromJunk.OriginalCode())

	absent := NewErrJsonRpcExceptionInternal(0, 0, "boom", nil, nil)
	assert.Equal(t, 0, absent.OriginalCode())
	assert.Equal(t, JsonRpcErrorNumber(0), absent.NormalizedCode())
	_, hasOriginal := absent.Details["originalCode"]
	assert.False(t, hasOriginal, "a zero code must not be written into details")

	assert.Equal(t,
		fmt.Sprintf("%d <- %s", fromInt.NormalizedCode(), fromInt.BaseError.CodeChain()),
		fromInt.CodeChain(),
		"the chain must lead with the normalized number so an operator can filter on it")
}

// The external exception is the shape eRPC writes on the wire. It carries no
// cause, and every accessor must say so rather than reaching into a nil.
func TestErrJsonRpcExceptionExternal(t *testing.T) {
	t.Parallel()

	e := NewErrJsonRpcExceptionExternal(-32000, "execution reverted", "0xdeadbeef")

	assert.Equal(t, "-32000: execution reverted", e.Error())
	assert.Equal(t, "-32000", e.CodeChain())
	assert.Equal(t, "execution reverted", e.DeepestMessage())
	assert.Nil(t, e.GetCause(), "an upstream-authored error has no eRPC cause")
	assert.False(t, e.HasCode(ErrCodeJsonRpcExceptionInternal))
	assert.False(t, e.HasCode(ErrCodeInvalidRequest))
	assert.Equal(t, "0xdeadbeef", e.Data)
}

// MarshalZerologObject decides what an operator reads in the log line. Every
// field the struct carries must appear, and a nil receiver must not panic.
func TestErrJsonRpcExceptionExternal_MarshalZerologObject(t *testing.T) {
	enableZerologOutput(t)

	assert.NotPanics(t, func() {
		var nilErr *ErrJsonRpcExceptionExternal
		nilErr.MarshalZerologObject(zerolog.Dict())
	})

	var buf strings.Builder
	logger := zerolog.New(&buf)

	logger.Error().Object("err",
		NewErrJsonRpcExceptionExternal(-32000, "execution reverted", "0xdeadbeef")).Msg("failed")
	line := buf.String()
	assert.Contains(t, line, `"code":-32000`, "the numeric code must be logged as a number")
	assert.Contains(t, line, `"message":"execution reverted"`)
	assert.Contains(t, line, `"data":"0xdeadbeef"`)

	// The encoder guards on `e.Data != nil`, but the constructor always stores a
	// string, so the interface is never nil and an empty data field is logged
	// as "". Pinned as today's behaviour; see the upstream bug note.
	buf.Reset()
	logger.Error().Object("err", NewErrJsonRpcExceptionExternal(-32601, "method not found", "")).Msg("failed")
	assert.Contains(t, buf.String(), `"data":""`)

	// A genuinely nil Data does drop the field.
	buf.Reset()
	logger.Error().Object("err", &ErrJsonRpcExceptionExternal{Code: -32601, Message: "method not found"}).Msg("failed")
	assert.NotContains(t, buf.String(), `"data"`)
}

// ---------------------------------------------------------------------------
// Consensus errors
// ---------------------------------------------------------------------------

// All three consensus errors expose their per-upstream causes the same way. A
// nil cause or a non-joined cause must give nothing rather than panic.
func TestConsensusErrors_Errors(t *testing.T) {
	t.Parallel()

	participants := []ParticipantInfo{{Upstream: "rpc-alpha", ResultHash: "0xaaa"}}
	causeA := errors.New("cause a")
	causeB := errors.New("cause b")

	for name, ctor := range map[string]func(string, []ParticipantInfo, []error) error{
		"dispute":             NewErrConsensusDispute,
		"composition dispute": NewErrConsensusCompositionDispute,
		"low participants":    NewErrConsensusLowParticipants,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			withCauses := ctor("not enough agreement", participants, []error{causeA, causeB})
			lister, ok := withCauses.(interface{ Errors() []error })
			require.True(t, ok)
			assert.Equal(t, []error{causeA, causeB}, lister.Errors())

			noCauses := ctor("not enough agreement", participants, nil)
			noneLister := noCauses.(interface{ Errors() []error })
			assert.Nil(t, noneLister.Errors(), "errors.Join of nothing leaves no cause")

			se := withCauses.(StandardError)
			assert.Equal(t, participants, se.Base().Details["participants"])
		})
	}
}

// A cause that is not a join must not be reported as a list of children.
func TestConsensusErrors_NonJoinedCause(t *testing.T) {
	t.Parallel()

	dispute := &ErrConsensusDispute{BaseError{Code: ErrCodeConsensusDispute, Cause: errors.New("solo")}}
	assert.Nil(t, dispute.Errors())

	composition := &ErrConsensusCompositionDispute{BaseError{Code: ErrCodeConsensusCompositionDispute, Cause: errors.New("solo")}}
	assert.Nil(t, composition.Errors())

	low := &ErrConsensusLowParticipants{BaseError{Code: ErrCodeConsensusLowParticipants, Cause: errors.New("solo")}}
	assert.Nil(t, low.Errors())
}

// SummarizeParticipants is the line an operator reads to see who agreed with
// whom. Every participant shape must render, and each must render distinctly.
func TestErrConsensusLowParticipants_SummarizeParticipants(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		participants interface{}
		want         string
	}{
		{name: "nil details", participants: nil, want: ""},
		{
			name:         "details hold the wrong type",
			participants: []string{"rpc-alpha"},
			want:         "",
		},
		{
			name:         "no participants at all",
			participants: []ParticipantInfo{},
			want:         "[]",
		},
		{
			name:         "an upstream with a result hash",
			participants: []ParticipantInfo{{Upstream: "rpc-alpha", ResultHash: "0xaaa"}},
			want:         "[rpc-alpha = 0xaaa]",
		},
		{
			name:         "an upstream with no result",
			participants: []ParticipantInfo{{Upstream: "rpc-beta"}},
			want:         "[rpc-beta = NoResult]",
		},
		{
			name:         "an upstream that errored",
			participants: []ParticipantInfo{{Upstream: "rpc-gamma", ErrSummary: "ErrEndpointRequestTimeout"}},
			want:         "[rpc-gamma = ErrEndpointRequestTimeout]",
		},
		{
			name:         "an error with no upstream name",
			participants: []ParticipantInfo{{ErrSummary: "ErrNetworkRequestTimeout"}},
			want:         "[ErrNetworkRequestTimeout]",
		},
		{
			name:         "a nameless participant with no error is skipped",
			participants: []ParticipantInfo{{ResultHash: "0xaaa"}},
			want:         "[]",
		},
		{
			name: "several participants join with a comma",
			participants: []ParticipantInfo{
				{Upstream: "rpc-alpha", ResultHash: "0xaaa"},
				{Upstream: "rpc-beta", ResultHash: "0xbbb"},
			},
			want: "[rpc-alpha = 0xaaa, rpc-beta = 0xbbb]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e := &ErrConsensusLowParticipants{BaseError{
				Code:    ErrCodeConsensusLowParticipants,
				Message: "not enough participants",
			}}
			if tc.participants != nil {
				e.Details = map[string]interface{}{"participants": tc.participants}
			}
			assert.Equal(t, tc.want, e.SummarizeParticipants())
		})
	}
}

// DeepestMessage stitches the code, the message and the participant summary
// into the one string the operator reads.
func TestErrConsensusLowParticipants_DeepestMessage(t *testing.T) {
	t.Parallel()

	e := NewErrConsensusLowParticipants("only 1 of 3 responded",
		[]ParticipantInfo{{Upstream: "rpc-alpha", ResultHash: "0xaaa"}}, nil).(*ErrConsensusLowParticipants)

	assert.Equal(t,
		fmt.Sprintf("%s: %s: %s", ErrCodeConsensusLowParticipants, "only 1 of 3 responded", "[rpc-alpha = 0xaaa]"),
		e.DeepestMessage())
}

// ---------------------------------------------------------------------------
// BaseError serialization
// ---------------------------------------------------------------------------

// MarshalJSON has one branch per cause shape. The HTTP layer writes the result
// straight to the client, so a branch that drops the cause hides the reason.
func TestBaseError_MarshalJSON_CauseShapes(t *testing.T) {
	t.Parallel()

	t.Run("a joined cause becomes an array", func(t *testing.T) {
		t.Parallel()

		e := BaseError{
			Code:    ErrCodeUpstreamsExhausted,
			Message: "all upstream attempts failed",
			Cause: errors.Join(
				NewErrEndpointUnsupported(errors.New("nope")),
				errors.New("a bare failure"),
			),
		}
		out, err := e.MarshalJSON()
		require.NoError(t, err)

		s := string(out)
		assert.Contains(t, s, `"cause":[`, "joined causes must serialize as an array")
		assert.Contains(t, s, "ErrEndpointUnsupported", "a StandardError child keeps its structure")
		assert.Contains(t, s, "a bare failure", "a plain child is reduced to its text")
	})

	t.Run("a plain cause is wrapped and collapses to its text", func(t *testing.T) {
		t.Parallel()

		// The plain cause is wrapped in BaseError{Code:"ErrGeneric"}, whose own
		// MarshalJSON collapses an ErrGeneric with no cause down to the bare
		// message. The client therefore reads the text, not an object.
		e := BaseError{Code: "ErrSomething", Message: "outer", Cause: errors.New("inner detail")}
		out, err := e.MarshalJSON()
		require.NoError(t, err)

		assert.JSONEq(t, `{"code":"ErrSomething","message":"outer","cause":"inner detail"}`, string(out))
	})

	t.Run("a StandardError cause keeps its own shape", func(t *testing.T) {
		t.Parallel()

		e := BaseError{Code: "ErrSomething", Message: "outer",
			Cause: NewErrEndpointUnsupported(errors.New("nope"))}
		out, err := e.MarshalJSON()
		require.NoError(t, err)
		assert.Contains(t, string(out), "ErrEndpointUnsupported")
	})

	t.Run("no cause omits the field", func(t *testing.T) {
		t.Parallel()

		e := BaseError{Code: "ErrSomething", Message: "outer"}
		out, err := e.MarshalJSON()
		require.NoError(t, err)
		assert.NotContains(t, string(out), `"cause"`)
	})

	t.Run("an unknown code with a cause carries the cause text", func(t *testing.T) {
		t.Parallel()

		for _, code := range []ErrorCode{"", "ErrUnknown", "ErrGeneric"} {
			e := BaseError{Code: code, Message: "outer", Cause: errors.New("inner detail")}
			out, err := e.MarshalJSON()
			require.NoError(t, err)
			assert.Contains(t, string(out), "inner detail")
		}
	})

	t.Run("an unknown code with no cause collapses to the message", func(t *testing.T) {
		t.Parallel()

		e := BaseError{Code: "ErrUnknown", Message: "just a message"}
		out, err := e.MarshalJSON()
		require.NoError(t, err)
		assert.JSONEq(t, `"just a message"`, string(out))
	})
}

// The zerolog encoder decides the shape of the error in the log. A joined cause
// must stay a list, so a bundle does not collapse to one child.
func TestBaseError_MarshalZerologObject(t *testing.T) {
	enableZerologOutput(t)

	assert.NotPanics(t, func() {
		var nilErr *BaseError
		nilErr.MarshalZerologObject(zerolog.Dict())
	})

	logLine := func(e *BaseError) string {
		var buf strings.Builder
		logger := zerolog.New(&buf)
		logger.Error().Object("err", e).Msg("failed")
		return buf.String()
	}

	// A joined cause goes through v.Interface, which JSON-encodes the []error.
	// A plain error has no exported fields, so every child collapses to {} and
	// its text never reaches the log. Pinned as today's behaviour; see the
	// upstream bug note.
	joined := logLine(&BaseError{
		Code:    ErrCodeUpstreamsExhausted,
		Message: "all upstream attempts failed",
		Cause:   errors.Join(errors.New("child a"), errors.New("child b")),
		Details: map[string]interface{}{"upstreams": 2},
	})
	assert.Contains(t, joined, `"code":"ErrUpstreamsExhausted"`)
	assert.Contains(t, joined, `"cause":[{},{}]`)
	assert.NotContains(t, joined, "child a", "the child's text is lost from the log today")
	assert.Contains(t, joined, `"details"`)

	// A StandardError child survives, because it carries its own marshaller.
	joinedStandard := logLine(&BaseError{
		Code:    ErrCodeUpstreamsExhausted,
		Message: "all upstream attempts failed",
		Cause:   errors.Join(NewErrEndpointUnsupported(errors.New("nope"))),
	})
	assert.Contains(t, joinedStandard, "ErrEndpointUnsupported")

	standard := logLine(&BaseError{
		Code:    "ErrOuter",
		Message: "outer",
		Cause:   NewErrEndpointUnsupported(errors.New("nope")),
	})
	assert.Contains(t, standard, "ErrEndpointUnsupported")

	plain := logLine(&BaseError{Code: "ErrOuter", Message: "outer", Cause: errors.New("bare cause")})
	assert.Contains(t, plain, `"cause":"bare cause"`, "a plain cause is logged as a string")

	bare := logLine(&BaseError{Code: "ErrOuter", Message: "outer"})
	assert.NotContains(t, bare, `"cause"`)
	assert.NotContains(t, bare, `"details"`)
}

// Error() renders the code, the message, the details and the cause. Details are
// only shown when there are any, so the common case stays readable.
func TestBaseError_Error_Rendering(t *testing.T) {
	t.Parallel()

	bare := &BaseError{Code: "ErrOuter", Message: "outer"}
	assert.Equal(t, "ErrOuter: outer", bare.Error())

	withDetails := &BaseError{Code: "ErrOuter", Message: "outer",
		Details: map[string]interface{}{"upstreamId": "rpc-alpha"}}
	assert.Equal(t, `ErrOuter: outer -> ({"upstreamId":"rpc-alpha"})`, withDetails.Error())

	withCause := &BaseError{Code: "ErrOuter", Message: "outer", Cause: errors.New("inner")}
	assert.Equal(t, "ErrOuter: outer \ncaused by: inner", withCause.Error())

	withBoth := &BaseError{Code: "ErrOuter", Message: "outer",
		Details: map[string]interface{}{"upstreamId": "rpc-alpha"}, Cause: errors.New("inner")}
	assert.Equal(t, `ErrOuter: outer -> ({"upstreamId":"rpc-alpha"}) `+"\ncaused by: inner", withBoth.Error())

	// An unmarshalable detail must still be rendered, not dropped.
	unmarshalable := &BaseError{Code: "ErrOuter", Message: "outer",
		Details: map[string]interface{}{"fn": func() {}}}
	assert.Contains(t, unmarshalable.Error(), "ErrOuter: outer -> (")
}

// ---------------------------------------------------------------------------
// ErrorSummary
// ---------------------------------------------------------------------------

// ErrorSummary labels metrics. In compact mode it must produce a bounded set of
// labels for every argument shape, including the ones that are not errors.
func TestErrorSummary_NonErrorArguments(t *testing.T) {
	for _, tc := range []struct {
		name        string
		arg         interface{}
		wantCompact string
		wantVerbose string
	}{
		{name: "a bare string", arg: "something went wrong",
			wantCompact: "StringError", wantVerbose: "something went wrong"},
		{name: "an integer", arg: 42, wantCompact: "UnknownError", wantVerbose: "XX"},
		{name: "a struct", arg: struct{ A int }{7},
			wantCompact: "UnknownError", wantVerbose: "{7}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetErrorLabelMode(ErrorLabelModeCompact)
			t.Cleanup(func() { SetErrorLabelMode(ErrorLabelModeVerbose) })

			assert.Equal(t, tc.wantCompact, ErrorSummary(tc.arg),
				"compact mode must map an unknown argument to a bounded label")

			SetErrorLabelMode(ErrorLabelModeVerbose)
			assert.Equal(t, tc.wantVerbose, ErrorSummary(tc.arg))
		})
	}
}

// A join of errors summarizes as the code chains, and a join of one collapses
// to that one error's summary.
func TestErrorSummary_JoinedErrors(t *testing.T) {
	t.Parallel()

	single := errors.Join(NewErrEndpointUnsupported(errors.New("nope")))
	assert.Equal(t, ErrorSummary(NewErrEndpointUnsupported(errors.New("nope"))), ErrorSummary(single),
		"a join of one must read like the one error")

	multi := errors.Join(
		NewErrEndpointUnsupported(errors.New("nope")),
		errors.New("a bare failure"),
	)
	summary := ErrorSummary(multi)
	assert.Contains(t, summary, "ErrEndpointUnsupported")
	assert.Contains(t, summary, "a bare failure")
	assert.Contains(t, summary, ", ", "several causes are joined with a comma")

	assert.Equal(t, "", ErrorSummary(nil))
}

// emptyJoin implements the multi-error shape with no children, which is what a
// caller sees when every cause was dropped.
type emptyJoin struct{}

func (emptyJoin) Error() string   { return "empty join" }
func (emptyJoin) Unwrap() []error { return nil }

func TestErrorSummary_EmptyJoinIsLabelled(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "UnknownMultipleErrors", ErrorSummary(emptyJoin{}),
		"a join with no children must still produce a bounded metric label")
}

// Compact mode folds a JSON-RPC cause down to its normalized number, which is
// the low-cardinality identity an operator groups by.
func TestErrorSummary_CompactFoldsJsonRpcCause(t *testing.T) {
	SetErrorLabelMode(ErrorLabelModeCompact)
	t.Cleanup(func() { SetErrorLabelMode(ErrorLabelModeVerbose) })

	jrpcCause := NewErrJsonRpcExceptionInternal(
		-32000, JsonRpcErrorServerSideException, "boom", nil, nil)
	wrapped := NewErrUpstreamRequest(jrpcCause, NewFakeUpstream("rpc-alpha"),
		"evm:1", "eth_call", 0, 1, 0, 0)

	assert.Equal(t,
		fmt.Sprintf("%s/%d", ErrCodeUpstreamRequest, JsonRpcErrorServerSideException),
		ErrorSummary(wrapped),
		"the label must carry the numeric JSON-RPC code, not the eRPC wrapper code")

	// A non-JSON-RPC cause folds to the cause's own code instead.
	plainCause := NewErrEndpointUnsupported(errors.New("nope"))
	assert.Equal(t,
		string(ErrCodeUpstreamRequest)+"/"+ErrCodeEndpointUnsupported,
		ErrorSummary(NewErrUpstreamRequest(plainCause, NewFakeUpstream("rpc-alpha"),
			"evm:1", "eth_call", 0, 1, 0, 0)))
}

// Compact mode is the mode that guards metric cardinality. A context error must
// map to a fixed label instead of the driver's own text.
func TestErrorSummary_CompactContextErrors(t *testing.T) {
	SetErrorLabelMode(ErrorLabelModeCompact)
	t.Cleanup(func() { SetErrorLabelMode(ErrorLabelModeVerbose) })

	assert.Equal(t, "ContextDeadlineExceeded", ErrorSummary(context.DeadlineExceeded))
	assert.Equal(t, "ContextCanceled", ErrorSummary(context.Canceled))
	assert.Equal(t, "GenericError", ErrorSummary(errors.New("some driver text 12345")))
}
