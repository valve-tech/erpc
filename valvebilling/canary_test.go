package valvebilling

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two fixtures, both obviously fixtures. They repeat words on purpose: the
// obvious synthetic secret — a run of the hex alphabet, or anything with real
// entropy — scores as a credential to a scanner. A repeated phrase carries the
// same length at near-zero entropy, so the fixture reads as a fixture to a tool
// as well as to a person. Both clear MinPepperLength.
const (
	canaryPepper      = "not-a-real-pepper-not-a-real-pepper-0000"
	canaryOtherPepper = "not-a-real-pepper-not-a-real-pepper-0001"
)

// independentCanaryVerifier recomputes the wire contract by hand.
//
// It deliberately does NOT call HashAPIKey. A test that wrote the value with
// the same function it is testing would pass on any construction, including a
// wrong one — it would confirm that the code agrees with itself. This spells
// out what the monorepo's writer must do: HMAC-SHA256 with the PEPPER AS THE
// KEY and the probe as the message, lowercase hex, first 32 characters.
func independentCanaryVerifier(t *testing.T, pepper, probe string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(pepper))
	_, err := mac.Write([]byte(probe))
	require.NoError(t, err)
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// startCanaryRedis runs a real redis-server on an ephemeral port.
//
// miniredis is enough for a GET, but the whole subject of this file is whether
// this process and another one agree about what is in a REAL keyspace, and the
// package already records one case where miniredis and redis-server disagreed
// about the answer (see limits_test.go on tonumber above int64). The happy path
// is worth running against the thing production runs.
func startCanaryRedis(t *testing.T) *redis.Client {
	t.Helper()

	bin, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skipf("redis-server is not on PATH: %v", err)
	}

	// Ask the kernel for a free port, then hand it to redis-server. The listener
	// is closed first, so there is a small window in which something else could
	// take the port; the readiness wait below turns that into a visible failure
	// rather than a flaky pass.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().(*net.TCPAddr)
	port := addr.Port
	require.NoError(t, ln.Close())

	cmd := exec.Command(bin,
		"--port", fmt.Sprintf("%d", port),
		"--bind", "127.0.0.1",
		// No persistence. The server is thrown away with the test.
		"--save", "",
		"--appendonly", "no",
		"--dir", t.TempDir(),
	)
	require.NoError(t, cmd.Start(), "could not start redis-server")
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	rdb := redis.NewClient(&redis.Options{Addr: fmt.Sprintf("127.0.0.1:%d", port)})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := rdb.Ping(ctx).Err(); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("redis-server on port %d never answered PING: %v", port, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return rdb
}

// The happy path, against a real redis-server: the api service wrote the
// verifier, this process holds the same pepper, and the check agrees.
func TestPepperCanary_MatchingPeppersPass(t *testing.T) {
	ctx := context.Background()
	rdb := startCanaryRedis(t)

	// Stand in for the api service's writer.
	require.NoError(t, rdb.Set(ctx, CanaryKey, independentCanaryVerifier(t, canaryPepper, CanaryProbe), 0).Err())

	res := CheckPepperCanary(ctx, rdb, canaryPepper)
	assert.Equal(t, CanaryMatch, res.Status, "matching peppers must agree: %s", res)
	assert.NoError(t, res.Err)
	assert.NotEmpty(t, res.Detail)
}

// The failure this file exists for. Two peppers that differ by one character
// must NOT pass, because nothing else in the system will notice.
func TestPepperCanary_DifferingPeppersFail(t *testing.T) {
	ctx := context.Background()
	rdb := startCanaryRedis(t)

	// The api service wrote its verifier with the other pepper.
	require.NoError(t, rdb.Set(ctx, CanaryKey, independentCanaryVerifier(t, canaryOtherPepper, CanaryProbe), 0).Err())

	res := CheckPepperCanary(ctx, rdb, canaryPepper)
	require.Equal(t, CanaryMismatch, res.Status, "differing peppers must be caught: %s", res)
	assert.NoError(t, res.Err, "a mismatch is a verdict, not a fault; Err is for CanaryUnknown")
	assert.Contains(t, res.Detail, "the two peppers differ",
		"a well-formed verifier that disagrees means the peppers differ, and the message must say so")
}

// The day-one case, and the one most likely to be got wrong. Before anything
// writes the key, the check has not run. That is neither agreement nor
// disagreement, and it must be reported as neither.
func TestPepperCanary_AbsentKeyIsItsOwnAnswer(t *testing.T) {
	ctx := context.Background()
	rdb := startCanaryRedis(t)
	require.NoError(t, rdb.Del(ctx, CanaryKey).Err())

	res := CheckPepperCanary(ctx, rdb, canaryPepper)

	require.Equal(t, CanaryAbsent, res.Status, "an absent key has its own status: %s", res)
	assert.NotEqual(t, CanaryMismatch, res.Status,
		"an absent key read as a mismatch refuses to boot a correctly configured deployment")
	assert.NotEqual(t, CanaryMatch, res.Status,
		"an absent key read as a match is the silent failure wearing a green light")
	assert.NoError(t, res.Err, "nothing failed; the check simply is not installed yet")

	// The state is survivable only because it is loud. Pin the two things a
	// caller needs from the message: that no comparison happened, and what to
	// do about it.
	assert.Contains(t, res.Detail, "NOT compared")
	assert.Contains(t, res.Detail, "count")
	assert.Contains(t, res.Detail, CanaryKey)
}

// A Redis fault must never be reported as a pepper disagreement. The two send
// an operator to opposite places, and one of those places is "rotate a
// credential that was correct".
func TestPepperCanary_RedisErrorIsNotAMismatch(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("connection reset by peer")

	res := CheckPepperCanary(ctx, failingCanaryReader{err: sentinel}, canaryPepper)

	require.Equal(t, CanaryUnknown, res.Status, "a failed read is not a verdict: %s", res)
	assert.NotEqual(t, CanaryMismatch, res.Status)
	require.Error(t, res.Err)
	assert.True(t, errors.Is(res.Err, sentinel),
		"the cause must survive in Err so a caller can test it with errors.Is, not by matching text")
	assert.Contains(t, res.Detail, "NOT a pepper mismatch")
}

// The same, against a real server that has gone away — the shape an operator
// actually meets, rather than a hand-made error.
func TestPepperCanary_AClosedClientIsAFaultNotAMismatch(t *testing.T) {
	ctx := context.Background()
	rdb := startCanaryRedis(t)
	require.NoError(t, rdb.Set(ctx, CanaryKey, independentCanaryVerifier(t, canaryPepper, CanaryProbe), 0).Err())
	require.NoError(t, rdb.Close())

	res := CheckPepperCanary(ctx, rdb, canaryPepper)
	assert.Equal(t, CanaryUnknown, res.Status, "a dead client is a fault, not a mismatch: %s", res)
	assert.Error(t, res.Err)
}

// A verifier written by a different CONSTRUCTION — here the full 64-character
// digest instead of the truncated one — must fail, and must say which kind of
// disagreement it is. Reported as "the peppers differ", it would get a correct
// pepper rotated and would not fix anything.
func TestPepperCanary_TellsAWrongConstructionFromAWrongPepper(t *testing.T) {
	ctx := context.Background()
	rdb := startCanaryRedis(t)

	mac := hmac.New(sha256.New, []byte(canaryPepper))
	_, err := mac.Write([]byte(CanaryProbe))
	require.NoError(t, err)
	full := hex.EncodeToString(mac.Sum(nil)) // 64 characters, right pepper, wrong shape
	require.Len(t, full, 64)
	require.NoError(t, rdb.Set(ctx, CanaryKey, full, 0).Err())

	res := CheckPepperCanary(ctx, rdb, canaryPepper)
	require.Equal(t, CanaryMismatch, res.Status, "an uninterpretable verifier must not pass: %s", res)
	assert.Contains(t, res.Detail, "different construction")
	assert.NotContains(t, res.Detail, "the two peppers differ")

	// Empty and short values take the same path and must not panic or pass.
	for _, junk := range []string{"", "0", strings.Repeat("z", 32), full[:31]} {
		require.NoError(t, rdb.Set(ctx, CanaryKey, junk, 0).Err())
		got := CheckPepperCanary(ctx, rdb, canaryPepper)
		assert.Equal(t, CanaryMismatch, got.Status, "a junk verifier %q must be a mismatch, not a pass", junk)
	}
}

// Guards a whole family of sloppy comparisons at once. A prefix test, a
// truncated compare, or a length-blind == would let this through.
func TestPepperCanary_APrefixOfTheVerifierIsNotAMatch(t *testing.T) {
	ctx := context.Background()
	want := independentCanaryVerifier(t, canaryPepper, CanaryProbe)

	for _, stored := range []string{want[:16], want + "0", " " + want, want + " "} {
		res := CheckPepperCanary(ctx, staticCanaryReader{value: stored}, canaryPepper)
		assert.Equal(t, CanaryMismatch, res.Status,
			"stored %q is not the verifier and must not pass", stored)
	}

	// And the exact value still passes, so the test above is not passing by
	// rejecting everything.
	res := CheckPepperCanary(ctx, staticCanaryReader{value: want}, canaryPepper)
	require.Equal(t, CanaryMatch, res.Status)
}

// The redaction test. Nothing this check renders may contain the pepper, any
// part of it, or either verifier — a boot log travels much further than a
// Redis keyspace does, and the verifier is the value that turns a pepper guess
// into a confirmed pepper.
//
// A test that only asserts absence passes trivially when the message is empty,
// so every case also asserts that the actionable part IS there.
func TestPepperCanary_LeaksNothingDerivedFromThePepper(t *testing.T) {
	ctx := context.Background()
	ours := independentCanaryVerifier(t, canaryPepper, CanaryProbe)
	theirs := independentCanaryVerifier(t, canaryOtherPepper, CanaryProbe)

	cases := []struct {
		why    string
		reader CanaryReader
		want   CanaryStatus
	}{
		{"match", staticCanaryReader{value: ours}, CanaryMatch},
		{"mismatch", staticCanaryReader{value: theirs}, CanaryMismatch},
		{"absent", staticCanaryReader{err: redis.Nil}, CanaryAbsent},
		{"unknown", failingCanaryReader{err: errors.New("dial tcp: refused")}, CanaryUnknown},
	}

	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			res := CheckPepperCanary(ctx, c.reader, canaryPepper)
			require.Equal(t, c.want, res.Status)

			rendered := res.String()
			require.NotEmpty(t, res.Detail, "an empty message would pass every absence check below for free")
			require.Contains(t, rendered, res.Detail)

			// The actionable part. Every message names the variable to fix or
			// the key to look at, so the assertions below are proving silence
			// about the secret rather than silence about everything.
			assert.True(t,
				strings.Contains(rendered, EnvPepper) || strings.Contains(rendered, CanaryKey),
				"a message that names neither %s nor %s tells the operator nothing: %q",
				EnvPepper, CanaryKey, rendered)

			// No window of the pepper appears. Eight characters is short enough
			// that an accidental echo of any part is caught, and long enough
			// that the fixture's own words do not collide with English.
			for i := 0; i+8 <= len(canaryPepper); i++ {
				window := canaryPepper[i : i+8]
				assert.NotContains(t, rendered, window,
					"the rendered result echoes %d characters of the pepper", len(window))
			}

			// Nor does either verifier, nor any 16-character window of one.
			for _, v := range []string{ours, theirs} {
				for i := 0; i+16 <= len(v); i++ {
					assert.NotContains(t, rendered, v[i:i+16],
						"the rendered result echoes a verifier; it must stay in Redis, not reach a log")
				}
			}

			// And no length that narrows the pepper. The only number in a
			// message is the length of the STORED value, which belongs to
			// Redis and is public.
			assert.NotContains(t, rendered, fmt.Sprintf("%d", len(canaryPepper)),
				"the rendered result states the pepper's length")
		})
	}
}

// The check must never write. If it wrote the verifier when it found none, the
// first process to boot would define what the correct pepper is — a
// wrong-peppered eRPC would publish its own verifier, agree with itself, and
// report the correct api service as the mismatch.
func TestPepperCanary_WritesNothing(t *testing.T) {
	ctx := context.Background()
	rdb := startCanaryRedis(t)

	require.NoError(t, rdb.FlushAll(ctx).Err())
	before, err := rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	require.Zero(t, before)

	res := CheckPepperCanary(ctx, rdb, canaryPepper)
	require.Equal(t, CanaryAbsent, res.Status)

	after, err := rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	assert.Zero(t, after, "the check created a key; it must only ever GET")
	assert.Equal(t, int64(0), rdb.Exists(ctx, CanaryKey).Val(),
		"the check wrote its own verifier, which would make it agree with itself forever")

	// It does not overwrite an existing one either.
	stored := independentCanaryVerifier(t, canaryOtherPepper, CanaryProbe)
	require.NoError(t, rdb.Set(ctx, CanaryKey, stored, 0).Err())
	require.Equal(t, CanaryMismatch, CheckPepperCanary(ctx, rdb, canaryPepper).Status)
	assert.Equal(t, stored, rdb.Get(ctx, CanaryKey).Val(),
		"the check replaced a verifier it disagreed with")
}

// The cross-repository wire contract. These three facts are what the monorepo's
// writer must match, and a change to any of them without a change on the other
// side is silent — the same class of failure as APIKeyHashLength.
func TestPepperCanary_WireContractIsPinned(t *testing.T) {
	assert.Equal(t, "valve:pepper:canary:v1", CanaryKey,
		"the key name is shared with the monorepo; changing it here alone means nothing is compared")
	assert.Equal(t, "valve:pepper:canary:probe:v1", CanaryProbe,
		"the probe message is shared with the monorepo; changing it here alone means every check mismatches")

	// The verifier is the output of the function that names real buckets, not
	// a parallel construction that could agree while the bucket names differ.
	fromHash, err := HashAPIKey(canaryPepper, CanaryProbe)
	require.NoError(t, err)
	assert.Equal(t, independentCanaryVerifier(t, canaryPepper, CanaryProbe), fromHash)
	assert.Len(t, fromHash, APIKeyHashLength)

	// The probe must not be shaped like an api key. Publishing HMAC(pepper,
	// probe) publishes one bucket name, so it has to be a message no customer
	// key can equal.
	assert.False(t, strings.HasPrefix(CanaryProbe, "vk_"),
		"the probe looks like an api key; pick a message no customer key can be")
}

// A pepper this process cannot use is a check that did not run, not a verdict
// about the api service.
func TestPepperCanary_AnUnusablePepperCannotRun(t *testing.T) {
	ctx := context.Background()
	want := independentCanaryVerifier(t, canaryPepper, CanaryProbe)

	for _, short := range []string{"", "short", strings.Repeat("x", MinPepperLength-1)} {
		res := CheckPepperCanary(ctx, staticCanaryReader{value: want}, short)
		assert.Equal(t, CanaryUnknown, res.Status, "a %d-character pepper cannot verify anything", len(short))
		assert.Error(t, res.Err)
	}

	// A nil reader is the same shape of answer, and must not panic.
	res := CheckPepperCanary(ctx, nil, canaryPepper)
	assert.Equal(t, CanaryUnknown, res.Status)
	assert.Error(t, res.Err)
}

// A disabled module has verified nothing. Answering "match" there would be the
// silent green light this whole file exists to remove.
func TestPepperCanary_DisabledModuleReportsUnknown(t *testing.T) {
	var m *Module
	res := m.CheckPepperCanary(context.Background())
	assert.Equal(t, CanaryUnknown, res.Status)
	assert.Error(t, res.Err)
	assert.NotEqual(t, CanaryMatch, res.Status)
}

// End to end through the module, against a real redis-server: the way a host
// actually calls this at boot.
func TestPepperCanary_ThroughTheModule(t *testing.T) {
	ctx := context.Background()
	rdb := startCanaryRedis(t)
	require.NoError(t, rdb.Set(ctx, CanaryKey, independentCanaryVerifier(t, canaryPepper, CanaryProbe), 0).Err())

	m, err := New(ctx, Config{
		Enabled:  true,
		RedisURL: "redis://" + rdb.Options().Addr,
		Pepper:   canaryPepper,
	}, NewPriceTable(map[string]int64{}, 6))
	require.NoError(t, err)
	require.NotNil(t, m)
	t.Cleanup(func() { _ = m.Close() })

	assert.Equal(t, CanaryMatch, m.CheckPepperCanary(ctx).Status)

	// And the module reports the mismatch its own pepper produces.
	other, err := New(ctx, Config{
		Enabled:  true,
		RedisURL: "redis://" + rdb.Options().Addr,
		Pepper:   canaryOtherPepper,
	}, NewPriceTable(map[string]int64{}, 6))
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	assert.Equal(t, CanaryMismatch, other.CheckPepperCanary(ctx).Status)
}

// staticCanaryReader answers every GET with one value, or one error. It exists
// so the message assertions do not need a server.
type staticCanaryReader struct {
	value string
	err   error
}

func (s staticCanaryReader) Get(ctx context.Context, key string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx, "get", key)
	if s.err != nil {
		cmd.SetErr(s.err)
		return cmd
	}
	cmd.SetVal(s.value)
	return cmd
}

// failingCanaryReader is a Redis that answers nothing.
type failingCanaryReader struct{ err error }

func (f failingCanaryReader) Get(ctx context.Context, key string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx, "get", key)
	cmd.SetErr(f.err)
	return cmd
}
