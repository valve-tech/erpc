package common

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Two hash functions decide whether eRPC treats two things as the same:
// canonicalizeTo feeds the consensus response hash, and hashValue feeds the
// cache key. Both write into an io.Writer, and both must abort on a write
// failure rather than hash a truncated byte stream — a short hash makes two
// different responses look identical to consensus, or two different requests
// share a cache entry.

// failAfterNWrites accepts n writes and then fails. Sweeping n over a range
// exercises every write site in a nested value without naming any of them.
type failAfterNWrites struct {
	remaining int
	written   int
	failed    int
	bytes     bytes.Buffer
}

func (w *failAfterNWrites) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		w.failed++
		return 0, errors.New("writer closed")
	}
	w.remaining--
	w.written += len(p)
	w.bytes.Write(p)
	return len(p), nil
}

func TestCanonicalizeTo_AbortsOnAWriteFailure(t *testing.T) {
	values := []interface{}{
		map[string]interface{}{"b": "0x2", "a": "0x1", "c": map[string]interface{}{"d": "0x3"}},
		[]interface{}{"0x1", "0x2", []interface{}{"0x3"}},
		map[string]interface{}{"list": []interface{}{"0x1", "0x2"}},
	}

	for vi, v := range values {
		// First find how many writes the value needs when nothing fails.
		counting := &failAfterNWrites{remaining: 1 << 20}
		wrote, err := canonicalizeTo(counting, v)
		require.NoError(t, err)
		require.True(t, wrote)
		total := (1 << 20) - counting.remaining
		require.Greater(t, total, 1, "value %d should need several writes", vi)

		for n := 0; n < total; n++ {
			t.Run(fmt.Sprintf("value%d/failAfter%d", vi, n), func(t *testing.T) {
				// The returned flag is not asserted: on the final closing
				// brace the function returns (true, err), and every caller
				// reads err first.
				//
				// Exactly ONE write may fail. A second failed attempt means
				// the first error was swallowed and canonicalization carried
				// on writing into a broken stream.
				w := &failAfterNWrites{remaining: n}
				_, err := canonicalizeTo(w, v)
				require.Error(t, err, "a write failure must surface, not be swallowed")
				require.Equal(t, 1, w.failed, "canonicalization must stop at the first write failure")
			})
		}
	}
}

// TestCanonicalizeTo_ReportsThatItWroteNothing pins the "wrote" flag for a
// container whose every member is emptyish. The parent uses that flag to drop
// the key entirely, which is what makes {"a":{}} and {} hash the same.
// (The resulting hashes themselves are pinned in json_rpc_canonical_test.go.)
func TestCanonicalizeTo_ReportsThatItWroteNothing(t *testing.T) {
	t.Run("an all-empty object writes nothing", func(t *testing.T) {
		w := &failAfterNWrites{remaining: 1 << 20}
		wrote, err := canonicalizeTo(w, map[string]interface{}{"a": nil, "b": ""})
		require.NoError(t, err)
		require.False(t, wrote)
	})

	t.Run("an all-empty array writes nothing", func(t *testing.T) {
		w := &failAfterNWrites{remaining: 1 << 20}
		wrote, err := canonicalizeTo(w, []interface{}{nil, ""})
		require.NoError(t, err)
		require.False(t, wrote)
	})
}

func TestHashValue_CoversEveryParameterType(t *testing.T) {
	tests := []struct {
		name  string
		value interface{}
	}{
		{"bool", true},
		{"int", 42},
		{"float", 1.5},
		{"string", "0xABC"},
		{"nil", nil},
		{"array", []interface{}{"0x1", 2, false}},
		{"object", map[string]interface{}{"to": "0x1", "data": "0x2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &failAfterNWrites{remaining: 1 << 20}
			require.NoError(t, hashValue(w, tt.value))
			require.Positive(t, w.written, "every supported type must contribute bytes")
		})
	}
}

func TestHashValue_RejectsAnUnsupportedType(t *testing.T) {
	type custom struct{ A int }

	w := &failAfterNWrites{remaining: 1 << 20}
	err := hashValue(w, custom{A: 1})

	require.Error(t, err, "an unhashable param must fail loudly, not hash to nothing")
	require.Contains(t, err.Error(), "unsupported type for value during hash")
	require.Zero(t, w.written)
}

func TestHashValue_AbortsOnAWriteFailure(t *testing.T) {
	nested := []interface{}{
		"0x1",
		map[string]interface{}{"b": "0x2", "a": []interface{}{"0x3", "0x4"}},
	}

	counting := &failAfterNWrites{remaining: 1 << 20}
	require.NoError(t, hashValue(counting, nested))
	total := (1 << 20) - counting.remaining
	require.Greater(t, total, 2)

	for n := 0; n < total; n++ {
		t.Run(fmt.Sprintf("failAfter%d", n), func(t *testing.T) {
			w := &failAfterNWrites{remaining: n}
			require.Error(t, hashValue(w, nested))
		})
	}
}

// TestCacheHash_IsCaseInsensitiveForStringParams pins the deliberate
// lowercasing: EVM addresses and hex data arrive in mixed case from different
// clients, and eRPC must not split one cache entry into two.
func TestCacheHash_IsCaseInsensitiveForStringParams(t *testing.T) {
	upper, err := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xABCDEF", "latest"}).CacheHash()
	require.NoError(t, err)
	lower, err := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xabcdef", "latest"}).CacheHash()
	require.NoError(t, err)

	require.Equal(t, upper, lower)
}

func TestCacheHash_SeparatesByMethodAndParameterValue(t *testing.T) {
	base := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xabc", "latest"})
	baseHash, err := base.CacheHash()
	require.NoError(t, err)
	require.Contains(t, baseHash, "eth_getBalance:")

	otherMethod, err := NewJsonRpcRequest("eth_getCode", []interface{}{"0xabc", "latest"}).CacheHash()
	require.NoError(t, err)
	require.NotEqual(t, baseHash, otherMethod)

	otherParam, err := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xabd", "latest"}).CacheHash()
	require.NoError(t, err)
	require.NotEqual(t, baseHash, otherParam)
}

// TestCacheHash_SeparatesParamListsThatConcatenateToTheSameBytes covers bug
// 118. hashValue used to write each value straight after the previous one, so
// any two parameter lists whose bytes concatenated the same way shared ONE
// cache key — and whichever request landed first served the other its data.
// Every pair below collided under the old hasher.
func TestCacheHash_SeparatesParamListsThatConcatenateToTheSameBytes(t *testing.T) {
	t.Parallel()

	hash := func(method string, params []interface{}) string {
		t.Helper()
		h, err := NewJsonRpcRequest(method, params).CacheHash()
		require.NoError(t, err)
		require.Contains(t, h, method+":", "every request must produce a real key")
		return h
	}

	// Three well-formed 32-byte log topics.
	topicA := "0x" + strings.Repeat("a", 64)
	topicB := "0x" + strings.Repeat("b", 64)
	topicC := "0x" + strings.Repeat("c", 64)

	cases := []struct {
		name   string
		method string
		left   []interface{}
		right  []interface{}
	}{
		{
			// The probe that confirmed the bug: the boundary between two
			// adjacent string params moves and nothing records that it moved.
			name:   "adjacent params",
			method: "eth_getStorageAt",
			left:   []interface{}{"0xabc", "0xdef", "latest"},
			right:  []interface{}{"0xabc0xdef", "", "latest"},
		},
		{
			// The reachable case, and the worst one: BOTH sides are valid
			// eth_getLogs filters that a node answers and eRPC caches, and
			// they ask different questions. On the left topic0 is A or B and
			// topic1 is C. On the right topic0 is A and topic1 is B or C. The
			// old hasher wrote A, B and C in that order for both.
			name:   "eth_getLogs topic nesting",
			method: "eth_getLogs",
			left: []interface{}{map[string]interface{}{
				"topics": []interface{}{[]interface{}{topicA, topicB}, topicC},
			}},
			right: []interface{}{map[string]interface{}{
				"topics": []interface{}{topicA, []interface{}{topicB, topicC}},
			}},
		},
		{
			name:   "an array against the string its members concatenate to",
			method: "eth_call",
			left:   []interface{}{[]interface{}{"0xa", "b"}},
			right:  []interface{}{"0xab"},
		},
		{
			// The boundary between an object key and its value.
			name:   "object key against object value",
			method: "eth_call",
			left:   []interface{}{map[string]interface{}{"ab": "c"}},
			right:  []interface{}{map[string]interface{}{"a": "bc"}},
		},
		{
			name:   "two arrays against one",
			method: "eth_call",
			left:   []interface{}{[]interface{}{"0xa"}, []interface{}{"0xb"}},
			right:  []interface{}{[]interface{}{"0xa", "0xb"}},
		},
		{
			// A number and the string that spells it must not share a key.
			name:   "a number against its own digits",
			method: "eth_call",
			left:   []interface{}{42},
			right:  []interface{}{"42"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.NotEqual(t, hash(tc.method, tc.left), hash(tc.method, tc.right),
				"two different requests must not share one cache key")
		})
	}
}

// TestHashValue_EncodesEveryStructureDistinctly states the rule the cases
// above sample: the byte stream hashValue writes must identify the value it
// came from. Any two distinct values that produce the same bytes are two
// requests that share one cached answer.
func TestHashValue_EncodesEveryStructureDistinctly(t *testing.T) {
	t.Parallel()

	values := []interface{}{
		nil,
		"",
		"null",
		"a",
		"ab",
		"a:b",
		"1",
		1,
		1.0,
		true,
		"true",
		[]interface{}{},
		[]interface{}{""},
		[]interface{}{"a"},
		[]interface{}{"ab"},
		[]interface{}{"a", "b"},
		[]interface{}{[]interface{}{"a"}, "b"},
		[]interface{}{[]interface{}{"a", "b"}},
		[]interface{}{nil},
		map[string]interface{}{},
		map[string]interface{}{"a": "b"},
		map[string]interface{}{"ab": ""},
		map[string]interface{}{"a": "", "b": ""},
		map[string]interface{}{"a": []interface{}{"b"}},
		// A string that spells a whole frame must not be read as one.
		"s1:a",
		[]interface{}{"s1:a"},
	}

	seen := make(map[string]interface{}, len(values))
	for _, v := range values {
		w := &failAfterNWrites{remaining: 1 << 20}
		require.NoError(t, hashValue(w, v))
		encoded := w.bytes.String()

		if prev, clash := seen[encoded]; clash {
			require.Failf(t, "two values encode to the same bytes",
				"%#v and %#v both encode to %q", prev, v, encoded)
		}
		seen[encoded] = v
	}
}

// TestCanonicalHash_SeparatesResponsesAnUpstreamCanForge covers bug 135.
// canonicalizeTo used to write JSON punctuation around its members and write
// string values raw, with no quotes and no escaping. A string could therefore
// spell structure that was never in the response, and a quantity could spell a
// number. Every pair below produced ONE hash under the old form, so consensus
// reported the two upstreams as agreeing.
//
// An upstream owns the bytes of its own response, which is what makes this
// reachable: a node that wants to pass consensus while answering differently
// pads one string field until its canonical form matches the honest answer.
func TestCanonicalHash_SeparatesResponsesAnUpstreamCanForge(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		left  string
		right string
	}{
		{
			// The first probe recorded in the entry. One member carries the
			// punctuation that separates two members.
			name:  "one member spells two",
			left:  `{"a":"1,\"b\":2"}`,
			right: `{"a":"1","b":"2"}`,
		},
		{
			// The second probe. The comma inside the string forges an array
			// element boundary.
			name:  "one array element spells two",
			left:  `{"a":["x","y"]}`,
			right: `{"a":["x,y"]}`,
		},
		{
			// The worst pair, because BOTH sides are well-formed answers to
			// the same call and neither needs a punctuation character. The
			// left says 291 and the right says 0x291, which is 657. Chains
			// whose results carry JSON numbers make this an everyday shape.
			name:  "a number against the quantity that spells it",
			left:  `{"result":291}`,
			right: `{"result":"0x291"}`,
		},
		{
			// The old form dropped the 0x prefix, so a quantity and a plain
			// string reached the hash as the same bytes.
			name:  "a quantity against the plain string of its digits",
			left:  `{"a":"0xab"}`,
			right: `{"a":"ab"}`,
		},
		{
			// A whole receipt forged from one field. The left is an honest
			// two-field receipt; the right carries a single junk string.
			name:  "a one-field body spells a two-field receipt",
			left:  `{"blockHash":"0xaa","status":"0x1"}`,
			right: `{"blockHash":"aa,\"status\":1"}`,
		},
		{
			// A string against the log entry it spells, inside an array.
			name:  "a log entry against the string that spells it",
			left:  `{"logs":[{"a":"0x1"}]}`,
			right: `{"logs":["{\"a\":1}"]}`,
		},
		{
			// A string against the nested object it spells.
			name:  "a nested object against the string that spells it",
			left:  `{"a":{"b":"0x1"}}`,
			right: `{"a":"{\"b\":1}"}`,
		},
		{
			// A boolean against the string that spells it.
			name:  "a boolean against its own spelling",
			left:  `{"removed":true}`,
			right: `{"removed":"true"}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.NotEqual(t, hashOf(t, tc.left), hashOf(t, tc.right),
				"two different responses must not hash the same")
		})
	}
}

// TestCanonicalHashWithIgnoredFields_FramesTheBodyItHashes states that the
// ignore-list path gets the same framing. It removes fields and then hashes
// what is left, so an unframed encoding would leave that path forgeable even
// after the plain one was fixed.
func TestCanonicalHashWithIgnoredFields_FramesTheBodyItHashes(t *testing.T) {
	t.Parallel()

	hash := func(raw string) string {
		t.Helper()
		jr := &JsonRpcResponse{result: []byte(raw)}
		h, err := jr.CanonicalHashWithIgnoredFields([]string{"blockTimestamp"})
		require.NoError(t, err)
		require.NotEmpty(t, h)
		return h
	}

	honest := hash(`{"blockTimestamp":"0x64","blockHash":"0xaa","status":"0x1"}`)
	forged := hash(`{"blockTimestamp":"0x99","blockHash":"aa,\"status\":1"}`)
	require.NotEqual(t, honest, forged,
		"removing a field must not remove the framing around the rest")

	// The control: the ignored field must still be ignored, otherwise the
	// inequality above proves nothing about the ignore list.
	same := hash(`{"blockTimestamp":"0x99","blockHash":"0xaa","status":"0x1"}`)
	require.Equal(t, honest, same)
}

// TestCanonicalizeTo_EncodesEveryStructureDistinctly states the rule the pairs
// above sample: the byte stream canonicalizeTo writes must identify the value
// it came from. Two distinct values that write the same bytes are two
// upstreams that consensus calls one.
//
// The deliberate identifications are NOT in this list, because they are the
// point of the canonical form: a padded quantity agrees with a bare one, and
// an emptyish member agrees with an absent one.
func TestCanonicalizeTo_EncodesEveryStructureDistinctly(t *testing.T) {
	t.Parallel()

	values := []interface{}{
		"a",
		"ab",
		"a,b",
		"0xab",
		"0xa",
		"1",
		float64(1),
		float64(291),
		"0x291",
		true,
		"true",
		[]interface{}{"a"},
		[]interface{}{"ab"},
		[]interface{}{"a", "b"},
		[]interface{}{[]interface{}{"a"}, "b"},
		[]interface{}{[]interface{}{"a", "b"}},
		map[string]interface{}{"a": "b"},
		map[string]interface{}{"ab": "c"},
		map[string]interface{}{"a": "b:c"},
		map[string]interface{}{"a": "b", "c": "d"},
		map[string]interface{}{"a": []interface{}{"b"}},
		map[string]interface{}{"a": map[string]interface{}{"b": "c"}},
		// A string that spells a whole frame must not be read as one.
		"o1:k1:as1:b",
		[]interface{}{"o1:k1:as1:b"},
		map[string]interface{}{"a": "s1:b"},
	}

	seen := make(map[string]interface{}, len(values))
	for _, v := range values {
		w := &failAfterNWrites{remaining: 1 << 20}
		wrote, err := canonicalizeTo(w, v)
		require.NoError(t, err)
		require.True(t, wrote, "%#v must write something", v)
		encoded := w.bytes.String()

		if prev, clash := seen[encoded]; clash {
			require.Failf(t, "two values encode to the same bytes",
				"%#v and %#v both encode to %q", prev, v, encoded)
		}
		seen[encoded] = v
	}
}
