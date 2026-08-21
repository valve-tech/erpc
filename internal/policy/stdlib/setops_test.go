package stdlib_test

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/stretchr/testify/require"
)

// Set operations and combinators are the plumbing every selection policy
// leans on. They never look dangerous in review, so they are exactly where
// an order or de-duplication bug survives: the policy still returns a
// plausible list, and the wrong upstream quietly serves every request.

type upSpec struct {
	id     string
	vendor string
	tags   []string
}

// runEval registers `ups` under `eval`, drives one tick and returns the
// resulting order. Input order is preserved, so a test can assert on it.
func runEval(t *testing.T, eval string, ups []upSpec) []string {
	t.Helper()
	engine, _, _, cancel := newTestEngine(t, eval)
	t.Cleanup(cancel)
	t.Cleanup(engine.Stop)

	specs := make([]struct {
		id     string
		vendor string
		tags   []string
	}, len(ups))
	for i, u := range ups {
		specs[i].id = u.id
		specs[i].vendor = u.vendor
		specs[i].tags = u.tags
	}
	list := mkUpsWithTags(specs)

	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: testEvalTimeout, EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return list }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	return ids(engine.GetOrdered("evm:1", "*", "*"))
}

func tiered() []upSpec {
	return []upSpec{
		{id: "a", vendor: "v1", tags: []string{"tier:main"}},
		{id: "b", vendor: "v2", tags: []string{"tier:fallback"}},
		{id: "c", vendor: "v1", tags: []string{"tier:main"}},
		{id: "d", vendor: "v3", tags: []string{"tier:fallback"}},
	}
}

// ─── union ────────────────────────────────────────────────────────────────

func TestStdlib_Union_AppendsTheReceiverFirstAndKeepsOrder(t *testing.T) {
	// This is how `preferTag({keepRest:true})` builds a two-tier order:
	// the preferred subset leads, everything else follows in its original
	// order. If union prepended `other` instead, the demoted tier would
	// take the head of the list and serve production traffic.
	got := runEval(t, `(upstreams, ctx) => upstreams.byTag('tier:fallback').union(upstreams)`, tiered())
	require.Equal(t, []string{"b", "d", "a", "c"}, got)
}

func TestStdlib_Union_DeduplicatesById(t *testing.T) {
	// A repeated upstream would consume a failover attempt on a node the
	// request has already tried and lost.
	//
	// The de-duplication has to be asserted INSIDE the JS chain. The Go
	// side de-dupes again when it materialises the order, so a plain
	// `union(upstreams)` would look correct even with the JS de-dup gone.
	// `skip(4)` on a 4-upstream pool is empty only if union really merged.
	got := runEval(t,
		`(upstreams, ctx) => upstreams.union(upstreams).skip(4).whenEmpty(() => upstreams.byId('a'))`,
		tiered())
	require.Equal(t, []string{"a"}, got, "union must merge to 4 entries, not concatenate to 8")

	// Same check from the other side: take(2) after a union must show two
	// DISTINCT upstreams, not the same one twice.
	pair := runEval(t, `(upstreams, ctx) => upstreams.byId('a').union(upstreams).take(2)`, tiered())
	require.Equal(t, []string{"a", "b"}, pair)
}

func TestStdlib_Union_KeepsTheFirstOccurrencePosition(t *testing.T) {
	// De-duplication must drop the LATER copy. Dropping the earlier one
	// would silently demote whatever the first step deliberately promoted.
	got := runEval(t, `(upstreams, ctx) => upstreams.byId('d').union(upstreams.byId('a')).union(upstreams)`, tiered())
	require.Equal(t, []string{"d", "a", "b", "c"}, got)
}

func TestStdlib_Union_WithAnEmptyOtherIsIdentity(t *testing.T) {
	got := runEval(t, `(upstreams, ctx) => upstreams.union(upstreams.byTag('tier:nonexistent'))`, tiered())
	require.Equal(t, []string{"a", "b", "c", "d"}, got)
}

func TestStdlib_Union_OnAnEmptyReceiverYieldsTheOther(t *testing.T) {
	got := runEval(t, `(upstreams, ctx) => upstreams.byTag('tier:nonexistent').union(upstreams)`, tiered())
	require.Equal(t, []string{"a", "b", "c", "d"}, got)
}

// ─── intersect / difference / unique / partition ──────────────────────────

func TestStdlib_Intersect_KeepsReceiverOrderNotArgumentOrder(t *testing.T) {
	// intersect is a filter on the receiver, so the receiver's ranking
	// survives. Taking the argument's order instead would discard a
	// preceding sortByScore.
	//
	// The argument is deliberately built in REVERSE (`d` then `a`), so the
	// two readings give different answers. Building it with byId would
	// return input order and hide the difference.
	got := runEval(t,
		`(upstreams, ctx) => upstreams.intersect(upstreams.byId('d').union(upstreams.byId('a')))`,
		tiered())
	require.Equal(t, []string{"a", "d"}, got, "the receiver's order wins, not the argument's")
}

func TestStdlib_Intersect_WithADisjointSetIsEmptyAndTriggersTheSafetyNet(t *testing.T) {
	// An intersection that empties the pool must be visible as an empty
	// result, so a `whenEmpty` safety net downstream can fire.
	got := runEval(t, `(upstreams, ctx) => upstreams.intersect(upstreams.byTag('tier:nonexistent')).whenEmpty(() => upstreams)`, tiered())
	require.Equal(t, []string{"a", "b", "c", "d"}, got)
}

func TestStdlib_Difference_RemovesTheArgumentAndKeepsOrder(t *testing.T) {
	got := runEval(t, `(upstreams, ctx) => upstreams.difference(upstreams.byTag('tier:fallback'))`, tiered())
	require.Equal(t, []string{"a", "c"}, got)
}

func TestStdlib_Difference_FromItselfIsEmpty(t *testing.T) {
	got := runEval(t, `(upstreams, ctx) => upstreams.difference(upstreams).whenEmpty(() => upstreams.byId('a'))`, tiered())
	require.Equal(t, []string{"a"}, got)
}

func TestStdlib_Unique_KeepsTheFirstUpstreamPerKey(t *testing.T) {
	// Vendor de-duplication is a blast-radius tool: one upstream per
	// vendor. Keeping the LAST would drop the best-ranked one.
	got := runEval(t, `(upstreams, ctx) => upstreams.unique(u => u.vendor)`, tiered())
	require.Equal(t, []string{"a", "b", "d"}, got, "v1 is represented by `a`, its first occurrence")
}

func TestStdlib_Unique_DefaultsToTheUpstreamId(t *testing.T) {
	// `concat` (not `union`) is used so there really are duplicates left
	// for `unique` to collapse, and `skip(4)` proves it collapsed them
	// inside JS rather than relying on the Go-side de-duplication.
	got := runEval(t,
		`(upstreams, ctx) => upstreams.concat(upstreams).unique().skip(4).whenEmpty(() => upstreams.byId('a'))`,
		tiered())
	require.Equal(t, []string{"a"}, got, "unique() must key on the upstream id by default")
}

func TestStdlib_Partition_SplitsInOrderWithoutLosingAnyone(t *testing.T) {
	// partition is the building block for hand-rolled tiering. Both halves
	// must keep input order, and nobody may be dropped on the floor.
	eval := `(upstreams, ctx) => {
		const parts = upstreams.partition(u => u.tags.indexOf('tier:fallback') >= 0);
		return parts[0].union(parts[1]);
	}`
	got := runEval(t, eval, tiered())
	require.Equal(t, []string{"b", "d", "a", "c"}, got)
}

func TestStdlib_Reject_IsTheInverseOfFilter(t *testing.T) {
	got := runEval(t, `(upstreams, ctx) => upstreams.reject(u => u.tags.indexOf('tier:fallback') >= 0)`, tiered())
	require.Equal(t, []string{"a", "c"}, got)
}

// ─── preferTag / whenEmpty interaction ────────────────────────────────────

func TestStdlib_PreferTag_UnmatchedTagReturnsEveryoneSoWhenEmptyNeverFires(t *testing.T) {
	// The trap: `preferTag` with no match is a PASS-THROUGH, not an empty
	// result. A `whenEmpty` safety net chained after it therefore never
	// runs, and the operator's intended fallback is dead code. Anyone
	// writing "prefer main, else use the reserve list" must use `byTag`.
	got := runEval(t,
		`(upstreams, ctx) => upstreams.preferTag('tier:nonexistent').whenEmpty(() => upstreams.byId('a'))`,
		tiered())
	require.Equal(t, []string{"a", "b", "c", "d"},
		got, "preferTag falls through to the whole pool, so whenEmpty sees a non-empty list")
}

func TestStdlib_ByTag_UnmatchedTagIsEmptySoWhenEmptyFires(t *testing.T) {
	// The contrast case for the test above: byTag is a hard filter and
	// does empty the pool, so the safety net runs.
	got := runEval(t,
		`(upstreams, ctx) => upstreams.byTag('tier:nonexistent').whenEmpty(() => upstreams.byId('a'))`,
		tiered())
	require.Equal(t, []string{"a"}, got)
}

func TestStdlib_PreferTag_IsAHardFilterWhenTheTagMatches(t *testing.T) {
	// Without keepRest the loser tier is DROPPED for the whole tick, not
	// demoted. That is the difference between "fallback reachable on
	// failover" and "fallback unreachable until the next tick".
	got := runEval(t, `(upstreams, ctx) => upstreams.preferTag('tier:main')`, tiered())
	require.Equal(t, []string{"a", "c"}, got)
}

func TestStdlib_PreferTag_MinHealthyGateFallsThroughToTheFallbackTag(t *testing.T) {
	// One main upstream, minHealthy 2 → the main tier is not enough, so
	// the declared fallback tier takes over wholesale.
	ups := []upSpec{
		{id: "a", vendor: "v1", tags: []string{"tier:main"}},
		{id: "b", vendor: "v2", tags: []string{"tier:fallback"}},
		{id: "d", vendor: "v3", tags: []string{"tier:fallback"}},
	}
	got := runEval(t,
		`(upstreams, ctx) => upstreams.preferTag('tier:main', { minHealthy: 2, fallback: 'tier:fallback' })`,
		ups)
	require.Equal(t, []string{"b", "d"}, got)
}

func TestStdlib_PreferTag_MinHealthyGateWithNoUsableFallbackKeepsEveryone(t *testing.T) {
	// Neither tier qualifies. Returning an empty list here would take the
	// network offline; the safe answer is the untouched pool.
	ups := []upSpec{{id: "a", vendor: "v1", tags: []string{"tier:main"}}}
	got := runEval(t,
		`(upstreams, ctx) => upstreams.preferTag('tier:main', { minHealthy: 5, fallback: 'tier:reserve' })`,
		ups)
	require.Equal(t, []string{"a"}, got)
}

func TestStdlib_PreferTag_KeepRestPutsThePreferredTierFirstAndKeepsTheRest(t *testing.T) {
	got := runEval(t, `(upstreams, ctx) => upstreams.preferTag('tier:fallback', { keepRest: true })`, tiered())
	require.Equal(t, []string{"b", "d", "a", "c"}, got,
		"keepRest turns the hard filter into a ranking — the rest stays reachable on failover")
}

func TestStdlib_DemoteTag_MovesMatchesLastAndPreservesOrderWithinEachGroup(t *testing.T) {
	got := runEval(t, `(upstreams, ctx) => upstreams.demoteTag('tier:fallback')`, tiered())
	require.Equal(t, []string{"a", "c", "b", "d"}, got)
}

func TestStdlib_ExcludeTag_DropsMatchesEntirely(t *testing.T) {
	got := runEval(t, `(upstreams, ctx) => upstreams.excludeTag('tier:fallback')`, tiered())
	require.Equal(t, []string{"a", "c"}, got)
}

// ─── preferVendor ─────────────────────────────────────────────────────────

func TestStdlib_PreferVendor_IsAHardFilterOnAMatch(t *testing.T) {
	got := runEval(t, `(upstreams, ctx) => upstreams.preferVendor('v1')`, tiered())
	require.Equal(t, []string{"a", "c"}, got)
}

func TestStdlib_PreferVendor_FallsBackThenPassesThrough(t *testing.T) {
	// Same three-way shape as preferTag: enough of the preferred vendor →
	// keep it; not enough but a fallback vendor exists → use that;
	// neither → keep everyone rather than empty the pool.
	fallback := runEval(t,
		`(upstreams, ctx) => upstreams.preferVendor('v1', { minHealthy: 3, fallback: 'v3' })`, tiered())
	require.Equal(t, []string{"d"}, fallback)

	passthrough := runEval(t,
		`(upstreams, ctx) => upstreams.preferVendor('v1', { minHealthy: 3, fallback: 'v9' })`, tiered())
	require.Equal(t, []string{"a", "b", "c", "d"}, passthrough)
}

// ─── combinators ──────────────────────────────────────────────────────────

func TestStdlib_If_TakesTheThenBranchAndTheElseBranch(t *testing.T) {
	yes := runEval(t, `(upstreams, ctx) => upstreams.if(true, u => u.byId('a'), u => u.byId('b'))`, tiered())
	require.Equal(t, []string{"a"}, yes)

	no := runEval(t, `(upstreams, ctx) => upstreams.if(false, u => u.byId('a'), u => u.byId('b'))`, tiered())
	require.Equal(t, []string{"b"}, no)
}

func TestStdlib_If_WithoutAnElseBranchPassesThrough(t *testing.T) {
	got := runEval(t, `(upstreams, ctx) => upstreams.if(false, u => u.byId('a'))`, tiered())
	require.Equal(t, []string{"a", "b", "c", "d"}, got)
}

func TestStdlib_If_EvaluatesAPredicateAgainstTheCurrentPool(t *testing.T) {
	// The condition must see the pool as it stands at that point in the
	// chain, not the original input — that is what makes "if fewer than N
	// survived, widen" expressible.
	fires := runEval(t,
		`(upstreams, ctx) => upstreams.byTag('tier:main').if(u => u.length < 3, u => u.union(upstreams))`,
		tiered())
	require.Equal(t, []string{"a", "c", "b", "d"}, fires)

	// The false case matters just as much: a predicate that returns false
	// must NOT run the then-branch. A `cond` treated as a bare truthiness
	// check would see the function object itself as true and widen every
	// tick, defeating the guard entirely.
	holds := runEval(t,
		`(upstreams, ctx) => upstreams.if(u => u.length < 2, u => u.byId('a'))`,
		tiered())
	require.Equal(t, []string{"a", "b", "c", "d"}, holds)
}

func TestStdlib_Unless_IsTheNegationOfIf(t *testing.T) {
	fires := runEval(t, `(upstreams, ctx) => upstreams.unless(false, u => u.byId('a'))`, tiered())
	require.Equal(t, []string{"a"}, fires)

	skips := runEval(t, `(upstreams, ctx) => upstreams.unless(true, u => u.byId('a'))`, tiered())
	require.Equal(t, []string{"a", "b", "c", "d"}, skips)
}

func TestStdlib_EnsureMin_WidensOnlyBelowTheFloor(t *testing.T) {
	// ensureMin is the "never route on fewer than N upstreams" guard. It
	// must not widen when the floor is already met, or a healthy tier gets
	// diluted with degraded upstreams on every tick.
	widens := runEval(t,
		`(upstreams, ctx) => upstreams.byId('a').ensureMin(2, u => u.union(upstreams))`, tiered())
	require.Equal(t, []string{"a", "b", "c", "d"}, widens)

	holds := runEval(t,
		`(upstreams, ctx) => upstreams.byTag('tier:main').ensureMin(2, u => u.union(upstreams))`, tiered())
	require.Equal(t, []string{"a", "c"}, holds)
}

func TestStdlib_FallbackTo_OnlyFiresOnAnEmptyPool(t *testing.T) {
	fires := runEval(t,
		`(upstreams, ctx) => upstreams.byTag('tier:nonexistent').fallbackTo(() => upstreams.byId('d'))`, tiered())
	require.Equal(t, []string{"d"}, fires)

	holds := runEval(t,
		`(upstreams, ctx) => upstreams.byTag('tier:main').fallbackTo(() => upstreams.byId('d'))`, tiered())
	require.Equal(t, []string{"a", "c"}, holds)
}

func TestStdlib_WhenNotEmpty_IsTheMirrorOfWhenEmpty(t *testing.T) {
	fires := runEval(t, `(upstreams, ctx) => upstreams.byTag('tier:main').whenNotEmpty(u => u.byId('c'))`, tiered())
	require.Equal(t, []string{"c"}, fires)

	skips := runEval(t,
		`(upstreams, ctx) => upstreams.byTag('tier:nonexistent').whenNotEmpty(u => u.byId('c')).whenEmpty(() => upstreams.byId('a'))`,
		tiered())
	require.Equal(t, []string{"a"}, skips)
}

// ─── slicing & rotation ───────────────────────────────────────────────────

func TestStdlib_Slicing_TailAndSkipVariants(t *testing.T) {
	require.Equal(t, []string{"c", "d"}, runEval(t, `(upstreams, ctx) => upstreams.pickBottom(2)`, tiered()))
	require.Equal(t, []string{"a", "b"}, runEval(t, `(upstreams, ctx) => upstreams.dropBottom(2)`, tiered()))
	require.Equal(t, []string{"c", "d"}, runEval(t, `(upstreams, ctx) => upstreams.skip(2)`, tiered()))
	require.Equal(t, []string{"a", "b"}, runEval(t, `(upstreams, ctx) => upstreams.take(2)`, tiered()))
}

func TestStdlib_Slicing_OversizedCountsDoNotEmptyOrDuplicateThePool(t *testing.T) {
	// An operator writing `pickTop(10)` on a 4-upstream network must get
	// all four, not an empty list and not a wrapped one.
	require.Equal(t, []string{"a", "b", "c", "d"}, runEval(t, `(upstreams, ctx) => upstreams.pickTop(10)`, tiered()))
	require.Equal(t, []string{"a", "b", "c", "d"}, runEval(t, `(upstreams, ctx) => upstreams.pickBottom(10)`, tiered()))
	require.Equal(t, []string{"a", "b", "c", "d"}, runEval(t, `(upstreams, ctx) => upstreams.dropBottom(10).whenEmpty(() => upstreams)`, tiered()))
}

func TestStdlib_RotateBy_RotatesLeftByTheGivenCount(t *testing.T) {
	// Round-robin depends on the exact rotation. Off by one and the
	// primary repeats every other tick instead of cycling evenly.
	require.Equal(t, []string{"b", "c", "d", "a"}, runEval(t, `(upstreams, ctx) => upstreams.rotateBy(1)`, tiered()))
	require.Equal(t, []string{"c", "d", "a", "b"}, runEval(t, `(upstreams, ctx) => upstreams.rotateBy(2)`, tiered()))
	require.Equal(t, []string{"a", "b", "c", "d"}, runEval(t, `(upstreams, ctx) => upstreams.rotateBy(4)`, tiered()),
		"a full turn must land back on the input order")
	require.Equal(t, []string{"a", "b", "c", "d"}, runEval(t, `(upstreams, ctx) => upstreams.rotateBy(0)`, tiered()))
}

func TestStdlib_Shuffle_IsAPermutationNotADrop(t *testing.T) {
	// A shuffle that lost or duplicated an upstream would silently shrink
	// the fleet. The seed makes the result deterministic per tick.
	got := runEval(t, `(upstreams, ctx) => upstreams.shuffle(7)`, tiered())
	require.ElementsMatch(t, []string{"a", "b", "c", "d"}, got)
	require.Len(t, got, 4)
}

// ─── selectors ────────────────────────────────────────────────────────────

func TestStdlib_ById_AndExcludeId_AcceptAListAndAGlob(t *testing.T) {
	require.Equal(t, []string{"a", "c"}, runEval(t, `(upstreams, ctx) => upstreams.byId(['a','c'])`, tiered()))
	require.Equal(t, []string{"b", "d"}, runEval(t, `(upstreams, ctx) => upstreams.excludeId(['a','c'])`, tiered()))
}

func TestStdlib_ByVendor_AndExcludeVendor_SplitOnTheVendorName(t *testing.T) {
	require.Equal(t, []string{"a", "c"}, runEval(t, `(upstreams, ctx) => upstreams.byVendor('v1')`, tiered()))
	require.Equal(t, []string{"b", "d"}, runEval(t, `(upstreams, ctx) => upstreams.excludeVendor('v1')`, tiered()))
}

func TestStdlib_Where_AndWhereNot_MatchOnASelectorObject(t *testing.T) {
	require.Equal(t, []string{"a", "c"},
		runEval(t, `(upstreams, ctx) => upstreams.where({ vendor: 'v1' })`, tiered()))
	require.Equal(t, []string{"b", "d"},
		runEval(t, `(upstreams, ctx) => upstreams.whereNot({ vendor: 'v1' })`, tiered()))
}

func TestStdlib_Where_CombinesEveryGivenFacetWithAnd(t *testing.T) {
	// Two facets narrow the match. If they were OR-ed, a policy meaning
	// "the main-tier nodes at this vendor" would also admit every other
	// main-tier node.
	require.Equal(t, []string{"a", "c"},
		runEval(t, `(upstreams, ctx) => upstreams.where({ vendor: 'v1', tag: 'tier:main' })`, tiered()))
	require.Equal(t, []string{"a"},
		runEval(t, `(upstreams, ctx) => upstreams.where({ vendor: 'v1', id: 'a' })`, tiered()))
	require.Equal(t, []string{"a", "b", "c", "d"},
		runEval(t, `(upstreams, ctx) => upstreams.where({ vendor: 'v1', tag: 'tier:fallback' }).whenEmpty(() => upstreams)`, tiered()))
}

func TestStdlib_Where_IgnoresAnUnknownSelectorShapeInsteadOfThrowing(t *testing.T) {
	// Observed behaviour, pinned as a trap. `where` expects a SELECTOR
	// OBJECT ({id, tag, vendor, type}); a predicate function has none of
	// those fields, so every facet check is skipped and the step becomes a
	// silent no-op. An operator who writes `where(u => ...)` — the shape
	// `reject` and `whereNot`-by-lambda suggest — gets no filtering and no
	// error. Only the returned order reveals it.
	require.Equal(t, []string{"a", "b", "c", "d"},
		runEval(t, `(upstreams, ctx) => upstreams.where(u => u.vendor === 'v1')`, tiered()))
	require.Equal(t, []string{"a", "b", "c", "d"},
		runEval(t, `(upstreams, ctx) => upstreams.where({})`, tiered()))
}

func TestStdlib_Tap_AndLabel_AreObservationOnly(t *testing.T) {
	// The debug helpers must never change routing. A `tap` that returned
	// its callback's value would let a stray `console.log` reorder traffic.
	got := runEval(t, `(upstreams, ctx) => upstreams.tap(u => u.length).label('after-tap')`, tiered())
	require.Equal(t, []string{"a", "b", "c", "d"}, got)
}
