package btc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
)

// cannedCaller answers one JSON-RPC method with a fixed payload. Using the
// ProbeCaller seam rather than an HTTP server keeps these tests about the
// FAMILY's judgement; the transport gets its own end-to-end test in
// probe_caller_test.go against a faked bitcoind.
type cannedCaller struct {
	result []byte
	err    error
	// calls records what the family asked for, so a test can assert the
	// family issues ONE probe rather than one per field it reads.
	calls []string
}

func (c *cannedCaller) CallJsonRpc(_ context.Context, method string, _ []interface{}) ([]byte, error) {
	c.calls = append(c.calls, method)
	return c.result, c.err
}
func (c *cannedCaller) CallREST(context.Context, string, string) (int, []byte, error) {
	return 0, nil, errors.New("btc family must not use REST")
}

// chainInfoJSON builds a getblockchaininfo payload.
func chainInfoJSON(t *testing.T, blocks, headers int64, progress float64, ibd bool) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{
		"chain":                "main",
		"blocks":               blocks,
		"headers":              headers,
		"verificationprogress": progress,
		"initialblockdownload": ibd,
	})
	if err != nil {
		t.Fatalf("marshal chainInfoJSON: %v", err)
	}
	return b
}

func TestFamily_Identity(t *testing.T) {
	f := New()
	if f.Family() != common.NetworkArchitecture("btc") {
		t.Fatalf("Family() = %q, want btc", f.Family())
	}
	if f.Transport() != common.TransportJsonRpc {
		t.Fatalf("Transport() = %v, want jsonrpc (bitcoind speaks JSON-RPC over HTTP POST)", f.Transport())
	}
}

func TestProbe_CaughtUpNodeIsHealthy(t *testing.T) {
	c := &cannedCaller{result: chainInfoJSON(t, 812345, 812345, 0.999999, false)}
	got := New().Probe(context.Background(), c)

	if got.Liveness != common.ChainHealthy {
		t.Fatalf("liveness = %v (%s), want healthy", got.Liveness, got.Detail)
	}
	if got.Tip != 812345 {
		t.Fatalf("tip = %d, want 812345", got.Tip)
	}
	if got.Err != nil {
		t.Fatalf("unexpected err: %v", got.Err)
	}
}

func TestProbe_IssuesExactlyOneCall(t *testing.T) {
	// The whole reason Probe returns liveness AND tip together. If this ever
	// becomes two calls, probe traffic doubles and the two answers can
	// disagree across the gap.
	c := &cannedCaller{result: chainInfoJSON(t, 100, 100, 1.0, false)}
	New().Probe(context.Background(), c)

	if len(c.calls) != 1 {
		t.Fatalf("probe made %d calls (%v), want exactly 1", len(c.calls), c.calls)
	}
	if c.calls[0] != "getblockchaininfo" {
		t.Fatalf("probe called %q, want getblockchaininfo", c.calls[0])
	}
}

func TestProbe_InitialBlockDownloadIsSyncingNotHealthy(t *testing.T) {
	// A node in IBD reports a plausible `blocks` value and a progress that can
	// round to ~1 on a fast sync. Trusting either alone serves stale data from
	// a node that has not validated the chain.
	//
	// blocks == headers and progress above the floor ON PURPOSE: every other
	// syncing signal is deliberately clean, so initialblockdownload is the ONLY
	// thing that can produce the verdict. An earlier version of this fixture
	// sat 345 blocks behind its headers, so the header-lag branch caught it and
	// the test passed even with the IBD check deleted.
	c := &cannedCaller{result: chainInfoJSON(t, 812345, 812345, 0.9999999, true)}
	got := New().Probe(context.Background(), c)

	if got.Liveness == common.ChainHealthy {
		t.Fatal("node in initialblockdownload reported healthy")
	}
	if got.Liveness != common.ChainSyncing {
		t.Fatalf("liveness = %v, want syncing", got.Liveness)
	}
	if got.Tip != 812345 {
		t.Fatalf("tip = %d, want the node's own 812345 even while syncing", got.Tip)
	}
	if got.Detail == "" {
		t.Error("syncing probe carried no reason for the operator")
	}
}

func TestProbe_BehindHeadersIsSyncing(t *testing.T) {
	// bitcoind knows the header chain before it has the blocks. A node 500
	// blocks behind its own headers is not serving the tip.
	c := &cannedCaller{result: chainInfoJSON(t, 811845, 812345, 0.99, false)}
	got := New().Probe(context.Background(), c)

	if got.Liveness != common.ChainSyncing {
		t.Fatalf("liveness = %v (%s), want syncing when blocks trail headers", got.Liveness, got.Detail)
	}
}

func TestProbe_WithinToleranceOfHeadersIsHealthy(t *testing.T) {
	// One block behind headers is normal: a block arrived and is being
	// connected. Treating that as syncing would flap every upstream on every
	// block and empty the pool.
	c := &cannedCaller{result: chainInfoJSON(t, 812344, 812345, 0.999999, false)}
	got := New().Probe(context.Background(), c)

	if got.Liveness != common.ChainHealthy {
		t.Fatalf("liveness = %v (%s), want healthy one block behind headers", got.Liveness, got.Detail)
	}
}

func TestProbe_TransportErrorIsDownAndKeepsTheCause(t *testing.T) {
	boom := errors.New("dial tcp 127.0.0.1:8332: connection refused")
	c := &cannedCaller{err: boom}
	got := New().Probe(context.Background(), c)

	if got.Liveness != common.ChainDown {
		t.Fatalf("liveness = %v, want down", got.Liveness)
	}
	if !errors.Is(got.Err, boom) {
		t.Fatalf("probe error %v does not wrap the transport cause; an operator "+
			"cannot tell a refused dial from a bad response", got.Err)
	}
	if got.Liveness.Serving() {
		t.Fatal("a down node reported as serving")
	}
}

func TestProbe_GarbageResponseIsDownNotHealthy(t *testing.T) {
	// Fail closed on an unparseable body. Failing open here would put a node
	// answering nonsense into rotation.
	c := &cannedCaller{result: []byte(`{"blocks": "not-a-number"`)}
	got := New().Probe(context.Background(), c)

	if got.Liveness != common.ChainDown {
		t.Fatalf("liveness = %v, want down on an undecodable response", got.Liveness)
	}
	if got.Err == nil {
		t.Error("undecodable response produced no error")
	}
}

func TestProbe_ZeroHeightIsNotHealthy(t *testing.T) {
	// A freshly-started node reports blocks:0. Serving from it would answer
	// every query with "not found".
	c := &cannedCaller{result: chainInfoJSON(t, 0, 812345, 0.0, false)}
	got := New().Probe(context.Background(), c)

	if got.Liveness == common.ChainHealthy {
		t.Fatal("a node at height 0 reported healthy")
	}
}

func TestClassify_EmptyBlockchainInfoNeverRotates(t *testing.T) {
	// Chain-state reads answer from the node's own view. Rotating on them
	// re-asks every peer for a value only this node can give.
	f := New()
	for _, m := range []string{"getblockchaininfo", "getblockcount", "getbestblockhash"} {
		got := f.Classify(common.ClassifyInput{Method: m, IsEmpty: true})
		if got != common.VerdictServe {
			t.Errorf("Classify(%s, empty) = %v, want serve", m, got)
		}
	}
}

func TestClassify_MissingTransactionRotates(t *testing.T) {
	// This is the case that justifies rotation on Bitcoin at all: a node
	// without txindex genuinely cannot answer getrawtransaction for an
	// arbitrary txid, while a peer with txindex can.
	got := New().Classify(common.ClassifyInput{Method: "getrawtransaction", IsEmpty: true})
	if got != common.VerdictRotate {
		t.Fatalf("Classify(getrawtransaction, empty) = %v, want rotate — a node "+
			"without txindex cannot answer, a peer with it can", got)
	}
}

func TestClassify_NonEmptyNeverRotates(t *testing.T) {
	// Negative control for the two tests above: the method list must only
	// matter when the result is actually empty.
	f := New()
	for _, m := range []string{"getrawtransaction", "getblock", "getblockchaininfo"} {
		if got := f.Classify(common.ClassifyInput{Method: m, IsEmpty: false}); got != common.VerdictServe {
			t.Errorf("Classify(%s, non-empty) = %v, want serve", m, got)
		}
	}
}

func TestClassify_ClientErrorDoesNotBurnOtherUpstreams(t *testing.T) {
	// A malformed request fails identically everywhere. Rotating it multiplies
	// one bad client call by the size of the pool.
	got := New().Classify(common.ClassifyInput{
		Method:  "getblock",
		ErrCode: common.ErrCodeEndpointClientSideException,
	})
	if got != common.VerdictClientError {
		t.Fatalf("Classify(client-side exception) = %v, want clientError", got)
	}
}

func TestClassify_MethodMatchIsCaseInsensitive(t *testing.T) {
	// Bitcoin RPC method names are lowercase by convention but callers vary.
	// A case-sensitive match would silently fall through to serve and
	// reintroduce the missing-transaction bug.
	if got := New().Classify(common.ClassifyInput{Method: "GetRawTransaction", IsEmpty: true}); got != common.VerdictRotate {
		t.Fatalf("Classify(GetRawTransaction, empty) = %v, want rotate", got)
	}
}

func TestRegistersItselfWithTheSharedRegistry(t *testing.T) {
	// The package's init() must make btc reachable the same way any other
	// family is, or none of the above is wired to anything.
	f, ok := common.LookupChainFamily("btc")
	if !ok {
		t.Fatal("btc family not registered; package init() did not run or failed")
	}
	if f.Transport() != common.TransportJsonRpc {
		t.Fatalf("registered btc family has transport %v", f.Transport())
	}
}
