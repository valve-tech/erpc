package valvebilling

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// EnvEnabled is the flag. It is read from the environment rather than from
// erpc.yaml on purpose.
//
// eRPC decodes its config with KnownFields(true), so an unrecognised top-level
// key is a hard load failure — which means teaching it the word "billing"
// requires a field in common/config.go, an upstream-owned file the fork has
// already edited +495/-215 and one of its worst recurring conflict sites.
// Reading the environment instead keeps this module's diff against upstream
// at exactly zero files. That is what lets reconcile/ws-plus-main and
// archive/harvest-onto-main merge as if this module were not here.
const (
	EnvEnabled  = "VALVE_BILLING_ENABLED"
	EnvRedisURL = "VALVE_BILLING_REDIS_URL"
	EnvPepper   = "VALVE_REDIS_KEY_PEPPER"
)

// Config is everything the module needs to run.
type Config struct {
	// Enabled gates the whole module. False means stock eRPC behaviour.
	Enabled bool

	// RedisURL is a full URL, parsed by go-redis, so it carries the scheme,
	// any password and any TLS choice.
	//
	// It is never defaulted to loopback. Redis has no password today and binds
	// 127.0.0.1, and that is changing; a module that assumed either would
	// start talking to the wrong Redis, or to the right one without
	// authenticating, at exactly the moment the infrastructure moved.
	RedisURL string

	// Pepper is VALVE_REDIS_KEY_PEPPER. It must match the api service's value
	// byte for byte, because the api writes the buckets this module reads.
	Pepper string
}

// LoadConfigFromEnv reads the module's configuration.
//
// When the flag is clear it returns a disabled Config and reads nothing else,
// so a deployment that does not use this module needs none of these variables
// set and cannot be broken by one being wrong.
//
// When the flag is set, every remaining value is REQUIRED. Nothing here falls
// back to a development default. The monorepo has a live example of why:
// VALVE_RATE_IP_SALT falls back to a hardcoded default, so a process missing
// it does not crash — it silently rate-limits a different population and the
// public tier gets double its budget. A missing value that fails at boot is a
// five-minute outage; one that silently addresses the wrong Redis namespace
// makes every account look unmetered, and nothing goes red.
func LoadConfigFromEnv() (Config, error) {
	raw, present := os.LookupEnv(EnvEnabled)
	if !present || raw == "" {
		return Config{Enabled: false}, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		// An unparseable flag is not "off". Somebody meant to turn this on.
		return Config{}, fmt.Errorf("valvebilling: %s=%q is not a boolean: %w", EnvEnabled, raw, err)
	}
	if !enabled {
		return Config{Enabled: false}, nil
	}

	cfg := Config{
		Enabled:  true,
		RedisURL: os.Getenv(EnvRedisURL),
		Pepper:   os.Getenv(EnvPepper),
	}
	if cfg.RedisURL == "" {
		return Config{}, fmt.Errorf(
			"valvebilling: %s is enabled but %s is unset; refusing to guess a Redis endpoint",
			EnvEnabled, EnvRedisURL)
	}
	if len(cfg.Pepper) < MinPepperLength {
		// Deliberately does not echo the value or any part of it.
		return Config{}, fmt.Errorf(
			"valvebilling: %s must be at least %d characters, got %d",
			EnvPepper, MinPepperLength, len(cfg.Pepper))
	}
	return cfg, nil
}

// Note on VALVE_RATE_IP_SALT: this module does not read it, because it hashes
// no IP addresses — every bucket it touches is named by account or by hashed
// API key. The brief flags that variable's silent development fallback as a
// hazard, and it remains one for the relay; requiring it here for a value this
// code never uses would be machinery nothing exercises.

// The tier-limit variables. The names are the TypeScript relay's own
// (packages/relay/src/meter.ts), so one deployment can feed both processes
// from one environment. These five numbers are deployment-wide in that relay
// and deployment-wide here.
const (
	EnvSlowThresholdUSD  = "SLOW_MODE_THRESHOLD_USD"
	EnvFullCreditsPerSec = "FULL_CREDITS_PER_SEC"
	EnvSlowCreditsPerSec = "SLOW_CREDITS_PER_SEC"
	EnvFullRateRPS       = "FULL_RATE_RPS"
	EnvSlowRateRPS       = "SLOW_RATE_RPS"
)

// EnvCreditsPerUSD names the ledger's denomination — how many credits one US
// dollar buys. The monorepo's ledger uses 10^9.
//
// It is REQUIRED and has no default here, which is the opposite of what it
// looks like it should be. The argument for a constant is that both processes
// must agree what a dollar is worth, and a knob lets them disagree. That
// argument fails on the evidence: the peg is ALREADY a hard-coded literal in
// two places that DO disagree. The relay's meter.ts says 10^9 and the web
// calculator's pricing.ts says 10^6, a factor of a thousand, and the landing
// page over-quotes by that much. A third literal in Go would not prevent a
// disagreement; it would add a third place to go stale.
//
// Required configuration turns a mismatch into a deployment error instead of a
// silent divergence. One value, supplied once, read by everything that needs
// it.
const EnvCreditsPerUSD = "VALVE_CREDITS_PER_USD"

// LoadTierLimitsFromEnv reads the five deployment-wide tier numbers.
//
// It fills SlowThreshold, FullCPS, SlowCPS, FullRPS and SlowRPS. It leaves
// DayLimit, CUSecondLimit, CUDayLimit and KeyRPS at zero, because those four
// belong to the API key rather than to the deployment: the monorepo reads them
// from the key record and passes them per request, and meter.ts's authorize()
// takes them as arguments for that reason. A process-wide value would give
// every key on this relay one quota, and that is a policy this package must
// not invent. The caller sets those four when it has a key record to read.
//
// # Every value is required and every value must be positive
//
// Nothing here falls back to a default. meter.ts defaults each of these when
// the variable is missing, and this deliberately does not copy those numbers.
// A default here is the same number written down in two repositories, which is
// how the two drift. valve/billing-module.md records the last time that
// happened: DEFAULT_CU stayed 20 in a comment for the whole life of the change
// that cut it to 6, and two readers copied the stale value.
//
// # Why a zero is refused
//
// Zero does not mean "no limit" to the script. It means "no gate".
// authorize.lua guards the credits-per-second bucket with `cpsLimit > 0` and
// the request-rate gate with `effRps > 0`, so a zero switches the gate off and
// the request passes.
//
// For the credits buckets that is a safety property, not a quota. Balance
// sufficiency is READ and never reserved, so every request in flight sees the
// same balance, and the credits-per-second bucket is the only thing that
// bounds how far an account can overdraw. FULL_CREDITS_PER_SEC=0 or
// SLOW_CREDITS_PER_SEC=0 makes the overdraft unbounded, and nothing goes red.
// meter.ts records that as a latent trap in its own configuration. This
// refuses to reproduce it, and authorize_test.go demonstrates the unbounded
// case against the real script.
//
// The rule covers all five, with no per-variable exception. A zero
// FULL_RATE_RPS or SLOW_RATE_RPS switches the per-second request gate off
// completely, including a per-key limit that would otherwise apply, because
// the script intersects the two and treats the tier cap as the ceiling. A zero
// SLOW_MODE_THRESHOLD_USD puts every account on the FULL tier; that does not
// remove the overdraft bound, but it multiplies it by FULL_CREDITS_PER_SEC /
// SLOW_CREDITS_PER_SEC, which is ten times at the monorepo's numbers.
//
// There is no opt-out flag for "run without the bound". Nothing sets these to
// zero today, so an opt-out would be machinery nothing exercises, and the
// razor rejects a knob whose one setting no real use case wants. A deployment
// that wants no metering clears VALVE_BILLING_ENABLED instead.
//
// An empty string is neither a zero nor an absence. os.Getenv returns "" for
// an unset variable and strconv would read that as 0, so this asks
// os.LookupEnv for presence and reports an empty value on its own.
func LoadTierLimitsFromEnv() (Limits, error) {
	creditsPerUSD, err := requiredPositiveInt(EnvCreditsPerUSD)
	if err != nil {
		return Limits{}, err
	}
	threshold, err := requiredCredits(EnvSlowThresholdUSD, creditsPerUSD)
	if err != nil {
		return Limits{}, err
	}
	fullCPS, err := requiredPositiveInt(EnvFullCreditsPerSec)
	if err != nil {
		return Limits{}, err
	}
	slowCPS, err := requiredPositiveInt(EnvSlowCreditsPerSec)
	if err != nil {
		return Limits{}, err
	}
	fullRPS, err := requiredPositiveInt(EnvFullRateRPS)
	if err != nil {
		return Limits{}, err
	}
	slowRPS, err := requiredPositiveInt(EnvSlowRateRPS)
	if err != nil {
		return Limits{}, err
	}
	return Limits{
		SlowThreshold: threshold,
		FullCPS:       fullCPS,
		SlowCPS:       slowCPS,
		FullRPS:       fullRPS,
		SlowRPS:       slowRPS,
	}, nil
}

// requiredValue returns a variable's value, or says which way it was missing.
//
// The two cases are reported apart on purpose. "Unset" is a deployment that
// never supplied the value; "empty" is usually a template that expanded to
// nothing. Both are refused, and neither becomes a zero.
func requiredValue(name string) (string, error) {
	raw, present := os.LookupEnv(name)
	if !present {
		return "", fmt.Errorf(
			"valvebilling: %s is unset; billing is enabled and this refuses to guess a limit", name)
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf(
			"valvebilling: %s is empty; an empty value is not a zero and not a default", name)
	}
	return trimmed, nil
}

// requiredPositiveInt reads one gate limit. Zero and below are refused: see
// LoadTierLimitsFromEnv on what the script does with them.
func requiredPositiveInt(name string) (int64, error) {
	raw, err := requiredValue(name)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("valvebilling: %s=%q is not a whole number: %w", name, raw, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf(
			"valvebilling: %s=%d must be greater than zero; authorize.lua skips the gate at zero "+
				"or below, and for the credits buckets that leaves the overdraft unbounded", name, v)
	}
	return v, nil
}

// requiredCredits reads a US dollar amount and returns it in credits.
func requiredCredits(name string, creditsPerUSD int64) (int64, error) {
	raw, err := requiredValue(name)
	if err != nil {
		return 0, err
	}
	usd, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("valvebilling: %s=%q is not a number: %w", name, raw, err)
	}
	// ParseFloat accepts "NaN" and "Inf", and every comparison against NaN is
	// false, so a range check alone lets NaN through. TypeScript's parseFloat
	// has the same hole, and meter.ts guards it with isNaN.
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0, fmt.Errorf("valvebilling: %s=%q is not a finite number", name, raw)
	}
	credits := math.Round(usd * float64(creditsPerUSD))
	if credits <= 0 {
		return 0, fmt.Errorf(
			"valvebilling: %s=%q is %.0f credits and must be greater than zero; a zero threshold "+
				"puts every account on the FULL tier and raises the overdraft bound", name, raw, credits)
	}
	if credits >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("valvebilling: %s=%q is too large to compare a balance against", name, raw)
	}
	return int64(credits), nil
}
