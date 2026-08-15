package common

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// HasErrorCode answers one question for 150+ call sites: does this error carry
// one of these codes anywhere in its chain? Callers pair it with errors.As —
// see architecture/evm/eth_sendRawTransaction.go, which gates the idempotency
// path on HasErrorCode and then reads the details with errors.As. The two must
// agree on what "in the chain" means, because errors.As follows a plain
// fmt.Errorf("%w") link. A gate that stops at that link sends a transaction
// that is already in the mempool out a second time.
//
// The tests below fix the traversal contract: every link a caller can build,
// in every order, and no false positive on an unrelated code.

func TestHasErrorCode_FollowsAPlainWrapChain(t *testing.T) {
	nonce := NewErrEndpointNonceException(errors.New("already known"), NonceExceptionReasonAlreadyKnown)

	t.Run("SingleWrap", func(t *testing.T) {
		wrapped := fmt.Errorf("while broadcasting: %w", nonce)

		require.True(t, HasErrorCode(wrapped, ErrCodeEndpointNonceException),
			"a %%w link must not hide the code that errors.As can still find")
		var typed *ErrEndpointNonceException
		require.True(t, errors.As(wrapped, &typed),
			"control: errors.As already walks this chain, so the gate must too")
	})

	t.Run("NestedWraps", func(t *testing.T) {
		wrapped := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", nonce))

		require.True(t, HasErrorCode(wrapped, ErrCodeEndpointNonceException))
	})

	t.Run("PlainLinkInsideAnErpcCause", func(t *testing.T) {
		// The same hole one level down: an eRPC error whose Cause is a plain
		// wrapper. BaseError.HasCode stops at the wrapper because it is not a
		// StandardError, so the walk has to continue past it.
		outer := &ErrEndpointExecutionException{BaseError{
			Code:    ErrCodeEndpointExecutionException,
			Message: "execution failed",
			Cause:   fmt.Errorf("transport: %w", nonce),
		}}

		require.True(t, HasErrorCode(outer, ErrCodeEndpointNonceException))
		require.True(t, HasErrorCode(outer, ErrCodeEndpointExecutionException),
			"the outer code must keep matching")
	})

	t.Run("PlainWrapAroundAJoin", func(t *testing.T) {
		joined := errors.Join(
			errors.New("unrelated"),
			NewErrEndpointCapacityExceeded(errors.New("429")),
		)

		require.True(t, HasErrorCode(fmt.Errorf("all upstreams failed: %w", joined),
			ErrCodeEndpointCapacityExceeded),
			"a join reached through a plain wrapper must still be searched")
	})

	t.Run("NoFalsePositive", func(t *testing.T) {
		wrapped := fmt.Errorf("while broadcasting: %w", nonce)

		require.False(t, HasErrorCode(wrapped, ErrCodeEndpointCapacityExceeded),
			"a wider walk must not start matching codes that are not in the chain")
		require.False(t, HasErrorCode(fmt.Errorf("plain: %w", errors.New("boom")),
			ErrCodeEndpointNonceException),
			"a chain with no eRPC error in it matches nothing")
		require.False(t, HasErrorCode(nil, ErrCodeEndpointNonceException))
	})
}
