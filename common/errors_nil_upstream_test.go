package common

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every upstream-aware error constructor must survive a nil upstream.
//
// An error constructor runs on the path that is ALREADY going wrong. A panic
// there replaces the diagnosis of the original fault with a stack trace about
// the reporting of it, so the one thing the operator needed is the one thing
// they lose.
//
// NewErrUpstreamMalformedResponse called upstream.Id() unguarded while its two
// siblings guarded. The table drives all three, so the next constructor added
// beside them is measured against the same rule rather than against a habit.
func TestUpstreamAwareErrorConstructors_ANilUpstreamIsNotAPanic(t *testing.T) {
	constructors := map[string]func(error, Upstream) error{
		"NewErrUpstreamMalformedResponse": NewErrUpstreamMalformedResponse,
		"NewErrEndpointMissingData":       NewErrEndpointMissingData,
		"NewErrEndpointContentValidation": NewErrEndpointContentValidation,
	}

	cause := errors.New("the fault worth reporting")

	for name, construct := range constructors {
		t.Run(name, func(t *testing.T) {
			var err error
			require.NotPanics(t, func() { err = construct(cause, nil) },
				"building an error about a fault must not become a second fault")
			require.Error(t, err)

			// The cause is the whole point: it is what the caller was trying
			// to report when the upstream turned out to be unknown.
			require.ErrorIs(t, err, cause,
				"the original cause must survive the missing upstream")

			// No upstreamId key at all, rather than a key holding nothing.
			// An absent upstream and an upstream with an empty id are
			// different events.
			std, ok := err.(StandardError)
			require.True(t, ok, "constructor did not build a StandardError")
			_, named := std.Base().Details["upstreamId"]
			require.False(t, named,
				"an unknown upstream must leave the key out, not name it as empty")
		})
	}
}
