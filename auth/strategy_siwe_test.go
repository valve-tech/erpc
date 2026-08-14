package auth

import (
	"context"
	"crypto/ecdsa"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/spruceid/siwe-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// siweSigner is a throwaway wallet. Every SIWE test signs with a real key so
// the signature path runs for real; a fixture signature would rot the first
// time the message text changed.
type siweSigner struct {
	key     *ecdsa.PrivateKey
	address string
}

func newSiweSigner(t *testing.T) *siweSigner {
	t.Helper()
	key, err := ethcrypto.GenerateKey()
	require.NoError(t, err)
	return &siweSigner{
		key:     key,
		address: ethcrypto.PubkeyToAddress(key.PublicKey).Hex(),
	}
}

// buildSiweMessage returns the EIP-4361 text and its EIP-191 signature.
func (s *siweSigner) buildSiweMessage(t *testing.T, domain string, options map[string]interface{}) (string, string) {
	t.Helper()
	if options == nil {
		options = map[string]interface{}{}
	}
	msg, err := siwe.InitMessage(domain, s.address, "https://"+domain+"/login", "abcdef1234567890", options)
	require.NoError(t, err)

	text := msg.String()
	digest := ethcrypto.Keccak256Hash(
		[]byte("\x19Ethereum Signed Message:\n" + strconv.Itoa(len(text)) + text),
	)
	sig, err := ethcrypto.Sign(digest.Bytes(), s.key)
	require.NoError(t, err)
	return text, hexutil.Encode(sig)
}

// TestSiweStrategy_Supports keeps the strategy off payloads it cannot judge.
func TestSiweStrategy_Supports(t *testing.T) {
	s := NewSiweStrategy(&common.SiweStrategyConfig{})
	assert.True(t, s.Supports(&AuthPayload{Type: common.AuthTypeSiwe}))
	for _, other := range []common.AuthType{
		common.AuthTypeSecret, common.AuthTypeJwt, common.AuthTypeNetwork, common.AuthTypeDatabase,
	} {
		assert.False(t, s.Supports(&AuthPayload{Type: other}), "must not claim a %s payload", other)
	}
}

// TestSiweStrategy_Authenticate_AcceptsAValidlySignedMessage is the positive
// case. It asserts the returned user id IS the signer address in lowercase.
// Downstream rate-limit budgets and metrics key on that id, so an empty or
// checksummed id would silently split one caller into two identities.
func TestSiweStrategy_Authenticate_AcceptsAValidlySignedMessage(t *testing.T) {
	signer := newSiweSigner(t)
	s := NewSiweStrategy(&common.SiweStrategyConfig{
		AllowedDomains:  []string{"app.example.com"},
		RateLimitBudget: "siwe-budget",
	})

	text, sig := signer.buildSiweMessage(t, "app.example.com", map[string]interface{}{
		"expirationTime": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	})

	user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
		Type: common.AuthTypeSiwe,
		Siwe: &SiwePayload{Message: text, Signature: sig},
	})

	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, strings.ToLower(signer.address), user.Id,
		"the authenticated user id must be the lowercased signer address")
	assert.Equal(t, "siwe-budget", user.RateLimitBudget,
		"the configured budget must reach the user or the caller runs unlimited")
}

// TestSiweStrategy_Authenticate_RejectsForTheRightReason proves each rejection
// path is reached on its own input. Each case asserts on the message, because
// a strategy that rejected everything for one reason would still look correct
// to a test that only checked "an error came back".
func TestSiweStrategy_Authenticate_RejectsForTheRightReason(t *testing.T) {
	signer := newSiweSigner(t)
	other := newSiweSigner(t)
	cfg := &common.SiweStrategyConfig{AllowedDomains: []string{"app.example.com"}}
	s := NewSiweStrategy(cfg)

	validText, validSig := signer.buildSiweMessage(t, "app.example.com", nil)

	t.Run("missing payload", func(t *testing.T) {
		user, err := s.Authenticate(context.Background(), nil, &AuthPayload{Type: common.AuthTypeSiwe})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing SIWE payload")
		assert.Nil(t, user)
	})

	t.Run("unparseable message", func(t *testing.T) {
		user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeSiwe,
			Siwe: &SiwePayload{Message: "this is not a siwe message", Signature: validSig},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse SIWE message")
		assert.Nil(t, user)
	})

	t.Run("signature from a different wallet", func(t *testing.T) {
		// The attack this blocks: replaying somebody else's signature against a
		// message that names your address.
		_, otherSig := other.buildSiweMessage(t, "app.example.com", nil)
		user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeSiwe,
			Siwe: &SiwePayload{Message: validText, Signature: otherSig},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to verify SIWE signature")
		assert.Nil(t, user)
	})

	t.Run("empty signature", func(t *testing.T) {
		user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeSiwe,
			Siwe: &SiwePayload{Message: validText, Signature: ""},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to verify SIWE signature")
		assert.Nil(t, user)
	})

	t.Run("domain not on the allowlist", func(t *testing.T) {
		evilText, evilSig := signer.buildSiweMessage(t, "evil.example.com", nil)
		user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeSiwe,
			Siwe: &SiwePayload{Message: evilText, Signature: evilSig},
		})
		require.Error(t, err, "a correctly signed message for another site must not authenticate here")
		assert.Contains(t, err.Error(), "evil.example.com is not allowed")
		assert.Nil(t, user)
	})

	t.Run("expired message", func(t *testing.T) {
		expiredText, expiredSig := signer.buildSiweMessage(t, "app.example.com", map[string]interface{}{
			"expirationTime": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		})
		user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeSiwe,
			Siwe: &SiwePayload{Message: expiredText, Signature: expiredSig},
		})
		require.Error(t, err, "an expired sign-in must not be replayable")
		assert.Contains(t, err.Error(), "expired")
		assert.Nil(t, user)
	})

	t.Run("message not yet valid", func(t *testing.T) {
		futureText, futureSig := signer.buildSiweMessage(t, "app.example.com", map[string]interface{}{
			"notBefore": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		})
		user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeSiwe,
			Siwe: &SiwePayload{Message: futureText, Signature: futureSig},
		})
		require.Error(t, err)
		assert.Nil(t, user)
	})
}

// TestSiweStrategy_isDomainAllowed proves the allowlist matches exactly. A
// substring or suffix match would let evil-app.example.com pass for
// app.example.com, which is the classic phishing shape.
func TestSiweStrategy_isDomainAllowed(t *testing.T) {
	s := NewSiweStrategy(&common.SiweStrategyConfig{
		AllowedDomains: []string{"app.example.com", "admin.example.com:8443"},
	})

	assert.True(t, s.isDomainAllowed("app.example.com"))
	assert.True(t, s.isDomainAllowed("admin.example.com:8443"))

	for _, denied := range []string{
		"", "example.com", "evil-app.example.com", "app.example.com.evil.io",
		"APP.EXAMPLE.COM", "app.example.com:8443", "admin.example.com",
	} {
		assert.False(t, s.isDomainAllowed(denied), "%q must not be allowed", denied)
	}

	t.Run("an empty allowlist denies every domain", func(t *testing.T) {
		empty := NewSiweStrategy(&common.SiweStrategyConfig{})
		assert.False(t, empty.isDomainAllowed("app.example.com"),
			"an unconfigured allowlist must be closed, not open")
		assert.False(t, empty.isDomainAllowed(""))
	})
}
