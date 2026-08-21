package evm

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// NormalizeHttpJsonRpc rewrites block tags into concrete hex heights before a
// request leaves eRPC. Two operator-visible things depend on it. First, cache
// keys: "latest" is a moving target, so a request left un-interpolated either
// misses the cache forever or poisons it with a stale answer under a name that
// never expires. Second, the block-availability check reads the number this
// function caches on the request; a wrong number routes the request to an
// upstream that cannot serve it.

// tagTestNetwork is a common.Network that also satisfies common.EvmNetwork, so
// the tag resolver can read a head from it. Both heights are settable because
// "latest" and "finalized" translate through separate config flags and a test
// must be able to move one without the other.
type tagTestNetwork struct {
	cfg             *common.NetworkConfig
	highestLatest   int64
	highestFinalize int64
}

func (n *tagTestNetwork) Id() string                             { return "evm:123" }
func (n *tagTestNetwork) Label() string                          { return "evm:123" }
func (n *tagTestNetwork) ProjectId() string                      { return "test-project" }
func (n *tagTestNetwork) Architecture() common.NetworkArchitecture {
	return common.ArchitectureEvm
}
func (n *tagTestNetwork) Config() *common.NetworkConfig { return n.cfg }
func (n *tagTestNetwork) Logger() *zerolog.Logger {
	lg := zerolog.Nop()
	return &lg
}
func (n *tagTestNetwork) GetMethodMetrics(string) common.TrackedMetrics { return nil }
func (n *tagTestNetwork) Forward(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (n *tagTestNetwork) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateUnknown
}
func (n *tagTestNetwork) EvmHighestLatestBlockNumber(context.Context) int64 {
	return n.highestLatest
}
func (n *tagTestNetwork) EvmHighestFinalizedBlockNumber(context.Context) int64 {
	return n.highestFinalize
}
func (n *tagTestNetwork) EvmLeaderUpstream(context.Context) common.Upstream { return nil }

var _ common.EvmNetwork = (*tagTestNetwork)(nil)

// normalizeCtx gives every test in this file a deadline, so a future change that
// introduces a blocking lookup fails fast instead of hanging the package.
func normalizeCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// newTagRequest builds a request whose network reports the given heads.
func newTagRequest(t *testing.T, network *tagTestNetwork, method string, params ...interface{}) (*common.NormalizedRequest, *common.JsonRpcRequest) {
	t.Helper()
	jrq := common.NewJsonRpcRequest(method, params)
	nrq := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	nrq.SetNetwork(network)
	return nrq, jrq
}

// paramAt reads a nested key out of the (possibly rewritten) params.
func paramAt(t *testing.T, jrq *common.JsonRpcRequest, idx int, key string) interface{} {
	t.Helper()
	jrq.RLock()
	defer jrq.RUnlock()
	if idx >= len(jrq.Params) {
		t.Fatalf("params has %d entries, wanted index %d", len(jrq.Params), idx)
	}
	m, ok := jrq.Params[idx].(map[string]interface{})
	if !ok {
		t.Fatalf("param %d is %T, wanted a map", idx, jrq.Params[idx])
	}
	return m[key]
}

func TestNormalizeHttpJsonRpc_TranslatesLatestToTheHighestKnownHead(t *testing.T) {
	t.Parallel()

	// A cached eth_getLogs answer keyed on the literal string "latest" is wrong
	// the moment the chain advances. Interpolating the tag gives the entry a
	// height it can be pinned to.
	net := &tagTestNetwork{highestLatest: 0x1234}
	nrq, jrq := newTagRequest(t, net, "eth_getLogs", map[string]interface{}{
		"fromBlock": "0x1200",
		"toBlock":   "latest",
	})

	NormalizeHttpJsonRpc(normalizeCtx(t), nrq, jrq)

	if got := paramAt(t, jrq, 0, "toBlock"); got != "0x1234" {
		t.Fatalf("toBlock = %v, want 0x1234 (the network's highest known head)", got)
	}
	if got := nrq.EvmBlockRef(); got != "latest" {
		t.Fatalf("EvmBlockRef = %v, want \"latest\"", got)
	}
	if got := nrq.EvmBlockNumber(); got != int64(0x1234) {
		t.Fatalf("EvmBlockNumber = %v, want 4660; the availability check reads this number", got)
	}
}

func TestNormalizeHttpJsonRpc_TranslatesFinalizedFromTheFinalizedHeadNotTheLatest(t *testing.T) {
	t.Parallel()

	// The two heads are separate counters. If "finalized" resolved from the
	// latest head, eRPC would hand out an unfinalized block under a name the
	// cache treats as permanently immutable — the worst kind of stale answer.
	net := &tagTestNetwork{highestLatest: 0x9999, highestFinalize: 0x1000}
	nrq, jrq := newTagRequest(t, net, "eth_getLogs", map[string]interface{}{
		"fromBlock": "finalized",
		"toBlock":   "0x1000",
	})

	NormalizeHttpJsonRpc(normalizeCtx(t), nrq, jrq)

	if got := paramAt(t, jrq, 0, "fromBlock"); got != "0x1000" {
		t.Fatalf("fromBlock = %v, want 0x1000 (the FINALIZED head, not the latest head 0x9999)", got)
	}
	if got := nrq.EvmBlockRef(); got != "finalized" {
		t.Fatalf("EvmBlockRef = %v, want \"finalized\"", got)
	}
}

func TestNormalizeHttpJsonRpc_CollapsesBlockRefToStarWhenBothTagsAppear(t *testing.T) {
	t.Parallel()

	// A range spanning finalized..latest is neither cacheable as finalized data
	// nor as a single tag. The "*" marker tells the cache layer that no single
	// tag describes this request.
	net := &tagTestNetwork{highestLatest: 0x2000, highestFinalize: 0x1000}
	nrq, jrq := newTagRequest(t, net, "eth_getLogs", map[string]interface{}{
		"fromBlock": "finalized",
		"toBlock":   "latest",
	})

	NormalizeHttpJsonRpc(normalizeCtx(t), nrq, jrq)

	if got := nrq.EvmBlockRef(); got != "*" {
		t.Fatalf("EvmBlockRef = %v, want \"*\"; a finalized..latest range must not be filed under either tag alone", got)
	}
	// ReqRefs list fromBlock before toBlock, so the LAST cached number is the
	// upper bound. Availability is checked against the highest block a request
	// touches, so caching the lower bound would let a request through to an
	// upstream that cannot reach its top end.
	if got := nrq.EvmBlockNumber(); got != int64(0x2000) {
		t.Fatalf("EvmBlockNumber = %v, want 8192 (the upper bound of the range)", got)
	}
}

func TestNormalizeHttpJsonRpc_PassesSafeAndPendingThroughUntouched(t *testing.T) {
	t.Parallel()

	// eRPC tracks neither the safe head nor the mempool. Guessing a height for
	// these tags would answer from a block the client did not ask for. Passing
	// them through lets the upstream, which does know, decide.
	for _, tag := range []string{"safe", "pending", "earliest"} {
		tag := tag
		t.Run(tag, func(t *testing.T) {
			t.Parallel()
			net := &tagTestNetwork{highestLatest: 0x5000, highestFinalize: 0x4000}
			nrq, jrq := newTagRequest(t, net, "eth_getLogs", map[string]interface{}{
				"fromBlock": tag,
				"toBlock":   tag,
			})

			NormalizeHttpJsonRpc(normalizeCtx(t), nrq, jrq)

			if got := paramAt(t, jrq, 0, "fromBlock"); got != tag {
				t.Fatalf("fromBlock = %v, want the untouched tag %q", got, tag)
			}
			if got := nrq.EvmBlockRef(); got != nil {
				t.Fatalf("EvmBlockRef = %v, want nil; %q names no tag eRPC tracks", got, tag)
			}
		})
	}
}

func TestNormalizeHttpJsonRpc_LeavesBlockHashParamsAlone(t *testing.T) {
	t.Parallel()

	// A 32-byte hash in fromBlock/blockHash is already exact. Rewriting it would
	// change which block the client asked for.
	hash := "0x1111111111111111111111111111111111111111111111111111111111111111"
	net := &tagTestNetwork{highestLatest: 0x5000}
	nrq, jrq := newTagRequest(t, net, "eth_getLogs", map[string]interface{}{
		"blockHash": hash,
	})

	NormalizeHttpJsonRpc(normalizeCtx(t), nrq, jrq)

	if got := paramAt(t, jrq, 0, "blockHash"); got != hash {
		t.Fatalf("blockHash = %v, want it untouched", got)
	}
	if got := nrq.EvmBlockNumber(); got != nil {
		t.Fatalf("EvmBlockNumber = %v, want nil; a hash carries no height", got)
	}
}

func TestNormalizeHttpJsonRpc_CanonicalisesNonCanonicalHexHeights(t *testing.T) {
	t.Parallel()

	// Clients send "0x01", "0x0000ff" and decimal numbers for the same block.
	// Left alone, each spelling becomes its own cache entry for identical data.
	cases := []struct {
		name string
		in   interface{}
		want string
		num  int64
	}{
		{name: "leading zero", in: "0x01", want: "0x1", num: 1},
		{name: "padded", in: "0x0000ff", want: "0xff", num: 255},
		{name: "json number", in: float64(4660), want: "0x1234", num: 4660},
		{name: "already canonical stays put", in: "0x1234", want: "0x1234", num: 4660},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			net := &tagTestNetwork{highestLatest: 0x99999}
			nrq, jrq := newTagRequest(t, net, "eth_getLogs", map[string]interface{}{
				"fromBlock": tc.in,
			})

			NormalizeHttpJsonRpc(normalizeCtx(t), nrq, jrq)

			if got := paramAt(t, jrq, 0, "fromBlock"); got != tc.want {
				t.Fatalf("fromBlock = %#v, want %q", got, tc.want)
			}
			if got := nrq.EvmBlockNumber(); got != tc.num {
				t.Fatalf("EvmBlockNumber = %v, want %d", got, tc.num)
			}
		})
	}
}

func TestNormalizeHttpJsonRpc_HonoursTheMethodOptOutFromTagTranslation(t *testing.T) {
	t.Parallel()

	// eth_getBlockByNumber sets translateLatestTag/translateFinalizedTag to
	// false: it is the method that DISCOVERS the head. Interpolating its tag
	// would freeze the head at whatever eRPC already believed and the poller
	// could never learn the chain advanced.
	net := &tagTestNetwork{highestLatest: 0x1234, highestFinalize: 0x1000}
	jrq := common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"latest", false})
	nrq := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	nrq.SetNetwork(net)

	NormalizeHttpJsonRpc(normalizeCtx(t), nrq, jrq)

	jrq.RLock()
	got := jrq.Params[0]
	jrq.RUnlock()
	if got != "latest" {
		t.Fatalf("params[0] = %v, want the untranslated tag \"latest\"", got)
	}
	// The tag is still RECORDED even though it is not rewritten — the cache
	// layer needs to know this answer is head-relative.
	if ref := nrq.EvmBlockRef(); ref != "latest" {
		t.Fatalf("EvmBlockRef = %v, want \"latest\" even when translation is off", ref)
	}
}

func TestNormalizeHttpJsonRpc_SkipInterpolationLeavesParamsButStillRecordsTheTag(t *testing.T) {
	t.Parallel()

	// A client that sets skipInterpolation wants its literal request forwarded.
	// eRPC still records that the request is head-relative, because the cache
	// layer must not file a "latest" answer as immutable just because the
	// rewrite was skipped. This asymmetry is deliberate; pin both halves.
	net := &tagTestNetwork{highestLatest: 0x1234}
	nrq, jrq := newTagRequest(t, net, "eth_getLogs", map[string]interface{}{
		"fromBlock": "latest",
	})
	nrq.SetDirectives(&common.RequestDirectives{SkipInterpolation: true})

	NormalizeHttpJsonRpc(normalizeCtx(t), nrq, jrq)

	if got := paramAt(t, jrq, 0, "fromBlock"); got != "latest" {
		t.Fatalf("fromBlock = %v, want the literal tag; skipInterpolation must forward it verbatim", got)
	}
	if ref := nrq.EvmBlockRef(); ref != "latest" {
		t.Fatalf("EvmBlockRef = %v, want \"latest\"; skipping the rewrite must not hide that this answer is head-relative", ref)
	}
}

func TestNormalizeHttpJsonRpc_SkipInterpolationAlsoLeavesNonCanonicalHexAlone(t *testing.T) {
	t.Parallel()

	// Hex canonicalisation runs on its own, without asking about
	// skipInterpolation — only the final write-back is gated. A client that
	// asked for its request to be forwarded verbatim must get "0x01" on the
	// wire, not "0x1", because some upstreams echo the parameter back and a
	// caller comparing bytes would see a request it never sent.
	net := &tagTestNetwork{highestLatest: 0x9999}
	nrq, jrq := newTagRequest(t, net, "eth_getLogs", map[string]interface{}{
		"fromBlock": "0x01",
	})
	nrq.SetDirectives(&common.RequestDirectives{SkipInterpolation: true})

	NormalizeHttpJsonRpc(normalizeCtx(t), nrq, jrq)

	if got := paramAt(t, jrq, 0, "fromBlock"); got != "0x01" {
		t.Fatalf("fromBlock = %v, want the literal \"0x01\"; skipInterpolation gates the write-back too", got)
	}
	// The height is still CACHED, because the availability check needs it even
	// for a request eRPC forwards untouched.
	if got := nrq.EvmBlockNumber(); got != int64(1) {
		t.Fatalf("EvmBlockNumber = %v, want 1; the availability check still needs the height", got)
	}
}

func TestNormalizeHttpJsonRpc_LeavesAnObjectParamIntactWhenTheRefPointsAtTheWholeObject(t *testing.T) {
	t.Parallel()

	// When a method's ref addresses param 0 as a whole and param 0 is an object,
	// replacing it with a bare hex string would destroy every other field the
	// client sent. The function records the tag and leaves the object alone.
	net := &tagTestNetwork{highestLatest: 0x1234}
	obj := map[string]interface{}{"blockTag": "latest", "somethingElse": "keep-me"}
	nrq, jrq := newTagRequest(t, net, "eth_getBlockByHash", obj)

	NormalizeHttpJsonRpc(normalizeCtx(t), nrq, jrq)

	jrq.RLock()
	got, ok := jrq.Params[0].(map[string]interface{})
	jrq.RUnlock()
	if !ok {
		t.Fatalf("params[0] = %T, want the object preserved", got)
	}
	if got["blockTag"] != "latest" || got["somethingElse"] != "keep-me" {
		t.Fatalf("params[0] = %v, want every field kept intact", got)
	}
	if ref := nrq.EvmBlockRef(); ref != "latest" {
		t.Fatalf("EvmBlockRef = %v, want \"latest\"; the tag must still be recorded", ref)
	}
}

func TestNormalizeHttpJsonRpc_IsANoOpForMethodsWithNoBlockRefs(t *testing.T) {
	t.Parallel()

	// The unknown-method path is the common one: every new chain method arrives
	// here first. It must pass through untouched and must not panic.
	net := &tagTestNetwork{highestLatest: 0x1234}
	nrq, jrq := newTagRequest(t, net, "some_brandNewMethod", "latest", 42)

	NormalizeHttpJsonRpc(normalizeCtx(t), nrq, jrq)

	jrq.RLock()
	params := jrq.Params
	jrq.RUnlock()
	if !reflect.DeepEqual(params, []interface{}{"latest", 42}) {
		t.Fatalf("params = %#v, want them untouched", params)
	}
	if ref := nrq.EvmBlockRef(); ref != nil {
		t.Fatalf("EvmBlockRef = %v, want nil for a method eRPC knows nothing about", ref)
	}
}

func TestNormalizeHttpJsonRpc_DoesNotMutateTheCallersOriginalParams(t *testing.T) {
	t.Parallel()

	// The request object is shared across the multiplexer and the retry loop.
	// If the rewrite edited the caller's map in place, a concurrent reader could
	// observe a half-rewritten filter — a data race that shows up as a wrong
	// eth_getLogs range under load, not as a crash.
	net := &tagTestNetwork{highestLatest: 0x1234}
	original := map[string]interface{}{"fromBlock": "latest", "address": "0xabc"}
	nrq, jrq := newTagRequest(t, net, "eth_getLogs", original)

	NormalizeHttpJsonRpc(normalizeCtx(t), nrq, jrq)

	if original["fromBlock"] != "latest" {
		t.Fatalf("the caller's own map was rewritten in place: fromBlock = %v", original["fromBlock"])
	}
	jrq.RLock()
	rewritten := jrq.Params[0].(map[string]interface{})
	jrq.RUnlock()
	if rewritten["fromBlock"] != "0x1234" {
		t.Fatalf("rewritten fromBlock = %v, want 0x1234", rewritten["fromBlock"])
	}
	if rewritten["address"] != "0xabc" {
		t.Fatalf("the copy dropped a sibling field: address = %v", rewritten["address"])
	}
}

func TestResolveBlockTagToHex_ReportsFailureRatherThanGuessing(t *testing.T) {
	t.Parallel()

	ctx := normalizeCtx(t)

	// No network, or a head eRPC has not learned yet. Returning a bogus "0x0"
	// here would rewrite every request to the genesis block.
	if hx, ok := resolveBlockTagToHex(ctx, nil, "latest"); ok {
		t.Fatalf("resolveBlockTagToHex(nil network) = %q, true; want a refusal", hx)
	}
	if hx, ok := resolveBlockTagToHex(ctx, &tagTestNetwork{}, "latest"); ok {
		t.Fatalf("resolveBlockTagToHex(head 0) = %q, true; want a refusal until a head is known", hx)
	}
	if hx, ok := resolveBlockTagToHex(ctx, &tagTestNetwork{highestLatest: 5}, "finalized"); ok {
		t.Fatalf("resolveBlockTagToHex(finalized, no finalized head) = %q, true; the latest head must not stand in", hx)
	}
	// Tags eRPC does not track are refused, not approximated.
	for _, tag := range []string{"safe", "pending", "earliest", "nonsense"} {
		if hx, ok := resolveBlockTagToHex(ctx, &tagTestNetwork{highestLatest: 5, highestFinalize: 3}, tag); ok {
			t.Fatalf("resolveBlockTagToHex(%q) = %q, true; want a refusal", tag, hx)
		}
	}
}

func TestSetByPath_RefusesToInventStructure(t *testing.T) {
	t.Parallel()

	// setByPath is the copy-on-write leaf setter behind every rewrite. It must
	// only ever overwrite a value that already exists. If it created missing keys
	// or grew slices, eRPC would send the upstream a filter the client never
	// wrote — silently changing the query's meaning.
	params := []interface{}{
		map[string]interface{}{"fromBlock": "latest"},
	}

	cases := []struct {
		name string
		path []interface{}
	}{
		{name: "index past the end", path: []interface{}{5, "fromBlock"}},
		{name: "negative index", path: []interface{}{-1, "fromBlock"}},
		{name: "key that does not exist", path: []interface{}{0, "toBlock"}},
		{name: "string key into a slice", path: []interface{}{"fromBlock"}},
		{name: "int index into a map", path: []interface{}{0, "fromBlock", 0}},
		{name: "unsupported key type", path: []interface{}{true}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, changed := replaceParamAtPath(params, tc.path, "0x1")
			if changed {
				t.Fatalf("replaceParamAtPath(%v) reported a change; it must refuse rather than invent structure", tc.path)
			}
			if !reflect.DeepEqual(out, params) {
				t.Fatalf("params were altered on a refused path: %#v", out)
			}
		})
	}

	// An empty path has no leaf to set.
	if _, changed := replaceParamAtPath(params, nil, "0x1"); changed {
		t.Fatal("replaceParamAtPath with an empty path reported a change")
	}
}

func TestSetByPath_ReplacesTheLeafWithoutTouchingTheInput(t *testing.T) {
	t.Parallel()

	inner := map[string]interface{}{"fromBlock": "latest", "keep": "me"}
	params := []interface{}{inner, "second"}

	out, changed := replaceParamAtPath(params, []interface{}{0, "fromBlock"}, "0x1234")
	if !changed {
		t.Fatal("replaceParamAtPath reported no change on a valid path")
	}
	if inner["fromBlock"] != "latest" {
		t.Fatalf("the input map was written through: fromBlock = %v", inner["fromBlock"])
	}
	// The SLICE must be copied too, not only the map inside it. Writing the new
	// map back into the caller's slice would publish a half-rewritten request to
	// anyone still holding it.
	if got := params[0].(map[string]interface{})["fromBlock"]; got != "latest" {
		t.Fatalf("the input slice now points at the rewritten map: params[0].fromBlock = %v", got)
	}
	newInner := out[0].(map[string]interface{})
	if newInner["fromBlock"] != "0x1234" || newInner["keep"] != "me" {
		t.Fatalf("copy = %v, want fromBlock rewritten and siblings kept", newInner)
	}
	if out[1] != "second" {
		t.Fatalf("out[1] = %v, want the untouched second param", out[1])
	}
}

func TestDeepCopyParams_IsolatesEveryNestedContainer(t *testing.T) {
	t.Parallel()

	// A shallow copy would share the inner map and slice with the caller, which
	// is exactly the race the copy exists to prevent.
	src := []interface{}{
		map[string]interface{}{
			"topics": []interface{}{"0xa", []interface{}{"0xb"}},
			"nested": map[string]interface{}{"fromBlock": "latest"},
		},
		"plain",
		nil,
	}

	dst := deepCopyParams(src)
	if !reflect.DeepEqual(dst, src) {
		t.Fatalf("deepCopyParams changed the value: %#v", dst)
	}

	// Mutate every level of the copy and prove none of it reaches the source.
	dstMap := dst[0].(map[string]interface{})
	dstMap["nested"].(map[string]interface{})["fromBlock"] = "0x1"
	dstMap["topics"].([]interface{})[0] = "0xZ"
	dstMap["topics"].([]interface{})[1].([]interface{})[0] = "0xY"

	srcMap := src[0].(map[string]interface{})
	if srcMap["nested"].(map[string]interface{})["fromBlock"] != "latest" {
		t.Fatal("writing the copy's nested map reached the source map")
	}
	if srcMap["topics"].([]interface{})[0] != "0xa" {
		t.Fatal("writing the copy's slice reached the source slice")
	}
	if srcMap["topics"].([]interface{})[1].([]interface{})[0] != "0xb" {
		t.Fatal("writing the copy's nested slice reached the source slice")
	}

	if deepCopyParams(nil) != nil {
		t.Fatal("deepCopyParams(nil) must stay nil, not become an empty slice")
	}
}
