package erpc

import (
	"fmt"
	"io"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Nodes disagree about how to hand over traces. Erigon and OpenEthereum answer
// trace_block; geth answers debug_traceBlockByNumber with a callTracer. The
// shim tries the first, and falls back to the second when the node says the
// method does not exist. The fallback is the branch that decides whether a geth
// operator gets traces at all, and it only runs on an error the happy path
// never produces.

const traceMethodMissing = "the method does not exist/is not available"

// gethCallTracerBlock renders what debug_traceBlockByNumber returns for a block
// with one transaction that makes one inner call. The outer entry wraps the
// tracer output under "result", which is the shape geth actually sends.
func gethCallTracerBlock(txHash string) string {
	return fmt.Sprintf(`[{
		"txHash": %q,
		"result": {
			"type": "CALL",
			"from": "0x1111111111111111111111111111111111111111",
			"to": "0x2222222222222222222222222222222222222222",
			"value": "0x1",
			"gas": "0x5208",
			"gasUsed": "0x5208",
			"input": "0x",
			"output": "0x",
			"transactionHash": %q,
			"transactionIndex": "0x0",
			"calls": [{
				"type": "STATICCALL",
				"from": "0x2222222222222222222222222222222222222222",
				"to": "0x3333333333333333333333333333333333333333",
				"gas": "0x100",
				"gasUsed": "0x10",
				"input": "0x",
				"output": "0x",
				"transactionHash": %q,
				"transactionIndex": "0x0"
			}]
		}
	}]`, txHash, txHash, txHash)
}

// collectTraces drains a QueryTraces stream.
func collectTraces(t *testing.T, stream interface {
	Recv() (*evm.QueryTracesResponse, error)
}) []*evm.Trace {
	t.Helper()
	var out []*evm.Trace
	for {
		page, err := stream.Recv()
		if err == io.EOF {
			return out
		}
		require.NoError(t, err)
		out = append(out, page.Traces...)
	}
}

// startTraceQuery asks for exactly one block, so the assertions describe that
// block's traces and nothing else.
func startTraceQuery(t *testing.T, h *grpcHarness) []*evm.Trace {
	t.Helper()
	ctx, cancel := h.callCtx(nil)
	defer cancel()
	from, to := "0x10", "0x10"
	stream, err := h.query.QueryTraces(ctx, &evm.QueryTracesRequest{FromBlock: &from, ToBlock: &to})
	require.NoError(t, err)
	return collectTraces(t, stream)
}

// TestGrpcQueryTraces_FallsBackToTheGethTracerWhenTraceBlockIsMissing is the
// geth path. trace_block is refused, the shim retries with the callTracer, and
// the nested call is flattened into its own row addressed under its parent.
func TestGrpcQueryTraces_FallsBackToTheGethTracerWhenTraceBlockIsMissing(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.replyBlockByNumber()
	h.node.replyError("trace_block", 200, -32601, traceMethodMissing)
	txHash := fmt.Sprintf("0x%064x", 0xbeef)
	h.node.reply("debug_traceBlockByNumber", gethCallTracerBlock(txHash))

	traces := startTraceQuery(t, h)

	require.Positive(t, h.node.callCount("debug_traceBlockByNumber"),
		"the shim must actually try the second tracer, not give up on the first")
	require.Len(t, traces, 2, "the tracer's nested call becomes its own trace row")

	assert.Empty(t, traces[0].TraceAddress, "the top-level call is addressed at the root")
	assert.Equal(t, []uint32{0}, traces[1].TraceAddress, "the inner call sits under it")
	for i, tr := range traces {
		assert.Equal(t, uint64(0x10), tr.BlockNumber, "trace %d must carry the block it came from", i)
	}
}

// TestGrpcQueryTraces_AcceptsATracerThatAnswersWithOneObject covers the second
// decoding shape. Some nodes answer debug_traceBlockByNumber with a bare
// callTracer object rather than an array of them; the shim reads both.
func TestGrpcQueryTraces_AcceptsATracerThatAnswersWithOneObject(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.replyBlockByNumber()
	h.node.replyError("trace_block", 200, -32601, traceMethodMissing)
	txHash := fmt.Sprintf("0x%064x", 0xfeed)
	h.node.reply("debug_traceBlockByNumber", fmt.Sprintf(`{
		"type": "CALL",
		"from": "0x1111111111111111111111111111111111111111",
		"to": "0x2222222222222222222222222222222222222222",
		"value": "0x2",
		"gas": "0x5208",
		"gasUsed": "0x5208",
		"input": "0x",
		"output": "0x",
		"transactionHash": %q,
		"transactionIndex": "0x0"
	}`, txHash))

	traces := startTraceQuery(t, h)

	require.Len(t, traces, 1)
	assert.Equal(t, uint64(0x10), traces[0].BlockNumber)
	assert.Empty(t, traces[0].TraceAddress)
}

// TestGrpcQueryTraces_ReportsThatTheNodeHasNoTracerAtAll is the exhausted case.
// Both tracers are refused, and the client must be told the node cannot serve
// traces rather than be handed an empty page it would read as "no traces here".
func TestGrpcQueryTraces_ReportsThatTheNodeHasNoTracerAtAll(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.replyBlockByNumber()
	h.node.replyError("trace_block", 200, -32601, traceMethodMissing)
	h.node.replyError("debug_traceBlockByNumber", 200, -32601, traceMethodMissing)

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	from, to := "0x10", "0x10"
	stream, err := h.query.QueryTraces(ctx, &evm.QueryTracesRequest{FromBlock: &from, ToBlock: &to})
	require.NoError(t, err)

	var pages int
	for {
		page, rerr := stream.Recv()
		if rerr != nil {
			err = rerr
			break
		}
		_ = page
		pages++
	}

	require.Error(t, err)
	assert.NotEqual(t, io.EOF, err, "an empty page would read as a block with no traces")
	assert.Contains(t, err.Error(), "trace_block",
		"the message must name the methods the node would need")
	assert.Contains(t, err.Error(), "debug_traceBlockByNumber")
	assert.Zero(t, pages, "no page is emitted for a block whose traces could not be read")
	assert.Positive(t, h.node.callCount("debug_traceBlockByNumber"),
		"both tracers must have been tried before the client is refused")
}
