package valvebilling

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/big"
	"os"
	"regexp"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Why this file exists.
//
// The SCRIPT cannot diverge from the monorepo's copy. Both languages key
// EVALSHA on the SHA1 of the exact body, so a change on either side moves the
// digest and TestAuthorizeScript_MatchesTheMonorepoDigest goes red.
//
// The ARGUMENTS carry no such pin. Go builds KEYS and ARGV in authorize.go;
// the relay builds them in the monorepo's credits-lua.ts. Put a value at a
// different offset, format a number differently, order the per-method triples
// differently, or drop one argument, and the SHARED script computes a
// different verdict from the same request. Nothing errors. That is the
// highest-value untested surface in this package, so the exact arguments are
// pinned here as a golden fixture.
//
// The fixture is compared against what the SHIPPED call path emits: the tests
// hand Authorize a fake redis.Scripter and record the real EVALSHA. No seam
// was added to authorize.go for this.
const argvGoldenFile = "testdata/authorize-argv-golden.json"

// Regenerate with:
//
//	go test ./valvebilling/ -run TestAuthorizeArgv_MatchesTheGoldenFixture -update-argv-golden
//
// Read the diff before you commit it. A diff here is a change to a contract
// shared with the TypeScript relay, so the question to answer is not "is the
// new value right" but "did credits-lua.ts change with it".
var updateArgvGolden = flag.Bool("update-argv-golden", false,
	"rewrite "+argvGoldenFile+" from the arguments Authorize currently sends")

// recordedScriptCall is one call Authorize made to the Scripter.
//
// Argv holds interface{} rather than string on purpose. Authorize formats
// every argument itself, and this type must be able to SHOW a caller that
// stopped doing so — go-redis would then format the value, and its rendering
// of a float differs from String(bigint) in TypeScript.
type recordedScriptCall struct {
	Command string
	Script  string
	Keys    []string
	Argv    []interface{}
}

// argvRecorder is a redis.Scripter that records the call and answers with a
// canned verdict. Authorize only needs the reply to parse, so the cheapest
// legal answer is the allow pair the script returns on the commit path.
type argvRecorder struct {
	calls []recordedScriptCall
}

var _ redis.Scripter = (*argvRecorder)(nil)

func (r *argvRecorder) record(command, script string, keys []string, args []interface{}) *redis.Cmd {
	r.calls = append(r.calls, recordedScriptCall{
		Command: command,
		Script:  script,
		Keys:    append([]string(nil), keys...),
		Argv:    append([]interface{}(nil), args...),
	})
	return redis.NewCmdResult([]interface{}{"ok", "FULL"}, nil)
}

func (r *argvRecorder) Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	return r.record("eval", script, keys, args)
}

func (r *argvRecorder) EvalSha(ctx context.Context, sha1 string, keys []string, args ...interface{}) *redis.Cmd {
	return r.record("evalsha", sha1, keys, args)
}

func (r *argvRecorder) EvalRO(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	return r.record("eval_ro", script, keys, args)
}

func (r *argvRecorder) EvalShaRO(ctx context.Context, sha1 string, keys []string, args ...interface{}) *redis.Cmd {
	return r.record("evalsha_ro", sha1, keys, args)
}

func (r *argvRecorder) ScriptExists(ctx context.Context, hashes ...string) *redis.BoolSliceCmd {
	out := make([]bool, len(hashes))
	for i := range out {
		out[i] = true
	}
	return redis.NewBoolSliceResult(out, nil)
}

func (r *argvRecorder) ScriptLoad(ctx context.Context, script string) *redis.StringCmd {
	return redis.NewStringResult(AuthorizeScriptSHA1, nil)
}

// recordAuthorize runs Authorize against the recorder and returns the single
// call it made, with every argument resolved to a string.
//
// It asserts the call is EVALSHA against the pinned digest. That is what makes
// the two languages share one cached script inside Redis; an EVAL here would
// still work and would still be wrong.
func recordAuthorize(t *testing.T, in AuthorizeInput) (recordedScriptCall, []string) {
	t.Helper()

	rec := &argvRecorder{}
	v, err := Authorize(context.Background(), rec, in)
	require.NoError(t, err)
	require.True(t, v.OK(), "the canned reply should parse as an allow")

	require.Len(t, rec.calls, 1, "Authorize must run the script exactly once per request")
	call := rec.calls[0]
	assert.Equal(t, "evalsha", call.Command,
		"Authorize stopped using EVALSHA; the relay and this process no longer share one cached script")
	assert.Equal(t, AuthorizeScriptSHA1, call.Script,
		"the digest on the wire is not the pinned one")

	argv := make([]string, len(call.Argv))
	for i, a := range call.Argv {
		s, ok := a.(string)
		require.True(t, ok,
			"ARGV[%d] is a %T, not a string; Authorize must format every argument itself, "+
				"because go-redis renders a Go number differently from String(x) in TypeScript", i+1, a)
		argv[i] = s
	}
	return call, argv
}

// argvCase is one representative request. The cases are chosen for what they
// would EXPOSE, not for coverage: a transposed limit, a changed number format,
// a reordered per-method triple, an off-by-one in the per-method base offset.
type argvCase struct {
	Name  string
	Why   string
	Input AuthorizeInput
}

// distinctLimits gives every one of the nine limits a different value.
//
// This is load-bearing. Limits arrive as nine adjacent decimal strings, so if
// two of them shared a value, swapping those two offsets would leave the
// golden fixture byte-identical and the gate would silently move. Distinct
// values make every transposition visible.
func distinctLimits() Limits {
	return Limits{
		DayLimit:      1000000,
		CUSecondLimit: 3000,
		CUDayLimit:    5000000,
		SlowThreshold: 7000,
		FullCPS:       11000,
		SlowCPS:       13,
		FullRPS:       17,
		SlowRPS:       19,
		KeyRPS:        23,
	}
}

// The api key id is 32 hex characters, the shape HashAPIKey emits, made of one
// repeated character. The obvious synthetic value — the hex alphabet twice —
// scores entropy 4.0 and secret scanners read it as a credential. A repeated
// character carries the same shape at near-zero entropy. Same reasoning as
// baseInput in authorize_test.go.
const (
	argvKeyID    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	argvAltKeyID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// argvFixedNow is 2023-11-14T22:13:20Z. Unix 1700000000 divides to day 19675
// with 61 hours to spare, so it sits nowhere near a boundary.
var argvFixedNow = time.Unix(1_700_000_000, 0).UTC()

// argvDayBoundary is the first second of day 19676: 19676 * 86400.
var argvDayBoundary = time.Unix(19676*86400, 0).UTC()

func argvCases() []argvCase {
	base := func() AuthorizeInput {
		return AuthorizeInput{
			AccountID: "acct_1",
			KeyID:     argvKeyID,
			Now:       argvFixedNow,
			Cost:      big.NewInt(100),
			CUCost:    5,
			Limits:    distinctLimits(),
		}
	}

	withMethods := func(in AuthorizeInput, m ...MethodBucket) AuthorizeInput {
		in.Methods = m
		return in
	}

	maxed := Limits{
		DayLimit: math.MaxInt64, CUSecondLimit: math.MaxInt64, CUDayLimit: math.MaxInt64,
		SlowThreshold: math.MaxInt64, FullCPS: math.MaxInt64, SlowCPS: math.MaxInt64,
		FullRPS: math.MaxInt64, SlowRPS: math.MaxInt64, KeyRPS: math.MaxInt64,
	}

	cases := []argvCase{
		{
			Name:  "no methods",
			Why:   "The ordinary request. The per-method gate is off, so ARGV[12] is 0 and ARGV ends there.",
			Input: base(),
		},
		{
			Name: "one method",
			Why:  "The first per-method triple. Its base offset is 12, so an off-by-one lands on the count itself.",
			Input: withMethods(base(),
				MethodBucket{Method: "eth_call", Count: 1, Limit: 5}),
		},
		{
			Name: "three methods",
			Why: "The offset the script computes as base = 12 + (i-1)*3. Every count and every limit " +
				"differs, and one limit is 0 (the gate off for that method alone), so a reordered or " +
				"mis-strided triple cannot land on the same fixture.",
			Input: withMethods(base(),
				MethodBucket{Method: "eth_call", Count: 1, Limit: 5},
				MethodBucket{Method: "eth_getLogs", Count: 2, Limit: 0},
				MethodBucket{Method: "eth_getBalance", Count: 3, Limit: 7}),
		},
		{
			Name: "cost above 2^53",
			Why: "The cost travels as a decimal STRING, matching String(bigint) in TypeScript. " +
				"2^53+1 is the first integer a float64 cannot hold, so a Go-side float would " +
				"round it to 9007199254740992 here.",
			Input: func() AuthorizeInput {
				in := base()
				in.Cost = new(big.Int).SetUint64(1<<53 + 1)
				return in
			}(),
		},
		{
			Name: "cost of 10^24 wei",
			Why: "A wei-scale amount. big.Int.String never emits an exponent; a float64 would " +
				"render this as 1e+24, which tonumber() in Lua would still parse — silently, " +
				"and with a different value.",
			Input: func() AuthorizeInput {
				in := base()
				in.Cost, _ = new(big.Int).SetString("1000000000000000000000000", 10)
				return in
			}(),
		},
		{
			Name: "cost of zero",
			Why: "Zero cost turns off the credits-per-second bucket inside the script " +
				"(chargeCps needs cost > 0). The wire form must be \"0\", not \"\" and not \"0e0\".",
			Input: func() AuthorizeInput {
				in := base()
				in.Cost = big.NewInt(0)
				in.CUCost = 0
				return in
			}(),
		},
		{
			Name: "every limit at zero",
			Why: "The no-gate state. The script skips a gate whose limit is 0, including the " +
				"credits-per-second bucket, which is the only bound on an overdraft. Pinned " +
				"because zero must arrive as the number 0 and not as an omitted argument.",
			Input: func() AuthorizeInput {
				in := base()
				in.Limits = Limits{}
				return in
			}(),
		},
		{
			Name: "every limit at MaxInt64",
			Why: "The decimal form of 9223372036854775807. A float64 renders it as " +
				"9223372036854776000, so this pins that the limits are formatted as integers.",
			Input: func() AuthorizeInput {
				in := base()
				in.Limits = maxed
				return in
			}(),
		},
		{
			Name: "account id carrying the key delimiter",
			Why: "Nothing escapes the account id: it is interpolated straight into colon-delimited " +
				"key names. Redis keys are binary safe, so this is not a bug, but the exact " +
				"unescaped result is pinned — a future sanitiser on either side would split the " +
				"ledger in two without an error.",
			Input: func() AuthorizeInput {
				in := base()
				in.AccountID = "acct:1 \"quoted\"\n\tsp ce\\é"
				in.KeyID = argvAltKeyID
				in.Methods = []MethodBucket{
					{Method: "eth_call:weird arg", Count: 1, Limit: 5},
				}
				return in
			}(),
		},
		{
			Name: "the first second of a day",
			Why: "day = sec / 86400 in Go and Math.floor(sec / 86400) in TypeScript. This second " +
				"is exactly 19676 * 86400, so it pins the boundary the two forms must agree on.",
			Input: func() AuthorizeInput {
				in := base()
				in.Now = argvDayBoundary
				return in
			}(),
		},
		{
			Name: "the last second of a day",
			Why:  "One second earlier still belongs to day 19675. Together with the case above, this pins the floor.",
			Input: func() AuthorizeInput {
				in := base()
				in.Now = argvDayBoundary.Add(-time.Second)
				return in
			}(),
		},
	}
	return cases
}

// argvGolden is the fixture. It keeps the input beside the arguments so a
// reader can see WHICH request produced them without running anything.
type argvGolden struct {
	Why        string           `json:"why"`
	ScriptSHA1 string           `json:"scriptSha1"`
	Cases      []argvGoldenCase `json:"cases"`
}

type argvGoldenCase struct {
	Name  string          `json:"name"`
	Why   string          `json:"why"`
	Input argvGoldenInput `json:"input"`
	// Keys and Argv are in the order Authorize sends them. Nothing here is
	// sorted, because the ORDER is the thing under test.
	Keys []string `json:"keys"`
	Argv []string `json:"argv"`
}

type argvGoldenInput struct {
	AccountID string             `json:"accountId"`
	KeyID     string             `json:"keyId"`
	NowUnix   int64              `json:"nowUnix"`
	Cost      string             `json:"cost"`
	CUCost    int64              `json:"cuCost"`
	Limits    argvGoldenLimits   `json:"limits"`
	Methods   []argvGoldenMethod `json:"methods"`
}

// The field order here mirrors the ARGV order, so the fixture reads top to
// bottom the way the script does.
type argvGoldenLimits struct {
	DayLimit      int64 `json:"dayLimit"`
	CUSecondLimit int64 `json:"cuSecondLimit"`
	CUDayLimit    int64 `json:"cuDayLimit"`
	SlowThreshold int64 `json:"slowThreshold"`
	FullCPS       int64 `json:"fullCps"`
	SlowCPS       int64 `json:"slowCps"`
	FullRPS       int64 `json:"fullRps"`
	SlowRPS       int64 `json:"slowRps"`
	KeyRPS        int64 `json:"keyRps"`
}

type argvGoldenMethod struct {
	Method string `json:"method"`
	Count  int64  `json:"count"`
	Limit  int64  `json:"limit"`
}

func goldenInputOf(in AuthorizeInput) argvGoldenInput {
	out := argvGoldenInput{
		AccountID: in.AccountID,
		KeyID:     in.KeyID,
		NowUnix:   in.Now.Unix(),
		Cost:      in.Cost.String(),
		CUCost:    in.CUCost,
		Limits: argvGoldenLimits{
			DayLimit:      in.Limits.DayLimit,
			CUSecondLimit: in.Limits.CUSecondLimit,
			CUDayLimit:    in.Limits.CUDayLimit,
			SlowThreshold: in.Limits.SlowThreshold,
			FullCPS:       in.Limits.FullCPS,
			SlowCPS:       in.Limits.SlowCPS,
			FullRPS:       in.Limits.FullRPS,
			SlowRPS:       in.Limits.SlowRPS,
			KeyRPS:        in.Limits.KeyRPS,
		},
		Methods: []argvGoldenMethod{},
	}
	for _, m := range in.Methods {
		out.Methods = append(out.Methods, argvGoldenMethod(m))
	}
	return out
}

// The golden fixture. A diff here is a change to the argument contract the
// TypeScript relay shares, so it must be read, not regenerated away.
func TestAuthorizeArgv_MatchesTheGoldenFixture(t *testing.T) {
	got := argvGolden{
		Why: "The exact KEYS and ARGV that valvebilling.Authorize sends to the shared authorize.lua. " +
			"Order is significant everywhere; nothing here is sorted. The TypeScript relay builds " +
			"the same two arrays in credits-lua.ts, and the script cannot tell the two apart. " +
			"Regenerate with: go test ./valvebilling/ -run TestAuthorizeArgv_MatchesTheGoldenFixture -update-argv-golden",
		ScriptSHA1: AuthorizeScriptSHA1,
	}
	for _, c := range argvCases() {
		call, argv := recordAuthorize(t, c.Input)
		got.Cases = append(got.Cases, argvGoldenCase{
			Name:  c.Name,
			Why:   c.Why,
			Input: goldenInputOf(c.Input),
			Keys:  call.Keys,
			Argv:  argv,
		})
	}

	if *updateArgvGolden {
		blob, err := json.MarshalIndent(got, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(argvGoldenFile, append(blob, '\n'), 0o644))
		t.Logf("rewrote %s with %d cases; read the diff before committing it", argvGoldenFile, len(got.Cases))
		return
	}

	raw, err := os.ReadFile(argvGoldenFile)
	require.NoError(t, err, "the golden fixture is the contract; without it nothing here is proven")
	var want argvGolden
	require.NoError(t, json.Unmarshal(raw, &want))

	assert.Equal(t, want.ScriptSHA1, got.ScriptSHA1,
		"the fixture was captured against a different script")

	byName := map[string]argvGoldenCase{}
	for _, c := range want.Cases {
		byName[c.Name] = c
	}
	for _, g := range got.Cases {
		w, ok := byName[g.Name]
		if !assert.True(t, ok, "case %q is not in the fixture; regenerate and read the diff", g.Name) {
			continue
		}
		delete(byName, g.Name)

		t.Run(g.Name, func(t *testing.T) {
			// The input is echoed into the fixture so that a changed test input
			// cannot pass by quietly rewriting what the case means.
			assert.Equal(t, w.Input, g.Input, "the fixture was captured from a different input")

			require.Equal(t, len(w.Keys), len(g.Keys), "the KEYS count changed")
			for i := range w.Keys {
				assert.Equal(t, w.Keys[i], g.Keys[i], "KEYS[%d]", i+1)
			}
			require.Equal(t, len(w.Argv), len(g.Argv), "the ARGV count changed")
			for i := range w.Argv {
				assert.Equal(t, w.Argv[i], g.Argv[i], "ARGV[%d]", i+1)
			}
		})
	}
	for name := range byName {
		assert.Fail(t, "the fixture holds a case nothing produces", "case %q", name)
	}
}

// Every offset, named after what authorize.lua reads at that position.
//
// A single deep-equal against the fixture tells a future reader that something
// changed. It does not tell them WHAT. These assertions do, and they state the
// expected value independently of authorize.go's own formatting.
func TestAuthorizeArgv_EachOffsetHoldsWhatTheLuaReadsThere(t *testing.T) {
	in := AuthorizeInput{
		AccountID: "acct_1",
		KeyID:     argvKeyID,
		Now:       argvFixedNow,
		Cost:      big.NewInt(100),
		CUCost:    5,
		Limits:    distinctLimits(),
		Methods: []MethodBucket{
			{Method: "eth_call", Count: 1, Limit: 5},
			{Method: "eth_getLogs", Count: 2, Limit: 0},
			{Method: "eth_getBalance", Count: 3, Limit: 7},
		},
	}
	call, argv := recordAuthorize(t, in)

	sec := in.Now.Unix() // 1700000000
	day := sec / 86400   // 19675
	acct := in.AccountID
	key := in.KeyID
	lim := in.Limits
	n := len(in.Methods)
	require.Equal(t, 3, n, "the per-method offsets below assume three methods")

	// KEYS, in the order the script reads them. Every index the script names
	// appears exactly once.
	for _, w := range []struct {
		luaIndex int
		readAs   string
		value    string
	}{
		{1, "EXISTS: the mode-1 per-request lock", "valve:credits:" + acct + ":per_request_lock"},
		{2, "GET/INCR: requests this day, per key", fmt.Sprintf("valve:rate:d:%s:%d", key, day)},
		{3, "GET/INCRBYFLOAT: compute units this second, per key", fmt.Sprintf("valve:rate:cu:s:%s:%d", key, sec)},
		{4, "GET/INCRBYFLOAT: compute units this day, per key", fmt.Sprintf("valve:rate:cu:d:%s:%d", key, day)},
		{5, "GET: the credit ceiling", "valve:credits:" + acct + ":ceiling"},
		{6, "GET: pending top-ups", "valve:credits:" + acct + ":pending"},
		{7, "GET: settled spend", "valve:credits:" + acct + ":spend"},
		{8, "GET: the closing flag", "valve:credits:" + acct + ":closing"},
		{9, "GET/INCRBY: the credits-per-second bucket, per account", "valve:credits:" + acct + ":cps"},
		{10, "GET/INCR: requests this second, per key", fmt.Sprintf("valve:rate:s:%s:%d", key, sec)},
	} {
		require.Greater(t, len(call.Keys), w.luaIndex-1, "KEYS[%d] is missing", w.luaIndex)
		assert.Equal(t, w.value, call.Keys[w.luaIndex-1], "KEYS[%d] — %s", w.luaIndex, w.readAs)
	}
	assert.Len(t, call.Keys, 10, "the script reads KEYS[1] through KEYS[10] and no further")

	// The fixed ARGV prefix.
	for _, w := range []struct {
		luaIndex int
		readAs   string
		value    string
	}{
		{1, "cost — a decimal string, matching String(bigint)", in.Cost.String()},
		{2, "cuCost", strconv.FormatInt(in.CUCost, 10)},
		{3, "dayLim", strconv.FormatInt(lim.DayLimit, 10)},
		{4, "cuSLim", strconv.FormatInt(lim.CUSecondLimit, 10)},
		{5, "cuDLim", strconv.FormatInt(lim.CUDayLimit, 10)},
		{6, "thresh — the FULL/SLOW tier boundary", strconv.FormatInt(lim.SlowThreshold, 10)},
		{7, "fullCps", strconv.FormatInt(lim.FullCPS, 10)},
		{8, "slowCps", strconv.FormatInt(lim.SlowCPS, 10)},
		{9, "fullRps", strconv.FormatInt(lim.FullRPS, 10)},
		{10, "slowRps", strconv.FormatInt(lim.SlowRPS, 10)},
		{11, "keyRps", strconv.FormatInt(lim.KeyRPS, 10)},
		{12, "nMeth — how many triples follow", strconv.Itoa(n)},
	} {
		require.Greater(t, len(argv), w.luaIndex-1, "ARGV[%d] is missing", w.luaIndex)
		assert.Equal(t, w.value, argv[w.luaIndex-1], "ARGV[%d] — %s", w.luaIndex, w.readAs)
	}

	// The per-method triples, at the offset the script computes. The arithmetic
	// below is a transcription of authorize.lua lines 68-72, deliberately not
	// of authorize.go — the point is to compare the two.
	for i := 1; i <= n; i++ {
		m := in.Methods[i-1]
		base := 12 + (i-1)*3
		require.Greater(t, len(argv), base+2, "the triple for method %d is missing", i)
		assert.Equal(t, fmt.Sprintf("valve:rate:s:m:%s:%s:%d", key, m.Method, sec), argv[base+1-1],
			"ARGV[%d] — the per-(key, method) per-second bucket key, read by GET and INCRBY", base+1)
		assert.Equal(t, strconv.FormatInt(m.Count, 10), argv[base+2-1],
			"ARGV[%d] — mBy, how many times this method appears in the request", base+2)
		assert.Equal(t, strconv.FormatInt(m.Limit, 10), argv[base+3-1],
			"ARGV[%d] — mLimit, 0 meaning the gate is off for this method", base+3)
	}

	assert.Len(t, argv, 12+3*n,
		"ARGV must be exactly the 12-argument prefix plus one triple per method")
}

// The declared method count must match the triples that follow.
//
// This is the failure mode with no error attached. Overstate nMeth and the
// script does tonumber(nil) past the end of ARGV; understate it and the last
// methods are never gated and never counted. Neither shows up as a Redis
// error, and this package cannot see either from the verdict alone.
func TestAuthorizeArgv_TheDeclaredMethodCountMatchesTheTriples(t *testing.T) {
	for _, c := range argvCases() {
		t.Run(c.Name, func(t *testing.T) {
			_, argv := recordAuthorize(t, c.Input)

			require.GreaterOrEqual(t, len(argv), 12, "the fixed prefix is 12 arguments")
			declared, err := strconv.Atoi(argv[11])
			require.NoError(t, err, "ARGV[12] must be a plain integer")

			assert.Equal(t, len(c.Input.Methods), declared, "ARGV[12] does not count the methods supplied")
			assert.Equal(t, 12+3*declared, len(argv),
				"ARGV declares %d methods but carries %d trailing arguments; the script would read past the end",
				declared, len(argv)-12)
		})
	}
}

// The cross-check against the script itself.
//
// Everything above pins today's layout. This derives the layout FROM
// authorize.lua, so a future re-vendored script that reads an eleventh key or
// a thirteenth fixed argument fails here rather than reading a nil in
// production.
func TestAuthorizeArgv_SuppliesEveryIndexTheLuaReadsAndNoOther(t *testing.T) {
	staticIndices := func(array string) []int {
		re := regexp.MustCompile(regexp.QuoteMeta(array) + `\[(\d+)\]`)
		seen := map[int]bool{}
		for _, m := range re.FindAllStringSubmatch(authorizeLua, -1) {
			n, err := strconv.Atoi(m[1])
			require.NoError(t, err)
			seen[n] = true
		}
		out := make([]int, 0, len(seen))
		for n := range seen {
			out = append(out, n)
		}
		sort.Ints(out)
		return out
	}

	// A gap means Go supplies a position nothing reads, which is dead weight
	// on every request and a sign the two sides disagree about the layout.
	contiguousFrom1 := func(t *testing.T, name string, idx []int) int {
		t.Helper()
		require.NotEmpty(t, idx, "found no %s[n] reads in the script", name)
		for i, n := range idx {
			require.Equal(t, i+1, n, "%s indices are not contiguous from 1: %v", name, idx)
		}
		return idx[len(idx)-1]
	}

	keyCount := contiguousFrom1(t, "KEYS", staticIndices("KEYS"))
	argvPrefix := contiguousFrom1(t, "ARGV", staticIndices("ARGV"))

	// The per-method tail. Both loops in the script — the gate loop and the
	// commit loop — compute the same base, and they must: a gate checked at
	// one offset and incremented at another counts the wrong bucket.
	baseRe := regexp.MustCompile(`local base\s*=\s*(\d+) \+ \(i - 1\) \* (\d+)`)
	bases := baseRe.FindAllStringSubmatch(authorizeLua, -1)
	require.Len(t, bases, 2, "expected the gate loop and the commit loop to compute a base each")
	for _, b := range bases[1:] {
		require.Equal(t, bases[0][1], b[1], "the two per-method loops start at different offsets")
		require.Equal(t, bases[0][2], b[2], "the two per-method loops use different strides")
	}
	baseOffset, err := strconv.Atoi(bases[0][1])
	require.NoError(t, err)
	stride, err := strconv.Atoi(bases[0][2])
	require.NoError(t, err)

	assert.Equal(t, argvPrefix, baseOffset,
		"the script's first per-method triple starts at %d, but it reads %d fixed arguments; "+
			"one of the two moved", baseOffset+1, argvPrefix)
	assert.Equal(t, 3, stride, "a per-method entry is a (key, count, limit) triple")

	for _, c := range argvCases() {
		t.Run(c.Name, func(t *testing.T) {
			call, argv := recordAuthorize(t, c.Input)
			assert.Len(t, call.Keys, keyCount,
				"the script reads KEYS[1..%d]; Authorize supplies %d", keyCount, len(call.Keys))
			assert.Len(t, argv, baseOffset+stride*len(c.Input.Methods),
				"the script reads %d fixed arguments plus %d per method",
				baseOffset, stride)
		})
	}
}

// Authorize sends the same bytes every time for the same input. A map iterated
// somewhere in the path would show up here and nowhere else.
func TestAuthorizeArgv_IsDeterministic(t *testing.T) {
	for _, c := range argvCases() {
		t.Run(c.Name, func(t *testing.T) {
			first, firstArgv := recordAuthorize(t, c.Input)
			second, secondArgv := recordAuthorize(t, c.Input)
			assert.Equal(t, first.Keys, second.Keys)
			assert.Equal(t, firstArgv, secondArgv)
		})
	}
}
