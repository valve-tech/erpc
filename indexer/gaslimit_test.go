package indexer

import (
	"encoding/json"
	"testing"
)

func TestGasLimitTrackerFallbackBeforeFirstHead(t *testing.T) {
	tr := NewGasLimitTracker(0, nil)
	if got := tr.GasLimit("evm:1"); got != 60_000_000 {
		t.Fatalf("fallback = %d, want 60000000", got)
	}
	if got := NewGasLimitTracker(5_000_000_000, nil).GasLimit("evm:1"); got != GasLimitCeiling {
		t.Fatalf("fallback above ceiling = %d, want %d", got, GasLimitCeiling)
	}
}

func TestGasLimitTrackerObservesHex(t *testing.T) {
	tr := NewGasLimitTracker(0, nil)
	tr.Observe("evm:1", json.RawMessage(`{"gasLimit":"0x391bd79"}`))
	if got := tr.GasLimit("evm:1"); got != 59_882_873 {
		t.Fatalf("gasLimit = %d, want 59882873", got)
	}
	if got := MaxLogsPerBlock(tr.GasLimit("evm:1")); got != 159_688 {
		t.Fatalf("MaxLogsPerBlock = %d, want 159688", got)
	}
	if got := tr.GasLimit("evm:369"); got != 60_000_000 {
		t.Fatalf("other network = %d, want fallback", got)
	}
}

func TestGasLimitTrackerIgnoresGarbage(t *testing.T) {
	tr := NewGasLimitTracker(0, nil)
	tr.Observe("evm:1", json.RawMessage(`{"gasLimit":"0x2adf96d"}`))
	for _, p := range []string{`{}`, `{"gasLimit":"0x"}`, `{"gasLimit":"zz"}`, `{"gasLimit":"0xzz"}`,
		`{"gasLimit":12345}`, `{"gasLimit":"0x0"}`, `not json`} {
		tr.Observe("evm:1", json.RawMessage(p))
		if got := tr.GasLimit("evm:1"); got != 44_956_013 {
			t.Fatalf("after %s: gasLimit = %d, want unchanged 44956013", p, got)
		}
	}
}

func TestGasLimitTrackerClampsAtCeiling(t *testing.T) {
	tr := NewGasLimitTracker(0, nil)
	tr.Observe("evm:1", json.RawMessage(`{"gasLimit":"0x3b9aca01"}`))
	tr.Observe("evm:1", json.RawMessage(`{"gasLimit":"0x3b9aca02"}`)) // warns once only
	if got := tr.GasLimit("evm:1"); got != 1_000_000_000 {
		t.Fatalf("gasLimit = %d, want 1000000000", got)
	}
}

func TestMaxLogsPerBlockRoundsUp(t *testing.T) {
	for in, want := range map[uint64]int{375: 1, 376: 2, 44_956_013: 119_883, 30_029_238: 80_078} {
		if got := MaxLogsPerBlock(in); got != want {
			t.Fatalf("MaxLogsPerBlock(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestIngestFeedsGasLimit(t *testing.T) {
	idx := newIndexer(t)
	idx.RegisterNetwork(newFakeNetwork("evm:1", 0))
	ev := StreamEvent{
		Kind:      KindNewHead,
		NetworkId: "evm:1",
		SourceId:  "ws:a",
		Block:     BlockRef{Number: 1, Hash: "0x01"},
		Payload:   json.RawMessage(`{"gasLimit":"0x2adf96d"}`),
	}
	idx.Ingest(ev)
	if got := idx.GasLimits().GasLimit("evm:1"); got != 44_956_013 {
		t.Fatalf("after head: gasLimit = %d, want 44956013", got)
	}
	// A duplicate head is deduped but its gas limit still counts.
	ev.Payload = json.RawMessage(`{"gasLimit":"0x2adf96e"}`)
	idx.Ingest(ev)
	if got := idx.GasLimits().GasLimit("evm:1"); got != 44_956_014 {
		t.Fatalf("after duplicate head: gasLimit = %d, want 44956014", got)
	}
}
