package auth

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memoryStrategyConfig builds a database-strategy config backed by the
// in-process memory connector, with the positive and negative caches enabled.
func memoryStrategyConfig() *common.DatabaseStrategyConfig {
	ttl := 5 * time.Minute
	maxSize := int64(1 << 20)
	maxCost := int64(1 << 20)
	numCounters := int64(1000)
	return &common.DatabaseStrategyConfig{
		Connector: &common.ConnectorConfig{
			Id:     "auth-memory",
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "1MB"},
		},
		Cache: &common.DatabaseStrategyCacheConfig{
			TTL:         &ttl,
			MaxSize:     &maxSize,
			MaxCost:     &maxCost,
			NumCounters: &numCounters,
		},
	}
}

func newMemoryStrategy(t *testing.T, cfg *common.DatabaseStrategyConfig) *DatabaseStrategy {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	logger := zerolog.New(zerolog.NewTestWriter(t))
	require.NoError(t, cfg.Connector.Memory.SetDefaults())

	s, err := NewDatabaseStrategy(ctx, &logger, cfg)
	require.NoError(t, err)
	require.NotNil(t, s)
	t.Cleanup(s.Close)
	return s
}

// storeRecord writes a record through the connector and waits until it is
// readable. The memory connector is backed by ristretto, whose Set is
// buffered — without the wait the very next Get races the write buffer.
func storeRecord(t *testing.T, conn data.Connector, apiKey, rangeKey string, value []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, conn.Set(ctx, apiKey, rangeKey, value, nil))
	require.Eventually(t, func() bool {
		_, err := conn.Get(ctx, data.ConnectorMainIndex, apiKey, rangeKey, nil)
		return err == nil
	}, 5*time.Second, 5*time.Millisecond, "the record must become readable before the test proceeds")
}

func secretPayload(apiKey string) *AuthPayload {
	return &AuthPayload{
		Type:   common.AuthTypeSecret,
		Secret: &SecretPayload{Value: apiKey},
	}
}

func userRecord(t *testing.T, userId, budget string, enabled *bool) []byte {
	t.Helper()
	rec := map[string]interface{}{"userId": userId}
	if budget != "" {
		rec["rateLimitBudget"] = budget
	}
	if enabled != nil {
		rec["enabled"] = *enabled
	}
	b, err := json.Marshal(rec)
	require.NoError(t, err)
	return b
}

// TestNewDatabaseStrategy_ConfigErrors pins the constructor's refusals. Each
// one must name what is missing so an operator can fix the YAML.
func TestNewDatabaseStrategy_ConfigErrors(t *testing.T) {
	t.Parallel()
	logger := zerolog.Nop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("nil config", func(t *testing.T) {
		s, err := NewDatabaseStrategy(ctx, &logger, nil)
		require.Error(t, err)
		assert.Nil(t, s)
		assert.Contains(t, err.Error(), "config is nil")
	})

	t.Run("nil connector config", func(t *testing.T) {
		s, err := NewDatabaseStrategy(ctx, &logger, &common.DatabaseStrategyConfig{})
		require.Error(t, err)
		assert.Nil(t, s)
		assert.Contains(t, err.Error(), "connector config is nil")
	})

	t.Run("unknown driver", func(t *testing.T) {
		s, err := NewDatabaseStrategy(ctx, &logger, &common.DatabaseStrategyConfig{
			Connector: &common.ConnectorConfig{Id: "bad", Driver: "no-such-driver"},
		})
		require.Error(t, err)
		assert.Nil(t, s)
		assert.Contains(t, err.Error(), "failed to create database connector")
	})
}

// TestNewDatabaseStrategy_BuildsConnectorAndCaches pins that the constructor
// wires a live connector and both caches, and that Supports accepts exactly
// the two payload types this strategy handles.
func TestNewDatabaseStrategy_BuildsConnectorAndCaches(t *testing.T) {
	t.Parallel()
	s := newMemoryStrategy(t, memoryStrategyConfig())

	require.NotNil(t, s.GetConnector(), "the strategy must expose its connector for admin operations")
	assert.Equal(t, "auth-memory", s.GetConnector().Id())
	assert.NotNil(t, s.cache, "the positive cache must be built when cache config is present")
	assert.NotNil(t, s.negCache, "the negative cache must be built alongside the positive one")
	assert.Equal(t, 5*time.Second, s.negTTL)

	assert.True(t, s.Supports(&AuthPayload{Type: common.AuthTypeSecret}))
	assert.True(t, s.Supports(&AuthPayload{Type: common.AuthTypeDatabase}))
	assert.False(t, s.Supports(&AuthPayload{Type: common.AuthTypeJwt}))
	assert.False(t, s.Supports(&AuthPayload{Type: common.AuthTypeNetwork}))
}

// TestNewDatabaseStrategy_NoCacheConfig pins that omitting the cache block
// leaves both caches nil — every request then reaches the connector.
func TestNewDatabaseStrategy_NoCacheConfig(t *testing.T) {
	t.Parallel()
	cfg := memoryStrategyConfig()
	cfg.Cache = nil
	s := newMemoryStrategy(t, cfg)

	assert.Nil(t, s.cache)
	assert.Nil(t, s.negCache)
	require.NotPanics(t, func() {
		s.InvalidateCache("anything")
		s.ClearCache()
	}, "cache management must be safe when no cache is configured")
}

// TestDatabaseStrategy_CacheManagement pins InvalidateCache and ClearCache
// against a live cache. Both assert the entry is PRESENT before and ABSENT
// after — an assertion on absence alone would pass on an empty cache.
func TestDatabaseStrategy_CacheManagement(t *testing.T) {
	t.Parallel()
	s := newMemoryStrategy(t, memoryStrategyConfig())

	seed := func(apiKey, userId string) {
		s.cache.SetWithTTL(apiKey, &common.User{Id: userId}, 1, time.Minute)
	}
	present := func(apiKey, wantUserId string) bool {
		u, found := s.cache.Get(apiKey)
		return found && u != nil && u.Id == wantUserId
	}

	t.Run("InvalidateCache removes one key and leaves the rest", func(t *testing.T) {
		seed("key-a", "user-a")
		seed("key-b", "user-b")
		require.Eventually(t, func() bool { return present("key-a", "user-a") && present("key-b", "user-b") },
			5*time.Second, 5*time.Millisecond, "both keys must be cached before invalidation")

		s.InvalidateCache("key-a")

		require.Eventually(t, func() bool {
			_, found := s.cache.Get("key-a")
			return !found
		}, 5*time.Second, 5*time.Millisecond, "the invalidated key must be gone")
		assert.True(t, present("key-b", "user-b"), "invalidation must not touch other keys")
	})

	t.Run("ClearCache removes everything", func(t *testing.T) {
		seed("key-c", "user-c")
		seed("key-d", "user-d")
		require.Eventually(t, func() bool { return present("key-c", "user-c") && present("key-d", "user-d") },
			5*time.Second, 5*time.Millisecond, "both keys must be cached before clearing")

		s.ClearCache()

		_, foundC := s.cache.Get("key-c")
		_, foundD := s.cache.Get("key-d")
		assert.False(t, foundC)
		assert.False(t, foundD)
	})
}

// TestDatabaseStrategy_CacheServesAuthenticatedUser pins the positive-cache
// hit path end to end: the cached user is returned with its identity and rate
// limit budget intact, and invalidating it forces a fresh connector read.
func TestDatabaseStrategy_CacheServesAuthenticatedUser(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s := newMemoryStrategy(t, memoryStrategyConfig())
	s.cache.SetWithTTL("cached-key", &common.User{Id: "cached-user", RateLimitBudget: "gold"}, 1, time.Minute)
	require.Eventually(t, func() bool {
		_, found := s.cache.Get("cached-key")
		return found
	}, 5*time.Second, 5*time.Millisecond)

	user, err := s.Authenticate(ctx, nil, secretPayload("cached-key"))
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, "cached-user", user.Id, "the cached identity must be returned")
	assert.Equal(t, "gold", user.RateLimitBudget, "the cached budget must survive the cache round trip")

	// After invalidation the connector is consulted, and it has no record.
	s.InvalidateCache("cached-key")
	require.Eventually(t, func() bool {
		_, found := s.cache.Get("cached-key")
		return !found
	}, 5*time.Second, 5*time.Millisecond)

	_, err = s.Authenticate(ctx, nil, secretPayload("cached-key"))
	require.Error(t, err, "an invalidated key must fall through to the connector")
	assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))
}

// TestDatabaseStrategy_Close pins that Close releases both caches. After
// Close, a previously present entry is no longer served.
func TestDatabaseStrategy_Close(t *testing.T) {
	t.Parallel()
	cfg := memoryStrategyConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := zerolog.New(zerolog.NewTestWriter(t))
	require.NoError(t, cfg.Connector.Memory.SetDefaults())

	s, err := NewDatabaseStrategy(ctx, &logger, cfg)
	require.NoError(t, err)

	s.cache.SetWithTTL("close-key", &common.User{Id: "close-user"}, 1, time.Minute)
	s.negCache.SetWithTTL("neg-key", struct{}{}, 1, time.Minute)
	require.Eventually(t, func() bool {
		_, a := s.cache.Get("close-key")
		_, b := s.negCache.Get("neg-key")
		return a && b
	}, 5*time.Second, 5*time.Millisecond, "both caches must hold their entry before Close")

	s.Close()

	_, found := s.cache.Get("close-key")
	assert.False(t, found, "the positive cache must not serve entries after Close")
	_, negFound := s.negCache.Get("neg-key")
	assert.False(t, negFound, "the negative cache must not serve entries after Close")

	// Close is called again by other lifecycle paths; it must stay safe.
	require.NotPanics(t, func() { s.Close() })
}

// TestDatabaseStrategy_NegativeCacheShortCircuitsUnknownKey pins that an
// unknown key is remembered as unknown, so a key-guessing flood does not turn
// into one database read per attempt.
func TestDatabaseStrategy_NegativeCacheShortCircuitsUnknownKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s := newMemoryStrategy(t, memoryStrategyConfig())

	_, err := s.Authenticate(ctx, nil, secretPayload("never-issued"))
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))

	require.Eventually(t, func() bool {
		_, found := s.negCache.Get("never-issued")
		return found
	}, 5*time.Second, 5*time.Millisecond, "an unknown key must be recorded in the negative cache")

	// The short-circuit still refuses the key.
	_, err = s.Authenticate(ctx, nil, secretPayload("never-issued"))
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))
}

// TestDatabaseStrategy_MissingAndEmptySecret pins the two payload refusals
// that happen before any connector work.
func TestDatabaseStrategy_MissingAndEmptySecret(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s := newMemoryStrategy(t, memoryStrategyConfig())

	_, err := s.Authenticate(ctx, nil, &AuthPayload{Type: common.AuthTypeSecret})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no secret provided")

	_, err = s.Authenticate(ctx, nil, secretPayload(""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty API key")
}

// TestDatabaseStrategy_AuthenticateAgainstMemoryConnector_WildcardRangeKey
// PINS A LIVE DEFECT. It asserts today's broken behaviour, not the desired
// behaviour.
//
// Defect: auth/strategy_database.go:179 reads the API-key record with the
// range key set to the literal string "*", and erpc/admin.go:334 and :420 do
// the same for updates and revocation. Only the PostgreSQL connector treats
// "*" as a wildcard (data/postgresql.go:1035 getWithWildcard rewrites "*" to
// a SQL LIKE "%"). The memory connector (data/memory.go:169) and the Redis
// connector (data/redis.go:426) build the literal key "<apiKey>:*" instead.
//
// What an operator observes: on a memory- or Redis-backed auth store an API
// key created through the admin API can never be authenticated with, updated,
// or revoked. Every request with that key returns "invalid API key", and
// admin revoke returns "failed to get current API key data".
//
// This test locks the behaviour in place so the eventual fix has to change it
// deliberately. Do not "fix" the test — fix the connector contract.
func TestDatabaseStrategy_AuthenticateAgainstMemoryConnector_WildcardRangeKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := memoryStrategyConfig()
	cfg.Cache = nil // keep every call on the connector path
	s := newMemoryStrategy(t, cfg)
	conn := s.GetConnector()

	// Write the record exactly as erpc/admin.go:175 writes it:
	// Set(ctx, apiKey, userId, payload, nil) — the range key is the user id.
	storeRecord(t, conn, "live-key", "user-42", userRecord(t, "user-42", "premium", nil))

	// The record IS retrievable when the real range key is used. This proves
	// the write landed, so the failure below is the wildcard, not a bad setup.
	stored, err := conn.Get(ctx, data.ConnectorMainIndex, "live-key", "user-42", nil)
	require.NoError(t, err, "the record must be readable by its concrete range key")
	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(stored, &decoded))
	require.Equal(t, "user-42", decoded["userId"])
	require.Equal(t, "premium", decoded["rateLimitBudget"])

	// DEFECT: the strategy reads with rangeKey "*", which the memory connector
	// treats as a literal, so the lookup misses.
	_, wildcardErr := conn.Get(ctx, data.ConnectorMainIndex, "live-key", "*", nil)
	require.Error(t, wildcardErr, "DEFECT PINNED: the memory connector does not expand the '*' range key")
	assert.True(t, common.HasErrorCode(wildcardErr, common.ErrCodeRecordNotFound))

	// Consequence: a key that exists in the store cannot authenticate.
	user, err := s.Authenticate(ctx, nil, secretPayload("live-key"))
	require.Error(t, err, "DEFECT PINNED: a stored API key cannot authenticate on a memory-backed store")
	assert.Nil(t, user)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))
	assert.Contains(t, err.Error(), "invalid API key")
}

// TestDatabaseStrategy_AuthenticateSucceedsWhenRangeKeyMatches shows the same
// strategy authenticates correctly once the stored range key is the literal
// "*" the lookup asks for. This is the control for the defect above: the
// authentication logic is sound, only the range key does not line up.
func TestDatabaseStrategy_AuthenticateSucceedsWhenRangeKeyMatches(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := memoryStrategyConfig()
	cfg.Cache = nil
	s := newMemoryStrategy(t, cfg)

	storeRecord(t, s.GetConnector(), "match-key", "*", userRecord(t, "user-99", "premium", nil))

	user, err := s.Authenticate(ctx, nil, secretPayload("match-key"))
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, "user-99", user.Id, "the authenticated identity must come from the stored record")
	assert.Equal(t, "premium", user.RateLimitBudget, "the stored rate limit budget must reach the caller")
}

// TestDatabaseStrategy_DisabledKeyIsRefused pins that the `enabled: false`
// flag on a stored record blocks authentication.
func TestDatabaseStrategy_DisabledKeyIsRefused(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := memoryStrategyConfig()
	cfg.Cache = nil
	s := newMemoryStrategy(t, cfg)

	disabled := false
	storeRecord(t, s.GetConnector(), "disabled-key", "*", userRecord(t, "user-off", "", &disabled))

	user, err := s.Authenticate(ctx, nil, secretPayload("disabled-key"))
	require.Error(t, err)
	assert.Nil(t, user)
	assert.Contains(t, err.Error(), "API key is disabled")

	// A record with `enabled: true` on the same store still authenticates, so
	// the refusal above is the flag and not a store-wide failure.
	enabled := true
	storeRecord(t, s.GetConnector(), "enabled-key", "*", userRecord(t, "user-on", "", &enabled))
	okUser, err := s.Authenticate(ctx, nil, secretPayload("enabled-key"))
	require.NoError(t, err)
	require.NotNil(t, okUser)
	assert.Equal(t, "user-on", okUser.Id)
}

// TestDatabaseStrategy_MalformedRecordsAreRefused pins the two record-shape
// refusals: unparsable JSON, and valid JSON with no user id.
func TestDatabaseStrategy_MalformedRecordsAreRefused(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := memoryStrategyConfig()
	cfg.Cache = nil
	s := newMemoryStrategy(t, cfg)

	storeRecord(t, s.GetConnector(), "broken-key", "*", []byte(`{not json`))
	_, err := s.Authenticate(ctx, nil, secretPayload("broken-key"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid user data format")

	storeRecord(t, s.GetConnector(), "nouser-key", "*", []byte(`{"rateLimitBudget":"gold"}`))
	_, err = s.Authenticate(ctx, nil, secretPayload("nouser-key"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing user ID in data")
}
