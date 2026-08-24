package auth

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// The database strategy names the caller's API key on ten log lines. Three of
// them are above Debug, so a normal production deployment keeps them, and the
// Warn fires on a revoked key — the exact moment a leaked credential is being
// probed. A log is shipped, aggregated and retained, so the key may not be on
// any of those lines.
//
// Each test below asserts BOTH halves: the raw key is absent, and the redacted
// stand-in is present. Absence alone would still pass if the log line were
// deleted or never fired, which is how a redaction test comes to prove nothing.

// synthetic API key, invented for these tests and never issued.
const syntheticApiKey = "vk_SYNTHETIC_REDACTION_TEST_KEY_0001"

// newLoggingStrategy builds a DatabaseStrategy whose logger writes JSON into
// the returned buffer at Debug level, so every one of the ten sites is
// captured, not only those above Debug.
func newLoggingStrategy(t *testing.T, fc *fakeConnector) (*DatabaseStrategy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.DebugLevel)
	return &DatabaseStrategy{
		logger:    &logger,
		cfg:       &common.DatabaseStrategyConfig{Connector: &common.ConnectorConfig{Id: "test-db", Driver: "postgresql"}},
		connector: fc,
	}, buf
}

func authPayload(key string) *AuthPayload {
	return &AuthPayload{Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: key}}
}

// requireRedacted asserts the captured log carries the stand-in and not the key.
func requireRedacted(t *testing.T, out, key string) {
	t.Helper()
	require.NotEmpty(t, out, "no log line was captured; the presence assertion below would be vacuous")
	require.NotContains(t, out, key, "the raw API key reached the log")
	require.Contains(t, out, util.RedactSecret(key),
		"the redacted stand-in is missing, so the line cannot be correlated: %s", out)
}

// TestAuthenticate_RevokedKeyDoesNotLeakKey covers the Warn at the disabled-key
// branch: the highest-value line in the file for an attacker.
func TestAuthenticate_RevokedKeyDoesNotLeakKey(t *testing.T) {
	fc := &fakeConnector{
		id: "test-db",
		getResult: func() ([]byte, error) {
			return []byte(`{"userId":"user-1","enabled":false}`), nil
		},
	}
	s, buf := newLoggingStrategy(t, fc)

	user, err := s.Authenticate(context.Background(), nil, authPayload(syntheticApiKey))
	require.Nil(t, user)
	require.Error(t, err)

	out := buf.String()
	require.Contains(t, out, "authentication attempt with disabled API key",
		"the revoked-key line did not fire, so this test proves nothing: %s", out)
	requireRedacted(t, out, syntheticApiKey)
}

// TestAuthenticate_MalformedRecordDoesNotLeakKeyOrRecord covers the Error at
// the parse-failure branch, which also used to print the whole stored record.
func TestAuthenticate_MalformedRecordDoesNotLeakKeyOrRecord(t *testing.T) {
	const record = `{"userId":"user-1","secretField":` // truncated: invalid JSON
	fc := &fakeConnector{
		id:        "test-db",
		getResult: func() ([]byte, error) { return []byte(record), nil },
	}
	s, buf := newLoggingStrategy(t, fc)

	user, err := s.Authenticate(context.Background(), nil, authPayload(syntheticApiKey))
	require.Nil(t, user)
	require.Error(t, err)

	out := buf.String()
	require.Contains(t, out, "failed to parse user data from database",
		"the parse-failure line did not fire, so this test proves nothing: %s", out)
	requireRedacted(t, out, syntheticApiKey)

	// The stored record itself is customer data and must not be printed.
	require.NotContains(t, out, "secretField", "the stored record reached the log")
	// What replaced it still identifies which record is broken.
	require.Contains(t, out, `"dataLen":`, "the record length is the truncation signal: %s", out)
	require.Contains(t, out, util.RedactSecret(record),
		"the record digest is what tells one bad record from many: %s", out)
}

// TestAuthenticate_MissingUserIdDoesNotLeakKeyOrRecord covers the second Error
// site, where the record is valid JSON but carries no user id.
func TestAuthenticate_MissingUserIdDoesNotLeakKeyOrRecord(t *testing.T) {
	const record = `{"userId":"","tenant":"acme-confidential"}`
	fc := &fakeConnector{
		id:        "test-db",
		getResult: func() ([]byte, error) { return []byte(record), nil },
	}
	s, buf := newLoggingStrategy(t, fc)

	user, err := s.Authenticate(context.Background(), nil, authPayload(syntheticApiKey))
	require.Nil(t, user)
	require.Error(t, err)

	out := buf.String()
	require.Contains(t, out, "missing user ID in database record",
		"the missing-user-id line did not fire, so this test proves nothing: %s", out)
	requireRedacted(t, out, syntheticApiKey)

	require.NotContains(t, out, "acme-confidential", "the stored record reached the log")
	require.Contains(t, out, util.RedactSecret(record),
		"the record digest is what tells one bad record from many: %s", out)
}

// TestAuthenticate_SuccessPathDoesNotLeakKey covers the Debug lines on the
// happy path. A Debug line is still a log, and operators raise the log level
// during an incident — precisely when a key is most worth stealing.
func TestAuthenticate_SuccessPathDoesNotLeakKey(t *testing.T) {
	fc := &fakeConnector{
		id: "test-db",
		getResult: func() ([]byte, error) {
			return []byte(`{"userId":"user-1","rateLimitBudget":"basic"}`), nil
		},
	}
	s, buf := newLoggingStrategy(t, fc)

	user, err := s.Authenticate(context.Background(), nil, authPayload(syntheticApiKey))
	require.NoError(t, err)
	require.NotNil(t, user)

	out := buf.String()
	require.Contains(t, out, "user authenticated successfully",
		"the success line did not fire, so this test proves nothing: %s", out)
	requireRedacted(t, out, syntheticApiKey)
}

// TestInvalidateCache_DoesNotLeakKey covers the tenth site, which sits outside
// Authenticate on the admin path.
func TestInvalidateCache_DoesNotLeakKey(t *testing.T) {
	s, buf := newLoggingStrategy(t, &fakeConnector{id: "test-db"})

	s.InvalidateCache(syntheticApiKey)

	out := buf.String()
	require.Contains(t, out, "invalidated API key cache entry",
		"the invalidation line did not fire, so this test proves nothing: %s", out)
	requireRedacted(t, out, syntheticApiKey)
}

// TestStrategyDatabase_NoRawApiKeyFieldRemains is the source-level backstop.
// The per-path tests above only reach the sites a test happens to drive. This
// one holds for a site nobody has written yet, which is where the next leak
// will come from — an eleventh log line, or a rebase that restores an old one.
func TestStrategyDatabase_NoRawApiKeyFieldRemains(t *testing.T) {
	b, err := os.ReadFile("strategy_database.go")
	require.NoError(t, err)
	src := string(b)
	// strings.Contains rather than require.NotContains: the latter prints the
	// whole file on failure, which buries the one line that matters.
	require.False(t, strings.Contains(src, `Str("apiKey", apiKey)`),
		"a log site names the raw API key; wrap it in util.RedactSecret")
	require.False(t, strings.Contains(src, `RawJSON("data", valueBytes)`),
		"a log site prints the whole stored user record")
}
