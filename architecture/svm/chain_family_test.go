package svm

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// scriptedCaller answers each method from a table and records the call order,
// so a test can pin what a probe costs an upstream per tick.
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
	return 0, nil, errors.New("svm family must not use REST")
}

func TestChainFamily_IsRegisteredUnderSvm(t *testing.T) {
	f, ok := common.LookupChainFamily(common.ArchitectureSvm)
	if !ok {
		t.Fatal("svm chain family not registered; every registry-driven gate " +
			"stops answering for svm")
	}
	if f.Transport() != common.TransportJsonRpc {
		t.Fatalf("transport = %v, want jsonrpc", f.Transport())
	}
}

func TestChainFamily_ValidateNetworkIdMatchesUtil(t *testing.T) {
	// The registered family and util's builtin are the same rule reached two
	// ways. If they drift, linking this package changes which configs load.
	f := &ChainFamily{}
	for _, body := range []string{"mainnet-beta", "devnet", "fogo:mainnet", "", "a:b:c", "bad/cluster", ":x"} {
		if got, want := f.ValidateNetworkId(body), util.IsSvmNetworkIdBody(body); got != want {
			t.Errorf("ValidateNetworkId(%q) = %v, util builtin = %v", body, got, want)
		}
	}
}

func TestChainFamily_BackCompatSvmIdsStillValidate(t *testing.T) {
	// "svm:<cluster>" is the pre-multi-chain form. Every config and cache key
	// written before the chain prefix existed uses it, so it must keep
	// validating with this package linked and the REGISTERED shape answering.
	for _, id := range []string{"svm:mainnet-beta", "svm:devnet", "svm:testnet", "svm:fogo:mainnet"} {
		if !util.IsValidNetworkId(id) {
			t.Errorf("util.IsValidNetworkId(%q) = false; existing svm configs stop loading", id)
		}
	}
	for _, id := range []string{"svm:", "svm:a:b:c", "svm::"} {
		if util.IsValidNetworkId(id) {
			t.Errorf("util.IsValidNetworkId(%q) = true; want the pre-registry rejection", id)
		}
	}
}

func TestSupportsEndpointScheme_OnlyHttp(t *testing.T) {
	// SVM upstreams were http/https-only before the client factory became
	// registry-driven, and that restriction is not incidental: nothing in the
	// SVM path has been run against the WebSocket or gRPC clients. The family
	// carries the restriction now, so the factory does not have to know it.
	f := &ChainFamily{}
	for _, scheme := range []string{"http", "https"} {
		if ok, _ := f.SupportsEndpointScheme(scheme); !ok {
			t.Errorf("SupportsEndpointScheme(%q) = false; svm upstreams must keep working", scheme)
		}
	}
	for _, scheme := range []string{"ws", "wss", "grpc", "grpc+bds"} {
		ok, reason := f.SupportsEndpointScheme(scheme)
		if ok {
			t.Errorf("SupportsEndpointScheme(%q) = true; this silently enables an "+
				"svm transport nobody has run", scheme)
		}
		if reason == "" {
			t.Errorf("SupportsEndpointScheme(%q) refused without a reason for the operator", scheme)
		}
	}
}

func TestProbe_HealthyNodeReportsItsSlot(t *testing.T) {
	c := &scriptedCaller{answers: map[string][]byte{
		"getSlot":   []byte(`312345678`),
		"getHealth": []byte(`"ok"`),
	}}
	got := (&ChainFamily{}).Probe(context.Background(), c)

	if got.Liveness != common.ChainHealthy {
		t.Fatalf("liveness = %v (%s), want healthy", got.Liveness, got.Detail)
	}
	if got.Tip != 312345678 {
		t.Fatalf("tip = %d, want the reported slot", got.Tip)
	}
}

func TestProbe_UnreachableNodeIsDownAndKeepsTheCause(t *testing.T) {
	boom := errors.New("dial tcp 127.0.0.1:8899: connection refused")
	c := &scriptedCaller{fails: map[string]error{"getSlot": boom}}
	got := (&ChainFamily{}).Probe(context.Background(), c)

	if got.Liveness != common.ChainDown {
		t.Fatalf("liveness = %v, want down", got.Liveness)
	}
	if !errors.Is(got.Err, boom) {
		t.Fatalf("probe error %v does not wrap the cause", got.Err)
	}
}

func TestProbe_ReachableButUnhealthyIsSyncingNotDown(t *testing.T) {
	// getSlot answered, so the node is reachable — a getHealth failure after
	// that is Solana's "node is behind" (-32005), not an outage. Calling it
	// down would hide a recoverable lag behind an outage alert, and calling it
	// healthy would serve stale reads.
	c := &scriptedCaller{
		answers: map[string][]byte{"getSlot": []byte(`312345678`)},
		fails:   map[string]error{"getHealth": errors.New("Node is behind by 1500 slots")},
	}
	got := (&ChainFamily{}).Probe(context.Background(), c)

	if got.Liveness != common.ChainSyncing {
		t.Fatalf("liveness = %v (%s), want syncing", got.Liveness, got.Detail)
	}
	if got.Tip != 312345678 {
		t.Fatalf("tip = %d, want the node's own slot even while behind", got.Tip)
	}
	if got.Detail == "" {
		t.Error("syncing probe carried no reason for the operator")
	}
}

func TestProbe_UndecodableSlotIsDownNotHealthy(t *testing.T) {
	c := &scriptedCaller{answers: map[string][]byte{"getSlot": []byte(`"not-a-slot"`)}}
	got := (&ChainFamily{}).Probe(context.Background(), c)

	if got.Liveness == common.ChainHealthy {
		t.Fatal("a node answering nonsense reported healthy")
	}
	if got.Err == nil {
		t.Error("undecodable slot produced no error")
	}
}

func TestProbe_ZeroSlotIsNotHealthy(t *testing.T) {
	c := &scriptedCaller{answers: map[string][]byte{
		"getSlot":   []byte(`0`),
		"getHealth": []byte(`"ok"`),
	}}
	if got := (&ChainFamily{}).Probe(context.Background(), c); got.Liveness == common.ChainHealthy {
		t.Fatalf("a node at slot 0 reported healthy (%s)", got.Detail)
	}
}

func TestClassify_NonRetryableWriteIsNeverRotated(t *testing.T) {
	// sendTransaction may still propagate from the node that appeared to fail,
	// and requestAirdrop MINTS per call. Rotating either one duplicates an
	// effect that already happened. The guard has to beat the missing-data
	// rule below, so it is asserted with that error code set.
	f := &ChainFamily{}
	for _, m := range []string{"sendTransaction", "sendRawTransaction", "requestAirdrop"} {
		got := f.Classify(common.ClassifyInput{Method: m, ErrCode: common.ErrCodeEndpointMissingData})
		if got == common.VerdictRotate {
			t.Errorf("Classify(%s) = rotate; a write must never be re-sent elsewhere", m)
		}
	}
}

func TestClassify_MissingDataRotates(t *testing.T) {
	// Solana reports a pruned block or a lost transaction history as an ERROR
	// (-32004, -32007, -32011 — see error_normalizer.go), which eRPC normalizes
	// to ErrCodeEndpointMissingData. A peer with deeper history can answer, so
	// this is the SVM case that is worth another upstream.
	got := (&ChainFamily{}).Classify(common.ClassifyInput{
		Method:  "getBlock",
		ErrCode: common.ErrCodeEndpointMissingData,
	})
	if got != common.VerdictRotate {
		t.Fatalf("Classify(getBlock, missing data) = %v, want rotate", got)
	}
}

func TestClassify_EmptyResultIsServed(t *testing.T) {
	// Negative control for the test above, and the emptyResultAccept lesson in
	// SVM form: a null getAccountInfo means the account does not exist, and a
	// node that lacks a block says so with an error code rather than with a
	// null. So an empty result is never a reason to rotate here — doing that
	// would re-ask every upstream for the same null.
	f := &ChainFamily{}
	for _, m := range []string{"getAccountInfo", "getBlock", "getTransaction", "getSignaturesForAddress"} {
		if got := f.Classify(common.ClassifyInput{Method: m, IsEmpty: true}); got != common.VerdictServe {
			t.Errorf("Classify(%s, empty) = %v, want serve", m, got)
		}
	}
}

func TestClassify_ClientErrorDoesNotBurnOtherUpstreams(t *testing.T) {
	got := (&ChainFamily{}).Classify(common.ClassifyInput{
		Method:  "getAccountInfo",
		ErrCode: common.ErrCodeEndpointClientSideException,
	})
	if got != common.VerdictClientError {
		t.Fatalf("Classify(client-side error) = %v, want clientError", got)
	}
}

func TestMatchesConfiguredChain_ClusterNamesMatchExactly(t *testing.T) {
	// Solana writes its cluster names in full on both sides, so equality
	// decides every case and anything looser would let one cluster pass for
	// another on a resemblance the names do not have.
	f := &ChainFamily{}
	for _, tc := range []struct {
		configured string
		observed   string
		want       bool
	}{
		{"mainnet-beta", "mainnet-beta", true},
		{"devnet", "devnet", true},
		{"testnet", "devnet", false},
		{"mainnet-beta", "mainnet", false},
		{"devnet", "", false},
	} {
		if got := f.MatchesConfiguredChain(tc.configured, tc.observed); got != tc.want {
			t.Errorf("MatchesConfiguredChain(%q, %q) = %v, want %v",
				tc.configured, tc.observed, got, tc.want)
		}
	}
}
