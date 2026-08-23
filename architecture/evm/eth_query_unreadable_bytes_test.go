package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover entry 161 in valve/upstream-bug-log.md, the hex-string twin
// of entry 132. Seven sites handed a wire string to common.HexToBytes and
// dropped the error, so a garbage value and an absent one both arrived as an
// empty slice. The rule is the one 132 established: absent stays absent,
// present-but-unreadable is reported.
//
// Each test below measures the two cases against each other. A test that only
// asserted the rejection would pass just as well if eRPC rejected everything,
// including the absences the proto has no way to express.

// --- protoTraceFromJSON ---

// TestProtoTraceFromJSON_APresentButUnreadableHexFieldIsAnError covers the six
// sites inside the decoder. The `from` case is the one with a named
// consequence: NativeTransfersFromTraces reads an empty sender as a transfer
// with no sender, which is a claim about the chain that the chain never made.
func TestProtoTraceFromJSON_APresentButUnreadableHexFieldIsAnError(t *testing.T) {
	for _, field := range []string{"from", "to", "input", "output", "transactionHash", "blockHash"} {
		t.Run(field, func(t *testing.T) {
			trace := map[string]interface{}{
				"traceType": "call",
				"callType":  "call",
				"from":      "0x0000000000000000000000000000000000000001",
				"value":     "0x0",
			}
			trace[field] = "not hex"

			_, err := protoTraceFromJSON(trace)
			require.Error(t, err, "%s is present and unreadable, so it must not decode as empty bytes", field)
			assert.Contains(t, err.Error(), field, "the error must name the field the operator has to fix")
		})
	}
}

// TestProtoTraceFromJSON_AnAbsentHexFieldStaysAbsent is the counterweight. The
// proto has no way to say "absent", so an omitted field keeps the nil. If this
// ever reddens, the rejection above has grown past what the wire forces.
func TestProtoTraceFromJSON_AnAbsentHexFieldStaysAbsent(t *testing.T) {
	got, err := protoTraceFromJSON(map[string]interface{}{
		"traceType": "call",
		"callType":  "call",
		"from":      "0x0000000000000000000000000000000000000001",
		"value":     "0x0",
	})
	require.NoError(t, err, "a trace that omits the optional hex fields is ordinary, not broken")
	assert.Empty(t, got.To, "a contract-creating call has no `to`, and that must stay expressible")
	assert.Empty(t, got.Input)
	assert.Empty(t, got.Output)
	assert.Empty(t, got.TransactionHash)
	assert.Empty(t, got.BlockHash)
}

// TestProtoTraceFromJSON_AnEmptyToStaysEmptyRatherThanErroring pins the one
// field that was already guarded. Parity writes `"to": ""` on a create, so an
// empty string here is a real answer and not a decode failure.
func TestProtoTraceFromJSON_AnEmptyToStaysEmptyRatherThanErroring(t *testing.T) {
	got, err := protoTraceFromJSON(map[string]interface{}{
		"traceType": "create",
		"from":      "0x0000000000000000000000000000000000000001",
		"to":        "",
		"value":     "0x0",
	})
	require.NoError(t, err, `an explicit empty "to" is how a create is written, not a broken hash`)
	assert.Empty(t, got.To)
}

// TestProtoTraceFromJSON_AReadableHexFieldStillDecodes is the second
// counterweight: the rule must not cost the ordinary path.
func TestProtoTraceFromJSON_AReadableHexFieldStillDecodes(t *testing.T) {
	got, err := protoTraceFromJSON(map[string]interface{}{
		"traceType":       "call",
		"callType":        "call",
		"from":            "0x0000000000000000000000000000000000000001",
		"to":              "0x0000000000000000000000000000000000000002",
		"transactionHash": "0x00000000000000000000000000000000000000000000000000000000000000ff",
		"value":           "0x0",
	})
	require.NoError(t, err)
	assert.Len(t, got.From, 20)
	assert.Len(t, got.To, 20)
	assert.Len(t, got.TransactionHash, 32)
}

// --- fetchTracesForBlock ---

// TestFetchTracesForBlock_AnUnreadableBlockHashIsAnError covers the seventh
// site, which is the expensive one: this hash is stamped onto EVERY trace of
// the block, so one unreadable value mislabels the whole block's worth.
func TestFetchTracesForBlock_AnUnreadableBlockHashIsAnError(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, _ *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		t.Fatalf("the shim must reject the block before tracing it; it sent %q", jrq.Method)
		return nil, nil
	})

	block := makeBlockResult(3, nil)
	block["hash"] = "not hex"

	_, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", block)
	require.Error(t, err, "a block hash that is present but unreadable must not be dropped to empty")
	assert.Contains(t, err.Error(), "hash")
}

// TestFetchTracesForBlock_ABlockHashOfTheWrongTypeIsAnError covers the OTHER
// dropped error at the same site. The type assertion used to turn a non-string
// hash into "", which then decoded to empty bytes — two silent steps, one
// wrong answer.
func TestFetchTracesForBlock_ABlockHashOfTheWrongTypeIsAnError(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, _ *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		t.Fatalf("the shim must reject the block before tracing it; it sent %q", jrq.Method)
		return nil, nil
	})

	block := makeBlockResult(3, nil)
	block["hash"] = float64(42)

	_, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", block)
	require.Error(t, err, "a hash that is not a string must be reported, not silently emptied")
	assert.Contains(t, err.Error(), "hash")
}
