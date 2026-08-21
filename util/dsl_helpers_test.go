package util

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// dsl.go is developer tooling: it turns natural-language scenarios into DSL
// lines with an LLM and caches the result next to the test that uses it. The
// network-calling half stays untested on purpose. The pure helpers below do
// not: they decide whether a developer gets the DSL for the scenarios they
// just wrote, or silently keeps running the previous generation.

func TestDslCache_RoundTripsAndCreatesTheParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "scenarios.json")
	in := &dslCache{
		Hash:  "abcdef0123456789",
		Items: []dslItem{{NL: "two nodes agree", DSL: "a,b => consensus"}},
	}

	require.NoError(t, writeDslCache(path, in))

	out := readDslCache(t, path)
	require.NotNil(t, out)
	require.Equal(t, in.Hash, out.Hash)
	require.Equal(t, in.Items, out.Items)

	// The writer stages through a .tmp file and renames. A leftover .tmp
	// means the rename did not happen and the cache is half-written.
	_, err := os.Stat(path + ".tmp")
	require.True(t, os.IsNotExist(err), "the staging file must be gone after a successful write")
}

func TestReadDslCache_ReturnsNilWhenThereIsNoCache(t *testing.T) {
	// nil means "generate". Returning an empty struct instead would look
	// like a cache with hash "" and no items, and the caller would compare
	// hashes against a value that never existed.
	require.Nil(t, readDslCache(t, filepath.Join(t.TempDir(), "absent.json")))
}

func TestReadDslCache_ReturnsNilWhenTheCacheIsCorrupt(t *testing.T) {
	// A truncated or hand-edited cache must send the caller back to the LLM,
	// not surface a partially decoded set of items.
	path := filepath.Join(t.TempDir(), "corrupt.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"hash":"abc","items":[{`), 0o600))
	require.Nil(t, readDslCache(t, path))
}

func TestSafeHash_ShortensAHashAndNamesTheEmptyCase(t *testing.T) {
	require.Equal(t, "abcdef01", safeHash("abcdef0123456789"))
	require.Equal(t, "<none>", safeHash(""), "an empty hash must read as absent, not as a blank match")
	require.Equal(t, "abc", safeHash("abc"), "a short hash must not be truncated past its length")
}

func TestSanitizeTestName_ProducesASingleLowercaseIdentifier(t *testing.T) {
	// The result becomes a Go subtest name. Spaces, dots and parentheses in a
	// subtest name break `go test -run`, so they have to collapse.
	require.Equal(t, "two_nodes_agree", SanitizeTestName("Two nodes agree"))
	require.Equal(t, "a_b_c", SanitizeTestName("a.b/c"))
	require.Equal(t, "leading_and_trailing", SanitizeTestName("  leading, and (trailing)  "))
	require.Equal(t, "one_two", SanitizeTestName("one---two"))
}

func TestChunkScenarios_CoversEveryItemExactlyOnce(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}

	chunks := chunkScenarios(items, 2)
	require.Equal(t, [][]string{{"a", "b"}, {"c", "d"}, {"e"}}, chunks,
		"a dropped tail chunk silently loses the last scenarios")

	require.Nil(t, chunkScenarios(nil, 2))
	require.Equal(t, [][]string{items}, chunkScenarios(items, 99),
		"a chunk larger than the input must not over-run the slice")

	// A non-positive size would loop forever without the default.
	require.Equal(t, [][]string{items}, chunkScenarios(items, 0))
	require.Equal(t, [][]string{items}, chunkScenarios(items, -1))
}

func TestParseRetryAfterHeader_ClampsAndRejects(t *testing.T) {
	// The caller sleeps for whatever this returns before retrying OpenAI.
	require.Equal(t, 3*time.Second, parseRetryAfterHeader("3"))
	require.Equal(t, 3*time.Second, parseRetryAfterHeader("  3  "))
	require.Equal(t, 5*time.Minute, parseRetryAfterHeader("100000"),
		"an unbounded Retry-After would park the test run for hours")

	// 0 means "fall back to exponential backoff".
	require.Equal(t, time.Duration(0), parseRetryAfterHeader("0"))
	require.Equal(t, time.Duration(0), parseRetryAfterHeader("-5"))
	require.Equal(t, time.Duration(0), parseRetryAfterHeader(""))
	require.Equal(t, time.Duration(0), parseRetryAfterHeader("Wed, 21 Oct 2015 07:28:00 GMT"),
		"the HTTP-date form is not supported and must not parse to something arbitrary")
}

func TestParseOpenAIItems_ReadsTheItemsShapeAndSanitizesTheName(t *testing.T) {
	items, err := parseOpenAIItems(`{"items":[{"nl":"Two nodes agree.","dsl":"  a,b => consensus  "}]}`)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "two_nodes_agree", items[0].NL)
	require.Equal(t, "a,b => consensus", items[0].DSL,
		"an untrimmed DSL line does not match the parser that consumes it")
}

func TestParseOpenAIItems_RejectsAnythingElse(t *testing.T) {
	// The model sometimes answers with prose or with the legacy bare-DSL
	// shape. Both must be an error so the caller does not cache an empty set.
	for _, content := range []string{
		`not json at all`,
		`{"items":[]}`,
		`{"dsl":["a,b => consensus"]}`,
	} {
		_, err := parseOpenAIItems(content)
		require.Error(t, err, "content %q must not parse", content)
	}
}
