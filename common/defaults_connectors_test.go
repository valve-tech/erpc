package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ConnectorConfig.SetDefaults is the last thing that runs before eRPC opens a
// store. Everything below is a value the operator did not type and that decides
// WHICH table, WHICH database and WHICH driver the process talks to. Getting one
// wrong does not fail at startup — it silently reads or writes the wrong place.

func TestConnectorConfig_SetDefaults_NamesAnUnnamedConnectorAfterItsScopeAndDriver(t *testing.T) {
	// Cache policies reference a connector by id, and two scopes may each hold a
	// redis connector. A name derived from the driver alone would collide, and
	// the cache would resolve to whichever connector was registered last.
	for scope, want := range map[connectorScope]string{
		connectorScopeCache:       "cache-redis",
		connectorScopeSharedState: "shared-state-redis",
		connectorScopeAuth:        "auth-redis",
	} {
		c := &ConnectorConfig{Driver: DriverRedis, Redis: &RedisConnectorConfig{URI: "redis://one:6379"}}
		require.NoError(t, c.SetDefaults(scope))
		require.Equal(t, want, c.Id)
	}

	// An id the operator wrote is the one cache policies reference. It must
	// never be rewritten.
	c := &ConnectorConfig{Id: "hot", Driver: DriverMemory}
	require.NoError(t, c.SetDefaults(connectorScopeCache))
	require.Equal(t, "hot", c.Id)
}

func TestConnectorConfig_SetDefaults_InfersTheDriverFromTheBlockTheOperatorWrote(t *testing.T) {
	// Writing a `redis:` block and omitting `driver:` is the common shorthand.
	// Without the inference the connector keeps an empty driver, validation
	// rejects the config, and the operator is told a key they did write is
	// missing.
	t.Run("memory", func(t *testing.T) {
		c := &ConnectorConfig{Memory: &MemoryConnectorConfig{}}
		require.NoError(t, c.SetDefaults(connectorScopeCache))
		require.Equal(t, DriverMemory, c.Driver)
		require.Equal(t, 100000, c.Memory.MaxItems)
		require.Equal(t, "1GB", c.Memory.MaxTotalSize)
	})
	t.Run("redis", func(t *testing.T) {
		c := &ConnectorConfig{Redis: &RedisConnectorConfig{URI: "redis://one:6379"}}
		require.NoError(t, c.SetDefaults(connectorScopeCache))
		require.Equal(t, DriverRedis, c.Driver)
		require.Equal(t, 8, c.Redis.ConnPoolSize)
	})
	t.Run("postgresql", func(t *testing.T) {
		c := &ConnectorConfig{PostgreSQL: &PostgreSQLConnectorConfig{ConnectionUri: "postgres://h/db"}}
		require.NoError(t, c.SetDefaults(connectorScopeCache))
		require.Equal(t, DriverPostgreSQL, c.Driver)
		require.Equal(t, "erpc_json_rpc_cache", c.PostgreSQL.Table)
	})
	t.Run("dynamodb", func(t *testing.T) {
		c := &ConnectorConfig{DynamoDB: &DynamoDBConnectorConfig{}}
		require.NoError(t, c.SetDefaults(connectorScopeCache))
		require.Equal(t, DriverDynamoDB, c.Driver)
		require.Equal(t, "erpc_json_rpc_cache", c.DynamoDB.Table)
	})
	t.Run("grpc", func(t *testing.T) {
		c := &ConnectorConfig{Grpc: &GrpcConnectorConfig{}}
		require.NoError(t, c.SetDefaults(connectorScopeCache))
		require.Equal(t, DriverGrpc, c.Driver)
		require.Equal(t, Duration(100*time.Millisecond), c.Grpc.GetTimeout)
	})
}

func TestConnectorConfig_SetDefaults_MaterializesTheBlockADriverNameImplies(t *testing.T) {
	// The mirror case: `driver: dynamodb` with no block. The connector layer
	// dereferences the block, so leaving it nil turns a bare driver name into a
	// nil-pointer panic at startup instead of a working default.
	for _, tc := range []struct {
		driver ConnectorDriverType
		check  func(t *testing.T, c *ConnectorConfig)
	}{
		{DriverMemory, func(t *testing.T, c *ConnectorConfig) { require.NotNil(t, c.Memory) }},
		{DriverRedis, func(t *testing.T, c *ConnectorConfig) { require.NotNil(t, c.Redis) }},
		{DriverPostgreSQL, func(t *testing.T, c *ConnectorConfig) { require.NotNil(t, c.PostgreSQL) }},
		{DriverDynamoDB, func(t *testing.T, c *ConnectorConfig) { require.NotNil(t, c.DynamoDB) }},
		{DriverGrpc, func(t *testing.T, c *ConnectorConfig) { require.NotNil(t, c.Grpc) }},
	} {
		t.Run(string(tc.driver), func(t *testing.T) {
			c := &ConnectorConfig{Driver: tc.driver}
			require.NoError(t, c.SetDefaults(connectorScopeCache))
			tc.check(t, c)
		})
	}
}

func TestConnectorConfig_SetDefaults_SurfacesAContradictoryRedisBlock(t *testing.T) {
	// The redis connector refuses `uri` and `addr` together. That refusal has to
	// travel out of the connector walk; swallowing it would boot eRPC pointed at
	// whichever of the two the driver happened to read.
	c := &ConnectorConfig{Redis: &RedisConnectorConfig{URI: "redis://one:6379", Addr: "two:6379"}}
	err := c.SetDefaults(connectorScopeCache)
	require.Error(t, err)
	require.Contains(t, err.Error(), "redis connector")
}

func TestConnectorConfig_SetDefaults_GivesEveryConnectorFailsafePolicyTheWildcardMatcher(t *testing.T) {
	// A failsafe entry with no matchMethod matches nothing, so a timeout the
	// operator wrote for their cache would never arm and a slow store would
	// stall every request that touches it.
	//
	// The wildcard is applied twice: ConnectorConfig.SetDefaults writes it, and
	// FailsafeConfig.SetDefaults writes it again. Deleting either one alone
	// changes nothing observable, so this asserts the OUTCOME rather than
	// pretending to cover one of two redundant guards.
	c := &ConnectorConfig{
		Driver:          DriverMemory,
		FailsafeForGets: []*FailsafeConfig{nil, {Timeout: &TimeoutPolicyConfig{Duration: NewStaticDuration(time.Second)}}},
		FailsafeForSets: []*FailsafeConfig{{MatchMethod: "eth_getLogs", Retry: &RetryPolicyConfig{MaxAttempts: 2}}},
	}
	require.NoError(t, c.SetDefaults(connectorScopeCache))

	require.Nil(t, c.FailsafeForGets[0], "a nil entry is skipped, not dereferenced")
	require.Equal(t, "*", c.FailsafeForGets[1].MatchMethod)
	require.Equal(t, "eth_getLogs", c.FailsafeForSets[0].MatchMethod,
		"an explicit matcher must not be widened to everything")
	require.NotNil(t, c.FailsafeForSets[0].Retry, "the nested policy is defaulted too")
	require.Greater(t, c.FailsafeForSets[0].Retry.BackoffFactor, float32(0))
}

// No test pins the `policy #%d in failsafeForGets` wrapper at
// common/defaults.go:1061. FailsafeConfig.SetDefaults cannot return a non-nil
// error — every leaf policy's SetDefaults returns a literal nil — so no input
// reaches that branch. A test that cannot produce the bug cannot detect it.
// The finding is logged instead; see the report.

// ---------------------------------------------------------------------------
// Per-scope table names
// ---------------------------------------------------------------------------

func TestDynamoDBConnectorConfig_SetDefaults_PicksTheTableItsScopeOwns(t *testing.T) {
	// Shared state, the cache and auth are three different datasets. One default
	// table name for all three would have replicas' lock rows sitting in the
	// cache table, and each writer overwriting the other's rows.
	for scope, want := range map[connectorScope]string{
		connectorScopeSharedState: "erpc_shared_state",
		connectorScopeCache:       "erpc_json_rpc_cache",
		connectorScopeAuth:        "erpc_auth",
	} {
		d := &DynamoDBConnectorConfig{}
		require.NoError(t, d.SetDefaults(scope))
		require.Equal(t, want, d.Table)
	}

	// An unknown scope is a programming error inside eRPC, not operator input.
	// It has to fail loudly rather than open a table named "".
	d := &DynamoDBConnectorConfig{}
	err := d.SetDefaults(connectorScope("nowhere"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "nowhere")

	// An explicit table wins over every scope default.
	own := &DynamoDBConnectorConfig{Table: "my_table"}
	require.NoError(t, own.SetDefaults(connectorScopeCache))
	require.Equal(t, "my_table", own.Table)
}

func TestDynamoDBConnectorConfig_SetDefaults_FillsTheKeyNamesTheSchemaDependsOn(t *testing.T) {
	// The connector builds every query from these names. An empty partition key
	// makes each request fail at the AWS SDK with a validation error, long after
	// startup reported success.
	d := &DynamoDBConnectorConfig{}
	require.NoError(t, d.SetDefaults(connectorScopeCache))

	require.Equal(t, "groupKey", d.PartitionKeyName)
	require.Equal(t, "requestKey", d.RangeKeyName)
	require.Equal(t, "idx_requestKey_groupKey", d.ReverseIndexName)
	require.Equal(t, "ttl", d.TTLAttributeName)
	require.Equal(t, Duration(5*time.Second), d.InitTimeout)
	require.Equal(t, Duration(1*time.Second), d.GetTimeout)
	require.Equal(t, Duration(2*time.Second), d.SetTimeout)
	require.Equal(t, Duration(5*time.Second), d.StatePollInterval)

	// Every one of them is an override, not a constant.
	own := &DynamoDBConnectorConfig{
		PartitionKeyName:  "pk",
		RangeKeyName:      "sk",
		ReverseIndexName:  "idx",
		TTLAttributeName:  "expires",
		InitTimeout:       Duration(time.Minute),
		GetTimeout:        Duration(2 * time.Minute),
		SetTimeout:        Duration(3 * time.Minute),
		StatePollInterval: Duration(4 * time.Minute),
	}
	require.NoError(t, own.SetDefaults(connectorScopeCache))
	require.Equal(t, "pk", own.PartitionKeyName)
	require.Equal(t, "sk", own.RangeKeyName)
	require.Equal(t, "idx", own.ReverseIndexName)
	require.Equal(t, "expires", own.TTLAttributeName)
	require.Equal(t, Duration(time.Minute), own.InitTimeout)
	require.Equal(t, Duration(2*time.Minute), own.GetTimeout)
	require.Equal(t, Duration(3*time.Minute), own.SetTimeout)
	require.Equal(t, Duration(4*time.Minute), own.StatePollInterval)
}

func TestPostgreSQLConnectorConfig_SetDefaults_SizesThePoolForItsScope(t *testing.T) {
	// The auth connector answers one query per request and lives beside the
	// cache pool on the same database. Giving it the cache's 4/32 pool would let
	// a busy gateway open eight times the connections it needs, and Postgres
	// refuses connections long before it refuses queries.
	auth := &PostgreSQLConnectorConfig{ConnectionUri: "postgres://h/db"}
	require.NoError(t, auth.SetDefaults(connectorScopeAuth))
	require.Equal(t, "erpc_auth", auth.Table)
	require.Equal(t, int32(1), auth.MinConns)
	require.Equal(t, int32(4), auth.MaxConns)

	shared := &PostgreSQLConnectorConfig{ConnectionUri: "postgres://h/db"}
	require.NoError(t, shared.SetDefaults(connectorScopeSharedState))
	require.Equal(t, "erpc_shared_state", shared.Table)
	require.Equal(t, int32(4), shared.MinConns)
	require.Equal(t, int32(32), shared.MaxConns)

	cache := &PostgreSQLConnectorConfig{ConnectionUri: "postgres://h/db"}
	require.NoError(t, cache.SetDefaults(connectorScopeCache))
	require.Equal(t, "erpc_json_rpc_cache", cache.Table)
	require.Equal(t, int32(4), cache.MinConns)
	require.Equal(t, int32(32), cache.MaxConns)

	bad := &PostgreSQLConnectorConfig{ConnectionUri: "postgres://h/db"}
	err := bad.SetDefaults(connectorScope("nowhere"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "nowhere")
}

func TestPostgreSQLConnectorConfig_SetDefaults_AppendsSslmodeAfterAnExistingQuery(t *testing.T) {
	// RDS IAM auth only works over TLS. The connection URI usually already
	// carries query parameters, so the separator has to switch from "?" to "&";
	// getting it wrong makes libpq read the whole thing as one malformed
	// parameter and the connector never connects.
	withQuery := &PostgreSQLConnectorConfig{
		ConnectionUri: "postgres://u@h:5432/db?pool_max_conns=10",
		IAMAuth:       &PostgreSQLIAMAuthConfig{Enabled: true},
	}
	require.NoError(t, withQuery.SetDefaults(connectorScopeCache))
	require.Equal(t, "postgres://u@h:5432/db?pool_max_conns=10&sslmode=require", withQuery.ConnectionUri)

	noQuery := &PostgreSQLConnectorConfig{
		ConnectionUri: "postgres://u@h:5432/db",
		IAMAuth:       &PostgreSQLIAMAuthConfig{Enabled: true},
	}
	require.NoError(t, noQuery.SetDefaults(connectorScopeCache))
	require.Equal(t, "postgres://u@h:5432/db?sslmode=require", noQuery.ConnectionUri)

	// An operator who already chose an sslmode keeps it — appending a second one
	// would override their choice.
	explicit := &PostgreSQLConnectorConfig{
		ConnectionUri: "postgres://u@h:5432/db?sslmode=verify-full",
		IAMAuth:       &PostgreSQLIAMAuthConfig{Enabled: true},
	}
	require.NoError(t, explicit.SetDefaults(connectorScopeCache))
	require.Equal(t, "postgres://u@h:5432/db?sslmode=verify-full", explicit.ConnectionUri)

	// Without IAM auth nothing is appended at all.
	plain := &PostgreSQLConnectorConfig{ConnectionUri: "postgres://u@h:5432/db"}
	require.NoError(t, plain.SetDefaults(connectorScopeCache))
	require.Equal(t, "postgres://u@h:5432/db", plain.ConnectionUri)
}

func TestRedisConnectorConfig_SetDefaults_BuildsTheUriFromTheDiscreteFields(t *testing.T) {
	// `addr` + `username` + `password` + `db` is the field-by-field spelling. It
	// has to become one URI, with the credentials percent-encoded — a raw "@" or
	// ":" in a password would otherwise split the URI at the wrong place and the
	// client would dial a host that does not exist.
	r := &RedisConnectorConfig{
		Addr:     "cache.example:6380",
		Username: "user@corp",
		Password: "p@ss:word",
		DB:       3,
	}
	require.NoError(t, r.SetDefaults())
	require.Equal(t, "redis://user%40corp:p%40ss%3Aword@cache.example:6380/3", r.URI)
	require.Empty(t, r.Addr, "the discrete fields are consumed, so nothing reads them twice")
	require.Empty(t, r.Username)
	require.Empty(t, r.Password)
	require.Equal(t, 0, r.DB)
}

func TestRedisConnectorConfig_SetDefaults_SuppliesThePortAndSchemeTheAddrOmits(t *testing.T) {
	// A bare hostname is what an operator copies out of a managed-Redis console.
	// Without the 6379 default the client dials port 0 and fails to connect.
	r := &RedisConnectorConfig{Addr: "cache.example"}
	require.NoError(t, r.SetDefaults())
	require.Equal(t, "redis://cache.example:6379/0", r.URI)

	// TLS switches the scheme, because a rediss:// client and a redis:// client
	// do not talk to each other.
	tls := &RedisConnectorConfig{Addr: "cache.example", TLS: &TLSConfig{Enabled: true}}
	require.NoError(t, tls.SetDefaults())
	require.Equal(t, "rediss://cache.example:6379/0", tls.URI)

	// A scheme already written into `addr` must not survive into the host part.
	prefixed := &RedisConnectorConfig{Addr: "rediss://cache.example:6380"}
	require.NoError(t, prefixed.SetDefaults())
	require.Equal(t, "redis://cache.example:6380/0", prefixed.URI)
}

func TestRedisConnectorConfig_SetDefaults_LeavesAnExplicitUriUntouched(t *testing.T) {
	// The URI is the operator's exact connection string, credentials included.
	// Rebuilding it would drop any parameter eRPC does not model.
	r := &RedisConnectorConfig{URI: "redis://u:p@cache.example:6379/7?protocol=3"}
	require.NoError(t, r.SetDefaults())
	require.Equal(t, "redis://u:p@cache.example:6379/7?protocol=3", r.URI)

	// The timeouts are still filled, because they are read whichever way the
	// connection was described.
	require.Equal(t, 8, r.ConnPoolSize)
	require.Equal(t, Duration(5*time.Second), r.InitTimeout)
	require.Equal(t, Duration(1*time.Second), r.GetTimeout)
	require.Equal(t, Duration(3*time.Second), r.SetTimeout)
	require.Equal(t, Duration(500*time.Millisecond), r.LockRetryInterval)
}

func TestMisbehaviorsDestinationConfig_SetDefaults_FillsTheFlushBudgetForS3(t *testing.T) {
	// Consensus misbehaviour records are exported for later analysis. With a zero
	// flush budget the S3 writer either uploads one object per record or never
	// uploads at all; neither is what an operator who wrote `type: s3` expects.
	s3 := &MisbehaviorsDestinationConfig{Type: MisbehaviorsDestinationTypeS3}
	require.NoError(t, s3.SetDefaults())
	require.Equal(t, "{timestampMs}-{method}-{networkId}", s3.FilePattern)
	require.NotNil(t, s3.S3)
	require.Equal(t, 100, s3.S3.MaxRecords)
	require.Equal(t, int64(1024*1024), s3.S3.MaxSize)
	require.Equal(t, Duration(60*time.Second), s3.S3.FlushInterval)
	require.Equal(t, "application/jsonl", s3.S3.ContentType)

	// The default destination is a local file, and it must not invent an S3
	// block — an empty S3 config would make the writer try to reach AWS.
	file := &MisbehaviorsDestinationConfig{}
	require.NoError(t, file.SetDefaults())
	require.Equal(t, MisbehaviorsDestinationTypeFile, file.Type)
	require.Nil(t, file.S3)
}
