package erpc

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grpcStreamStarter opens one server-streaming call and returns a recv func.
// It is what lets one table cover all five QueryService methods plus
// StreamBlocks without repeating the plumbing.
type grpcStreamStarter struct {
	name string
	// wantProcessorCode is the status code a processor error reaches the client
	// with. The QueryService handlers return the processor's error unchanged, so
	// gRPC labels it Unknown; StreamBlocks runs it through the status mapper
	// first, so it arrives as Internal.
	wantProcessorCode codes.Code
	start             func(ctx context.Context, h *grpcHarness) (func() error, error)
}

// grpcQueryStarters covers every method of the BDS QueryService.
func grpcQueryStarters(from, to string) []grpcStreamStarter {
	return []grpcStreamStarter{
		{"QueryBlocks", codes.Unknown, func(ctx context.Context, h *grpcHarness) (func() error, error) {
			s, err := h.query.QueryBlocks(ctx, &evm.QueryBlocksRequest{FromBlock: &from, ToBlock: &to})
			if err != nil {
				return nil, err
			}
			return func() error { _, e := s.Recv(); return e }, nil
		}},
		{"QueryTransactions", codes.Unknown, func(ctx context.Context, h *grpcHarness) (func() error, error) {
			s, err := h.query.QueryTransactions(ctx, &evm.QueryTransactionsRequest{FromBlock: &from, ToBlock: &to})
			if err != nil {
				return nil, err
			}
			return func() error { _, e := s.Recv(); return e }, nil
		}},
		{"QueryLogs", codes.Unknown, func(ctx context.Context, h *grpcHarness) (func() error, error) {
			s, err := h.query.QueryLogs(ctx, &evm.QueryLogsRequest{FromBlock: &from, ToBlock: &to})
			if err != nil {
				return nil, err
			}
			return func() error { _, e := s.Recv(); return e }, nil
		}},
		{"QueryTraces", codes.Unknown, func(ctx context.Context, h *grpcHarness) (func() error, error) {
			s, err := h.query.QueryTraces(ctx, &evm.QueryTracesRequest{FromBlock: &from, ToBlock: &to})
			if err != nil {
				return nil, err
			}
			return func() error { _, e := s.Recv(); return e }, nil
		}},
		{"QueryTransfers", codes.Unknown, func(ctx context.Context, h *grpcHarness) (func() error, error) {
			s, err := h.query.QueryTransfers(ctx, &evm.QueryTransfersRequest{FromBlock: &from, ToBlock: &to})
			if err != nil {
				return nil, err
			}
			return func() error { _, e := s.Recv(); return e }, nil
		}},
		{"StreamBlocks", codes.Internal, func(ctx context.Context, h *grpcHarness) (func() error, error) {
			s, err := h.stream.StreamBlocks(ctx, &evm.StreamBlocksRequest{})
			if err != nil {
				return nil, err
			}
			return func() error { _, e := s.Recv(); return e }, nil
		}},
	}
}

// TestGrpcStreams_RejectAStreamWithoutAProject proves every streaming handler
// validates its metadata before it opens anything. A gRPC server-streaming call
// reports the error on the first Recv, not on the call itself.
func TestGrpcStreams_RejectAStreamWithoutAProject(t *testing.T) {
	h := newGrpcHarness(t, nil)
	for _, sc := range grpcQueryStarters("0x10", "0x12") {
		t.Run(sc.name, func(t *testing.T) {
			ctx, cancel := h.callCtx(map[string]string{"x-erpc-project": ""})
			defer cancel()
			recv, err := sc.start(ctx, h)
			require.NoError(t, err)
			err = recv()
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Equal(t, "x-erpc-project metadata required", status.Convert(err).Message())
		})
	}
}

// TestGrpcStreams_RejectAnUnknownProject carries the failure through the
// processor rather than the metadata guard, so it also proves each handler
// forwards the processor's verdict instead of swallowing it.
func TestGrpcStreams_RejectAnUnknownProject(t *testing.T) {
	h := newGrpcHarness(t, nil)
	for _, sc := range grpcQueryStarters("0x10", "0x12") {
		t.Run(sc.name, func(t *testing.T) {
			ctx, cancel := h.callCtx(map[string]string{"x-erpc-project": "nope"})
			defer cancel()
			recv, err := sc.start(ctx, h)
			require.NoError(t, err)
			err = recv()
			require.Error(t, err)
			assert.Contains(t, status.Convert(err).Message(), "nope",
				"the client must learn which project was not found")
			assert.Equal(t, sc.wantProcessorCode, status.Code(err),
				"an unknown project is not a malformed request, and each service "+
					"has its own settled way of labelling a processor failure")
		})
	}
}

// TestGrpcQueryBlocks_StreamsTheRequestedRange is the real streaming path: with
// no BDS-native upstream, eRPC shims the query onto plain eth_getBlockByNumber
// calls and pushes the headers back down the stream.
func TestGrpcQueryBlocks_StreamsTheRequestedRange(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.replyBlockByNumber()

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	from, to := "0x10", "0x12"
	stream, err := h.query.QueryBlocks(ctx, &evm.QueryBlocksRequest{FromBlock: &from, ToBlock: &to})
	require.NoError(t, err)

	var numbers []uint64
	for {
		page, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		for _, b := range page.Blocks {
			numbers = append(numbers, b.Number)
		}
	}
	assert.Equal(t, []uint64{0x10, 0x11, 0x12}, numbers,
		"the stream must carry every block in the requested range, in order")
}

// TestGrpcQuery_EveryMethodStreamsAPage proves each QueryService handler wires
// the executor's pages into its own stream. Each response type is distinct, so
// a handler that sent the wrong one would panic in the type assertion and the
// client would get Internal instead of a page.
func TestGrpcQuery_EveryMethodStreamsAPage(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.replyBlockByNumber()
	h.node.reply("eth_getLogs", `[]`)
	h.node.reply("trace_block", `[]`)

	from, to := "0x10", "0x11"
	ctx, cancel := h.callCtx(nil)
	defer cancel()

	// Every page the executor builds names the range it answered for. Asserting
	// that range is what proves the handler forwarded THE page it was given and
	// not a fresh empty one of the right type.
	assertRange := func(t *testing.T, recv func() (*evm.CursorBlock, *evm.CursorBlock, error)) {
		t.Helper()
		pages := 0
		for {
			gotFrom, gotTo, err := recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			require.NotNil(t, gotFrom, "the page must name the range it answered for")
			require.NotNil(t, gotTo)
			assert.Equal(t, uint64(0x10), gotFrom.Number)
			assert.Equal(t, uint64(0x11), gotTo.Number)
			pages++
		}
		assert.Positive(t, pages, "the handler must have sent at least one page")
	}

	t.Run("QueryBlocks", func(t *testing.T) {
		s, err := h.query.QueryBlocks(ctx, &evm.QueryBlocksRequest{FromBlock: &from, ToBlock: &to})
		require.NoError(t, err)
		assertRange(t, func() (*evm.CursorBlock, *evm.CursorBlock, error) {
			p, e := s.Recv()
			if e != nil {
				return nil, nil, e
			}
			return p.FromBlock, p.ToBlock, nil
		})
	})
	t.Run("QueryTransactions", func(t *testing.T) {
		s, err := h.query.QueryTransactions(ctx, &evm.QueryTransactionsRequest{FromBlock: &from, ToBlock: &to})
		require.NoError(t, err)
		assertRange(t, func() (*evm.CursorBlock, *evm.CursorBlock, error) {
			p, e := s.Recv()
			if e != nil {
				return nil, nil, e
			}
			return p.FromBlock, p.ToBlock, nil
		})
	})
	t.Run("QueryLogs", func(t *testing.T) {
		s, err := h.query.QueryLogs(ctx, &evm.QueryLogsRequest{FromBlock: &from, ToBlock: &to})
		require.NoError(t, err)
		assertRange(t, func() (*evm.CursorBlock, *evm.CursorBlock, error) {
			p, e := s.Recv()
			if e != nil {
				return nil, nil, e
			}
			return p.FromBlock, p.ToBlock, nil
		})
	})
	t.Run("QueryTraces", func(t *testing.T) {
		s, err := h.query.QueryTraces(ctx, &evm.QueryTracesRequest{FromBlock: &from, ToBlock: &to})
		require.NoError(t, err)
		assertRange(t, func() (*evm.CursorBlock, *evm.CursorBlock, error) {
			p, e := s.Recv()
			if e != nil {
				return nil, nil, e
			}
			return p.FromBlock, p.ToBlock, nil
		})
	})
	t.Run("QueryTransfers", func(t *testing.T) {
		s, err := h.query.QueryTransfers(ctx, &evm.QueryTransfersRequest{FromBlock: &from, ToBlock: &to})
		require.NoError(t, err)
		assertRange(t, func() (*evm.CursorBlock, *evm.CursorBlock, error) {
			p, e := s.Recv()
			if e != nil {
				return nil, nil, e
			}
			return p.FromBlock, p.ToBlock, nil
		})
	})
}

// TestGrpcStreamBlocks_PushesEachNewHeadToTheClient is the live half of the
// stream service: the head advances on the upstream and the subscriber gets a
// header without asking again.
func TestGrpcStreamBlocks_PushesEachNewHeadToTheClient(t *testing.T) {
	h := newGrpcHarness(t, func(c *common.Config) {
		c.Projects[0].Upstreams[0].Evm.StatePollerInterval = common.Duration(200 * time.Millisecond)
		c.Projects[0].Upstreams[0].Evm.StatePollerDebounce = common.Duration(50 * time.Millisecond)
	})
	h.node.replyBlockByNumber()
	start := h.node.latestHead()

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	stream, err := h.stream.StreamBlocks(ctx, &evm.StreamBlocksRequest{})
	require.NoError(t, err)

	// Keep the head moving until a header arrives, so the test does not depend
	// on the timing of one poll landing after the subscription.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				h.node.bumpHead()
			}
		}
	}()

	resp, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, resp.Header)
	assert.Greater(t, resp.Header.Number, start,
		"a tip subscriber must receive blocks newer than the head it joined at")
}

// TestGrpcQueryLogs_StreamsTheRequestedRange covers the second QueryService
// method over the same shim, using eth_getLogs.
func TestGrpcQueryLogs_StreamsTheRequestedRange(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.replyBlockByNumber()
	addr := "0x1111111111111111111111111111111111111111"
	topic := "0x" + fmt.Sprintf("%064x", 0xaa)
	h.node.reply("eth_getLogs", fmt.Sprintf(`[{
		"address":%q,"topics":[%q],"data":"0x01",
		"blockNumber":"0x10","blockHash":%q,
		"transactionHash":%q,"transactionIndex":"0x0","logIndex":"0x0"
	}]`, addr, topic, grpcBlockHash(0x10), grpcBlockHash(0x99)))

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	from, to := "0x10", "0x11"
	stream, err := h.query.QueryLogs(ctx, &evm.QueryLogsRequest{FromBlock: &from, ToBlock: &to})
	require.NoError(t, err)

	total := 0
	for {
		page, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		total += len(page.Logs)
	}
	assert.Positive(t, total, "the shim must stream the node's logs back to the client")
	assert.Positive(t, h.node.callCount("eth_getLogs"),
		"the range must really have been fetched from the upstream")
}
