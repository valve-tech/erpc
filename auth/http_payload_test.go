package auth

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The credential every case below carries. Each assertion checks that this
// exact value ARRIVES in the payload. An assertion that only checked "the
// payload is not the wrong secret" would pass on an empty field, and an empty
// secret authenticates nobody — or, worse, matches a strategy configured with
// an empty value.
const httpTestSecret = "SUPERSECRETAPIKEY"

func basicAuthHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// TestNewPayloadFromHttp_SelectsTheStrategyTheCallerAskedFor walks every input
// surface the HTTP ingress accepts. An operator cares because a caller that
// presents a bearer token must reach the JWT strategy, not fall through to the
// network strategy and get authenticated by IP alone.
func TestNewPayloadFromHttp_SelectsTheStrategyTheCallerAskedFor(t *testing.T) {
	cases := []struct {
		name     string
		headers  http.Header
		args     url.Values
		wantType common.AuthType
		// exactly one of the three below is checked, based on wantType
		wantSecret string
		wantJwt    string
		wantSiwe   *SiwePayload
	}{
		{
			name:       "deprecated token query argument",
			args:       url.Values{"token": {httpTestSecret}},
			wantType:   common.AuthTypeSecret,
			wantSecret: httpTestSecret,
		},
		{
			name:       "secret query argument",
			args:       url.Values{"secret": {httpTestSecret}},
			wantType:   common.AuthTypeSecret,
			wantSecret: httpTestSecret,
		},
		{
			name:       "X-ERPC-Secret-Token header",
			headers:    http.Header{"X-Erpc-Secret-Token": {httpTestSecret}},
			wantType:   common.AuthTypeSecret,
			wantSecret: httpTestSecret,
		},
		{
			name:       "basic authorization uses the password as the secret",
			headers:    http.Header{"Authorization": {basicAuthHeader("ignored-user", httpTestSecret)}},
			wantType:   common.AuthTypeSecret,
			wantSecret: httpTestSecret,
		},
		{
			name:       "basic authorization keeps a password that contains a colon",
			headers:    http.Header{"Authorization": {basicAuthHeader("user", "pa:ss:word")}},
			wantType:   common.AuthTypeSecret,
			wantSecret: "pa:ss:word",
		},
		{
			name:       "basic authorization accepts an empty username",
			headers:    http.Header{"Authorization": {basicAuthHeader("", httpTestSecret)}},
			wantType:   common.AuthTypeSecret,
			wantSecret: httpTestSecret,
		},
		{
			name:     "bearer authorization",
			headers:  http.Header{"Authorization": {"Bearer my.jwt.token"}},
			wantType: common.AuthTypeJwt,
			wantJwt:  "my.jwt.token",
		},
		{
			name:     "authorization scheme is case insensitive",
			headers:  http.Header{"Authorization": {"BEARER my.jwt.token"}},
			wantType: common.AuthTypeJwt,
			wantJwt:  "my.jwt.token",
		},
		{
			name:     "leading whitespace does not hide the scheme",
			headers:  http.Header{"Authorization": {"   Bearer my.jwt.token"}},
			wantType: common.AuthTypeJwt,
			wantJwt:  "my.jwt.token",
		},
		{
			name:     "jwt query argument",
			args:     url.Values{"jwt": {"my.jwt.token"}},
			wantType: common.AuthTypeJwt,
			wantJwt:  "my.jwt.token",
		},
		{
			name:     "siwe query arguments",
			args:     url.Values{"signature": {"0xsig"}, "message": {"example.com wants you to sign in"}},
			wantType: common.AuthTypeSiwe,
			wantSiwe: &SiwePayload{Signature: "0xsig", Message: "example.com wants you to sign in"},
		},
		{
			name: "siwe headers",
			headers: http.Header{
				"X-Siwe-Message":   {"example.com wants you to sign in"},
				"X-Siwe-Signature": {"0xsig"},
			},
			wantType: common.AuthTypeSiwe,
			wantSiwe: &SiwePayload{Signature: "0xsig", Message: "example.com wants you to sign in"},
		},
		{
			name:     "no credentials at all falls back to the network strategy",
			wantType: common.AuthTypeNetwork,
		},
		{
			name:     "an unknown authorization scheme falls back to the network strategy",
			headers:  http.Header{"Authorization": {"Digest abcdef"}},
			wantType: common.AuthTypeNetwork,
		},
		{
			name:     "an authorization header with no space falls back to the network strategy",
			headers:  http.Header{"Authorization": {"Bearer"}},
			wantType: common.AuthTypeNetwork,
		},
		{
			name:     "a siwe signature without a message falls back to the network strategy",
			args:     url.Values{"signature": {"0xsig"}},
			wantType: common.AuthTypeNetwork,
		},
		{
			name:     "a siwe message header without a signature header falls back to the network strategy",
			headers:  http.Header{"X-Siwe-Message": {"example.com wants you to sign in"}},
			wantType: common.AuthTypeNetwork,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := tc.headers
			if headers == nil {
				headers = http.Header{}
			}
			args := tc.args
			if args == nil {
				args = url.Values{}
			}

			ap, err := NewPayloadFromHttp("eth_chainId", "203.0.113.7:1234", headers, args)
			require.NoError(t, err)
			require.NotNil(t, ap)

			assert.Equal(t, "eth_chainId", ap.Method, "the method must reach the payload for allowMethods matching")
			assert.Equal(t, tc.wantType, ap.Type)

			switch tc.wantType {
			case common.AuthTypeSecret:
				require.NotNil(t, ap.Secret, "a secret payload must be present")
				assert.Equal(t, tc.wantSecret, ap.Secret.Value,
					"the exact secret must reach the strategy; an empty value would authenticate the wrong caller")
			case common.AuthTypeJwt:
				require.NotNil(t, ap.Jwt)
				assert.Equal(t, tc.wantJwt, ap.Jwt.Token)
			case common.AuthTypeSiwe:
				require.NotNil(t, ap.Siwe)
				assert.Equal(t, tc.wantSiwe.Signature, ap.Siwe.Signature)
				assert.Equal(t, tc.wantSiwe.Message, ap.Siwe.Message)
			case common.AuthTypeNetwork:
				assert.Nil(t, ap.Secret, "the network fallback must not carry a half-built secret payload")
				assert.Nil(t, ap.Jwt)
				assert.Nil(t, ap.Siwe)
			}
		})
	}
}

// TestNewPayloadFromHttp_PrecedenceIsStable pins the order the parser checks
// its inputs. Precedence matters to an operator: a caller who sends both a
// query secret and a bearer token must land on exactly one strategy, and the
// same one on every request, or authentication becomes non-deterministic.
func TestNewPayloadFromHttp_PrecedenceIsStable(t *testing.T) {
	headers := http.Header{
		"X-Erpc-Secret-Token": {"header-secret"},
		"Authorization":       {"Bearer header.jwt"},
		"X-Siwe-Message":      {"example.com wants you to sign in"},
		"X-Siwe-Signature":    {"0xsig"},
	}
	args := url.Values{
		"token":     {"deprecated-token"},
		"secret":    {"query-secret"},
		"jwt":       {"query.jwt"},
		"signature": {"0xsig"},
		"message":   {"example.com wants you to sign in"},
	}

	// 1. token query argument beats everything.
	ap, err := NewPayloadFromHttp("eth_chainId", "", headers, args)
	require.NoError(t, err)
	require.Equal(t, common.AuthTypeSecret, ap.Type)
	require.NotNil(t, ap.Secret)
	assert.Equal(t, "deprecated-token", ap.Secret.Value)

	// 2. secret query argument beats the header and the bearer token.
	args.Del("token")
	ap, err = NewPayloadFromHttp("eth_chainId", "", headers, args)
	require.NoError(t, err)
	require.NotNil(t, ap.Secret)
	assert.Equal(t, "query-secret", ap.Secret.Value)

	// 3. the secret header beats the Authorization header.
	args.Del("secret")
	ap, err = NewPayloadFromHttp("eth_chainId", "", headers, args)
	require.NoError(t, err)
	require.NotNil(t, ap.Secret)
	assert.Equal(t, "header-secret", ap.Secret.Value)

	// 4. Authorization beats the jwt query argument.
	headers.Del("X-Erpc-Secret-Token")
	ap, err = NewPayloadFromHttp("eth_chainId", "", headers, args)
	require.NoError(t, err)
	require.Equal(t, common.AuthTypeJwt, ap.Type)
	require.NotNil(t, ap.Jwt)
	assert.Equal(t, "header.jwt", ap.Jwt.Token)

	// 5. the jwt query argument beats the SIWE arguments.
	headers.Del("Authorization")
	ap, err = NewPayloadFromHttp("eth_chainId", "", headers, args)
	require.NoError(t, err)
	require.Equal(t, common.AuthTypeJwt, ap.Type)
	require.NotNil(t, ap.Jwt)
	assert.Equal(t, "query.jwt", ap.Jwt.Token)

	// 6. SIWE query arguments beat the SIWE headers.
	args.Del("jwt")
	args.Set("message", "query.example.com wants you to sign in")
	ap, err = NewPayloadFromHttp("eth_chainId", "", headers, args)
	require.NoError(t, err)
	require.Equal(t, common.AuthTypeSiwe, ap.Type)
	require.NotNil(t, ap.Siwe)
	assert.Equal(t, "query.example.com wants you to sign in", ap.Siwe.Message)

	// 7. with the query arguments gone the SIWE headers are used.
	args.Del("signature")
	args.Del("message")
	ap, err = NewPayloadFromHttp("eth_chainId", "", headers, args)
	require.NoError(t, err)
	require.Equal(t, common.AuthTypeSiwe, ap.Type)
	require.NotNil(t, ap.Siwe)
	assert.Equal(t, "example.com wants you to sign in", ap.Siwe.Message)
}

// TestNewPayloadFromHttp_RejectsMalformedBasicAuth proves the parser reports an
// error rather than building a payload from garbage. Falling through to the
// network strategy here would turn a broken credential into an IP-only
// authentication, which is a privilege escalation.
func TestNewPayloadFromHttp_RejectsMalformedBasicAuth(t *testing.T) {
	t.Run("value is not valid base64", func(t *testing.T) {
		headers := http.Header{"Authorization": {"Basic !!!not-base64!!!"}}
		ap, err := NewPayloadFromHttp("eth_chainId", "", headers, url.Values{})
		require.Error(t, err)
		assert.Nil(t, ap, "no payload may be returned when the credential cannot be decoded")
	})

	t.Run("decoded value has no colon", func(t *testing.T) {
		headers := http.Header{
			"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte("nocolonhere"))},
		}
		ap, err := NewPayloadFromHttp("eth_chainId", "", headers, url.Values{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid basic auth")
		assert.Nil(t, ap)
	})
}

// TestNormalizeSiweMessage_DecodesBase64AndPassesPlainTextThrough covers both
// halves of the branch. A browser client may base64 the message to survive
// header transport; a curl user sends it raw. Getting this wrong makes the SIWE
// parser reject a legitimate sign-in.
func TestNormalizeSiweMessage_DecodesBase64AndPassesPlainTextThrough(t *testing.T) {
	const plain = "example.com wants you to sign in with your Ethereum account:\n0xabc"

	t.Run("base64 input is decoded", func(t *testing.T) {
		encoded := base64.StdEncoding.EncodeToString([]byte(plain))
		assert.Equal(t, plain, normalizeSiweMessage(encoded))
	})

	t.Run("plain text input survives untouched", func(t *testing.T) {
		// A raw SIWE message is not valid base64 (it has spaces and newlines),
		// so the decoder must fail and the original must be returned.
		assert.Equal(t, plain, normalizeSiweMessage(plain))
	})

	t.Run("the http parser applies the decoding", func(t *testing.T) {
		encoded := base64.StdEncoding.EncodeToString([]byte(plain))
		ap, err := NewPayloadFromHttp("eth_chainId", "", http.Header{}, url.Values{
			"signature": {"0xsig"},
			"message":   {encoded},
		})
		require.NoError(t, err)
		require.NotNil(t, ap.Siwe)
		assert.Equal(t, plain, ap.Siwe.Message,
			"the SIWE parser downstream needs the decoded message, not the base64 blob")
	})
}
