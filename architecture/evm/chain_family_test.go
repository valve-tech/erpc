package evm

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// scriptedCaller answers each method from a table, and records the order it
// was asked in. A probe that asks for more than it needs costs every upstream
// an extra round trip on every poll tick, so the order and the count are part
// of what these tests pin.
type scriptedCaller struct {
	answers map[string][]byte
	fails   map[string]error
	calls   []string
}

func (s *scriptedCaller) CallJsonRpc(_ context.Context, method string, _ []interface{}) ([]byte, error) {
	s.calls = append(s.calls, method)
	if err, ok := s.fails[method]; ok {
		return nil, err
	}
	if raw, ok := s.answers[method]; ok {
		return raw, nil
	}
	return nil, errors.New("scriptedCaller: unexpected method " + method)
}

func (s *scriptedCaller) CallREST(context.Context, string, string) (int, []byte, error) {
	return 0, nil, errors.New("evm family must not use REST")
}

func TestChainFamily_IsRegisteredUnderEvm(t *testing.T) {
	// Without this registration every registry-driven gate — architecture
	// validation, the client factory, network-id validation — stops answering
	// for the fork's biggest architecture.
	f, ok := common.LookupChainFamily(common.ArchitectureEvm)
	if !ok {
		t.Fatal("evm chain family not registered; package init() did not run or failed")
	}
	if f.Transport() != common.TransportJsonRpc {
		t.Fatalf("transport = %v, want jsonrpc", f.Transport())
	}
}

func TestChainFamily_ValidateNetworkIdMatchesUtil(t *testing.T) {
	// The registered family and util's builtin must agree on every string:
	// they are the same rule reached by two paths, and a binary that links
	// this package must not validate ids differently from one that does not.
	f := &ChainFamily{}
	for _, body := range []string{"1", "42161", "0", "-5", "abc", "", "1:2", "0x1"} {
		if got, want := f.ValidateNetworkId(body), util.IsEvmNetworkIdBody(body); got != want {
			t.Errorf("ValidateNetworkId(%q) = %v, util builtin = %v; the two paths "+
				"have drifted and linking evm now changes which configs load", body, got, want)
		}
	}
}

func TestChainFamily_EvmNetworkIdsStillValidateThroughUtil(t *testing.T) {
	// The behaviour every existing config and cache key depends on, asserted
	// with this package linked (so the REGISTERED shape is what answers).
	for _, id := range []string{"evm:1", "evm:42161", "evm:11155111"} {
		if !util.IsValidNetworkId(id) {
			t.Errorf("util.IsValidNetworkId(%q) = false; existing evm configs stop loading", id)
		}
	}
	for _, id := range []string{"evm:", "evm:abc", "evm:1:2"} {
		if util.IsValidNetworkId(id) {
			t.Errorf("util.IsValidNetworkId(%q) = true; want the pre-registry rejection", id)
		}
	}
}

func TestProbe_SyncedNodeIsHealthyAndReportsTheTip(t *testing.T) {
	c := &scriptedCaller{answers: map[string][]byte{
		"eth_syncing":     []byte(`false`),
		"eth_blockNumber": []byte(`"0xc65d40"`),
	}}
	got := (&ChainFamily{}).Probe(context.Background(), c)

	if got.Liveness != common.ChainHealthy {
		t.Fatalf("liveness = %v (%s), want healthy", got.Liveness, got.Detail)
	}
	if got.Tip != 0xc65d40 {
		t.Fatalf("tip = %d, want %d", got.Tip, 0xc65d40)
	}
}

func TestProbe_SyncingNodeNeverAsksForTheHead(t *testing.T) {
	// A node that admits it is syncing is already answered. Asking it for
	// eth_blockNumber as well doubles probe traffic against exactly the node
	// that is least able to serve it.
	c := &scriptedCaller{answers: map[string][]byte{
		"eth_syncing": []byte(`{"startingBlock":"0x0","currentBlock":"0x10","highestBlock":"0xc65d40"}`),
	}}
	got := (&ChainFamily{}).Probe(context.Background(), c)

	if got.Liveness != common.ChainSyncing {
		t.Fatalf("liveness = %v (%s), want syncing", got.Liveness, got.Detail)
	}
	if got.Tip != 0x10 {
		t.Fatalf("tip = %d, want the node's own currentBlock 16", got.Tip)
	}
	if len(c.calls) != 1 {
		t.Fatalf("probe made %d calls (%v); a syncing node needs no second call", len(c.calls), c.calls)
	}
	if got.Detail == "" {
		t.Error("syncing probe carried no reason for the operator")
	}
}

func TestProbe_UnreachableNodeIsDownAndKeepsTheCause(t *testing.T) {
	boom := errors.New("dial tcp 127.0.0.1:8545: connection refused")
	c := &scriptedCaller{fails: map[string]error{"eth_syncing": boom}}
	got := (&ChainFamily{}).Probe(context.Background(), c)

	if got.Liveness != common.ChainDown {
		t.Fatalf("liveness = %v, want down", got.Liveness)
	}
	if !errors.Is(got.Err, boom) {
		t.Fatalf("probe error %v does not wrap the cause; an operator cannot tell "+
			"a refused dial from a bad answer", got.Err)
	}
}

func TestProbe_UndecodableHeadIsDownNotHealthy(t *testing.T) {
	// Fail closed. Failing open would put a node answering nonsense into
	// rotation with a tip of zero, which reads as "furthest behind".
	c := &scriptedCaller{answers: map[string][]byte{
		"eth_syncing":     []byte(`false`),
		"eth_blockNumber": []byte(`"not-hex"`),
	}}
	got := (&ChainFamily{}).Probe(context.Background(), c)

	if got.Liveness == common.ChainHealthy {
		t.Fatal("a node answering nonsense reported healthy")
	}
	if got.Err == nil {
		t.Error("undecodable head produced no error")
	}
}

func TestProbe_ZeroHeightIsNotHealthy(t *testing.T) {
	// A node that has imported nothing answers every query with "not found".
	c := &scriptedCaller{answers: map[string][]byte{
		"eth_syncing":     []byte(`false`),
		"eth_blockNumber": []byte(`"0x0"`),
	}}
	if got := (&ChainFamily{}).Probe(context.Background(), c); got.Liveness == common.ChainHealthy {
		t.Fatalf("a node at height 0 reported healthy (%s)", got.Detail)
	}
}

func TestClassify_EmptyCallResultIsServedNotRotated(t *testing.T) {
	// The measured bug this seam exists for: eth_call returning a 32-byte zero
	// word is a real answer (a zero balance, a false bool). Rotating on it
	// re-asked every upstream for the same zero — 299,997 empty responses drove
	// ~1.75M redundant calls on evm:369.
	got := (&ChainFamily{}).Classify(common.ClassifyInput{Method: "eth_call", IsEmpty: true})
	if got != common.VerdictServe {
		t.Fatalf("Classify(eth_call, empty) = %v, want serve", got)
	}
}

func TestClassify_EmptyTransactionLookupRotates(t *testing.T) {
	// Negative control for the test above. A null eth_getTransactionByHash IS
	// treated as missing data (DefaultMarkEmptyAsErrorMethods): another
	// upstream may hold the transaction, so serving the null would drop a
	// transaction the pool can see.
	got := (&ChainFamily{}).Classify(common.ClassifyInput{Method: "eth_getTransactionByHash", IsEmpty: true})
	if got != common.VerdictRotate {
		t.Fatalf("Classify(eth_getTransactionByHash, empty) = %v, want rotate", got)
	}
}

func TestClassify_EmptyReceiptIsServed(t *testing.T) {
	// eth_getTransactionReceipt is deliberately absent from BOTH default lists
	// (see the note on DefaultMarkEmptyAsErrorMethods): a null receipt for a
	// pending transaction is a real answer, and rotating on it drove
	// retry loops that outran the network timeout. A family that rotated on
	// every unlisted method would reintroduce that.
	got := (&ChainFamily{}).Classify(common.ClassifyInput{Method: "eth_getTransactionReceipt", IsEmpty: true})
	if got != common.VerdictServe {
		t.Fatalf("Classify(eth_getTransactionReceipt, empty) = %v, want serve", got)
	}
}

func TestClassify_ClientErrorDoesNotBurnOtherUpstreams(t *testing.T) {
	got := (&ChainFamily{}).Classify(common.ClassifyInput{
		Method:  "eth_call",
		ErrCode: common.ErrCodeEndpointClientSideException,
	})
	if got != common.VerdictClientError {
		t.Fatalf("Classify(client-side error) = %v, want clientError — a malformed "+
			"request fails identically on every upstream", got)
	}
}

func TestClassify_NonEmptyNeverRotates(t *testing.T) {
	// Negative control: the method list must only matter when the result is
	// actually empty.
	for _, m := range []string{"eth_call", "eth_getTransactionReceipt", "eth_chainId"} {
		if got := (&ChainFamily{}).Classify(common.ClassifyInput{Method: m}); got != common.VerdictServe {
			t.Errorf("Classify(%s, non-empty) = %v, want serve", m, got)
		}
	}
}
