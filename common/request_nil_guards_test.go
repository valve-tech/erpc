package common

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A nil *NormalizedRequest is not a bug in these callers: the error and
// shutdown paths log, meter and inspect a request that was never built. Every
// guard below stands between that and a panic in the middle of an error handler.
func TestNormalizedRequest_NilReceiverAnswersInsteadOfPanicking(t *testing.T) {
	var r *NormalizedRequest

	assert.Nil(t, r.ExecState(), "no request means no execution counters")
	assert.Zero(t, r.CreditUnitsTotal(), "a request that never ran spent nothing")
	assert.Nil(t, r.CreditUnitsByVendor())
	assert.Nil(t, r.LastUpstream(), "nothing was ever tried")
	assert.Nil(t, r.Directives())

	// The setters must swallow the call rather than write through a nil pointer.
	assert.NotPanics(t, func() {
		r.SetUser(&User{Id: "u1"})
		r.SetDirectives(&RequestDirectives{})
		r.SetAllowClientDirectiveMatcher(func(string) bool { return true })
	})
}

// SetUser must ignore a nil user rather than store it. A later authentication
// step that hands over nil must not erase the identity an earlier step
// established — that identity is what rate limiting and billing key on.
func TestNormalizedRequest_SetUser_IgnoresANilUser(t *testing.T) {
	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))

	r.SetUser(nil)
	assert.Equal(t, "n/a", r.UserId(), "a nil user leaves the request unattributed")

	r.SetUser(&User{Id: "u1"})
	require.Equal(t, "u1", r.UserId(), "a real user lands")

	r.SetUser(nil)
	assert.Equal(t, "u1", r.UserId(), "a later nil user must not erase the identity already set")
}

// A cloned directive set must never alias the original, and cloning a nil set
// must produce an empty one rather than nil. Callers write to the clone.
func TestRequestDirectives_Clone(t *testing.T) {
	var nilDirectives *RequestDirectives
	clone := nilDirectives.Clone()
	require.NotNil(t, clone, "a nil directive set clones into an empty one, not into nil")
	assert.False(t, clone.RetryEmpty)

	original := &RequestDirectives{RetryEmpty: true, UseUpstream: "u1"}
	copied := original.Clone()
	require.NotSame(t, original, copied)
	assert.True(t, copied.RetryEmpty)
	assert.Equal(t, "u1", copied.UseUpstream)

	copied.RetryEmpty = false
	assert.True(t, original.RetryEmpty, "writing to the clone must not reach the original")
}

// isDirectiveAllowed gates client-supplied directives. With no matcher every
// directive is allowed, which is the default an operator gets; with a matcher
// an empty key is refused outright so a malformed header cannot match a
// permissive pattern.
func TestNormalizedRequest_IsDirectiveAllowed(t *testing.T) {
	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))

	assert.True(t, r.isDirectiveAllowed("x-erpc-retry-empty"),
		"without a matcher the operator has not restricted anything")
	assert.True(t, r.isDirectiveAllowed(""))

	var asked []string
	r.SetAllowClientDirectiveMatcher(func(key string) bool {
		asked = append(asked, key)
		return key == "x-erpc-retry-empty"
	})

	assert.True(t, r.isDirectiveAllowed("x-erpc-retry-empty"))
	assert.False(t, r.isDirectiveAllowed("x-erpc-use-upstream"))
	assert.False(t, r.isDirectiveAllowed(""), "an empty key must be refused without consulting the matcher")
	assert.Equal(t, []string{"x-erpc-retry-empty", "x-erpc-use-upstream"}, asked)
}

// The traced lock helpers exist so a stalled request lock shows up as a span.
// They must take the lock they name and emit only under detailed tracing.
func TestNormalizedRequest_LockWithTrace(t *testing.T) {
	t.Run("with detailed tracing the spans are recorded", func(t *testing.T) {
		h := newTracingHarness(t, true)
		r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))

		r.LockWithTrace(context.Background())
		r.Unlock()
		r.RLockWithTrace(context.Background())
		r.RUnlock()

		require.NotNil(t, h.endedNamed("Request.Lock"))
		require.NotNil(t, h.endedNamed("Request.RLock"))
		assert.Empty(t, h.startedButNotEnded())
	})

	t.Run("without detailed tracing the locks still work and emit nothing", func(t *testing.T) {
		h := newTracingHarness(t, false)
		r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))

		r.LockWithTrace(context.Background())
		r.Unlock()
		r.RLockWithTrace(context.Background())
		r.RUnlock()

		assert.Empty(t, h.ended())
	})
}
