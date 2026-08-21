package btc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/erpc/erpc/common"
)

// A testnet bitcoind in a mainnet pool serves testnet blocks to mainnet
// clients, and every one of them is wrong. These tests pin the two halves that
// stop it: the probe carries the node's own answer, and the family decides
// whether that answer means the configured chain.

// chainInfoNamed builds a getblockchaininfo payload for a caught-up node on
// `chain`. An empty `chain` omits the field, which is the older client.
func chainInfoNamed(t *testing.T, chain string) []byte {
	t.Helper()
	result := map[string]interface{}{
		"blocks":               812345,
		"headers":              812345,
		"verificationprogress": 0.999999,
		"initialblockdownload": false,
	}
	if chain != "" {
		result["chain"] = chain
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal chainInfoNamed: %v", err)
	}
	return b
}

func TestProbe_CarriesTheChainTheNodeReported(t *testing.T) {
	// Verbatim. bitcoind's word is the evidence; anything eRPC rewrites here it
	// can no longer show an operator.
	for _, want := range []string{"main", "test", "regtest", "signet", "testnet4"} {
		got := New().Probe(context.Background(), &cannedCaller{result: chainInfoNamed(t, want)})
		if got.Chain != want {
			t.Fatalf("probe reported chain %q, want %q verbatim", got.Chain, want)
		}
	}
}

func TestProbe_MissingChainIsEmptyNotGuessed(t *testing.T) {
	// A client that omits the field states nothing. Filling in a plausible
	// "main" would manufacture the evidence the mismatch check runs on.
	got := New().Probe(context.Background(), &cannedCaller{result: chainInfoNamed(t, "")})
	if got.Chain != "" {
		t.Fatalf("probe invented chain %q for a node that reported none", got.Chain)
	}
	if got.Liveness != common.ChainHealthy {
		t.Fatalf("liveness = %v, want healthy: a missing chain name is not an outage", got.Liveness)
	}
}

func TestProbe_ChainIsReportedEvenWhenSyncing(t *testing.T) {
	// The wrong chain is worth refusing whether or not the node is caught up,
	// so the evidence must survive every liveness verdict.
	c := &cannedCaller{result: chainInfoJSONNamed(t, "test", 700000, 812345)}
	got := New().Probe(context.Background(), c)
	if got.Liveness == common.ChainHealthy {
		t.Fatalf("liveness = healthy for a node 112345 blocks behind its headers")
	}
	if got.Chain != "test" {
		t.Fatalf("syncing probe reported chain %q, want test", got.Chain)
	}
}

// chainInfoJSONNamed is chainInfoJSON with the chain name under the test's
// control.
func chainInfoJSONNamed(t *testing.T, chain string, blocks, headers int64) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{
		"chain":                chain,
		"blocks":               blocks,
		"headers":              headers,
		"verificationprogress": 0.999999,
		"initialblockdownload": false,
	})
	if err != nil {
		t.Fatalf("marshal chainInfoJSONNamed: %v", err)
	}
	return b
}

func TestMatchesConfiguredChain(t *testing.T) {
	f := New()
	for _, tc := range []struct {
		name       string
		configured string
		observed   string
		want       bool
	}{
		// The pair that made this hard, and the one operators actually write.
		{"mainnet config, mainnet node", "mainnet", "main", true},
		{"testnet config, testnet node", "testnet", "test", true},
		// The bug. Live evidence: a testnet endpoint reports "test".
		{"mainnet config, testnet node", "mainnet", "test", false},
		{"testnet config, mainnet node", "testnet", "main", false},
		// Names bitcoind does not shorten pass through untouched.
		{"regtest", "regtest", "regtest", true},
		{"signet", "signet", "signet", true},
		{"regtest config, mainnet node", "regtest", "main", false},
		{"signet config, testnet node", "signet", "test", false},
		// An operator who writes the wire value must not be punished for it.
		{"operator wrote the wire value", "main", "main", true},
		// Case is not identity. A node shouting its name is the same node.
		{"case folded", "Mainnet", "MAIN", true},
		// testnet3 and testnet4 are different chains, and Bitcoin Core names
		// the newer one in full. Nothing here may confuse them.
		{"testnet config, testnet4 node", "testnet", "testnet4", false},
		{"testnet4 config, testnet4 node", "testnet4", "testnet4", true},
		{"testnet4 config, mainnet node", "testnet4", "main", false},
		// A private signet or a bitcoind-compatible chain names itself, and the
		// rule holds without eRPC knowing the name.
		{"private signet", "mysignet", "mysignet", true},
		{"unrelated names", "mainnet", "doge", false},
		// A node that reports NO chain is not in this table on purpose. "No
		// evidence" is not a naming question, so the caller answers it once for
		// every family instead — see the bootstrap tests in upstream/.
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.MatchesConfiguredChain(tc.configured, tc.observed); got != tc.want {
				t.Fatalf("MatchesConfiguredChain(%q, %q) = %v, want %v",
					tc.configured, tc.observed, got, tc.want)
			}
		})
	}
}

func TestMatchesConfiguredChain_AgainstAFakeBitcoind(t *testing.T) {
	// The whole path on the real wire shape: what a node answers, what the
	// probe carries, and what the family makes of it.
	node := newFakeBitcoind(t, 812345, 812345)
	node.chain = "test"

	probe := New().Probe(context.Background(), callerFor(node))
	if probe.Chain != "test" {
		t.Fatalf("probe.Chain = %q, want test from the wire", probe.Chain)
	}
	if New().MatchesConfiguredChain("mainnet", probe.Chain) {
		t.Fatal("a testnet bitcoind matched a mainnet pool; it would serve testnet blocks to mainnet clients")
	}
	if !New().MatchesConfiguredChain("testnet", probe.Chain) {
		t.Fatal("a testnet bitcoind was refused from a testnet pool")
	}
}
