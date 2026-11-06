package auth

import (
	"encoding/base64"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"net/http"
	"net/url"
	"testing"
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

func TestNewPayloadFromHttp_SignalPrecedence(t *testing.T) {
	siweMsg := "example.com wants you to sign in"
	siweMsgB64 := base64.StdEncoding.EncodeToString([]byte(siweMsg))
	basic := func(creds string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
	}

	tests := []struct {
		name       string
		headers    http.Header
		args       url.Values
		wantType   common.AuthType
		wantSecret string
		wantJwt    string
		wantSig    string
		wantMsg    string
	}{
		{
			name:     "no signals defaults to network",
			wantType: common.AuthTypeNetwork,
		},
		{
			name:       "deprecated token query param wins over everything",
			args:       url.Values{"token": {"tok"}, "secret": {"sec"}, "jwt": {"j"}},
			headers:    http.Header{"X-Erpc-Secret-Token": {"hdr"}, "Authorization": {"Bearer b"}},
			wantType:   common.AuthTypeSecret,
			wantSecret: "tok",
		},
		{
			name:       "secret query param wins over header",
			args:       url.Values{"secret": {"sec"}},
			headers:    http.Header{"X-Erpc-Secret-Token": {"hdr"}},
			wantType:   common.AuthTypeSecret,
			wantSecret: "sec",
		},
		{
			name:       "secret header wins over authorization",
			headers:    http.Header{"X-Erpc-Secret-Token": {"hdr"}, "Authorization": {"Bearer b"}},
			wantType:   common.AuthTypeSecret,
			wantSecret: "hdr",
		},
		{
			name:       "basic auth password is the secret, username ignored",
			headers:    http.Header{"Authorization": {basic("alice:pa:ss")}},
			wantType:   common.AuthTypeSecret,
			wantSecret: "pa:ss",
		},
		{
			name:       "auth scheme is case-insensitive and value is trimmed",
			headers:    http.Header{"Authorization": {"  BASIC " + base64.StdEncoding.EncodeToString([]byte("u:p")) + "  "}},
			wantType:   common.AuthTypeSecret,
			wantSecret: "p",
		},
		{
			name:     "bearer token becomes jwt",
			headers:  http.Header{"Authorization": {"Bearer eyJ.abc.def"}},
			wantType: common.AuthTypeJwt,
			wantJwt:  "eyJ.abc.def",
		},
		{
			name:     "unknown authorization scheme falls back to network and shadows jwt query",
			headers:  http.Header{"Authorization": {"Digest abc"}},
			args:     url.Values{"jwt": {"ignored"}},
			wantType: common.AuthTypeNetwork,
		},
		{
			name:     "authorization without a value falls back to network",
			headers:  http.Header{"Authorization": {"Bearer"}},
			wantType: common.AuthTypeNetwork,
		},
		{
			name:     "jwt query param",
			args:     url.Values{"jwt": {"q.jwt.tok"}},
			wantType: common.AuthTypeJwt,
			wantJwt:  "q.jwt.tok",
		},
		{
			name:     "siwe query params with base64 message are decoded",
			args:     url.Values{"signature": {"0xsig"}, "message": {siweMsgB64}},
			wantType: common.AuthTypeSiwe,
			wantSig:  "0xsig",
			wantMsg:  siweMsg,
		},
		{
			name:     "siwe query params with plain message are kept verbatim",
			args:     url.Values{"signature": {"0xsig"}, "message": {siweMsg}},
			wantType: common.AuthTypeSiwe,
			wantSig:  "0xsig",
			wantMsg:  siweMsg,
		},
		{
			name:     "siwe signature without message is ignored",
			args:     url.Values{"signature": {"0xsig"}},
			wantType: common.AuthTypeNetwork,
		},
		{
			name:     "siwe headers",
			headers:  http.Header{"X-Siwe-Message": {siweMsgB64}, "X-Siwe-Signature": {"0xhdr"}},
			wantType: common.AuthTypeSiwe,
			wantSig:  "0xhdr",
			wantMsg:  siweMsg,
		},
		{
			name:     "siwe message header without signature falls back to network",
			headers:  http.Header{"X-Siwe-Message": {siweMsgB64}},
			wantType: common.AuthTypeNetwork,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers := tc.headers
			if headers == nil {
				headers = http.Header{}
			}
			args := tc.args
			if args == nil {
				args = url.Values{}
			}
			ap, err := NewPayloadFromHttp("eth_call", "10.0.0.1:1234", headers, args)
			require.NoError(t, err)
			assert.Equal(t, "eth_call", ap.Method)
			assert.Equal(t, tc.wantType, ap.Type)

			if tc.wantSecret != "" {
				require.NotNil(t, ap.Secret)
				assert.Equal(t, tc.wantSecret, ap.Secret.Value)
			} else {
				assert.Nil(t, ap.Secret)
			}
			if tc.wantJwt != "" {
				require.NotNil(t, ap.Jwt)
				assert.Equal(t, tc.wantJwt, ap.Jwt.Token)
			} else {
				assert.Nil(t, ap.Jwt)
			}
			if tc.wantSig != "" {
				require.NotNil(t, ap.Siwe)
				assert.Equal(t, tc.wantSig, ap.Siwe.Signature)
				assert.Equal(t, tc.wantMsg, ap.Siwe.Message)
			} else {
				assert.Nil(t, ap.Siwe)
			}
		})
	}
}

func TestNewPayloadFromHttp_MalformedBasicAuth(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "not base64", value: "Basic !!!not-base64!!!"},
		{name: "no colon separator", value: "Basic " + base64.StdEncoding.EncodeToString([]byte("justuser"))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ap, err := NewPayloadFromHttp("eth_call", "", http.Header{"Authorization": {tc.value}}, url.Values{})
			require.Error(t, err)
			assert.Nil(t, ap)
		})
	}
}

func TestNewPayloadFromGrpc_Signals(t *testing.T) {
	siweMsg := "grpc.example wants you to sign in"
	tests := []struct {
		name       string
		md         metadata.MD
		wantType   common.AuthType
		wantSecret string
		wantSig    string
		wantMsg    string
		wantErr    bool
	}{
		{
			name:       "secret token metadata wins over authorization",
			md:         metadata.Pairs("x-erpc-secret-token", "s3", "authorization", "Bearer x"),
			wantType:   common.AuthTypeSecret,
			wantSecret: "s3",
		},
		{
			name:     "siwe metadata decodes base64 message",
			md:       metadata.Pairs("x-siwe-message", base64.StdEncoding.EncodeToString([]byte(siweMsg)), "x-siwe-signature", "0xabc"),
			wantType: common.AuthTypeSiwe,
			wantSig:  "0xabc",
			wantMsg:  siweMsg,
		},
		{
			name:     "siwe message without signature defaults to network",
			md:       metadata.Pairs("x-siwe-message", siweMsg),
			wantType: common.AuthTypeNetwork,
		},
		{
			name:     "empty metadata defaults to network",
			md:       metadata.MD{},
			wantType: common.AuthTypeNetwork,
		},
		{
			name:    "basic auth that is not base64 is rejected",
			md:      metadata.Pairs("authorization", "Basic %%%"),
			wantErr: true,
		},
		{
			name:    "basic auth without colon is rejected",
			md:      metadata.Pairs("authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("nocolon"))),
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ap, err := NewPayloadFromGrpc("eth_chainId", tc.md)
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, ap)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantType, ap.Type)
			if tc.wantSecret != "" {
				require.NotNil(t, ap.Secret)
				assert.Equal(t, tc.wantSecret, ap.Secret.Value)
			}
			if tc.wantSig != "" {
				require.NotNil(t, ap.Siwe)
				assert.Equal(t, tc.wantSig, ap.Siwe.Signature)
				assert.Equal(t, tc.wantMsg, ap.Siwe.Message)
			} else {
				assert.Nil(t, ap.Siwe)
			}
		})
	}
}
