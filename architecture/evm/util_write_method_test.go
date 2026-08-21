package evm

import (
	"errors"
	"strings"
	"testing"
)

// IsNonRetryableWriteMethod is the guard that stops the router from sending the
// same state-changing call twice. A retry or a hedge on one of these methods
// creates a second filter, a second access list, or a second submitted block —
// side effects an operator cannot undo. These tests pin exactly which methods
// carry the guard and, just as importantly, which do NOT.

func TestIsNonRetryableWriteMethod_GuardsEveryListedWriteMethod(t *testing.T) {
	t.Parallel()

	// Each name here has an unrepeatable side effect on the node. If the guard
	// stops naming one, retry and hedge start duplicating it.
	guarded := []string{
		"eth_sendTransaction",
		"eth_createAccessList",
		"eth_submitTransaction",
		"eth_submitWork",
		"eth_newFilter",
		"eth_newBlockFilter",
		"eth_newPendingTransactionFilter",
	}

	for _, method := range guarded {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			if !IsNonRetryableWriteMethod(method) {
				t.Fatalf("IsNonRetryableWriteMethod(%q) = false; a retry or hedge would duplicate this write", method)
			}
		})
	}
}

func TestIsNonRetryableWriteMethod_LeavesSendRawTransactionRetryable(t *testing.T) {
	t.Parallel()

	// eth_sendRawTransaction is DELIBERATELY absent from the guard. A raw
	// transaction is idempotent by nonce, and the dedicated idempotency path
	// (eth_sendRawTransaction.go, config idempotentTransactionBroadcast) turns a
	// duplicate broadcast into a success instead of an error. Adding it to the
	// guard would disable failover for the one write eRPC can safely repeat.
	if IsNonRetryableWriteMethod("eth_sendRawTransaction") {
		t.Fatal("eth_sendRawTransaction must stay retryable; the idempotency path exists precisely so it can fail over")
	}
	if IsNonRetryableWriteMethod("eth_sendrawtransaction") {
		t.Fatal("lowercase eth_sendrawtransaction must stay retryable too")
	}
}

func TestIsNonRetryableWriteMethod_IgnoresTheCasingTheClientChose(t *testing.T) {
	t.Parallel()

	// Hook dispatch matches methods case-insensitively, so a client can reach the
	// same node method with any casing. If this guard matched case-sensitively, a
	// client could escape it by sending "ETH_SENDTRANSACTION" and get its write
	// retried on every upstream in the pool.
	for _, method := range []string{
		"ETH_SENDTRANSACTION",
		"Eth_SendTransaction",
		"eth_SENDtransaction",
		"ETH_NEWFILTER",
	} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			if !IsNonRetryableWriteMethod(method) {
				t.Fatalf("IsNonRetryableWriteMethod(%q) = false; casing must not open an escape hatch", method)
			}
		})
	}
}

func TestIsNonRetryableWriteMethod_LeavesReadMethodsAndUnknownsRetryable(t *testing.T) {
	t.Parallel()

	// The fallthrough is the primary path: eRPC must keep failing over for every
	// method it has never heard of. A guard that defaulted to "do not retry"
	// would silently disable failover for each new chain method.
	for _, method := range []string{
		"eth_call",
		"eth_getLogs",
		"eth_blockNumber",
		"eth_getBlockByNumber",
		"debug_traceTransaction",
		"some_brandNewMethodNobodyHasSeen",
		"",
		"eth_sendTransactionExtra", // a longer name must not match by prefix
		"send_eth_sendTransaction", // nor by substring
	} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			if IsNonRetryableWriteMethod(method) {
				t.Fatalf("IsNonRetryableWriteMethod(%q) = true; unknown and read methods must stay retryable", method)
			}
		})
	}
}

// IsMissingDataError decides whether an upstream reply means "I do not hold this
// data" (retry elsewhere) or a genuine failure. It reads err.Error(), so the
// caller must never hand it a nil error.

func TestIsMissingDataError_RecognisesEachVendorPhrase(t *testing.T) {
	t.Parallel()

	// One phrase per branch. If a branch is deleted, exactly one case fails and
	// names the vendor whose pruned-node replies stop failing over.
	phrases := []string{
		"missing trie node 0xabc (path )",
		"header not found",
		"could not find block",
		"unknown block",
		"Unknown block",
		"height must be less than or equal to the current blockchain height",
		"invalid blockhash finalized",
		"Expect block number from id be greater",
		"block not found",
		"Block not found",
		"block height passed is invalid",
		"cannot query unfinalized data",
		"height is not available",
		"genesis is not traceable",
		"could not find FinalizeBlock",
		"no historical rpc is available for this block height",
		"transaction not found",
		"cannot find transaction",
		"after last accepted block",
		"No state available",
		"trie does not contain the node",
		"greater than latest",
		"not currently canonical",
		"requested data is not available",
	}

	for _, phrase := range phrases {
		phrase := phrase
		t.Run(phrase, func(t *testing.T) {
			t.Parallel()
			if !IsMissingDataError(errors.New(phrase)) {
				t.Fatalf("IsMissingDataError(%q) = false; this upstream reply must fail over to a node that holds the data", phrase)
			}
		})
	}
}

func TestIsMissingDataError_RequiresBothHalvesOfEachPairedPhrase(t *testing.T) {
	t.Parallel()

	// Two branches match only when BOTH substrings appear. Half a phrase on its
	// own is a different error and must not be laundered into "missing data",
	// which would make eRPC retry a request that will fail identically everywhere.
	pairs := []struct {
		name  string
		whole string
		half  []string
	}{
		{
			name:  "blocks specified ... cannot be found",
			whole: "one of the blocks specified in filter cannot be found",
			half:  []string{"one of the blocks specified in filter", "the resource cannot be found"},
		},
		{
			name:  "historical state ... is not available",
			whole: "historical state for block 100 is not available",
			half:  []string{"historical state pruning is enabled", "the feature is not available"},
		},
	}

	for _, p := range pairs {
		p := p
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			if !IsMissingDataError(errors.New(p.whole)) {
				t.Fatalf("IsMissingDataError(%q) = false, want true", p.whole)
			}
			for _, h := range p.half {
				if IsMissingDataError(errors.New(h)) {
					t.Fatalf("IsMissingDataError(%q) = true; only half the paired phrase matched", h)
				}
			}
		})
	}

	// The "historical state" branch accepts either wording of the second half.
	if !IsMissingDataError(errors.New("historical state is unavailable on this node")) {
		t.Fatal(`the "historical state ... unavailable" wording must also count as missing data`)
	}
}

func TestIsMissingDataError_RejectsOrdinaryFailures(t *testing.T) {
	t.Parallel()

	// A misclassification here is expensive in both directions. Calling a rate
	// limit "missing data" makes eRPC mark upstreams as lacking history they
	// actually hold, which distorts every later routing decision.
	for _, msg := range []string{
		"rate limit exceeded",
		"execution reverted",
		"invalid api key",
		"context deadline exceeded",
		"",
		"nonce too low",
	} {
		msg := msg
		t.Run(msg, func(t *testing.T) {
			t.Parallel()
			if IsMissingDataError(errors.New(msg)) {
				t.Fatalf("IsMissingDataError(%q) = true; an ordinary failure must not be treated as pruned data", msg)
			}
		})
	}
}

func TestIsMissingDataError_IsCaseSensitiveWhereTheCodeSaysItIs(t *testing.T) {
	t.Parallel()

	// Unlike the write-method guard, this matcher is deliberately case-sensitive
	// and lists both casings a vendor sends ("unknown block" AND "Unknown block").
	// This test records that fact so a future reader does not mistake the
	// duplicated entries for dead code and delete one.
	if !IsMissingDataError(errors.New("unknown block")) || !IsMissingDataError(errors.New("Unknown block")) {
		t.Fatal("both casings of \"unknown block\" are listed on purpose; both must match")
	}
	if IsMissingDataError(errors.New(strings.ToUpper("unknown block"))) {
		t.Fatal("UNKNOWN BLOCK is not a casing any listed vendor sends; matching it would be an unforced commitment")
	}
}
