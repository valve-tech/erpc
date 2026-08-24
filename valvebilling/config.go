package valvebilling

import (
	"fmt"
	"os"
	"strconv"
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
