package svm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
)

// nonSvmUpstream stands for any upstream that does not carry an SVM state
// poller — an EVM or BTC upstream sharing a pool, or an SVM upstream still
// mid-bootstrap.
func nonSvmUpstream(t *testing.T, id string) common.Upstream {
	t.Helper()
	u := common.NewFakeUpstream(id)
	if _, isSvm := common.Upstream(u).(common.SvmUpstream); isSvm {
		t.Fatalf("test premise broken: %s implements SvmUpstream", id)
	}
	return u
}

// upstreamWithNoPoller is an SVM upstream whose poller has not been built yet.
func upstreamWithNoPoller(id string) common.Upstream {
	return &slotLagUpstream{id: id, poller: nil}
}

// upstreamWithNullPoller is an SVM upstream holding a poller that reports
// itself as the null object — bootstrap started but produced nothing.
func upstreamWithNullPoller(id string) common.Upstream {
	return &slotLagUpstream{id: id, poller: &pollerAtSlot{null: true}}
}

// An upstream that is not SVM must never be dropped by an SVM slot rule.
// Dropping it would silently remove another chain's node from a mixed pool,
// and the request it was carrying would fail with no explanation.
func TestFilterByFinalizedSlotLag_NeverDropsANonSvmUpstream(t *testing.T) {
	other := nonSvmUpstream(t, "evm-1")
	ups := []common.Upstream{
		upstreamAt("current", 1000),
		other,
		upstreamAt("stale", 100),
	}

	got := FilterByFinalizedSlotLag(ups, 50, 1000)

	if len(got) != 2 {
		t.Fatalf("filter kept %d upstreams, want 2", len(got))
	}
	found := false
	for _, u := range got {
		if u == other {
			found = true
		}
	}
	if !found {
		t.Fatal("a non-SVM upstream was dropped by the SVM slot-lag rule")
	}
}

// A poller that does not exist yet, or reports itself null, means bootstrap has
// not finished. Excluding such an upstream would break forwarding for every
// newly-registered node during the window it needs to come up.
func TestFilterByFinalizedSlotLag_NeverDropsAnUpstreamStillBootstrapping(t *testing.T) {
	cases := map[string]common.Upstream{
		"no poller yet":      upstreamWithNoPoller("booting"),
		"null-object poller": upstreamWithNullPoller("booting"),
	}
	for name, booting := range cases {
		t.Run(name, func(t *testing.T) {
			ups := []common.Upstream{upstreamAt("current", 1000), booting}

			got := FilterByFinalizedSlotLag(ups, 50, 1000)

			if len(got) != 2 {
				t.Fatalf("filter kept %d upstreams, want 2 — a bootstrapping node was excluded", len(got))
			}
		})
	}
}

// HighestFinalizedSlot must skip upstreams with no usable poller rather than
// counting them as zero or panicking. It feeds the reference slot, so a panic
// here takes down every consensus request.
func TestHighestFinalizedSlot_SkipsUpstreamsWithNoUsablePoller(t *testing.T) {
	ups := []common.Upstream{
		upstreamWithNoPoller("booting"),
		upstreamWithNullPoller("null"),
		nonSvmUpstream(t, "evm-1"),
		upstreamAt("current", 4242),
	}

	if got := HighestFinalizedSlot(ups); got != 4242 {
		t.Fatalf("HighestFinalizedSlot = %d, want 4242", got)
	}
}

// ReferenceFinalizedSlot must ignore the same upstreams. A non-SVM upstream in
// the pool must not become the reference, and a bootstrapping one must not
// drag it to zero.
func TestReferenceFinalizedSlot_IgnoresNonSvmAndBootstrappingUpstreams(t *testing.T) {
	ups := []common.Upstream{
		nonSvmUpstream(t, "evm-1"),
		upstreamWithNoPoller("booting"),
		upstreamWithNullPoller("null"),
		upstreamAt("a", 1000),
		upstreamAt("b", 990),
	}

	if got := ReferenceFinalizedSlot(ups, 100); got != 1000 {
		t.Fatalf("ReferenceFinalizedSlot = %d, want 1000", got)
	}
}

// The runner-up must be tracked even when it arrives AFTER the leader. The
// clamp that defends against a single lying upstream needs the second-highest
// slot; if the scan only updated it while the leader changed, an out-of-order
// pool would leave it at zero and the clamp would never engage.
func TestReferenceFinalizedSlot_TracksTheRunnerUpWhateverTheOrder(t *testing.T) {
	// The liar is first, so the honest runner-up is only ever seen by the
	// "not a new leader but higher than second" branch.
	ups := []common.Upstream{
		upstreamAt("liar", 9_000_000),
		upstreamAt("honest-a", 1000),
		upstreamAt("honest-b", 999),
	}

	got := ReferenceFinalizedSlot(ups, 100)
	if got != 1000 {
		t.Fatalf("reference = %d, want the honest runner-up 1000; a single liar set the bar", got)
	}
}

// minContextSlot filtering must apply the same defensive rules: a non-SVM
// upstream is not judged by an SVM slot, and a bootstrapping one is not
// excluded before it has reported anything.
func TestFilterByMinContextSlot_NeverDropsNonSvmOrBootstrappingUpstreams(t *testing.T) {
	other := nonSvmUpstream(t, "evm-1")
	booting := upstreamWithNoPoller("booting")
	ups := []common.Upstream{
		other,
		booting,
		upstreamAtSlots("behind", 500, 500),
		upstreamAtSlots("ahead", 2000, 2000),
	}

	got := FilterByMinContextSlot(ups, 1000, false)

	if len(got) != 3 {
		t.Fatalf("filter kept %d upstreams, want 3 (both defensive cases plus the ahead node)", len(got))
	}
	for _, u := range got {
		if su, ok := u.(*slotLagUpstream); ok && su.id == "behind" {
			t.Fatal("an upstream known to be behind minContextSlot was still forwarded to")
		}
	}
}

// MinContextSlotOf must return 0 for a nil request rather than panicking. It is
// called on every SVM request, so a panic here is a whole-process outage.
func TestMinContextSlotOf_NilRequestIsZeroNotAPanic(t *testing.T) {
	if got := MinContextSlotOf(context.Background(), nil); got != 0 {
		t.Fatalf("MinContextSlotOf(nil) = %d, want 0", got)
	}
}

// A request whose body is not JSON-RPC has no minContextSlot to find. Returning
// 0 lets the request take the normal path, where the real parse error surfaces
// with a useful message.
func TestMinContextSlotOf_UnparseableRequestIsZero(t *testing.T) {
	r := common.NewNormalizedRequest([]byte(`definitely not json`))
	if got := MinContextSlotOf(context.Background(), r); got != 0 {
		t.Fatalf("MinContextSlotOf on an unparseable request = %d, want 0", got)
	}
}
