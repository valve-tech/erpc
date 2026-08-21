package integrity

import (
	"testing"
	"time"
)

// BlockRef tells the resolver how to re-fetch the canonical version of a
// response the engine has doubts about. The choice matters: a block HASH pins
// one specific block forever, while a block NUMBER means "whatever is at that
// height now". Re-fetching by number across a reorg returns a different, valid
// block, so a real corruption and a routine reorg become indistinguishable and
// the engine either clears a genuine fault or condemns a healthy upstream.

func TestBlockRef_PrefersTheHashOverTheHeightForEverySource(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		method string
		raw    string
		want   string
	}{
		{
			name:   "block header",
			method: MethodGetBlockByNumber,
			raw:    `{"hash":"0xaaa","number":"0x10"}`,
			want:   "0xaaa",
		},
		{
			name:   "block receipts",
			method: MethodGetBlockReceipts,
			raw:    `[{"blockHash":"0xbbb","blockNumber":"0x10"}]`,
			want:   "0xbbb",
		},
		{
			name:   "single receipt",
			method: MethodGetTransactionReceipt,
			raw:    `{"blockHash":"0xccc","blockNumber":"0x10"}`,
			want:   "0xccc",
		},
		{
			name:   "logs",
			method: MethodGetLogs,
			raw:    `[{"blockHash":"0xddd","blockNumber":"0x10"}]`,
			want:   "0xddd",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := newDecoded(tc.method, []byte(tc.raw)).BlockRef()
			if got != tc.want {
				t.Fatalf("BlockRef() = %q, want the immutable hash %q; a height re-fetch cannot survive a reorg", got, tc.want)
			}
		})
	}
}

func TestBlockRef_FallsBackToTheHeightWhenNoHashIsCarried(t *testing.T) {
	t.Parallel()

	// Some upstreams omit blockHash on receipts and logs. A height is weaker
	// than a hash but far better than nothing: with an empty ref the resolver
	// cannot re-fetch at all and the engine has to drop the verdict.
	for _, tc := range []struct {
		name   string
		method string
		raw    string
	}{
		{name: "receipts without a block hash", method: MethodGetBlockReceipts, raw: `[{"blockNumber":"0x10"}]`},
		{name: "logs without a block hash", method: MethodGetLogs, raw: `[{"blockNumber":"0x10"}]`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := newDecoded(tc.method, []byte(tc.raw)).BlockRef(); got != "0x10" {
				t.Fatalf("BlockRef() = %q, want the height 0x10 as the fallback", got)
			}
		})
	}
}

func TestBlockRef_ReadsPastAHeaderThatCarriesNoHash(t *testing.T) {
	t.Parallel()

	// A header decoded with an empty hash must not stop the search. Returning
	// "" here would leave the resolver with nothing, even though the receipts
	// in the same response name the block exactly.
	d := newDecoded(MethodGetBlockByNumber, []byte(`{"number":"0x10"}`))
	if got := d.BlockRef(); got != "" {
		t.Fatalf("BlockRef() = %q; a header response carries no receipts or logs to fall back on", got)
	}

	// The receipts path is where the fallback actually happens.
	r := newDecoded(MethodGetBlockReceipts, []byte(`[{"blockHash":"","blockNumber":"0x10"}]`))
	if got := r.BlockRef(); got != "0x10" {
		t.Fatalf("BlockRef() = %q, want 0x10; an empty blockHash must fall through to the height", got)
	}
}

func TestBlockRef_ReturnsNothingRatherThanGuessing(t *testing.T) {
	t.Parallel()

	// An empty ref is the resolver's signal to skip. Inventing one would make it
	// fetch the wrong block and compare unrelated data.
	for _, tc := range []struct {
		name   string
		method string
		raw    string
	}{
		{name: "empty receipt array", method: MethodGetBlockReceipts, raw: `[]`},
		{name: "empty log array", method: MethodGetLogs, raw: `[]`},
		{name: "null result", method: MethodGetBlockByNumber, raw: `null`},
		{name: "garbled body", method: MethodGetBlockByNumber, raw: `not json`},
		{name: "a method this package does not decode", method: "eth_call", raw: `"0x1"`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := newDecoded(tc.method, []byte(tc.raw)).BlockRef(); got != "" {
				t.Fatalf("BlockRef() = %q, want an empty ref so the resolver skips instead of fetching the wrong block", got)
			}
		})
	}
}

func TestFailureClass_LabelsBothVerdictsDistinctly(t *testing.T) {
	t.Parallel()

	// The label lands in logs and in the integrity archive. An operator triaging
	// an alert reads it to decide whether to retry or to pull the upstream, so
	// the two classes must never print the same word.
	if Deterministic.String() != "deterministic" {
		t.Fatalf("Deterministic.String() = %q", Deterministic.String())
	}
	if ReorgSensitive.String() != "reorg-sensitive" {
		t.Fatalf("ReorgSensitive.String() = %q", ReorgSensitive.String())
	}
	if Deterministic.String() == ReorgSensitive.String() {
		t.Fatal("both failure classes print the same label; triage cannot tell them apart")
	}
}

func TestHasChecks_AnswersTheCheapSkipQuestionCaseInsensitively(t *testing.T) {
	t.Parallel()

	// Every response asks this before the engine is built. A false negative
	// silently disables integrity checking for that method, and a client can
	// trigger it just by changing the casing it sends.
	for _, method := range []string{
		"eth_getBlockByNumber",
		"ETH_GETBLOCKBYNUMBER",
		"eth_getblockbynumber",
		"eth_getLogs",
	} {
		if !HasChecks(method) {
			t.Fatalf("HasChecks(%q) = false; integrity checking would be skipped for this method", method)
		}
	}

	// The fallthrough: a method nobody registered a check for must skip cheaply.
	for _, method := range []string{"eth_call", "some_brandNewMethod", ""} {
		if HasChecks(method) {
			t.Fatalf("HasChecks(%q) = true; the engine would be built for a method with no checks", method)
		}
	}
}

func TestArchitectureByName_ResolvesAKnownFamilyAndRefusesAnUnknownOne(t *testing.T) {
	t.Parallel()

	// The families are the chain-shape profiles the checks run against. A silent
	// zero-value on an unknown name would apply post-merge Ethereum invariants
	// to a chain that does not satisfy them, and every response would look
	// corrupt.
	if _, ok := ArchitectureByName("neither-a-family-nor-anything-else"); ok {
		t.Fatal("ArchitectureByName invented a family for an unknown name")
	}

	// Resolve whatever family the profile table actually uses, rather than
	// hard-coding a name this test would then have to chase.
	for chainId := range chains {
		spec := chains[chainId]
		if spec.Architecture == "" {
			continue
		}
		a, ok := ArchitectureByName(spec.Architecture)
		if !ok {
			t.Fatalf("chain %d names family %q, which ArchitectureByName cannot resolve", chainId, spec.Architecture)
		}
		if a.Header == nil && a.Fee == nil && len(a.Disable) == 0 && a.StateContext == nil {
			t.Fatalf("family %q resolved to an entirely empty profile", spec.Architecture)
		}
		return
	}
	t.Skip("no chain in the profile table names a family")
}

func TestExhaustionWindow_MatchesTheWindowTheTrackerActuallyUses(t *testing.T) {
	t.Parallel()

	// The alert line prints this window next to the count. If the accessor and
	// the tracker ever disagree, every exhaustion alert states a rate that is
	// simply wrong, and an operator sizes their retry budget from it.
	if ExhaustionWindow() != exhaustionWindow {
		t.Fatalf("ExhaustionWindow() = %v but the tracker uses %v; the alert would misstate the rate",
			ExhaustionWindow(), exhaustionWindow)
	}
	if ExhaustionWindow() <= 0 {
		t.Fatalf("ExhaustionWindow() = %v; a non-positive window makes the alert rate meaningless", ExhaustionWindow())
	}
	// Sanity bound: a window far outside minutes-to-hours would make the alert
	// either constant noise or effectively silent.
	if ExhaustionWindow() < time.Minute || ExhaustionWindow() > 24*time.Hour {
		t.Fatalf("ExhaustionWindow() = %v, outside any workable alerting range", ExhaustionWindow())
	}
}
