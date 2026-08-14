package evm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upstreamPostForward_eth_sendRawTransaction is the per-upstream half of the
// idempotency path. It turns a nonce rejection into a success ONLY when the
// broadcast provably landed. Getting that wrong in either direction is
// expensive: a false success makes a caller believe a transfer went through,
// and a false error makes a caller re-broadcast a transaction that is already
// mined.
//
// Every failure route in this hook returns the same normalized -32003, so the
// assertions below check the discriminating property instead: which requests
// the hook sent, and which message survived into the normalized error.

// nonceErr builds the typed nonce exception the error normalizer produces,
// wrapping the upstream's own wording.
func nonceErr(reason common.NonceExceptionReason, upstreamMsg string) error {
	return common.NewErrEndpointNonceException(errors.New(upstreamMsg), reason)
}

// sendRawTxNetwork is a network with a real logger — the hook dereferences
// n.Logger() unguarded.
func sendRawTxNetwork(idempotent *bool) *testNetwork {
	return &testNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:                        1,
			IdempotentTransactionBroadcast: idempotent,
		},
	}}
}

func TestUpstreamPostForward_ethSendRawTransaction(t *testing.T) {
	ctx := context.Background()

	t.Run("SuccessPassesThroughWithoutProbing", func(t *testing.T) {
		up := newForwardingUpstream(1)
		okResp := common.NewNormalizedResponse().WithJsonRpcResponse(
			common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"`+sendRawTxFixtureHash+`"`), nil))

		resp, err := upstreamPostForward_eth_sendRawTransaction(
			ctx, sendRawTxNetwork(nil), up, makeSendRawTxRequest(t), okResp, nil)

		require.NoError(t, err)
		assert.Same(t, okResp, resp)
		assert.Empty(t, up.allCalls(), "a clean broadcast must not trigger a verification probe")
	})

	t.Run("DisabledIdempotencyReturnsTheRejectionUntouched", func(t *testing.T) {
		disabled := false
		up := newForwardingUpstream(1)
		orig := nonceErr(common.NonceExceptionReasonAlreadyKnown, "already known")

		resp, err := upstreamPostForward_eth_sendRawTransaction(
			ctx, sendRawTxNetwork(&disabled), up, makeSendRawTxRequest(t), nil, orig)

		assert.Same(t, orig, err, "the operator opted out, so the raw rejection must reach the caller")
		assert.Nil(t, resp)
		assert.Empty(t, up.allCalls())
	})

	t.Run("NonNonceErrorPassesThroughUntouched", func(t *testing.T) {
		up := newForwardingUpstream(1)
		orig := common.NewErrEndpointExecutionException(
			common.NewErrJsonRpcExceptionInternal(-32000, common.JsonRpcErrorEvmReverted, "insufficient funds", nil, nil))

		_, err := upstreamPostForward_eth_sendRawTransaction(
			ctx, sendRawTxNetwork(nil), up, makeSendRawTxRequest(t), nil, orig)

		assert.Same(t, orig, err, "insufficient funds is a real rejection, never idempotent")
		assert.Empty(t, up.allCalls())
	})

	t.Run("AWrappedNonceExceptionIsNotRecognised", func(t *testing.T) {
		// Current behaviour, pinned: the gate uses common.HasErrorCode, which
		// does not walk a plain fmt.Errorf("%w") chain, so a nonce exception
		// wrapped anywhere above the endpoint layer never reaches the
		// idempotency path — even though the errors.As below it would find it.
		// See the report.
		up := newForwardingUpstream(1)
		wrapped := fmt.Errorf("while broadcasting: %w",
			nonceErr(common.NonceExceptionReasonAlreadyKnown, "already known"))

		resp, err := upstreamPostForward_eth_sendRawTransaction(
			ctx, sendRawTxNetwork(nil), up, makeSendRawTxRequest(t), nil, wrapped)

		assert.Same(t, wrapped, err, "the wrapped rejection reaches the caller unchanged")
		assert.Nil(t, resp)
		assert.Empty(t, up.allCalls())
	})

	t.Run("NonceCodeWithoutTheTypedDetailsPassesThrough", func(t *testing.T) {
		// The code matches but the concrete type does not, so the reason cannot
		// be read. Guessing a reason here would convert an unknown rejection
		// into a fabricated success.
		up := newForwardingUpstream(1)
		orig := &common.BaseError{
			Code:    common.ErrCodeEndpointNonceException,
			Message: "nonce problem",
		}

		_, err := upstreamPostForward_eth_sendRawTransaction(
			ctx, sendRawTxNetwork(nil), up, makeSendRawTxRequest(t), nil, orig)

		require.Error(t, err)
		assert.Same(t, error(orig), err)
		assert.Empty(t, up.allCalls())
	})

	t.Run("UnknownReasonPassesThrough", func(t *testing.T) {
		up := newForwardingUpstream(1)
		orig := nonceErr(common.NonceExceptionReason("nonce_from_the_future"), "weird")

		_, err := upstreamPostForward_eth_sendRawTransaction(
			ctx, sendRawTxNetwork(nil), up, makeSendRawTxRequest(t), nil, orig)

		assert.Same(t, orig, err)
		assert.Empty(t, up.allCalls(), "an unrecognised reason must not be verified on-chain")
	})

	t.Run("UndecodableTransactionPassesThrough", func(t *testing.T) {
		// Without the signed bytes there is no hash, so nothing can be
		// verified and nothing may be synthesised.
		up := newForwardingUpstream(1)
		rq := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["0xnothex"]}`))
		orig := nonceErr(common.NonceExceptionReasonAlreadyKnown, "already known")

		_, err := upstreamPostForward_eth_sendRawTransaction(
			ctx, sendRawTxNetwork(nil), up, rq, nil, orig)

		assert.Same(t, orig, err)
		assert.Empty(t, up.allCalls())
	})

	t.Run("AlreadyKnownBecomesSuccessWithoutProbing", func(t *testing.T) {
		// "already known" means the node itself holds the transaction, so no
		// on-chain lookup is needed or sent.
		up := newForwardingUpstream(1)
		orig := nonceErr(common.NonceExceptionReasonAlreadyKnown, "already known")

		resp, err := upstreamPostForward_eth_sendRawTransaction(
			ctx, sendRawTxNetwork(nil), up, makeSendRawTxRequest(t), nil, orig)

		require.NoError(t, err)
		require.NotNil(t, resp)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), sendRawTxFixtureHash,
			"the caller must get back the hash of the transaction it signed")
		assert.Empty(t, up.allCalls(), "already-known needs no verification round trip")
	})

	t.Run("NonceTooLowVerifiedOnChainBecomesSuccess", func(t *testing.T) {
		up := newForwardingUpstream(1)
		up.on("eth_getTransactionByHash", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `{"hash":"`+sendRawTxFixtureHash+`","blockNumber":"0x123"}`)
		})
		orig := nonceErr(common.NonceExceptionReasonNonceTooLow, "nonce too low")

		resp, err := upstreamPostForward_eth_sendRawTransaction(
			ctx, sendRawTxNetwork(nil), up, makeSendRawTxRequest(t), nil, orig)

		require.NoError(t, err)
		require.NotNil(t, resp)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), sendRawTxFixtureHash)
		// Discriminating: the probe must ask the SAME upstream for the hash
		// derived from the signed bytes.
		require.Len(t, up.methodCalls("eth_getTransactionByHash"), 1)
		assert.True(t,
			strings.Contains(strings.ToLower(up.methodCalls("eth_getTransactionByHash")[0]), sendRawTxFixtureHash),
			"the probe must carry the locally-derived tx hash")
	})

	t.Run("NonceTooLowNotOnChainBecomesANormalizedRejection", func(t *testing.T) {
		// A DIFFERENT transaction burned this nonce. The caller must see a
		// terminal client-side rejection, not the raw endpoint error and not a
		// success.
		up := newForwardingUpstream(1)
		up.on("eth_getTransactionByHash", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `null`)
		})
		orig := nonceErr(common.NonceExceptionReasonNonceTooLow, "nonce too low: address 0xabc, tx: 5 state: 7")

		resp, err := upstreamPostForward_eth_sendRawTransaction(
			ctx, sendRawTxNetwork(nil), up, makeSendRawTxRequest(t), nil, orig)

		require.Error(t, err)
		assert.Nil(t, resp)
		// Discriminating: the hook must REPLACE the error, keep the upstream's
		// wording, and mark it non-retryable. Asserting only "an error came
		// back" would also pass on the pass-through branches above.
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException),
			"the rejection must be normalized to a client-side exception")
		assert.Contains(t, err.Error(), "state: 7", "the upstream's diagnosis must survive")
		var jre *common.ErrJsonRpcExceptionInternal
		require.True(t, errors.As(err, &jre))
		assert.EqualValues(t, common.JsonRpcErrorTransactionRejected, jre.NormalizedCode())
	})

	t.Run("VerificationTransportFailureBecomesANormalizedRejection", func(t *testing.T) {
		up := newForwardingUpstream(1)
		up.on("eth_getTransactionByHash", func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, errors.New("connection reset")
		})
		orig := nonceErr(common.NonceExceptionReasonNonceTooLow, "nonce too low")

		_, err := upstreamPostForward_eth_sendRawTransaction(
			ctx, sendRawTxNetwork(nil), up, makeSendRawTxRequest(t), nil, orig)

		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException))
		// Discriminating: an unverifiable nonce-too-low must NOT become a
		// success, and the probe must have actually been attempted.
		assert.Equal(t, 1, up.callCount("eth_getTransactionByHash"))
	})

	t.Run("VerificationJsonRpcErrorBecomesANormalizedRejection", func(t *testing.T) {
		up := newForwardingUpstream(1)
		up.on("eth_getTransactionByHash", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResultBesideError(req, `{"hash":"`+sendRawTxFixtureHash+`"}`, -32000, "tx indexing disabled")
		})
		orig := nonceErr(common.NonceExceptionReasonNonceTooLow, "nonce too low")

		resp, err := upstreamPostForward_eth_sendRawTransaction(
			ctx, sendRawTxNetwork(nil), up, makeSendRawTxRequest(t), nil, orig)

		require.Error(t, err)
		assert.Nil(t, resp, "an error member must veto the tx object beside it")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException))
	})
}

func TestCreateNormalizedNonceTooLowError(t *testing.T) {
	t.Run("KeepsTheUpstreamsWordingFromTheCause", func(t *testing.T) {
		err := createNormalizedNonceTooLowError(
			nonceErr(common.NonceExceptionReasonNonceTooLow, "nonce too low: next nonce 12, tx nonce 9"))

		assert.Contains(t, err.Error(), "next nonce 12, tx nonce 9")
	})

	t.Run("KeepsTheWordingFromAJsonRpcCause", func(t *testing.T) {
		inner := common.NewErrJsonRpcExceptionInternal(
			-32000, common.JsonRpcErrorTransactionRejected, "replacement transaction underpriced", nil, nil)
		err := createNormalizedNonceTooLowError(
			common.NewErrEndpointNonceException(inner, common.NonceExceptionReasonNonceTooLow))

		assert.Contains(t, err.Error(), "replacement transaction underpriced")
	})

	t.Run("FallsBackToAGenericMessageForAnUnrelatedError", func(t *testing.T) {
		err := createNormalizedNonceTooLowError(errors.New("something else entirely"))

		assert.Contains(t, err.Error(), "nonce too low")
		assert.NotContains(t, err.Error(), "something else entirely",
			"an unrelated cause must not be presented as the nonce diagnosis")
	})

	t.Run("IsNotRetryableTowardTheNetwork", func(t *testing.T) {
		// The whole point of normalizing: without this flag the router keeps
		// re-broadcasting a transaction whose nonce is permanently spent.
		err := createNormalizedNonceTooLowError(
			nonceErr(common.NonceExceptionReasonNonceTooLow, "nonce too low"))

		se, ok := err.(common.StandardError)
		require.True(t, ok)
		assert.False(t, common.IsRetryableTowardNetwork(se))
	})
}
