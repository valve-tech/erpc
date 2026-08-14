package auth

import (
	"context"
	"net"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func networkRequestFromIP(ip string) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
	req.SetClientIP(ip)
	return req
}

// TestNewNetworkStrategy_RejectsUnparseableConfig proves a typo in the operator's
// allowlist fails at startup. If the constructor silently dropped the bad entry,
// erpc would boot with a smaller allowlist than the operator wrote and would
// reject legitimate traffic with no explanation.
func TestNewNetworkStrategy_RejectsUnparseableConfig(t *testing.T) {
	t.Run("invalid IP", func(t *testing.T) {
		s, err := NewNetworkStrategy(&common.NetworkStrategyConfig{
			AllowedIPs: []string{"10.0.0.1", "not-an-ip"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-an-ip")
		assert.Nil(t, s)
	})

	t.Run("a bare IP in the CIDR list is rejected", func(t *testing.T) {
		// 10.0.0.0 without a mask is a common operator slip. net.ParseCIDR
		// rejects it, and so must the constructor.
		s, err := NewNetworkStrategy(&common.NetworkStrategyConfig{
			AllowedCIDRs: []string{"10.0.0.0"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid CIDR")
		assert.Nil(t, s)
	})

	t.Run("a valid config builds every entry", func(t *testing.T) {
		s, err := NewNetworkStrategy(&common.NetworkStrategyConfig{
			AllowedIPs:   []string{"10.0.0.1", "2001:db8::1"},
			AllowedCIDRs: []string{"192.168.0.0/24", "2001:db8:1::/48"},
		})
		require.NoError(t, err)
		require.NotNil(t, s)
		assert.Len(t, s.allowedIPs, 2, "every allowed IP must be kept, not just the last one")
		assert.Len(t, s.allowedCIDRs, 2, "every allowed CIDR must be kept")
	})
}

// TestNetworkStrategy_Supports keeps the strategy off payloads it cannot judge.
// If it claimed a secret payload, the registry would authenticate an API-key
// caller by IP and never check the key.
func TestNetworkStrategy_Supports(t *testing.T) {
	s, err := NewNetworkStrategy(&common.NetworkStrategyConfig{})
	require.NoError(t, err)

	assert.True(t, s.Supports(&AuthPayload{Type: common.AuthTypeNetwork}))
	for _, other := range []common.AuthType{
		common.AuthTypeSecret, common.AuthTypeJwt, common.AuthTypeSiwe, common.AuthTypeDatabase,
	} {
		assert.False(t, s.Supports(&AuthPayload{Type: other}), "must not claim a %s payload", other)
	}
}

// TestNetworkStrategy_Authenticate_AllowsOnlyListedClients is the core
// allow/deny table. Each accepted case asserts the returned user id is the
// expected value, not merely that an error is absent — a nil user with a nil
// error would sail past a "no error" check and leave the caller unattributed.
func TestNetworkStrategy_Authenticate_AllowsOnlyListedClients(t *testing.T) {
	cfg := &common.NetworkStrategyConfig{
		AllowedIPs:   []string{"203.0.113.5", "2001:db8::5"},
		AllowedCIDRs: []string{"192.168.10.0/24"},
	}

	cases := []struct {
		name       string
		clientIP   string
		wantAllow  bool
		wantUserId string
	}{
		{"exact IPv4 match", "203.0.113.5", true, "203.0.113.5"},
		{"exact IPv6 match", "2001:db8::5", true, "2001:db8::5"},
		{"inside the allowed CIDR", "192.168.10.77", true, "192.168.10.0/24"},
		{"first address of the allowed CIDR", "192.168.10.0", true, "192.168.10.0/24"},
		{"last address of the allowed CIDR", "192.168.10.255", true, "192.168.10.0/24"},
		{"one below the allowed CIDR", "192.168.9.255", false, ""},
		{"one above the allowed CIDR", "192.168.11.0", false, ""},
		{"neighbour of the allowed IP", "203.0.113.6", false, ""},
		{"loopback while localhost is not allowed", "127.0.0.1", false, ""},
		{"unrelated public address", "8.8.8.8", false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewNetworkStrategy(cfg)
			require.NoError(t, err)

			user, err := s.Authenticate(context.Background(), networkRequestFromIP(tc.clientIP), &AuthPayload{
				Type: common.AuthTypeNetwork,
			})

			if !tc.wantAllow {
				require.Error(t, err, "IP %s must be rejected", tc.clientIP)
				assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized),
					"a denied client must get an unauthorized error, not some other failure")
				assert.Nil(t, user)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, user, "an accepted client must come back with a user")
			assert.Equal(t, tc.wantUserId, user.Id)
		})
	}
}

// TestNetworkStrategy_Authenticate_RejectsAMissingOrUnparseableClientIP guards
// the degenerate inputs. An empty client IP means the ingress could not resolve
// the caller, and the strategy must deny rather than treat "" as a match.
func TestNetworkStrategy_Authenticate_RejectsAMissingOrUnparseableClientIP(t *testing.T) {
	s, err := NewNetworkStrategy(&common.NetworkStrategyConfig{
		AllowLocalhost: true,
		AllowedCIDRs:   []string{"0.0.0.0/0"},
	})
	require.NoError(t, err)

	t.Run("nil request", func(t *testing.T) {
		user, err := s.Authenticate(context.Background(), nil, &AuthPayload{Type: common.AuthTypeNetwork})
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))
		assert.Nil(t, user)
	})

	for _, ip := range []string{"", "not-an-ip", "203.0.113.5:8545"} {
		t.Run("client IP "+ip, func(t *testing.T) {
			// 0.0.0.0/0 admits every real address, so reaching an error here
			// proves the parse failed before any allowlist check.
			user, err := s.Authenticate(context.Background(), networkRequestFromIP(ip), &AuthPayload{
				Type: common.AuthTypeNetwork,
			})
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))
			assert.Nil(t, user)
		})
	}
}

// TestNetworkStrategy_Authenticate_AllowLocalhost proves the localhost switch
// admits every loopback form and nothing else. An operator who turns this on
// for a sidecar must not accidentally admit the wider private range.
func TestNetworkStrategy_Authenticate_AllowLocalhost(t *testing.T) {
	s, err := NewNetworkStrategy(&common.NetworkStrategyConfig{AllowLocalhost: true})
	require.NoError(t, err)

	for _, ip := range []string{"127.0.0.1", "127.0.0.53", "::1"} {
		t.Run("allows "+ip, func(t *testing.T) {
			user, err := s.Authenticate(context.Background(), networkRequestFromIP(ip), &AuthPayload{
				Type: common.AuthTypeNetwork,
			})
			require.NoError(t, err)
			require.NotNil(t, user)
			assert.Equal(t, net.ParseIP(ip).String(), user.Id)
		})
	}

	for _, ip := range []string{"10.0.0.1", "192.168.1.1", "128.0.0.1"} {
		t.Run("still denies "+ip, func(t *testing.T) {
			user, err := s.Authenticate(context.Background(), networkRequestFromIP(ip), &AuthPayload{
				Type: common.AuthTypeNetwork,
			})
			require.Error(t, err, "allowLocalhost must not widen the allowlist beyond loopback")
			assert.Nil(t, user)
		})
	}
}

// TestIsLocalhost pins the loopback predicate directly, including the IPv4
// address mapped into IPv6 space, which a dual-stack listener reports.
func TestIsLocalhost(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "127.1.2.3", "::1", "::ffff:127.0.0.1"} {
		assert.True(t, isLocalhost(net.ParseIP(ip)), "%s must count as localhost", ip)
	}
	for _, ip := range []string{"0.0.0.0", "10.0.0.1", "::", "2001:db8::1", "126.255.255.255", "128.0.0.1"} {
		assert.False(t, isLocalhost(net.ParseIP(ip)), "%s must not count as localhost", ip)
	}
}

// TestNetworkStrategy_Authenticate_IPAsUser proves the ipAsUser switch changes
// the identity a CIDR match reports. This is the difference between rate
// limiting a whole subnet as one caller and rate limiting each client
// separately, so an operator notices immediately when it is wrong.
func TestNetworkStrategy_Authenticate_IPAsUser(t *testing.T) {
	base := common.NetworkStrategyConfig{AllowedCIDRs: []string{"192.168.10.0/24"}}

	t.Run("off: the whole CIDR shares one identity", func(t *testing.T) {
		cfg := base
		s, err := NewNetworkStrategy(&cfg)
		require.NoError(t, err)

		first, err := s.Authenticate(context.Background(), networkRequestFromIP("192.168.10.7"), &AuthPayload{Type: common.AuthTypeNetwork})
		require.NoError(t, err)
		second, err := s.Authenticate(context.Background(), networkRequestFromIP("192.168.10.8"), &AuthPayload{Type: common.AuthTypeNetwork})
		require.NoError(t, err)

		assert.Equal(t, "192.168.10.0/24", first.Id)
		assert.Equal(t, first.Id, second.Id, "without ipAsUser both clients must share the CIDR identity")
	})

	t.Run("on: each client keeps its own identity", func(t *testing.T) {
		cfg := base
		cfg.IPAsUser = true
		s, err := NewNetworkStrategy(&cfg)
		require.NoError(t, err)

		first, err := s.Authenticate(context.Background(), networkRequestFromIP("192.168.10.7"), &AuthPayload{Type: common.AuthTypeNetwork})
		require.NoError(t, err)
		second, err := s.Authenticate(context.Background(), networkRequestFromIP("192.168.10.8"), &AuthPayload{Type: common.AuthTypeNetwork})
		require.NoError(t, err)

		assert.Equal(t, "192.168.10.7", first.Id)
		assert.Equal(t, "192.168.10.8", second.Id)
	})
}

// TestNetworkStrategy_Authenticate_AttachesRateLimitBudget checks every accept
// path carries the configured budget. A dropped budget means the caller is
// silently unlimited, which is exactly the failure an operator cannot see in
// logs until the bill arrives.
func TestNetworkStrategy_Authenticate_AttachesRateLimitBudget(t *testing.T) {
	cases := []struct {
		name     string
		cfg      common.NetworkStrategyConfig
		clientIP string
	}{
		{
			name:     "localhost path",
			cfg:      common.NetworkStrategyConfig{AllowLocalhost: true, RateLimitBudget: "local-budget"},
			clientIP: "127.0.0.1",
		},
		{
			name:     "exact IP path",
			cfg:      common.NetworkStrategyConfig{AllowedIPs: []string{"203.0.113.5"}, RateLimitBudget: "ip-budget"},
			clientIP: "203.0.113.5",
		},
		{
			name:     "CIDR path",
			cfg:      common.NetworkStrategyConfig{AllowedCIDRs: []string{"192.168.10.0/24"}, RateLimitBudget: "cidr-budget"},
			clientIP: "192.168.10.7",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			s, err := NewNetworkStrategy(&cfg)
			require.NoError(t, err)

			user, err := s.Authenticate(context.Background(), networkRequestFromIP(tc.clientIP), &AuthPayload{
				Type: common.AuthTypeNetwork,
			})
			require.NoError(t, err)
			require.NotNil(t, user)
			assert.Equal(t, cfg.RateLimitBudget, user.RateLimitBudget,
				"the configured budget must reach the user or the caller runs unlimited")
		})
	}

	t.Run("no budget configured leaves the field empty", func(t *testing.T) {
		s, err := NewNetworkStrategy(&common.NetworkStrategyConfig{AllowLocalhost: true})
		require.NoError(t, err)
		user, err := s.Authenticate(context.Background(), networkRequestFromIP("127.0.0.1"), &AuthPayload{
			Type: common.AuthTypeNetwork,
		})
		require.NoError(t, err)
		require.NotNil(t, user)
		assert.Equal(t, "", user.RateLimitBudget)
	})
}
