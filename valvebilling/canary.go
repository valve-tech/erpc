package valvebilling

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// The pepper canary: a boot-time check that this process and the api service
// hold the SAME VALVE_REDIS_KEY_PEPPER.
//
// # The failure it catches
//
// HashAPIKey names a Redis bucket with HMAC-SHA256(pepper, apiKey), truncated
// to 32 hex characters. The pepper is the HMAC key, so a pepper that differs
// by one byte moves every one of those bucket names to an unrelated place in
// the keyspace. Nothing errors. Redis answers every read, and every read
// misses.
//
// What that costs today, measured against this package rather than assumed:
// the CREDIT buckets are named by accountId (see keys.go), so balances and
// spend survive a wrong pepper. The RATE buckets do not. Authorize builds
// valve:rate:d, valve:rate:s, valve:rate:cu:s, valve:rate:cu:d and the
// per-method buckets from the hashed key, so a wrong pepper gives this process
// a private set of rate counters. The relay and this process then enforce the
// same per-key limits SEPARATELY, each on its own half of the traffic, and a
// key gets roughly twice its allowance with nothing going red. That is the
// exact shape config.go cites for VALVE_RATE_IP_SALT: a value that silently
// falls back rate-limits a different population.
//
// The blast radius grows the moment a key record lands in Redis addressed by
// the hashed key. valve/periodic-enforcement.md is planning exactly that, and
// there a wrong pepper reads every account as having no record — so every
// account is refused, or every account is unmetered, depending on how the
// caller treats a missing record. Neither goes red.
//
// # Why a dedicated verifier key, and what it costs
//
// The weaker-looking option is to probe a key the api service ALREADY writes,
// which would need no change in the monorepo and could run today. This module
// cannot do it. Every pepper-derived key it knows is named
// HMAC(pepper, apiKey) for some customer's api key, and at boot this process
// holds no api key, so it cannot reproduce a single one of those names. A
// probe needs a message BOTH sides know, and no such message exists in the
// keyspace today. Presence alone proves nothing either: this module writes the
// rate buckets too, so finding one says only that somebody with some pepper
// was here.
//
// So the dedicated verifier is forced, and its cost is real and must be
// scheduled: the check is worth nothing until something on the other side
// writes the key. That is a monorepo change and a deployment-ordering hazard —
// a check that cannot run until the other side ships is worth less than one
// that runs today. It is bought with the CanaryAbsent state below, which keeps
// the gap visible instead of pretending the check ran.
//
// The writer does not have to be the api service. Anything that holds the
// pepper can write the key once — a deploy step, a migration, an operator.
// This module never writes it; see CheckPepperCanary for why.
//
// # Why storing the verifier is safe, and where it is not
//
// The stored value is HMAC-SHA256(pepper, a fixed PUBLIC string), truncated to
// 128 bits. HMAC-SHA256 is a pseudorandom function: there is no known way to
// invert it or to extract its key, so the only attack on a stolen verifier is
// to guess the pepper and recompute. MinPepperLength puts a floor of 32
// characters under that guess, which is at least 128 bits even for a pepper
// drawn from the hex alphabet alone. Truncating the tag to 128 bits does not
// help the attacker: a shorter tag admits MORE false candidates, so it makes a
// brute-force search less conclusive, never more.
//
// Be honest about the one thing it does change. Rate-bucket names are HMACs
// over a message the attacker does not have, so they are poor guessing oracles.
// The verifier is an HMAC over a message the attacker DOES have. A pepper that
// is a memorable phrase carries far less entropy than its length suggests and
// becomes offline-guessable. The pepper must be 32 or more RANDOM characters.
// That is why MinPepperLength is a security floor and not a tuning knob.
//
// Neither the stored verifier nor the computed one is ever logged. They are
// public in Redis, but a log travels further than a keyspace does, and the
// verifier is the one value that turns a pepper guess into a confirmed pepper.

// CanaryKey is where the verifier lives. CanaryProbe is the message it is
// computed over.
//
// Both are a cross-repository wire contract, exactly like APIKeyHashLength.
// They are constants and NOT configurable: a key name an operator can change
// on one side is one more way for the two sides to address different things
// silently, which is the failure this whole file exists to catch.
//
// The v1 suffix is on both. If the construction ever changes, the new writer
// uses a new key, so an old reader sees CanaryAbsent — visible and survivable —
// rather than CanaryMismatch, which would report a pepper disagreement that
// does not exist and send an operator to rotate a correct credential.
//
// The probe is deliberately not shaped like an api key. Real keys begin "vk_".
// Publishing HMAC(pepper, probe) publishes one bucket name, so the message must
// be one no customer key can ever equal.
//
// The writing side computes exactly what HashAPIKey computes, which in the
// monorepo is hashApiKey. In TypeScript:
//
//	await redis.set("valve:pepper:canary:v1",
//	    hashApiKey("valve:pepper:canary:probe:v1", process.env.VALVE_REDIS_KEY_PEPPER));
//
// Read the pepper from the environment inside the writing process. Do NOT
// write the key with a shell one-liner that passes the pepper as an argument —
// `openssl dgst -hmac "$PEPPER"` puts the credential in the process table where
// any user on the box can read it.
const (
	CanaryKey   = "valve:pepper:canary:v1"
	CanaryProbe = "valve:pepper:canary:probe:v1"
)

// CanaryStatus is the answer, and it has four values rather than two.
//
// A boolean cannot carry this result. "Did not match" and "was never written"
// are different facts with opposite correct responses, and collapsing them
// either refuses to boot against a correctly configured deployment or hides the
// unverified state that this check exists to reveal.
type CanaryStatus string

const (
	// CanaryMatch means the two peppers agree. This is the only value that
	// permits silence.
	CanaryMatch CanaryStatus = "match"

	// CanaryMismatch means a verifier is stored and this process does not
	// compute it. The two sides address different key namespaces. Nothing will
	// error at runtime, so a caller that boots anyway boots into the silent
	// failure. Refuse to start.
	CanaryMismatch CanaryStatus = "mismatch"

	// CanaryAbsent means no verifier is stored, so the peppers were NOT
	// compared. This is the day-one state, before anything writes the key.
	//
	// It is the third answer, and it is neither of the two obvious ones.
	// Treating it as a mismatch refuses to boot a deployment that is correctly
	// configured, on no evidence at all — the check is simply not installed
	// yet. Treating it as agreement is the silent failure this file was built
	// to catch, wearing a green light. So it is reported as its own state: not
	// fatal, never silent, and it must be LOGGED AND COUNTED.
	//
	// The counter is what makes it survivable, and the two cases it separates
	// have different signatures. A rollout in progress is a counter that goes
	// to zero when the writer ships. A forgotten check is a counter that never
	// does. valve/billing-module.md reaches the same answer for the cold-start
	// range price: bill the flat price and count it, because the count is what
	// keeps a bypass visible.
	CanaryAbsent CanaryStatus = "absent"

	// CanaryUnknown means the check could not run: Redis failed the read, or
	// this process's own pepper is unusable. It is NOT a mismatch, and the two
	// must never be conflated — an unreachable Redis reported as a pepper
	// disagreement sends an operator to rotate a credential that is correct.
	// Err carries the cause.
	CanaryUnknown CanaryStatus = "unknown"
)

// CanaryResult is the check's whole answer.
//
// The caller switches on Status. All four arms need a decision, and the
// compiler will not force one, so the switch is written out here:
//
//	switch res := m.CheckPepperCanary(ctx); res.Status {
//	case valvebilling.CanaryMatch:
//	    // proceed
//	case valvebilling.CanaryMismatch:
//	    return fmt.Errorf("refusing to start: %s", res)   // fatal
//	case valvebilling.CanaryAbsent, valvebilling.CanaryUnknown:
//	    log.Warn().Msg(res.String())                      // and increment a counter
//	}
//
// This package does not decide which of those is fatal, in the same way it owns
// no refresh timer: the host decides, and it gets the fact it needs to decide
// with.
type CanaryResult struct {
	// Status is the outcome. It is always set.
	Status CanaryStatus

	// Detail says what happened and what to do about it, in one sentence a
	// person reading a boot log can act on. It never contains the pepper, any
	// part of it, its length, or either verifier.
	Detail string

	// Err is the underlying cause when Status is CanaryUnknown, and nil
	// otherwise. It is kept separate from Detail so a caller can test it with
	// errors.Is rather than by matching text.
	Err error
}

// String renders the result for a log line. It is safe to log verbatim.
func (c CanaryResult) String() string {
	if c.Err != nil {
		return fmt.Sprintf("valvebilling: pepper canary %s: %s: %v", c.Status, c.Detail, c.Err)
	}
	return fmt.Sprintf("valvebilling: pepper canary %s: %s", c.Status, c.Detail)
}

// CanaryReader is the single Redis operation this check needs.
//
// It is one method rather than redis.Cmdable because that is the whole point:
// a type that can only GET cannot be made to write the verifier by a later
// edit, and the check's value depends on this side never writing it.
type CanaryReader interface {
	Get(ctx context.Context, key string) *redis.StringCmd
}

// CheckPepperCanary compares this process's pepper against the stored verifier.
//
// It issues exactly one command, GET, and it NEVER writes. That is the load-
// bearing property of the design, not an implementation detail. A module that
// wrote the verifier when it found none would let whichever process booted
// first define what the correct pepper is: a wrong-peppered eRPC would publish
// its own wrong verifier, agree with itself forever, and then report the
// CORRECT api service as the mismatch. The check would confirm the error it
// exists to catch.
//
// It also writes nothing derived from the pepper anywhere else. Redis is a
// shared store, the pepper is a credential, and the verifier that does live
// there is put there by whoever owns the credential — not by a reader.
func CheckPepperCanary(ctx context.Context, r CanaryReader, pepper string) CanaryResult {
	// Computed with HashAPIKey itself, not with a second HMAC written out
	// here. The check must exercise the FUNCTION THAT NAMES BUCKETS, so that a
	// change to the construction or the truncation shows up as a mismatch. A
	// parallel implementation could agree with the api service while the real
	// bucket names disagreed, which is the failure wearing a green light.
	want, err := HashAPIKey(pepper, CanaryProbe)
	if err != nil {
		// Unreachable through Module: LoadConfigFromEnv refuses a short pepper
		// at boot. Reported rather than ignored because a caller can build a
		// Config by hand, and a check that cannot run must say so.
		return CanaryResult{
			Status: CanaryUnknown,
			Detail: "this process's own " + EnvPepper + " cannot be used, so the peppers were not compared",
			Err:    err,
		}
	}

	if r == nil {
		return CanaryResult{
			Status: CanaryUnknown,
			Detail: "no Redis client was supplied, so the peppers were not compared",
			Err:    errors.New("valvebilling: nil canary reader"),
		}
	}

	stored, err := r.Get(ctx, CanaryKey).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return CanaryResult{
			Status: CanaryAbsent,
			Detail: "no verifier is stored at " + CanaryKey + ", so " + EnvPepper +
				" was NOT compared against the api service; this is expected until the api writes the key, " +
				"and until it does a pepper mismatch stays undetectable — log and count this, do not read it as agreement",
		}
	case err != nil:
		return CanaryResult{
			Status: CanaryUnknown,
			Detail: "Redis did not answer the read of " + CanaryKey +
				", so the peppers were not compared; this is a Redis fault, NOT a pepper mismatch",
			Err: err,
		}
	}

	// hmac.Equal, not ==. The comparison runs in constant time for equal
	// lengths and does not short-circuit on the first differing byte, so it
	// leaks no position through timing. That matters here because the verifier
	// is the value that turns a pepper guess into a confirmed pepper, and a
	// byte-at-a-time oracle would let an attacker with no Redis access build it
	// from the outside. It is also safe on unequal lengths, where == would
	// simply be a different, sloppier answer.
	if hmac.Equal([]byte(want), []byte(stored)) {
		return CanaryResult{
			Status: CanaryMatch,
			Detail: "this process's " + EnvPepper + " matches the verifier stored at " + CanaryKey,
		}
	}

	// The detail distinguishes "a different pepper wrote this" from "a
	// different CONSTRUCTION wrote this". Both must fail, but they send the
	// operator to different places, and a wrong-shaped value reported as a
	// pepper disagreement gets a correct credential rotated. Only the shape of
	// the STORED value is described, never its content and never ours.
	shape := "the stored value has the expected shape, so the two peppers differ"
	if !looksLikeCanaryVerifier(stored) {
		shape = fmt.Sprintf(
			"the stored value is %d characters and is not %d lowercase hex characters, so it may have been "+
				"written by a different construction rather than by a different pepper",
			len(stored), APIKeyHashLength)
	}
	return CanaryResult{
		Status: CanaryMismatch,
		Detail: "this process's " + EnvPepper + " does not match the verifier stored at " + CanaryKey +
			", so this process and the api service address different Redis key namespaces and every " +
			"per-key bucket this process touches is one nobody else writes; " + shape +
			" — set " + EnvPepper + " to the value the api service uses",
	}
}

// looksLikeCanaryVerifier reports whether a stored value has the shape
// HashAPIKey emits: exactly APIKeyHashLength lowercase hex characters.
//
// It shapes the message only. It never changes the verdict — an unrecognised
// value is still a mismatch, because a verifier this module cannot interpret is
// exactly as much of a red flag as one that disagrees.
func looksLikeCanaryVerifier(s string) bool {
	if len(s) != APIKeyHashLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// CheckPepperCanary runs the check against the module's own Redis and pepper.
//
// Call it once at boot, after New. It is nil-safe: a disabled module reports
// CanaryUnknown rather than CanaryMatch, because a module that holds no pepper
// has verified nothing, and answering "match" would be the silent green light
// this file exists to remove.
func (m *Module) CheckPepperCanary(ctx context.Context) CanaryResult {
	if m == nil {
		return CanaryResult{
			Status: CanaryUnknown,
			Detail: "billing is disabled, so there is no pepper to check",
			Err:    errDisabled("CheckPepperCanary"),
		}
	}
	return CheckPepperCanary(ctx, m.rdb, m.cfg.Pepper)
}
