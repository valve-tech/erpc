package common

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These tests cover the top of the config validation tree — the checks an
// operator hits on the very first boot. Every case below asserts that a bad
// value is REJECTED and that the message names the field, because a config
// error that reaches production as a silent default is exactly the failure this
// package exists to prevent.

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// minimalValidConfig is the smallest config that passes Config.Validate. Each
// test mutates one field, so a rejection can only come from that field.
func minimalValidConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		Server: &ServerConfig{
			MaxTimeout: Duration(30 * time.Second).Ptr(),
		},
		HealthCheck: &HealthCheckConfig{},
		Projects: []*ProjectConfig{
			{
				Id: "main",
				Upstreams: []*UpstreamConfig{
					{Id: "up1", Endpoint: "https://rpc.example.com"},
				},
			},
		},
	}
}

func TestConfig_Validate_AcceptsTheMinimalConfig(t *testing.T) {
	require.NoError(t, minimalValidConfig(t).Validate())
}

// A missing server or healthCheck block must fail loudly. Booting without them
// leaves the operator with no listener and no liveness endpoint.
func TestConfig_Validate_RequiresServerHealthCheckAndProjects(t *testing.T) {
	t.Run("server missing", func(t *testing.T) {
		c := minimalValidConfig(t)
		c.Server = nil
		require.ErrorContains(t, c.Validate(), "server config is required")
	})
	t.Run("healthCheck missing", func(t *testing.T) {
		c := minimalValidConfig(t)
		c.HealthCheck = nil
		require.ErrorContains(t, c.Validate(), "healthCheck config is required")
	})
	t.Run("projects missing", func(t *testing.T) {
		c := minimalValidConfig(t)
		c.Projects = nil
		require.ErrorContains(t, c.Validate(), "projects config is required")
	})
}

// Config.Validate must surface the child's error verbatim. A parent that
// replaced it with its own tidy message would hide which field is wrong.
func TestConfig_Validate_PropagatesEveryChildError(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{
			"metrics",
			func(c *Config) {
				c.Metrics = &MetricsConfig{ErrorLabelMode: "loud"}
			},
			"metrics.errorLabelMode",
		},
		{
			"admin",
			func(c *Config) {
				c.Admin = &AdminConfig{CORS: &CORSConfig{}}
			},
			"cors.allowedOrigins is required",
		},
		{
			"database",
			func(c *Config) {
				c.Database = &DatabaseConfig{
					EvmJsonRpcCache: &CacheConfig{
						Connectors: []*ConnectorConfig{{Id: "", Driver: DriverMemory}},
					},
				}
			},
			"connector.id is required",
		},
		{
			"project",
			func(c *Config) { c.Projects[0].Id = "" },
			"project id is required",
		},
		{
			"rateLimiters",
			func(c *Config) {
				c.RateLimiters = &RateLimiterConfig{Store: &RateLimitStoreConfig{Driver: "cassandra"}}
			},
			"rateLimiters.store.type",
		},
		{
			"proxyPools",
			func(c *Config) {
				c.ProxyPools = []*ProxyPoolConfig{{ID: "", Urls: []string{"http://p"}}}
			},
			"proxyPool.*.id is required",
		},
		{
			"indexer",
			func(c *Config) { c.Indexer = &IndexerConfig{CanonicalChainDepth: -1} },
			"indexer.canonicalChainDepth",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := minimalValidConfig(t)
			tc.mutate(c)
			err := c.Validate()
			require.Error(t, err, "the bad %s block must reach the top", tc.name)
			require.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// The indexer treats 0 as "use the internal default", so 0 must pass while a
// negative value must not: a negative ring-buffer size would panic much later,
// inside the reorg tracker, far from the config that caused it.
func TestIndexerConfig_Validate_ZeroMeansDefaultNegativeIsRejected(t *testing.T) {
	require.NoError(t, (&IndexerConfig{}).Validate())
	require.ErrorContains(t, (&IndexerConfig{CanonicalChainDepth: -1}).Validate(), "canonicalChainDepth must be >= 0")
	require.ErrorContains(t, (&IndexerConfig{DedupWindowSize: -1}).Validate(), "dedupWindowSize must be >= 0")
}

func TestServerConfig_Validate_ListenerNeedsHostAndPort(t *testing.T) {
	yes, no := true, false

	cases := []struct {
		name    string
		cfg     *ServerConfig
		wantSub string
	}{
		{
			"v4 listener without host",
			&ServerConfig{ListenV4: &yes, HttpPortV4: intPtr(4000), MaxTimeout: Duration(time.Second).Ptr()},
			"server.httpHostV4 is not set",
		},
		{
			"v4 listener without port",
			&ServerConfig{ListenV4: &yes, HttpHostV4: strPtr("0.0.0.0"), MaxTimeout: Duration(time.Second).Ptr()},
			"server.httpPortV4 is not set",
		},
		{
			"v6 listener without host",
			&ServerConfig{ListenV6: &yes, HttpPortV6: intPtr(4000), MaxTimeout: Duration(time.Second).Ptr()},
			"server.httpHostV6 is not set",
		},
		{
			"v6 listener without port",
			&ServerConfig{ListenV6: &yes, HttpHostV6: strPtr("::"), MaxTimeout: Duration(time.Second).Ptr()},
			"server.httpPortV6 is not set",
		},
		{
			"maxTimeout missing",
			&ServerConfig{},
			"server.maxTimeout is required",
		},
		{
			"maxTimeout zero",
			&ServerConfig{MaxTimeout: Duration(0).Ptr()},
			"server.maxTimeout is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, tc.cfg.Validate(), tc.wantSub)
		})
	}

	// listenV4=false must not demand a host or port — the operator turned the
	// listener off on purpose.
	off := &ServerConfig{ListenV4: &no, ListenV6: &no, MaxTimeout: Duration(time.Second).Ptr()}
	require.NoError(t, off.Validate())
}

// A trusted-forwarder list decides whose X-Forwarded-For eRPC believes. A typo
// that validated would silently trust nobody (or worse, be read as a literal),
// so every entry must parse as an IP or a CIDR.
func TestServerConfig_Validate_TrustedForwardersMustParse(t *testing.T) {
	base := func(entries ...string) *ServerConfig {
		return &ServerConfig{
			MaxTimeout:          Duration(time.Second).Ptr(),
			TrustedIPForwarders: entries,
		}
	}

	require.NoError(t, base("10.0.0.1", "10.0.0.0/8", "::1", "2001:db8::/32").Validate())

	require.ErrorContains(t, base("").Validate(), "empty entry")
	require.ErrorContains(t, base("   ").Validate(), "empty entry")
	require.ErrorContains(t, base("10.0.0.0/99").Validate(), "not a valid CIDR")
	require.ErrorContains(t, base("not-an-ip").Validate(), "not a valid IP address")
}

func TestMetricsConfig_Validate_EnabledNeedsAHostAndPort(t *testing.T) {
	on, off := true, false

	// Disabled metrics need nothing — an operator who turned them off must not
	// be forced to fill in a port.
	require.NoError(t, (&MetricsConfig{Enabled: &off}).Validate())

	require.ErrorContains(t,
		(&MetricsConfig{Enabled: &on, Port: intPtr(4001)}).Validate(),
		"metrics.hostV4 or metrics.hostV6 is required")
	require.ErrorContains(t,
		(&MetricsConfig{Enabled: &on, HostV4: strPtr("0.0.0.0")}).Validate(),
		"metrics.port is required")
	require.NoError(t,
		(&MetricsConfig{Enabled: &on, HostV6: strPtr("::"), Port: intPtr(4001)}).Validate())
}

// errorLabelMode picks how much of an error message lands in a Prometheus
// label. An unknown value would produce a cardinality surprise, so reject it.
func TestMetricsConfig_Validate_ErrorLabelModeIsAClosedSet(t *testing.T) {
	require.NoError(t, (&MetricsConfig{}).Validate())
	require.NoError(t, (&MetricsConfig{ErrorLabelMode: ErrorLabelModeVerbose}).Validate())
	require.NoError(t, (&MetricsConfig{ErrorLabelMode: ErrorLabelModeCompact}).Validate())
	require.ErrorContains(t,
		(&MetricsConfig{ErrorLabelMode: "chatty"}).Validate(),
		"must be either 'verbose' or 'compact'")
}

// A histogram bucket list that does not parse would make Prometheus reject the
// whole registry at scrape time, long after boot reported success.
func TestMetricsConfig_Validate_HistogramBucketsMustAllParse(t *testing.T) {
	require.NoError(t, (&MetricsConfig{HistogramBuckets: "0.1, 0.5, 1, 2.5"}).Validate())

	err := (&MetricsConfig{HistogramBuckets: "0.1,fast,1"}).Validate()
	require.ErrorContains(t, err, "invalid float value")
	require.Contains(t, err.Error(), "fast", "the message must name the offending entry")
}

func TestRateLimiterConfig_Validate_StoreDriverIsAClosedSet(t *testing.T) {
	require.ErrorContains(t, (&RateLimiterConfig{}).Validate(), "rateLimiters.store is required")

	require.NoError(t, (&RateLimiterConfig{Store: &RateLimitStoreConfig{Driver: "memory"}}).Validate())
	// The driver is matched case-insensitively after trimming, so an operator's
	// stray whitespace or capital must not fail the boot.
	require.NoError(t, (&RateLimiterConfig{Store: &RateLimitStoreConfig{Driver: " MEMORY "}}).Validate())

	require.ErrorContains(t,
		(&RateLimiterConfig{Store: &RateLimitStoreConfig{Driver: ""}}).Validate(),
		"must be one of: redis, memory")
	require.ErrorContains(t,
		(&RateLimiterConfig{Store: &RateLimitStoreConfig{Driver: "etcd"}}).Validate(),
		"must be one of: redis, memory")
}

func TestRateLimiterConfig_Validate_RedisStoreChecks(t *testing.T) {
	t.Run("redis block required", func(t *testing.T) {
		err := (&RateLimiterConfig{Store: &RateLimitStoreConfig{Driver: "redis"}}).Validate()
		require.ErrorContains(t, err, "rateLimiters.store.redis is required")
	})

	t.Run("nearLimitRatio must be a fraction", func(t *testing.T) {
		for _, ratio := range []float32{-0.1, 1, 1.5} {
			err := (&RateLimiterConfig{Store: &RateLimitStoreConfig{
				Driver:         "redis",
				Redis:          &RedisConnectorConfig{URI: "redis://localhost:6379"},
				NearLimitRatio: ratio,
			}}).Validate()
			require.ErrorContains(t, err, "nearLimitRatio must be > 0 and < 1", "ratio %v", ratio)
		}
	})

	t.Run("zero ratio means unset", func(t *testing.T) {
		require.NoError(t, (&RateLimiterConfig{Store: &RateLimitStoreConfig{
			Driver: "redis",
			Redis:  &RedisConnectorConfig{URI: "redis://localhost:6379"},
		}}).Validate())
	})

	// The redis connector's own error must reach the top wrapped, not replaced.
	t.Run("connector error is wrapped not swallowed", func(t *testing.T) {
		err := (&RateLimiterConfig{Store: &RateLimitStoreConfig{
			Driver: "redis",
			Redis:  &RedisConnectorConfig{URI: "memcache://localhost"},
		}}).Validate()
		require.ErrorContains(t, err, "rateLimiters.store.redis is invalid")
		require.ErrorContains(t, err, "invalid URI scheme")
	})
}

func TestRateLimitBudgetConfig_Validate_NeedsAtLeastOneRule(t *testing.T) {
	require.ErrorContains(t, (&RateLimitBudgetConfig{Id: "b"}).Validate(),
		"budget.rules is required")

	// A rule error must reach the top through the budget.
	err := (&RateLimitBudgetConfig{
		Id:    "b",
		Rules: []*RateLimitRuleConfig{{Method: "*", Period: RateLimitPeriod(99)}},
	}).Validate()
	require.ErrorContains(t, err, "period must be one of")
}

func TestRateLimitRuleConfig_Validate_MethodAndPeriod(t *testing.T) {
	require.ErrorContains(t, (&RateLimitRuleConfig{Period: RateLimitPeriodSecond}).Validate(),
		"rules.*.method is required")

	for _, p := range []RateLimitPeriod{
		RateLimitPeriodSecond, RateLimitPeriodMinute, RateLimitPeriodHour,
		RateLimitPeriodDay, RateLimitPeriodWeek, RateLimitPeriodMonth, RateLimitPeriodYear,
	} {
		require.NoError(t, (&RateLimitRuleConfig{Method: "*", Period: p}).Validate(), "period %v", p)
	}

	require.ErrorContains(t,
		(&RateLimitRuleConfig{Method: "*", Period: RateLimitPeriod(42)}).Validate(),
		"period must be one of")

	// waitTime is deprecated and only warns. An operator upgrading from an old
	// config must not be blocked by it.
	require.NoError(t,
		(&RateLimitRuleConfig{Method: "*", Period: RateLimitPeriodSecond, WaitTime: Duration(time.Second)}).Validate())
}

func TestProxyPoolConfig_Validate_RequiresIdAndSupportedSchemes(t *testing.T) {
	require.ErrorContains(t, (&ProxyPoolConfig{}).Validate(), "proxyPool.*.id is required")
	require.ErrorContains(t, (&ProxyPoolConfig{ID: "p"}).Validate(), "proxyPool.*.urls is required")

	require.NoError(t, (&ProxyPoolConfig{
		ID:   "p",
		Urls: []string{"http://a:1", "HTTPS://b:2", "socks5://c:3"},
	}).Validate())

	err := (&ProxyPoolConfig{ID: "p", Urls: []string{"ftp://a"}}).Validate()
	require.ErrorContains(t, err, "must be valid HTTP, HTTPS, or SOCKS5 URLs")
	require.Contains(t, err.Error(), "ftp://a", "the message must name the rejected URL")
}

func TestSharedStateConfig_Validate_TimeoutRelationships(t *testing.T) {
	base := func() *SharedStateConfig {
		return &SharedStateConfig{
			Connector:       &ConnectorConfig{Id: "s", Driver: DriverMemory, Memory: &MemoryConnectorConfig{}},
			FallbackTimeout: Duration(2 * time.Second),
			LockTtl:         Duration(10 * time.Second),
			LockMaxWait:     Duration(500 * time.Millisecond),
			UpdateMaxWait:   Duration(500 * time.Millisecond),
		}
	}

	require.NoError(t, base().Validate())

	cases := []struct {
		name    string
		mutate  func(*SharedStateConfig)
		wantSub string
	}{
		{"no connector", func(s *SharedStateConfig) { s.Connector = nil }, "sharedState.connector is required"},
		{"no fallbackTimeout", func(s *SharedStateConfig) { s.FallbackTimeout = 0 }, "fallbackTimeout is required"},
		{"fallbackTimeout too small", func(s *SharedStateConfig) { s.FallbackTimeout = Duration(50 * time.Millisecond) }, "at least 100ms"},
		{"no lockTtl", func(s *SharedStateConfig) { s.LockTtl = 0 }, "lockTtl is required"},
		{"lockTtl too small", func(s *SharedStateConfig) { s.LockTtl = Duration(500 * time.Millisecond) }, "at least 1s"},
		// A lock that expires before one remote round-trip finishes lets a
		// second replica take the lock mid-write.
		{"lockTtl under fallbackTimeout", func(s *SharedStateConfig) {
			s.LockTtl = Duration(1 * time.Second)
			s.FallbackTimeout = Duration(5 * time.Second)
		}, "should be at least as long as fallbackTimeout"},
		// The foreground budgets must stay under the network timeout, or the
		// request path inherits the full remote latency.
		{"lockMaxWait at fallbackTimeout", func(s *SharedStateConfig) { s.LockMaxWait = s.FallbackTimeout }, "lockMaxWait"},
		{"updateMaxWait at fallbackTimeout", func(s *SharedStateConfig) { s.UpdateMaxWait = s.FallbackTimeout }, "updateMaxWait"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			tc.mutate(s)
			require.ErrorContains(t, s.Validate(), tc.wantSub)
		})
	}

	// The connector's own error must reach the top unchanged.
	t.Run("connector error propagates", func(t *testing.T) {
		s := base()
		s.Connector = &ConnectorConfig{Id: "s", Driver: "cassandra"}
		require.ErrorContains(t, s.Validate(), "driver 'cassandra' is invalid")
	})
}

func TestCORSConfig_Validate_RequiresAtLeastOneOrigin(t *testing.T) {
	require.ErrorContains(t, (&CORSConfig{}).Validate(), "allowedOrigins is required")
	require.NoError(t, (&CORSConfig{AllowedOrigins: []string{"*"}}).Validate())
}

// HealthCheck and Admin only delegate. The test proves the delegation happens —
// a parent that skipped it would accept a broken auth block.
func TestHealthCheckAndAdmin_Validate_DelegateToAuth(t *testing.T) {
	bad := &AuthConfig{}

	require.ErrorContains(t, (&HealthCheckConfig{Auth: bad}).Validate(), "auth.strategies is required")
	require.ErrorContains(t, (&AdminConfig{Auth: bad}).Validate(), "auth.strategies is required")

	require.NoError(t, (&HealthCheckConfig{}).Validate())
	require.NoError(t, (&AdminConfig{}).Validate())
}

func TestConfig_Validate_RejectsDuplicateProjectChildIds(t *testing.T) {
	t.Run("duplicate upstream id", func(t *testing.T) {
		c := minimalValidConfig(t)
		c.Projects[0].Upstreams = append(c.Projects[0].Upstreams,
			&UpstreamConfig{Id: "up1", Endpoint: "https://other.example.com"})
		err := c.Validate()
		require.ErrorContains(t, err, "upstreams.*.id must be unique")
		require.Contains(t, err.Error(), "up1")
	})

	t.Run("duplicate network id", func(t *testing.T) {
		c := minimalValidConfig(t)
		ntw := func() *NetworkConfig {
			return &NetworkConfig{
				Architecture: ArchitectureEvm,
				Evm: &EvmNetworkConfig{
					ChainId:                     1,
					FallbackFinalityDepth:       128,
					FallbackStatePollerDebounce: Duration(time.Second),
					GetLogsMaxAllowedRange:      1000,
				},
			}
		}
		c.Projects[0].Networks = []*NetworkConfig{ntw(), ntw()}
		require.ErrorContains(t, c.Validate(), "networks.*.id must be unique")
	})

	t.Run("duplicate network alias", func(t *testing.T) {
		c := minimalValidConfig(t)
		ntw := func(chainId int64) *NetworkConfig {
			return &NetworkConfig{
				Architecture: ArchitectureEvm,
				Alias:        "primary",
				Evm: &EvmNetworkConfig{
					ChainId:                     chainId,
					FallbackFinalityDepth:       128,
					FallbackStatePollerDebounce: Duration(time.Second),
					GetLogsMaxAllowedRange:      1000,
				},
			}
		}
		c.Projects[0].Networks = []*NetworkConfig{ntw(1), ntw(42161)}
		require.ErrorContains(t, c.Validate(), "networks.*.alias must be unique")
	})

	t.Run("duplicate provider id", func(t *testing.T) {
		c := minimalValidConfig(t)
		prov := func() *ProviderConfig {
			return &ProviderConfig{Id: "alchemy", Vendor: "alchemy", UpstreamIdTemplate: "<PROVIDER>-<NETWORK>"}
		}
		c.Projects[0].Providers = []*ProviderConfig{prov(), prov()}
		require.ErrorContains(t, c.Validate(), "providers.*.id must be unique")
	})
}

// A project with neither upstreams nor providers routes nothing. It must not
// boot: an empty project answers every request with "no upstreams" instead of
// telling the operator at startup.
func TestProjectConfig_Validate_NeedsUpstreamsOrProviders(t *testing.T) {
	c := minimalValidConfig(t)
	c.Projects[0].Upstreams = nil
	require.ErrorContains(t, c.Validate(), "upstreams or project.*.providers is required")

	// Providers alone are enough.
	c.Projects[0].Providers = []*ProviderConfig{
		{Id: "alchemy", Vendor: "alchemy", UpstreamIdTemplate: "<PROVIDER>"},
	}
	require.NoError(t, c.Validate())
}

func TestProjectConfig_Validate_RateLimitBudgetMustExist(t *testing.T) {
	c := minimalValidConfig(t)
	c.Projects[0].RateLimitBudget = "ghost"
	err := c.Validate()
	require.ErrorContains(t, err, "does not exist in config.rateLimiters")
	require.Contains(t, err.Error(), "ghost")

	// Declaring the budget clears it.
	c.RateLimiters = &RateLimiterConfig{
		Store: &RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*RateLimitBudgetConfig{
			{Id: "ghost", Rules: []*RateLimitRuleConfig{{Method: "*", Period: RateLimitPeriodSecond}}},
		},
	}
	require.NoError(t, c.Validate())
}

func TestProjectConfig_Validate_AllowClientDirectivesMustCompile(t *testing.T) {
	c := minimalValidConfig(t)
	c.Projects[0].AllowClientDirectives = strPtr("(unclosed")
	require.ErrorContains(t, c.Validate(), "allowClientDirectives pattern is invalid")

	c.Projects[0].AllowClientDirectives = strPtr("use-upstream|retry-*")
	require.NoError(t, c.Validate())
}

func TestProviderConfig_Validate_RequiredFieldsAndNetworkFilters(t *testing.T) {
	base := func() *ProviderConfig {
		return &ProviderConfig{Id: "p", Vendor: "alchemy", UpstreamIdTemplate: "<PROVIDER>"}
	}
	cfg := &Config{}

	require.NoError(t, base().Validate(cfg))

	t.Run("required fields", func(t *testing.T) {
		p := base()
		p.Id = ""
		require.ErrorContains(t, p.Validate(cfg), "providers.*.id is required")

		p = base()
		p.Vendor = ""
		require.ErrorContains(t, p.Validate(cfg), "providers.*.vendor is required")

		p = base()
		p.UpstreamIdTemplate = ""
		require.ErrorContains(t, p.Validate(cfg), "upstreamIdTemplate is required")
	})

	// onlyNetworks and ignoreNetworks express opposite intents. Accepting both
	// would leave the resolution order undefined and silently drop networks.
	t.Run("filters are mutually exclusive", func(t *testing.T) {
		p := base()
		p.OnlyNetworks = []string{"evm:1"}
		p.IgnoreNetworks = []string{"evm:10"}
		require.ErrorContains(t, p.Validate(cfg), "mutually exclusive")
	})

	t.Run("network ids must be well formed", func(t *testing.T) {
		p := base()
		p.OnlyNetworks = []string{"mainnet"}
		require.ErrorContains(t, p.Validate(cfg), "onlyNetworks.* 'mainnet' is invalid")

		p = base()
		p.IgnoreNetworks = []string{"mainnet"}
		require.ErrorContains(t, p.Validate(cfg), "ignoreNetworks.* 'mainnet' is invalid")

		p = base()
		p.OnlyNetworks = []string{"evm:1", "evm:42161"}
		require.NoError(t, p.Validate(cfg))
	})

	// A provider override is an upstream config, and its errors must reach the
	// top — an override with a bad rateLimitCountMode is as fatal as one on a
	// plain upstream.
	t.Run("override errors propagate", func(t *testing.T) {
		p := base()
		p.Overrides = map[string]*UpstreamConfig{
			"evm:1": {Id: "o", RateLimitCountMode: "guess"},
		}
		require.ErrorContains(t, p.Validate(cfg), "rateLimitCountMode 'guess' is invalid")
	})
}

func TestAuthConfig_Validate_StrategyTypesAndUniqueConnectors(t *testing.T) {
	require.ErrorContains(t, (&AuthConfig{}).Validate(), "auth.strategies is required")

	t.Run("each type needs its block", func(t *testing.T) {
		cases := []struct {
			typ     AuthType
			wantSub string
		}{
			{AuthTypeNetwork, "auth.*.network is required"},
			{AuthTypeSecret, "auth.*.secret is required"},
			{AuthTypeJwt, "auth.*.jwt is required"},
			{AuthTypeSiwe, "auth.*.siwe is required"},
			{AuthTypeDatabase, "auth.*.database is required"},
		}
		for _, tc := range cases {
			err := (&AuthStrategyConfig{Type: tc.typ}).Validate()
			require.ErrorContains(t, err, tc.wantSub)
		}
	})

	t.Run("missing and unknown type", func(t *testing.T) {
		require.ErrorContains(t, (&AuthStrategyConfig{}).Validate(), "auth.*.type is required")
		require.ErrorContains(t, (&AuthStrategyConfig{Type: "oauth"}).Validate(), "auth.*.type 'oauth' is invalid")
	})

	t.Run("allowClientDirectives must compile", func(t *testing.T) {
		s := &AuthStrategyConfig{
			Type:                  AuthTypeSecret,
			Secret:                &SecretStrategyConfig{Value: "s3cret"},
			AllowClientDirectives: strPtr("(unclosed"),
		}
		require.ErrorContains(t, s.Validate(), "allowClientDirectives pattern is invalid")
	})

	// Two database strategies sharing one connector id would read each other's
	// cached credentials, so the pair must be rejected at boot.
	t.Run("database connector ids must be unique", func(t *testing.T) {
		strategy := func() *AuthStrategyConfig {
			return &AuthStrategyConfig{
				Type: AuthTypeDatabase,
				Database: &DatabaseStrategyConfig{
					Connector: &ConnectorConfig{Id: "shared", Driver: DriverMemory, Memory: &MemoryConnectorConfig{}},
				},
			}
		}
		a := &AuthConfig{Strategies: []*AuthStrategyConfig{strategy(), strategy()}}
		err := a.Validate()
		require.ErrorContains(t, err, "connector ID 'shared' is not unique")
	})
}

func TestSecretStrategyConfig_Validate_RequiresAValue(t *testing.T) {
	require.ErrorContains(t, (&SecretStrategyConfig{}).Validate(), "auth.*.secret.value is required")
	require.NoError(t, (&SecretStrategyConfig{Value: "s3cret"}).Validate())
}

func TestDatabaseStrategyConfig_Validate_CacheBounds(t *testing.T) {
	conn := func() *ConnectorConfig {
		return &ConnectorConfig{Id: "c", Driver: DriverMemory, Memory: &MemoryConnectorConfig{}}
	}

	require.ErrorContains(t, (&DatabaseStrategyConfig{}).Validate(), "database.connector is required")
	require.NoError(t, (&DatabaseStrategyConfig{Connector: conn()}).Validate())

	neg := -time.Second
	zero := int64(0)
	negCount := int64(-1)

	require.ErrorContains(t,
		(&DatabaseStrategyConfig{Connector: conn(), Cache: &DatabaseStrategyCacheConfig{TTL: &neg}}).Validate(),
		"cache.ttl must be non-negative")
	require.ErrorContains(t,
		(&DatabaseStrategyConfig{Connector: conn(), Cache: &DatabaseStrategyCacheConfig{MaxSize: &zero}}).Validate(),
		"cache.maxSize must be positive")
	require.ErrorContains(t,
		(&DatabaseStrategyConfig{Connector: conn(), Cache: &DatabaseStrategyCacheConfig{MaxCost: &zero}}).Validate(),
		"cache.maxCost must be positive")
	require.ErrorContains(t,
		(&DatabaseStrategyConfig{Connector: conn(), Cache: &DatabaseStrategyCacheConfig{NumCounters: &negCount}}).Validate(),
		"cache.numCounters must be positive")

	// The connector's own error must reach the top rather than being replaced
	// by a generic strategy error.
	bad := &DatabaseStrategyConfig{Connector: &ConnectorConfig{Id: "c", Driver: "cassandra"}}
	require.ErrorContains(t, bad.Validate(), "driver 'cassandra' is invalid")
}

// The strategies that accept anything still need a test: a future guard added
// without thought would break configs that are legal today.
func TestNetworkAndSiweStrategy_Validate_AcceptEmptyBlocks(t *testing.T) {
	require.NoError(t, (&NetworkStrategyConfig{}).Validate())
	require.NoError(t, (&SiweStrategyConfig{}).Validate())
	require.NoError(t, (&MemoryConnectorConfig{}).Validate())
}

func TestJwtStrategyConfig_Validate_KeysOrJwksUrl(t *testing.T) {
	require.ErrorContains(t, (&JwtStrategyConfig{}).Validate(),
		"verificationKeys or auth.*.jwt.verificationJwksUrl is required")

	require.NoError(t, (&JwtStrategyConfig{
		VerificationKeys: map[string]string{"kid": "-----BEGIN PUBLIC KEY-----"},
	}).Validate())

	require.NoError(t, (&JwtStrategyConfig{VerificationJwksUrl: "https://issuer.example.com/jwks"}).Validate())

	// A URL with a scheme but no host cannot be fetched. Accepting it would
	// leave every token unverifiable at request time.
	for _, bad := range []string{"ftp://issuer/jwks", "https://:443/jwks", "issuer.example.com/jwks"} {
		require.ErrorContains(t,
			(&JwtStrategyConfig{VerificationJwksUrl: bad}).Validate(),
			"must be a valid HTTP or HTTPS URL", "url %q", bad)
	}

	t.Run("claim matchers must carry values", func(t *testing.T) {
		j := &JwtStrategyConfig{
			VerificationJwksUrl: "https://issuer.example.com/jwks",
			ClaimMatchers:       map[string][]string{"sub": {}},
		}
		require.ErrorContains(t, j.Validate(), "claimMatchers.sub: must not be empty")

		j.ClaimMatchers = map[string][]string{"sub": {"alice", "  "}}
		require.ErrorContains(t, j.Validate(), "claimMatchers.sub: empty value not allowed")

		j.ClaimMatchers = map[string][]string{"sub": {"alice"}}
		require.NoError(t, j.Validate())
	})
}

// A blank JWKS URL must be treated as unset, not as a URL to parse. Trimming
// happens before the emptiness test, so whitespace alone is still "unset".
func TestJwtStrategyConfig_Validate_BlankJwksUrlIsUnset(t *testing.T) {
	err := (&JwtStrategyConfig{VerificationJwksUrl: "   "}).Validate()
	require.ErrorContains(t, err, "verificationKeys or auth.*.jwt.verificationJwksUrl is required")
	require.False(t, strings.Contains(err.Error(), "must be a valid HTTP"),
		"a blank URL must be reported as missing, not as malformed")
}
