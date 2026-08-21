package erpc

import (
	"context"
	"errors"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// When an upstream speaks BDS natively, eRPC does not decompose the query at
// all: it opens the upstream's own stream and hands each page straight back.
// There is one pipe-through per query method, and they are separate functions,
// so a method wired to the wrong stream — or to no stream — is not caught by
// testing one of them.

// TestPipeThrough_EachQueryMethodStreamsItsOwnUpstreamStream drives all five
// methods through Execute. Each fake answers only its own method, so a handler
// wired to the wrong stream gets "unexpected ..." instead of a page.
func TestPipeThrough_EachQueryMethodStreamsItsOwnUpstreamStream(t *testing.T) {
	from, to := util.StringPtr("0x1"), util.StringPtr("0x2")

	t.Run("QueryTransactions", func(t *testing.T) {
		calls := 0
		ups := newTestQueryUpstream(t, "bds", &fakeGrpcBdsClient{queryClient: &fakeQueryServiceClient{
			queryTransactionsFn: func(ctx context.Context, in *evm.QueryTransactionsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransactionsResponse], error) {
				calls++
				assert.Equal(t, "0x1", in.GetFromBlock(), "the upstream must be asked for the range the client asked for")
				return &fakeServerStreamingClient[evm.QueryTransactionsResponse]{
					responses: []*evm.QueryTransactionsResponse{{
						Transactions: []*evm.Transaction{{BlockNumber: proto.Uint64(7), TransactionIndex: proto.Uint32(3)}},
					}},
				}, nil
			},
		}})
		qe := newTestQueryExecutor(t, "eth_queryTransactions", ups)

		var pages []*evm.QueryTransactionsResponse
		err := qe.Execute(context.Background(), &evm.QueryTransactionsRequest{FromBlock: from, ToBlock: to},
			func(p proto.Message) error {
				pages = append(pages, p.(*evm.QueryTransactionsResponse))
				return nil
			})
		require.NoError(t, err)
		require.Equal(t, 1, calls)
		require.Len(t, pages, 1)
		require.Len(t, pages[0].Transactions, 1)
		assert.Equal(t, uint64(7), pages[0].Transactions[0].GetBlockNumber())
	})

	t.Run("QueryTraces", func(t *testing.T) {
		calls := 0
		ups := newTestQueryUpstream(t, "bds", &fakeGrpcBdsClient{queryClient: &fakeQueryServiceClient{
			queryTracesFn: func(ctx context.Context, in *evm.QueryTracesRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTracesResponse], error) {
				calls++
				assert.Equal(t, "0x2", in.GetToBlock())
				return &fakeServerStreamingClient[evm.QueryTracesResponse]{
					responses: []*evm.QueryTracesResponse{{
						Traces: []*evm.Trace{{BlockNumber: 9}},
					}},
				}, nil
			},
		}})
		qe := newTestQueryExecutor(t, "eth_queryTraces", ups)

		var pages []*evm.QueryTracesResponse
		err := qe.Execute(context.Background(), &evm.QueryTracesRequest{FromBlock: from, ToBlock: to},
			func(p proto.Message) error {
				pages = append(pages, p.(*evm.QueryTracesResponse))
				return nil
			})
		require.NoError(t, err)
		require.Equal(t, 1, calls)
		require.Len(t, pages, 1)
		require.Len(t, pages[0].Traces, 1)
		assert.Equal(t, uint64(9), pages[0].Traces[0].BlockNumber)
	})

	t.Run("QueryTransfers", func(t *testing.T) {
		calls := 0
		ups := newTestQueryUpstream(t, "bds", &fakeGrpcBdsClient{queryClient: &fakeQueryServiceClient{
			queryTransfersFn: func(ctx context.Context, in *evm.QueryTransfersRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransfersResponse], error) {
				calls++
				return &fakeServerStreamingClient[evm.QueryTransfersResponse]{
					responses: []*evm.QueryTransfersResponse{{
						Transfers: []*evm.NativeTransfer{{BlockNumber: 11}},
					}},
				}, nil
			},
		}})
		qe := newTestQueryExecutor(t, "eth_queryTransfers", ups)

		var pages []*evm.QueryTransfersResponse
		err := qe.Execute(context.Background(), &evm.QueryTransfersRequest{FromBlock: from, ToBlock: to},
			func(p proto.Message) error {
				pages = append(pages, p.(*evm.QueryTransfersResponse))
				return nil
			})
		require.NoError(t, err)
		require.Equal(t, 1, calls)
		require.Len(t, pages, 1)
		require.Len(t, pages[0].Transfers, 1)
		assert.Equal(t, uint64(11), pages[0].Transfers[0].BlockNumber)
	})
}

// TestPipeThrough_TriesTheNextUpstreamWhenTheStreamNeverOpens covers the
// failure that happens before any page exists. Nothing was delivered, so the
// query is safe to restart elsewhere, and the client must not see the failure
// at all.
func TestPipeThrough_TriesTheNextUpstreamWhenTheStreamNeverOpens(t *testing.T) {
	firstCalls, secondCalls := 0, 0

	first := newTestQueryUpstream(t, "bds-1", &fakeGrpcBdsClient{queryClient: &fakeQueryServiceClient{
		queryTransactionsFn: func(ctx context.Context, in *evm.QueryTransactionsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransactionsResponse], error) {
			firstCalls++
			return nil, errors.New("upstream refused the stream")
		},
	}})
	second := newTestQueryUpstream(t, "bds-2", &fakeGrpcBdsClient{queryClient: &fakeQueryServiceClient{
		queryTransactionsFn: func(ctx context.Context, in *evm.QueryTransactionsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransactionsResponse], error) {
			secondCalls++
			return &fakeServerStreamingClient[evm.QueryTransactionsResponse]{
				responses: []*evm.QueryTransactionsResponse{{
					Transactions: []*evm.Transaction{{BlockNumber: proto.Uint64(7)}},
				}},
			}, nil
		},
	}})

	qe := newTestQueryExecutor(t, "eth_queryTransactions", first, second)

	var pages []*evm.QueryTransactionsResponse
	err := qe.Execute(context.Background(), &evm.QueryTransactionsRequest{
		FromBlock: util.StringPtr("0x1"), ToBlock: util.StringPtr("0x2"),
	}, func(p proto.Message) error {
		pages = append(pages, p.(*evm.QueryTransactionsResponse))
		return nil
	})

	require.NoError(t, err, "a stream that never opened delivered nothing, so the query can move on")
	assert.Equal(t, 1, firstCalls)
	assert.Equal(t, 1, secondCalls)
	require.Len(t, pages, 1)
	require.Len(t, pages[0].Transactions, 1)
	assert.Equal(t, uint64(7), pages[0].Transactions[0].GetBlockNumber())
}

// TestPipeThrough_StopsWhenTheConsumerRejectsAPage is the other side of the
// stream. When the caller's own writer fails — a gRPC client that hung up
// mid-answer — the walk must stop and report a page as already emitted, so the
// query is not silently restarted on another upstream and delivered twice.
func TestPipeThrough_StopsWhenTheConsumerRejectsAPage(t *testing.T) {
	firstCalls, secondCalls := 0, 0

	newStream := func(counter *int) *fakeQueryServiceClient {
		return &fakeQueryServiceClient{
			queryTracesFn: func(ctx context.Context, in *evm.QueryTracesRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTracesResponse], error) {
				*counter++
				return &fakeServerStreamingClient[evm.QueryTracesResponse]{
					responses: []*evm.QueryTracesResponse{
						{Traces: []*evm.Trace{{BlockNumber: 1}}, CursorBlock: &evm.CursorBlock{Number: 1}},
						{Traces: []*evm.Trace{{BlockNumber: 2}}},
					},
				}, nil
			},
		}
	}

	first := newTestQueryUpstream(t, "bds-1", &fakeGrpcBdsClient{queryClient: newStream(&firstCalls)})
	second := newTestQueryUpstream(t, "bds-2", &fakeGrpcBdsClient{queryClient: newStream(&secondCalls)})
	qe := newTestQueryExecutor(t, "eth_queryTraces", first, second)

	deliverErr := errors.New("client hung up")
	delivered := 0
	err := qe.Execute(context.Background(), &evm.QueryTracesRequest{
		FromBlock: util.StringPtr("0x1"), ToBlock: util.StringPtr("0x2"),
	}, func(p proto.Message) error {
		delivered++
		return deliverErr
	})

	require.Error(t, err)
	assert.Equal(t, 1, delivered, "the walk stops on the page the consumer rejected")
	assert.Equal(t, 1, firstCalls)
	assert.Zero(t, secondCalls, "a query whose first page was already handed over must not be re-run")

	var streamErr *StreamError
	require.ErrorAs(t, err, &streamErr)
	assert.True(t, streamErr.PageEmitted)
	require.NotNil(t, streamErr.LastCursor)
	assert.Equal(t, uint64(1), streamErr.LastCursor.Number)
	assert.ErrorIs(t, err, deliverErr, "the cause must survive the wrapper")
	assert.Equal(t, deliverErr.Error(), err.Error(),
		"the wrapper reports the cause's message, so a log line names the real failure")
}
