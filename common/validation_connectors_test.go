package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Connector validation guards the cache and shared-state backends. A connector
// that validates but cannot connect fails at request time, where erpc/init.go
// downgrades the failure to a warning — so the cache silently does not exist.
// Every check below therefore has to bite at config load.

func TestConnectorConfig_Validate_IdAndDriverAreRequired(t *testing.T) {
	require.ErrorContains(t, (&ConnectorConfig{}).Validate(), "connector.id is required")
	require.ErrorContains(t, (&ConnectorConfig{Id: "c"}).Validate(), "connector.driver is required")

	err := (&ConnectorConfig{Id: "c", Driver: "cassandra"}).Validate()
	require.ErrorContains(t, err, "driver 'cassandra' is invalid")
	require.Contains(t, err.Error(), "memory", "the message must list the drivers that do exist")
}

// Each driver needs its own settings block. Without one the connector would
// build with an empty struct and every cache read would miss silently.
func TestConnectorConfig_Validate_EachDriverNeedsItsBlock(t *testing.T) {
	cases := []struct {
		driver  ConnectorDriverType
		wantSub string
	}{
		{DriverMemory, "connector.memory is required"},
		{DriverRedis, "connector.redis is required"},
		{DriverPostgreSQL, "connector.postgresql is required"},
		{DriverDynamoDB, "connector.dynamodb is required"},
	}
	for _, tc := range cases {
		t.Run(string(tc.driver), func(t *testing.T) {
			require.ErrorContains(t, (&ConnectorConfig{Id: "c", Driver: tc.driver}).Validate(), tc.wantSub)
		})
	}

	// gRPC is the exception: it has defaults for every knob, so a bare block is
	// legal and no block at all is legal too.
	require.NoError(t, (&ConnectorConfig{Id: "c", Driver: DriverGrpc}).Validate())
}

// Two driver blocks on one connector leave the driver choice ambiguous. The
// loser's settings would be read as if they applied, so reject the pair.
func TestConnectorConfig_Validate_DriverBlocksAreMutuallyExclusive(t *testing.T) {
	mem := &MemoryConnectorConfig{}
	redis := &RedisConnectorConfig{URI: "redis://localhost:6379"}
	pg := validPostgres()
	dyn := validDynamo()

	cases := []struct {
		name    string
		cfg     *ConnectorConfig
		wantSub string
	}{
		{"memory+redis", &ConnectorConfig{Id: "c", Driver: DriverMemory, Memory: mem, Redis: redis}, "connector.memory is mutually exclusive"},
		{"redis+postgres", &ConnectorConfig{Id: "c", Driver: DriverRedis, Redis: redis, PostgreSQL: pg}, "connector.redis is mutually exclusive"},
		{"postgres+dynamo", &ConnectorConfig{Id: "c", Driver: DriverPostgreSQL, PostgreSQL: pg, DynamoDB: dyn}, "connector.postgresql is mutually exclusive"},
		{"dynamo+memory", &ConnectorConfig{Id: "c", Driver: DriverDynamoDB, DynamoDB: dyn, Memory: mem}, "connector.memory is mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, tc.cfg.Validate(), tc.wantSub)
		})
	}
}

func validDynamo() *DynamoDBConnectorConfig {
	return &DynamoDBConnectorConfig{
		Table:             "erpc",
		PartitionKeyName:  "pk",
		RangeKeyName:      "rk",
		ReverseIndexName:  "idx",
		TTLAttributeName:  "ttl",
		InitTimeout:       Duration(5 * time.Second),
		GetTimeout:        Duration(time.Second),
		SetTimeout:        Duration(time.Second),
		StatePollInterval: Duration(10 * time.Second),
	}
}

func validPostgres() *PostgreSQLConnectorConfig {
	return &PostgreSQLConnectorConfig{
		ConnectionUri: "postgres://user@host:5432/db",
		Table:         "erpc",
		MinConns:      1,
		MaxConns:      10,
		InitTimeout:   Duration(5 * time.Second),
		GetTimeout:    Duration(time.Second),
		SetTimeout:    Duration(time.Second),
	}
}

func TestDynamoDBConnectorConfig_Validate_EveryFieldIsRequired(t *testing.T) {
	require.NoError(t, validDynamo().Validate())

	cases := []struct {
		name    string
		mutate  func(*DynamoDBConnectorConfig)
		wantSub string
	}{
		{"table", func(d *DynamoDBConnectorConfig) { d.Table = "" }, "dynamodb.table is required"},
		{"partitionKeyName", func(d *DynamoDBConnectorConfig) { d.PartitionKeyName = "" }, "partitionKeyName is required"},
		{"rangeKeyName", func(d *DynamoDBConnectorConfig) { d.RangeKeyName = "" }, "rangeKeyName is required"},
		{"reverseIndexName", func(d *DynamoDBConnectorConfig) { d.ReverseIndexName = "" }, "reverseIndexName is required"},
		{"ttlAttributeName", func(d *DynamoDBConnectorConfig) { d.TTLAttributeName = "" }, "ttlAttributeName is required"},
		{"initTimeout", func(d *DynamoDBConnectorConfig) { d.InitTimeout = 0 }, "initTimeout is required"},
		{"getTimeout", func(d *DynamoDBConnectorConfig) { d.GetTimeout = 0 }, "getTimeout is required"},
		{"setTimeout", func(d *DynamoDBConnectorConfig) { d.SetTimeout = 0 }, "setTimeout is required"},
		{"statePollInterval", func(d *DynamoDBConnectorConfig) { d.StatePollInterval = 0 }, "statePollInterval is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDynamo()
			tc.mutate(d)
			require.ErrorContains(t, d.Validate(), tc.wantSub)
		})
	}
}

func TestPostgreSQLConnectorConfig_Validate_RequiredFields(t *testing.T) {
	require.NoError(t, validPostgres().Validate())

	cases := []struct {
		name    string
		mutate  func(*PostgreSQLConnectorConfig)
		wantSub string
	}{
		{"connectionUri", func(p *PostgreSQLConnectorConfig) { p.ConnectionUri = "" }, "connectionUri is required"},
		{"table", func(p *PostgreSQLConnectorConfig) { p.Table = "" }, "postgresql.table is required"},
		{"minConns", func(p *PostgreSQLConnectorConfig) { p.MinConns = 0 }, "minConns is required"},
		{"maxConns", func(p *PostgreSQLConnectorConfig) { p.MaxConns = 0 }, "maxConns is required"},
		{"initTimeout", func(p *PostgreSQLConnectorConfig) { p.InitTimeout = 0 }, "initTimeout is required"},
		{"getTimeout", func(p *PostgreSQLConnectorConfig) { p.GetTimeout = 0 }, "getTimeout is required"},
		{"setTimeout", func(p *PostgreSQLConnectorConfig) { p.SetTimeout = 0 }, "setTimeout is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPostgres()
			tc.mutate(p)
			require.ErrorContains(t, p.Validate(), tc.wantSub)
		})
	}
}

// IAM auth signs a short-lived token for DBUser while pgx connects as the URI
// user. Any disagreement between the two produces an auth failure that looks
// like a credentials problem, so config load has to catch it.
func TestPostgreSQLConnectorConfig_Validate_IAMAuthRules(t *testing.T) {
	withIAM := func(uri string, iam *PostgreSQLIAMAuthConfig) *PostgreSQLConnectorConfig {
		p := validPostgres()
		p.ConnectionUri = uri
		p.IAMAuth = iam
		return p
	}
	good := func() *PostgreSQLIAMAuthConfig {
		return &PostgreSQLIAMAuthConfig{Enabled: true, Endpoint: "host:5432", DBUser: "user"}
	}

	require.NoError(t,
		withIAM("postgres://user@host:5432/db?sslmode=require", good()).Validate())

	t.Run("disabled iam skips every check", func(t *testing.T) {
		iam := &PostgreSQLIAMAuthConfig{Enabled: false}
		require.NoError(t, withIAM("postgres://user@host:5432/db", iam).Validate())
	})

	t.Run("endpoint must exist and carry a port", func(t *testing.T) {
		iam := good()
		iam.Endpoint = ""
		require.ErrorContains(t, withIAM("postgres://user@host/db?sslmode=require", iam).Validate(),
			"iamAuth.endpoint could not be derived")

		iam = good()
		iam.Endpoint = "host"
		require.ErrorContains(t, withIAM("postgres://user@host/db?sslmode=require", iam).Validate(),
			"must include a port")
	})

	t.Run("dbUser must exist", func(t *testing.T) {
		iam := good()
		iam.DBUser = ""
		require.ErrorContains(t, withIAM("postgres://host:5432/db?sslmode=require", iam).Validate(),
			"iamAuth.dbUser could not be derived")
	})

	t.Run("static password is rejected", func(t *testing.T) {
		err := withIAM("postgres://user:hunter2@host:5432/db?sslmode=require", good()).Validate()
		require.ErrorContains(t, err, "cannot combine IAM auth with a static password")
	})

	t.Run("dbUser must match the uri user", func(t *testing.T) {
		iam := good()
		iam.DBUser = "someone_else"
		err := withIAM("postgres://user@host:5432/db?sslmode=require", iam).Validate()
		require.ErrorContains(t, err, "does not match the user in connectionUri")
	})

	// sslmode=disable would send the signed token over plaintext. Checking only
	// that "sslmode=" is present would let it through, so assert each value.
	t.Run("sslmode must enforce TLS", func(t *testing.T) {
		for _, mode := range []string{"require", "verify-ca", "verify-full"} {
			require.NoError(t,
				withIAM("postgres://user@host:5432/db?sslmode="+mode, good()).Validate(), "sslmode=%s", mode)
		}
		for _, mode := range []string{"disable", "allow", "prefer", ""} {
			err := withIAM("postgres://user@host:5432/db?sslmode="+mode, good()).Validate()
			require.ErrorContains(t, err, "iamAuth requires SSL", "sslmode=%q must be refused", mode)
		}
	})

	t.Run("aws credential mode is a closed set", func(t *testing.T) {
		for _, mode := range []string{"file", "env", "secret"} {
			iam := good()
			iam.Auth = &AwsAuthConfig{Mode: mode}
			require.NoError(t,
				withIAM("postgres://user@host:5432/db?sslmode=require", iam).Validate(), "mode %s", mode)
		}
		iam := good()
		iam.Auth = &AwsAuthConfig{Mode: "imds"}
		require.ErrorContains(t,
			withIAM("postgres://user@host:5432/db?sslmode=require", iam).Validate(),
			"iamAuth.auth.mode \"imds\" is invalid")
	})
}

func TestRedisConnectorConfig_Validate_UriSchemeAndLockInterval(t *testing.T) {
	require.ErrorContains(t, (&RedisConnectorConfig{}).Validate(), "redis.uri is required")
	require.ErrorContains(t, (&RedisConnectorConfig{URI: "   "}).Validate(), "redis.uri is required")

	require.NoError(t, (&RedisConnectorConfig{URI: "redis://localhost:6379"}).Validate())
	require.NoError(t, (&RedisConnectorConfig{URI: "rediss://localhost:6379"}).Validate())
	require.ErrorContains(t, (&RedisConnectorConfig{URI: "memcache://localhost"}).Validate(),
		"invalid URI scheme")

	// A sub-100ms retry interval hammers Redis with lock polls under contention.
	require.ErrorContains(t,
		(&RedisConnectorConfig{URI: "redis://localhost:6379", LockRetryInterval: Duration(10 * time.Millisecond)}).Validate(),
		"lockRetryInterval should be at least 100ms")
	// Zero means "use the default" and must stay legal.
	require.NoError(t,
		(&RedisConnectorConfig{URI: "redis://localhost:6379", LockRetryInterval: 0}).Validate())
	require.NoError(t,
		(&RedisConnectorConfig{URI: "redis://localhost:6379", LockRetryInterval: Duration(100 * time.Millisecond)}).Validate())
}

func TestRedisConnectorConfig_Validate_IAMAuthRules(t *testing.T) {
	withIAM := func(uri string, iam *RedisIAMAuthConfig) *RedisConnectorConfig {
		return &RedisConnectorConfig{URI: uri, IAMAuth: iam}
	}
	good := func() *RedisIAMAuthConfig {
		return &RedisIAMAuthConfig{Enabled: true, CacheName: "cache", UserID: "user"}
	}

	require.NoError(t, withIAM("rediss://cache.example.com:6379", good()).Validate())

	t.Run("disabled iam skips every check", func(t *testing.T) {
		require.NoError(t,
			withIAM("redis://localhost:6379", &RedisIAMAuthConfig{Enabled: false}).Validate())
	})

	t.Run("cacheName and userID are required", func(t *testing.T) {
		iam := good()
		iam.CacheName = ""
		require.ErrorContains(t, withIAM("rediss://c:6379", iam).Validate(), "iamAuth.cacheName is required")

		iam = good()
		iam.UserID = ""
		require.ErrorContains(t, withIAM("rediss://c:6379", iam).Validate(), "iamAuth.userID is required")
	})

	// The IAM token is a bearer credential; plaintext redis:// would leak it.
	t.Run("plaintext scheme is refused", func(t *testing.T) {
		require.ErrorContains(t, withIAM("redis://c:6379", good()).Validate(),
			"iamAuth requires in-transit TLS")
	})

	t.Run("static password is refused", func(t *testing.T) {
		require.ErrorContains(t, withIAM("rediss://user:hunter2@c:6379", good()).Validate(),
			"cannot combine IAM auth with a static password")
	})

	t.Run("aws credential mode is a closed set", func(t *testing.T) {
		iam := good()
		iam.Auth = &AwsAuthConfig{Mode: "env"}
		require.NoError(t, withIAM("rediss://c:6379", iam).Validate())

		iam = good()
		iam.Auth = &AwsAuthConfig{Mode: "imds"}
		require.ErrorContains(t, withIAM("rediss://c:6379", iam).Validate(),
			"iamAuth.auth.mode \"imds\" is invalid")
	})
}

// A connector-level failsafe has no upstream and no latency metric source, so
// two policies cannot work there. Accepting them would produce a policy that
// never fires and an operator who believes it does.
func TestConnectorConfig_Validate_RejectsUnsupportedFailsafePolicies(t *testing.T) {
	base := func(fs *FailsafeConfig, forGets bool) *ConnectorConfig {
		c := &ConnectorConfig{Id: "c", Driver: DriverMemory, Memory: &MemoryConnectorConfig{}}
		if forGets {
			c.FailsafeForGets = []*FailsafeConfig{fs}
		} else {
			c.FailsafeForSets = []*FailsafeConfig{fs}
		}
		return c
	}

	t.Run("consensus is refused on gets and sets", func(t *testing.T) {
		fs := &FailsafeConfig{MatchMethod: "*", Consensus: &ConsensusPolicyConfig{MaxParticipants: 2, AgreementThreshold: 2}}

		err := base(fs, true).Validate()
		require.ErrorContains(t, err, "consensus is not supported for connector-level failsafe")
		require.Contains(t, err.Error(), "failsafeForGets[0]", "the message must locate the offending policy")

		err = base(fs, false).Validate()
		require.ErrorContains(t, err, "consensus is not supported")
		require.Contains(t, err.Error(), "failsafeForSets[0]")
	})

	t.Run("hedge quantile is refused", func(t *testing.T) {
		fs := &FailsafeConfig{
			MatchMethod: "*",
			Hedge:       &HedgePolicyConfig{Delay: &AdaptiveDuration{Quantile: 0.9, Base: Duration(50 * time.Millisecond)}},
		}
		require.ErrorContains(t, base(fs, true).Validate(),
			"hedge quantile is not supported for connector-level failsafe")
	})

	t.Run("a fixed hedge delay is accepted", func(t *testing.T) {
		fs := &FailsafeConfig{
			MatchMethod: "*",
			Hedge:       &HedgePolicyConfig{Delay: &AdaptiveDuration{Base: Duration(50 * time.Millisecond)}},
		}
		require.NoError(t, base(fs, true).Validate())
	})

	t.Run("a nil entry is skipped", func(t *testing.T) {
		require.NoError(t, base(nil, true).Validate())
	})
}

func TestCacheConfig_Validate_ConnectorIdsMustBeUnique(t *testing.T) {
	conn := func(id string) *ConnectorConfig {
		return &ConnectorConfig{Id: id, Driver: DriverMemory, Memory: &MemoryConnectorConfig{}}
	}

	require.NoError(t, (&CacheConfig{Connectors: []*ConnectorConfig{conn("a"), conn("b")}}).Validate())

	err := (&CacheConfig{Connectors: []*ConnectorConfig{conn("a"), conn("a")}}).Validate()
	require.ErrorContains(t, err, "connectors.*.id must be unique")
	require.Contains(t, err.Error(), "'a'", "the message must name the duplicated id")
}

func TestCachePolicyConfig_Validate_RequiredFieldsAndConnectorReference(t *testing.T) {
	cache := &CacheConfig{
		Connectors: []*ConnectorConfig{{Id: "mem", Driver: DriverMemory, Memory: &MemoryConnectorConfig{}}},
	}
	base := func() *CachePolicyConfig {
		return &CachePolicyConfig{Network: "*", Method: "*", Connector: "mem"}
	}

	require.NoError(t, base().Validate(cache))

	t.Run("required fields", func(t *testing.T) {
		p := base()
		p.Network = ""
		require.ErrorContains(t, p.Validate(cache), "policies.*.network is required")

		p = base()
		p.Method = ""
		require.ErrorContains(t, p.Validate(cache), "policies.*.method is required")

		p = base()
		p.Connector = ""
		require.ErrorContains(t, p.Validate(cache), "policies.*.connector is required")
	})

	// A policy naming a connector that does not exist would never store or
	// serve anything, and nothing at runtime reports the dangling reference.
	t.Run("connector must exist", func(t *testing.T) {
		p := base()
		p.Connector = "ghost"
		err := p.Validate(cache)
		require.ErrorContains(t, err, "does not exist in cache.connectors")
		require.Contains(t, err.Error(), "ghost")
	})
}

func TestCachePolicyConfig_Validate_ItemSizeBounds(t *testing.T) {
	cache := &CacheConfig{
		Connectors: []*ConnectorConfig{{Id: "mem", Driver: DriverMemory, Memory: &MemoryConnectorConfig{}}},
	}
	base := func() *CachePolicyConfig {
		return &CachePolicyConfig{Network: "*", Method: "*", Connector: "mem"}
	}

	t.Run("sizes must parse", func(t *testing.T) {
		p := base()
		p.MinItemSize = strPtr("big")
		require.ErrorContains(t, p.Validate(cache), "minItemSize is invalid")

		p = base()
		p.MaxItemSize = strPtr("huge")
		require.ErrorContains(t, p.Validate(cache), "maxItemSize is invalid")
	})

	// min > max makes the policy match nothing. It is silent at runtime: every
	// entry simply fails the size gate and the cache appears cold.
	t.Run("min must not exceed max", func(t *testing.T) {
		p := base()
		p.MinItemSize = strPtr("10mb")
		p.MaxItemSize = strPtr("1kb")
		require.ErrorContains(t, p.Validate(cache), "minItemSize must be less than or equal to maxItemSize")

		p = base()
		p.MinItemSize = strPtr("1kb")
		p.MaxItemSize = strPtr("10mb")
		require.NoError(t, p.Validate(cache))
	})
}

func TestCachePolicyConfig_Validate_AppliesToAndTTL(t *testing.T) {
	cache := &CacheConfig{
		Connectors: []*ConnectorConfig{{Id: "mem", Driver: DriverMemory, Memory: &MemoryConnectorConfig{}}},
	}
	base := func() *CachePolicyConfig {
		return &CachePolicyConfig{Network: "*", Method: "*", Connector: "mem"}
	}

	t.Run("appliesTo is a closed set", func(t *testing.T) {
		for _, v := range []CachePolicyAppliesTo{"", CachePolicyAppliesToBoth, CachePolicyAppliesToGet, CachePolicyAppliesToSet} {
			p := base()
			p.AppliesTo = v
			require.NoError(t, p.Validate(cache), "appliesTo %q", v)
		}
		p := base()
		p.AppliesTo = "read"
		require.ErrorContains(t, p.Validate(cache), "appliesTo must be one of: get, set, both")
	})

	// A block-time TTL describes head freshness. On any other finality the data
	// is immutable, so a dynamic TTL there would silently shorten a cache that
	// never needed to expire.
	t.Run("blockTimeMultiplier requires realtime finality", func(t *testing.T) {
		p := base()
		p.TTL = &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 1, Fallback: Duration(2 * time.Second)}
		p.Finality = DataFinalityStateFinalized
		require.ErrorContains(t, p.Validate(cache), "blockTimeMultiplier is only supported when finality is 'realtime'")

		p.Finality = DataFinalityStateRealtime
		require.NoError(t, p.Validate(cache))
	})

	t.Run("a negative multiplier is refused", func(t *testing.T) {
		p := base()
		p.TTL = &BlockTimeAdaptiveDuration{BlockTimeMultiplier: -1}
		require.ErrorContains(t, p.Validate(cache), "blockTimeMultiplier must be >= 0")
	})
}

func TestCompressionConfig_Validate_AlgorithmLevelAndThreshold(t *testing.T) {
	require.NoError(t, (&CompressionConfig{}).Validate())
	require.NoError(t, (&CompressionConfig{Algorithm: "zstd", ZstdLevel: "best", Threshold: 1024}).Validate())

	require.ErrorContains(t, (&CompressionConfig{Algorithm: "gzip"}).Validate(),
		"algorithm must be 'zstd'")

	for _, lvl := range []string{"fastest", "default", "better", "best"} {
		require.NoError(t, (&CompressionConfig{ZstdLevel: lvl}).Validate(), "level %s", lvl)
	}
	require.ErrorContains(t, (&CompressionConfig{ZstdLevel: "turbo"}).Validate(),
		"zstdLevel must be one of")

	require.ErrorContains(t, (&CompressionConfig{Threshold: -1}).Validate(),
		"threshold must be greater than or equal to 0")
}

// The cache is the only place SVM and EVM share a config type. A malformed
// svmJsonRpcCache used to sail through startup and fail later inside the SVM
// cache constructor, which init.go downgrades to a warning.
func TestDatabaseConfig_Validate_ChecksEvmAndSvmCachesAlike(t *testing.T) {
	badCache := func() *CacheConfig {
		return &CacheConfig{
			Policies: []*CachePolicyConfig{{Network: "*", Method: "*", Connector: "ghost"}},
		}
	}

	require.ErrorContains(t, (&DatabaseConfig{EvmJsonRpcCache: badCache()}).Validate(),
		"does not exist in cache.connectors")
	require.ErrorContains(t, (&DatabaseConfig{SvmJsonRpcCache: badCache()}).Validate(),
		"does not exist in cache.connectors")

	// The shared-state block must be checked too.
	require.ErrorContains(t,
		(&DatabaseConfig{SharedState: &SharedStateConfig{}}).Validate(),
		"sharedState.connector is required")

	require.NoError(t, (&DatabaseConfig{}).Validate())
}
