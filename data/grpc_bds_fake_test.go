package data

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeBdsServer is a Blockchain Data Standards gRPC server for the data
// package. It answers ChainId and GetBlockByNumber, which are the only two
// calls GrpcConnector makes: the bootstrap probe uses ChainId, and the head
// poller uses GetBlockByNumber with the "earliest", "latest" and "finalized"
// tags.
//
// It keeps the shape of clients/grpc_bds_resilience_extras_test.go's
// happyRPCServer — same registration, same t-scoped start helper — but adds
// per-tag answers and per-tag failures so a test can decide what each of the
// poller's three calls sees. The two servers cannot be shared: the clients one
// is unexported in its own package.
//
// Reuse it for any data-package test that needs a real BDS endpoint:
//
//	addr, srv, stop := startFakeBdsServer(t, 1)
//	defer stop()
//	srv.SetBlock("latest", 100, 1700000000)
//	srv.FailTag("finalized", codes.Unavailable, "no finalized view")
type fakeBdsServer struct {
	evm.UnimplementedRPCQueryServiceServer

	mu           sync.Mutex
	chainID      uint64
	blocks       map[string]*evm.BlockHeader
	failures     map[string]error
	lastMetadata metadata.MD

	chainIdCalls atomic.Int64
	blockCalls   atomic.Int64
}

// LastMetadata returns the gRPC metadata the server saw on its most recent
// call. Header-propagation tests read it to prove a configured header reached
// the wire rather than only the connector's own map.
func (s *fakeBdsServer) LastMetadata() metadata.MD {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastMetadata == nil {
		return metadata.MD{}
	}
	return s.lastMetadata.Copy()
}

// recordMetadata must be called with s.mu held.
func (s *fakeBdsServer) recordMetadata(ctx context.Context) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		s.lastMetadata = md.Copy()
	}
}

func newFakeBdsServer(chainID uint64) *fakeBdsServer {
	return &fakeBdsServer{
		chainID:  chainID,
		blocks:   make(map[string]*evm.BlockHeader),
		failures: make(map[string]error),
	}
}

// SetBlock arms the answer for one block tag ("earliest", "latest",
// "finalized") or for a concrete hex block number.
func (s *fakeBdsServer) SetBlock(tag string, number, timestamp uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failures, tag)
	s.blocks[tag] = &evm.BlockHeader{
		Number:    number,
		Timestamp: timestamp,
		Hash:      []byte{0xab, 0xcd},
	}
}

// FailTag makes one block tag return a gRPC error instead of a block.
func (s *fakeBdsServer) FailTag(tag string, code codes.Code, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blocks, tag)
	s.failures[tag] = status.Error(code, msg)
}

// ClearTag makes one block tag answer with no block at all, which is what a
// server that has not indexed that tag yet returns.
func (s *fakeBdsServer) ClearTag(tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blocks, tag)
	delete(s.failures, tag)
}

// BlockCalls reports how many GetBlockByNumber calls the server has served.
func (s *fakeBdsServer) BlockCalls() int64 { return s.blockCalls.Load() }

// ChainIdCalls reports how many ChainId probes the server has served.
func (s *fakeBdsServer) ChainIdCalls() int64 { return s.chainIdCalls.Load() }

func (s *fakeBdsServer) ChainId(ctx context.Context, _ *evm.ChainIdRequest) (*evm.ChainIdResponse, error) {
	s.chainIdCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordMetadata(ctx)
	return &evm.ChainIdResponse{ChainId: s.chainID}, nil
}

func (s *fakeBdsServer) GetBlockByNumber(ctx context.Context, req *evm.GetBlockByNumberRequest) (*evm.GetBlockResponse, error) {
	s.blockCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordMetadata(ctx)
	if err := s.failures[req.GetBlockNumber()]; err != nil {
		return nil, err
	}
	blk := s.blocks[req.GetBlockNumber()]
	if blk == nil {
		return &evm.GetBlockResponse{}, nil
	}
	return &evm.GetBlockResponse{Block: blk}, nil
}

// startFakeBdsServer starts a fake BDS server on a loopback port and returns
// its address, the server, and a stop function. The server is stopped when the
// test ends even if the caller forgets.
func startFakeBdsServer(t *testing.T, chainID uint64) (string, *fakeBdsServer, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "the fake BDS server could not listen")

	srv := grpc.NewServer()
	fake := newFakeBdsServer(chainID)
	evm.RegisterRPCQueryServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()

	var once sync.Once
	stop := func() { once.Do(srv.Stop) }
	t.Cleanup(stop)
	return lis.Addr().String(), fake, stop
}
