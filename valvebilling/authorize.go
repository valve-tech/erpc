package valvebilling

import (
	"context"
	_ "embed"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// authorizeLua is the metering decision, verbatim from the monorepo's
// AUTHORIZE_LUA (packages/utils/src/credits-lua.ts) as of a08e9b9.
//
// It is embedded rather than reimplemented, and it is embedded BYTE for byte
// rather than reformatted. go-redis and ioredis both key EVALSHA on the SHA1
// of the exact body, so an identical copy means this process and the
// TypeScript relay share one cached script inside Redis instead of loading
// two that could differ. Reformatting this file — even adding a trailing
// newline — silently forks that script. The digest test exists to catch it.
//
//go:embed authorize.lua
var authorizeLua string

// AuthorizeScriptSHA1 is the digest the monorepo's copy hashes to. It is
// stated here as a constant so a drifted copy fails a test rather than
// quietly running a second script in production.
const AuthorizeScriptSHA1 = "e261a53c458cbd91147367a7e4bb5c568b599efc" //gitleaks:allow — SHA1 of a public Lua script, not a credential

// authorizeScript wraps the body with go-redis's EVALSHA-then-EVAL fallback,
// which is what ioredis's defineCommand does on the TypeScript side.
var authorizeScript = redis.NewScript(authorizeLua)

// Tier is what the script grants a request that passes every gate. It is not
// a policy this package decides; it comes back from Redis.
type Tier string

const (
	TierFull Tier = "FULL"
	TierSlow Tier = "SLOW"
	TierNone Tier = "NONE"
)

// CodeOK is the one code that means the request may proceed. Every other
// value is a rejection reason, and this package deliberately does not
// enumerate them: the set lives in the Lua script and grows there. Callers
// test OK and log the code.
const CodeOK = "ok"

// Verdict is the script's answer: a code and the tier it judged the account
// at. A rejection still carries a tier, because the tier is decided before
// several of the gates.
type Verdict struct {
	Code string
	Tier Tier
}

// OK reports whether the request may proceed.
func (v Verdict) OK() bool { return v.Code == CodeOK }

// MethodBucket is one per-second per-method counter. These travel in ARGV
// rather than KEYS because their number varies per request while the script
// registers a fixed key count. See authorize.lua's header for why that is
// safe here.
type MethodBucket struct {
	// Method is the JSON-RPC method name. The bucket key is built from it.
	Method string
	// Count is how many times this method appears in the request. A batch of
	// the same method counts fully, so a caller cannot batch around the cap.
	Count int64
	// Limit is the per-second cap for this method. Zero disables the gate.
	Limit int64
}

// Limits are the per-request policy numbers the script compares against.
// Every one of them is supplied by the caller, so this package holds no
// pricing or plan policy of its own.
type Limits struct {
	DayLimit      int64
	CUSecondLimit int64
	CUDayLimit    int64
	SlowThreshold int64
	FullCPS       int64
	SlowCPS       int64
	FullRPS       int64
	SlowRPS       int64
	KeyRPS        int64
}

// AuthorizeInput is one authorization request.
type AuthorizeInput struct {
	// AccountID identifies the paying account. It names the credit keys.
	AccountID string
	// KeyID is the HASHED api key — the output of HashAPIKey, never the key
	// itself. Redis key names leak through SCAN, MONITOR and RDB backups, and
	// a real key was exposed that way on 2026-08-02.
	KeyID string
	// Now is the instant the request is billed at. It is a parameter rather
	// than time.Now() so a test can pin the second and day buckets.
	Now time.Time
	// Cost is the credit cost in wei. It is a big integer because the pricing
	// table's amount_wei is Postgres numeric and exceeds 2^53; it is passed to
	// Redis as a decimal string, exactly as String(bigint) does in TypeScript.
	Cost *big.Int
	// CUCost is the compute-unit cost for the CU-per-second and CU-per-day
	// gates.
	CUCost int64
	// Limits are the policy numbers for this request.
	Limits Limits
	// Methods are the per-method buckets. Empty means the per-method gate is
	// not applied, which is the non-public-tier case.
	Methods []MethodBucket
}

// Authorize runs the metering decision inside Redis and returns its verdict.
//
// It moves no counter that the script does not move. In particular it does
// NOT record spend: that is capture's job, after the upstream answers. The
// split is deliberate and must stay — a failed upstream costs the customer
// nothing, and there is no refund path to lean on if it were folded in.
func Authorize(ctx context.Context, rdb redis.Scripter, in AuthorizeInput) (Verdict, error) {
	if in.Cost == nil {
		return Verdict{}, fmt.Errorf("valvebilling: cost is nil; resolve it before authorizing")
	}
	if in.AccountID == "" || in.KeyID == "" {
		return Verdict{}, fmt.Errorf("valvebilling: accountId and keyId are both required")
	}

	sec := in.Now.Unix()
	day := sec / 86400

	keys := []string{
		perRequestLockKey(in.AccountID),
		fmt.Sprintf("valve:rate:d:%s:%d", in.KeyID, day),
		fmt.Sprintf("valve:rate:cu:s:%s:%d", in.KeyID, sec),
		fmt.Sprintf("valve:rate:cu:d:%s:%d", in.KeyID, day),
		ceilingKey(in.AccountID),
		pendingKey(in.AccountID),
		spendKey(in.AccountID),
		closingKey(in.AccountID),
		cpsBucketKey(in.AccountID),
		fmt.Sprintf("valve:rate:s:%s:%d", in.KeyID, sec),
	}

	args := []interface{}{
		in.Cost.String(),
		strconv.FormatInt(in.CUCost, 10),
		strconv.FormatInt(in.Limits.DayLimit, 10),
		strconv.FormatInt(in.Limits.CUSecondLimit, 10),
		strconv.FormatInt(in.Limits.CUDayLimit, 10),
		strconv.FormatInt(in.Limits.SlowThreshold, 10),
		strconv.FormatInt(in.Limits.FullCPS, 10),
		strconv.FormatInt(in.Limits.SlowCPS, 10),
		strconv.FormatInt(in.Limits.FullRPS, 10),
		strconv.FormatInt(in.Limits.SlowRPS, 10),
		strconv.FormatInt(in.Limits.KeyRPS, 10),
		strconv.FormatInt(int64(len(in.Methods)), 10),
	}
	for _, m := range in.Methods {
		args = append(args,
			fmt.Sprintf("valve:rate:s:m:%s:%s:%d", in.KeyID, m.Method, sec),
			strconv.FormatInt(m.Count, 10),
			strconv.FormatInt(m.Limit, 10),
		)
	}

	raw, err := authorizeScript.Run(ctx, rdb, keys, args...).Result()
	if err != nil {
		// A Redis failure is NOT a rejection. Returning a rejection verdict
		// here would let an unreachable Redis look like an out-of-credit
		// customer, which is the wrong answer to give and the wrong thing to
		// page on. The caller decides whether to fail open or closed.
		return Verdict{}, fmt.Errorf("valvebilling: authorize script failed: %w", err)
	}

	pair, ok := raw.([]interface{})
	if !ok || len(pair) != 2 {
		return Verdict{}, fmt.Errorf("valvebilling: authorize returned %T with %d element(s), want [code, tier]", raw, lenOf(raw))
	}
	code, okCode := pair[0].(string)
	tier, okTier := pair[1].(string)
	if !okCode || !okTier {
		return Verdict{}, fmt.Errorf("valvebilling: authorize returned non-string pair %T/%T", pair[0], pair[1])
	}
	return Verdict{Code: code, Tier: Tier(tier)}, nil
}

func lenOf(v interface{}) int {
	if s, ok := v.([]interface{}); ok {
		return len(s)
	}
	return -1
}
