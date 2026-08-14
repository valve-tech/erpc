package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// hashOf is a helper: build a response around a raw result and hash it.
func hashOf(t *testing.T, result string) string {
	t.Helper()
	jr := &JsonRpcResponse{result: []byte(result)}
	h, err := jr.CanonicalHash()
	require.NoError(t, err)
	require.NotEmpty(t, h)
	return h
}

// TestCanonicalHash_IgnoresMemberOrder is the whole point of the canonical
// form. Upstreams serialize object members in whatever order their encoder
// picks, so a hash that depended on order would report a consensus dispute on
// every request for byte-different but semantically identical bodies.
func TestCanonicalHash_IgnoresMemberOrder(t *testing.T) {
	t.Parallel()

	a := hashOf(t, `{"blockNumber":"0x10","hash":"0xaa","miner":"0xbb"}`)
	b := hashOf(t, `{"miner":"0xbb","hash":"0xaa","blockNumber":"0x10"}`)
	require.Equal(t, a, b)
}

// TestCanonicalHash_KeepsArrayOrder is the other half. Log ordering is
// meaningful, so two upstreams returning the same logs in a different order
// must NOT be treated as agreeing.
func TestCanonicalHash_KeepsArrayOrder(t *testing.T) {
	t.Parallel()

	a := hashOf(t, `[{"i":"0x1"},{"i":"0x2"}]`)
	b := hashOf(t, `[{"i":"0x2"},{"i":"0x1"}]`)
	require.NotEqual(t, a, b)
}

// TestCanonicalHash_NormalizesLeadingZeroesInHex covers the real vendor split:
// some nodes pad quantities, some do not. Without this, half the fleet would
// permanently disagree with the other half on the same block.
func TestCanonicalHash_NormalizesLeadingZeroesInHex(t *testing.T) {
	t.Parallel()

	padded := hashOf(t, `{"gasUsed":"0x0005208"}`)
	bare := hashOf(t, `{"gasUsed":"0x5208"}`)
	require.Equal(t, padded, bare)

	// A different value must still differ — otherwise the normalization is
	// erasing information rather than formatting.
	require.NotEqual(t, bare, hashOf(t, `{"gasUsed":"0x5209"}`))
}

// TestCanonicalHash_TreatsZeroValuedFieldsAsAbsent pins the deliberate
// emptyish rule: a field carrying 0x0, "", [] or {} is dropped before hashing,
// so an upstream that omits the field agrees with one that sends it empty.
func TestCanonicalHash_TreatsZeroValuedFieldsAsAbsent(t *testing.T) {
	t.Parallel()

	full := hashOf(t, `{"hash":"0xaa","logs":[],"root":"0x0","extra":"","meta":{}}`)
	trimmed := hashOf(t, `{"hash":"0xaa"}`)
	require.Equal(t, full, trimmed)

	// The number 0 and the boolean false are handled by different branches;
	// 0 is emptyish, false is explicitly NOT.
	require.Equal(t, trimmed, hashOf(t, `{"hash":"0xaa","count":0}`))
	require.NotEqual(t, trimmed, hashOf(t, `{"hash":"0xaa","ok":false}`))

	// A NON-empty array whose every element canonicalizes to nothing must also
	// vanish. This is the branch that decides whether a log list of all-zero
	// entries counts as data.
	require.Equal(t, trimmed, hashOf(t, `{"hash":"0xaa","logs":[{"ts":"0x0"}]}`))
	require.NotEqual(t, trimmed, hashOf(t, `{"hash":"0xaa","logs":[{"ts":"0x1"}]}`))
}

// TestCanonicalHash_ADocumentThatIsAllZeroesHashesLikeAnEmptyOne records a
// consequence an operator should know: a receipt whose every member is zero
// canonicalizes to nothing at all, so it agrees with a null result. Consensus
// therefore cannot tell "all-zero body" from "no body".
func TestCanonicalHash_ADocumentThatIsAllZeroesHashesLikeAnEmptyOne(t *testing.T) {
	t.Parallel()

	allZero := hashOf(t, `{"a":"0x0","b":[],"c":{"d":""}}`)
	empty := hashOf(t, `{}`)
	require.Equal(t, allZero, empty)
}

// TestCanonicalHash_DistinguishesScalarKinds guards against the canonical form
// flattening everything into the same byte stream.
func TestCanonicalHash_DistinguishesScalarKinds(t *testing.T) {
	t.Parallel()

	require.NotEqual(t, hashOf(t, `{"v":true}`), hashOf(t, `{"v":"0x1"}`))
	require.NotEqual(t, hashOf(t, `{"v":1}`), hashOf(t, `{"v":2}`))
	require.NotEqual(t, hashOf(t, `[1,2]`), hashOf(t, `[2,1]`))
}

// TestCanonicalHash_MemoizesAndIsClearedByFree checks the cache does not go
// stale in the one way that matters: after Free the body is gone, so a cached
// hash would describe data the response no longer holds.
func TestCanonicalHash_MemoizesAndIsClearedByFree(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{result: []byte(`{"hash":"0xaa"}`)}
	first, err := jr.CanonicalHash()
	require.NoError(t, err)

	second, err := jr.CanonicalHash()
	require.NoError(t, err)
	require.Equal(t, first, second)

	_, ok := jr.canonicalHashWithIgnored.Load(defaultCanonicalHashPlaceholder)
	require.True(t, ok, "the second call must have been served from the cache")

	jr.Free()
	_, ok = jr.canonicalHashWithIgnored.Load(defaultCanonicalHashPlaceholder)
	require.False(t, ok)
}

// TestCanonicalHash_RejectsAMalformedBody — a truncated body must produce an
// error, not a hash of whatever parsed. Two truncated bodies hashing equal
// would look like consensus agreement on garbage.
func TestCanonicalHash_RejectsAMalformedBody(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{result: []byte(`{"hash":"0xaa"`)}
	h, err := jr.CanonicalHash()
	require.Error(t, err)
	require.Empty(t, h)
}

// TestCanonicalHash_OfNilResponseIsEmptyWithoutError keeps the consensus
// executor's comparison loop safe when an attempt produced nothing.
func TestCanonicalHash_OfNilResponseIsEmptyWithoutError(t *testing.T) {
	t.Parallel()

	var jr *JsonRpcResponse
	h, err := jr.CanonicalHash()
	require.NoError(t, err)
	require.Empty(t, h)

	h, err = jr.CanonicalHashWithIgnoredFields([]string{"a"})
	require.NoError(t, err)
	require.Empty(t, h)
}

// TestCanonicalHashWithIgnoredFields_EmptyListMatchesThePlainHash proves the
// no-op configuration is genuinely a no-op and shares the same cache slot.
func TestCanonicalHashWithIgnoredFields_EmptyListMatchesThePlainHash(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{result: []byte(`{"hash":"0xaa","ts":"0x64"}`)}
	plain, err := jr.CanonicalHash()
	require.NoError(t, err)

	ignored, err := jr.CanonicalHashWithIgnoredFields(nil)
	require.NoError(t, err)
	require.Equal(t, plain, ignored)

	// It must share the default cache slot rather than opening a second one
	// keyed on a nil pointer; otherwise every no-op call re-hashes the body.
	fresh := &JsonRpcResponse{result: []byte(`{"hash":"0xaa","ts":"0x64"}`)}
	_, err = fresh.CanonicalHashWithIgnoredFields(nil)
	require.NoError(t, err)
	cached, ok := fresh.canonicalHashWithIgnored.Load(defaultCanonicalHashPlaceholder)
	require.True(t, ok, "an empty ignore list must fall through to the plain hash")
	require.Equal(t, plain, cached.(string))
}

// TestCanonicalHashWithIgnoredFields_MakesDifferingBodiesAgree is the consensus
// use case: two upstreams disagree only on a field the operator declared
// irrelevant. Both directions are asserted — ignoring must hide THAT field and
// nothing else.
func TestCanonicalHashWithIgnoredFields_MakesDifferingBodiesAgree(t *testing.T) {
	t.Parallel()

	a := &JsonRpcResponse{result: []byte(`{"status":"0x1","blockTimestamp":"0x64"}`)}
	b := &JsonRpcResponse{result: []byte(`{"status":"0x1","blockTimestamp":"0x65"}`)}
	c := &JsonRpcResponse{result: []byte(`{"status":"0x2","blockTimestamp":"0x64"}`)}

	ignore := []string{"blockTimestamp"}

	ha, err := a.CanonicalHashWithIgnoredFields(ignore)
	require.NoError(t, err)
	hb, err := b.CanonicalHashWithIgnoredFields(ignore)
	require.NoError(t, err)
	hc, err := c.CanonicalHashWithIgnoredFields(ignore)
	require.NoError(t, err)

	require.Equal(t, ha, hb, "the ignored field must not affect the hash")
	require.NotEqual(t, ha, hc, "a real difference must still show up")

	// Without the ignore list the two must part company again, otherwise the
	// removal is not doing anything and the equality above proves nothing.
	pa, err := a.CanonicalHash()
	require.NoError(t, err)
	pb, err := b.CanonicalHash()
	require.NoError(t, err)
	require.NotEqual(t, pa, pb)
}

// TestCanonicalHashWithIgnoredFields_WalksNestedAndWildcardPaths covers the two
// path shapes the shipped consensus defaults rely on: a dotted nested path and
// an array wildcard.
func TestCanonicalHashWithIgnoredFields_WalksNestedAndWildcardPaths(t *testing.T) {
	t.Parallel()

	body := func(ts1, ts2 string) *JsonRpcResponse {
		return &JsonRpcResponse{result: []byte(
			`{"receipt":{"status":"0x1","ts":"` + ts1 + `"},` +
				`"logs":[{"idx":"0x1","blockTimestamp":"` + ts2 + `"}]}`)}
	}

	ignore := []string{"receipt.ts", "logs.*.blockTimestamp"}

	h1, err := body("0x64", "0x64").CanonicalHashWithIgnoredFields(ignore)
	require.NoError(t, err)
	h2, err := body("0x99", "0x99").CanonicalHashWithIgnoredFields(ignore)
	require.NoError(t, err)
	require.Equal(t, h1, h2)

	// The sibling fields the paths do NOT name must still count.
	other := &JsonRpcResponse{result: []byte(
		`{"receipt":{"status":"0x2","ts":"0x64"},"logs":[{"idx":"0x1","blockTimestamp":"0x64"}]}`)}
	h3, err := other.CanonicalHashWithIgnoredFields(ignore)
	require.NoError(t, err)
	require.NotEqual(t, h1, h3)
}

// TestRemoveFieldsByPaths_APrefixPathPoisonsTheRootTree pins a DEFECT, not a
// desired behaviour. When one ignore path is a prefix of another ("logs" plus
// "logs.*.blockTimestamp"), the tree builder fails to descend into the leaf it
// already wrote and grafts the remaining segments onto the ROOT. The root then
// carries a "*" entry, which removeFieldsRecursive applies to every other
// top-level member. An operator sees two genuinely different bodies reported as
// agreeing. See the report accompanying this change; the assertions below
// record today's output so the fix has something to break.
func TestRemoveFieldsByPaths_APrefixPathPoisonsTheRootTree(t *testing.T) {
	t.Parallel()

	obj := map[string]interface{}{
		"logs": []interface{}{map[string]interface{}{"blockTimestamp": "0x64"}},
		"receipt": map[string]interface{}{
			"blockTimestamp": "0x64",
			"status":         "0x1",
		},
	}

	got := removeFieldsByPaths(obj, []string{"logs", "logs.*.blockTimestamp"})
	m, ok := got.(map[string]interface{})
	require.True(t, ok)

	_, hasLogs := m["logs"]
	require.False(t, hasLogs, "the plain 'logs' path removes logs, as asked")

	receipt, ok := m["receipt"].(map[string]interface{})
	require.True(t, ok)
	_, leaked := receipt["blockTimestamp"]
	require.False(t, leaked,
		"DEFECT: receipt.blockTimestamp was never named by any ignore path, "+
			"yet the root-level wildcard grafted by the prefix collision strips it")
	require.Equal(t, "0x1", receipt["status"], "unrelated members must survive")

	// The correct tree — built when the longer path comes first — leaves the
	// sibling alone. Same two paths, opposite order, different answer.
	reordered := removeFieldsByPaths(map[string]interface{}{
		"logs": []interface{}{map[string]interface{}{"blockTimestamp": "0x64"}},
		"receipt": map[string]interface{}{
			"blockTimestamp": "0x64",
			"status":         "0x1",
		},
	}, []string{"logs.*.blockTimestamp", "logs"})
	rm := reordered.(map[string]interface{})
	rr := rm["receipt"].(map[string]interface{})
	require.Equal(t, "0x64", rr["blockTimestamp"],
		"path order must not change which fields are removed, but today it does")
}

// TestRemoveFieldsByPaths_LeavesTheObjectAloneWhenThereIsNothingToRemove keeps
// the cheap exits honest.
func TestRemoveFieldsByPaths_LeavesTheObjectAloneWhenThereIsNothingToRemove(t *testing.T) {
	t.Parallel()

	obj := map[string]interface{}{"a": "0x1"}

	// The no-op path must hand back the ORIGINAL object. Rebuilding a copy for
	// every response with no ignore list is pure allocation on the hot path,
	// so assert identity by mutating what came back.
	got := removeFieldsByPaths(obj, nil).(map[string]interface{})
	got["injected"] = "x"
	require.Equal(t, "x", obj["injected"], "an empty path list must not copy the object")
	delete(obj, "injected")

	require.Nil(t, removeFieldsByPaths(nil, []string{"a"}))

	// A path that matches nothing must not drop anything.
	kept := removeFieldsByPaths(obj, []string{"zzz"}).(map[string]interface{})
	require.Equal(t, "0x1", kept["a"])
}

// TestRemoveFieldsByPaths_WildcardOnANonArrayIsNotDestructive — an operator can
// point a "*" path at a field that turns out to be a scalar on some chains.
// That must leave the value intact rather than blanking it.
func TestRemoveFieldsByPaths_WildcardOnANonArrayIsNotDestructive(t *testing.T) {
	t.Parallel()

	obj := map[string]interface{}{"logs": "0xdeadbeef"}
	got := removeFieldsByPaths(obj, []string{"logs.*.ts"}).(map[string]interface{})
	require.Equal(t, "0xdeadbeef", got["logs"])
}

// TestRemoveLeadingZeroes_TrimsOnlyHexQuantities covers the normalizer directly,
// including the inputs it must leave untouched. Over-trimming would corrupt an
// address or a decimal into a different value.
func TestRemoveLeadingZeroes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unprefixed hex quantity", "0x0005208", "5208"},
		{"already bare", "0x5208", "5208"},
		{"all zeroes collapse to nothing", "0x0000", ""},
		{"upper-case prefix", "0X0010", "10"},
		{"quoted hex keeps the trailing quote", `"0x0a"`, `a"`},
		{"quoted all-zero hex becomes nil", `"0x00"`, ""},
		{"decimal string is untouched", "0012", "0012"},
		{"short input is untouched", "0x", "0x"},
		{"plain word is untouched", "latest", "latest"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, string(removeLeadingZeroes([]byte(tc.in))))
		})
	}
}

// TestIsEmptyishValue_ClassifiesEveryBranch drives the filter that decides
// which members reach the hash at all.
func TestIsEmptyishValue(t *testing.T) {
	t.Parallel()

	empty := []interface{}{
		nil,
		"",
		"0x",
		"0x0",
		"0x000000",
		[]interface{}{},
		map[string]interface{}{},
		float64(0),
	}
	for _, v := range empty {
		require.True(t, isEmptyishValue(v), "%#v must be emptyish", v)
	}

	notEmpty := []interface{}{
		"0x1",
		"0x0001",
		"latest",
		[]interface{}{nil},
		map[string]interface{}{"a": nil},
		float64(1),
		true,
		false, // booleans are never emptyish, including false
	}
	for _, v := range notEmpty {
		require.False(t, isEmptyishValue(v), "%#v must not be emptyish", v)
	}
}

// TestCanonicalizeTo_WritesNothingForAnEmptyishDocument checks the "wrote"
// signal that the map and array branches use to drop members entirely.
func TestCanonicalizeTo_WritesNothingForAnEmptyishDocument(t *testing.T) {
	t.Parallel()

	var sink discardCounter
	wrote, err := canonicalizeTo(&sink, map[string]interface{}{"a": "0x0"})
	require.NoError(t, err)
	require.False(t, wrote)
	require.Equal(t, 0, sink.n, "an all-empty object must not emit a single byte")

	wrote, err = canonicalizeTo(&sink, map[string]interface{}{"a": "0x1"})
	require.NoError(t, err)
	require.True(t, wrote)
	require.Greater(t, sink.n, 0)
}

type discardCounter struct{ n int }

func (d *discardCounter) Write(p []byte) (int, error) { d.n += len(p); return len(p), nil }
