package data

import (
	"context"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

const (
	ConnectorMainIndex    = "idx_main"
	ConnectorReverseIndex = "idx_reverse"
)

// ConnectorApiKeyRangeKey is the range key that holds an API-key record.
//
// A record is addressed by (partitionKey = the API key, rangeKey =
// ConnectorApiKeyRangeKey). The reader knows the API key and nothing else, so
// the range key must be a constant the reader can write down. The user the key
// belongs to travels inside the record body, not in the key.
//
// The value is the literal "*". That is deliberate, and it is what lets a store
// written by an older eRPC keep working. Older versions stored the record at
// (apiKey, userId), which no reader could locate. PostgreSQL is the one driver
// that expands "*" on a main-index range key (see getWithWildcard), so on a
// PostgreSQL store a read at this key matches both the record written here and
// a record left behind at the user id. Every other driver matches the literal,
// which is exactly what this constant writes.
//
// Do not read this value as "match anything". Memory, Redis and DynamoDB do a
// plain key comparison on the main index.
const ConnectorApiKeyRangeKey = "*"

type DistributedLock interface {
	Unlock(ctx context.Context) error
	IsNil() bool
}

type KeyValuePair struct {
	PartitionKey string
	RangeKey     string
	Value        []byte
}

// CounterInt64State is the canonical JSON payload stored for shared int64 counters.
//
// NOTE:
// - UpdatedAt is unix milliseconds; UpdatedAt <= 0 indicates uninitialized state.
// - UpdatedBy is best-effort (e.g., hostname/pod name) and is used for diagnostics only.
// - Value can be 0 for valid cases like earliest block = genesis; use UpdatedAt to check initialization.
type CounterInt64State struct {
	Value     int64  `json:"v"`
	UpdatedAt int64  `json:"t"`
	UpdatedBy string `json:"b,omitempty"`
}

// Connector is a two-level key/value store: a partition key selects a group,
// and a range key selects a record inside it.
//
// # What a key means
//
// On ConnectorMainIndex a record is addressed exactly. Both keys are compared
// as literals, and a caller must know both to read, overwrite or delete a
// record. There is no portable way to ask a connector for "any record under
// this partition key" — Memory is backed by ristretto and Redis is a flat
// keyspace, so neither can enumerate range keys without a full scan.
//
// On ConnectorReverseIndex a trailing "*" on the partition key means "the most
// recently written partition key with this prefix". Memory and Redis serve it
// from a pointer entry that Set writes and Delete removes; PostgreSQL and
// DynamoDB serve it from a real index. This is the only wildcard the interface
// promises, and it exists for the response cache, which knows a network but not
// the block ref it wants.
//
// PostgreSQL additionally expands "*" anywhere in either key into a SQL LIKE
// pattern. Treat that as a driver detail, not as contract. New code must not
// depend on it. One existing caller does — see ConnectorApiKeyRangeKey — and
// that dependency is what keeps records written by an older eRPC readable.
//
// The gRPC connector is read-only and answers from request metadata rather than
// from either key. Set and Delete on it always fail, so it can serve a cache but
// never a store an operator writes to.
type Connector interface {
	Id() string
	Get(ctx context.Context, index, partitionKey, rangeKey string, metadata interface{}) ([]byte, error)
	// Note if "value" is going to be stored/kept in memory for longer than response lifecycle it must be
	// copied to a new memory location because B2Str is used to provide "value" as a string reference.
	Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error
	// Delete removes the record at (partitionKey, rangeKey). On every writable
	// driver it is idempotent: deleting a record that is not there is not an
	// error. The read-only gRPC connector always fails.
	Delete(ctx context.Context, partitionKey, rangeKey string) error
	List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error)
	Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error)
	WatchCounterInt64(ctx context.Context, key string) (<-chan CounterInt64State, func(), error)
	PublishCounterInt64(ctx context.Context, key string, value CounterInt64State) error
}

// CacheHeadReporter is an optional capability implemented by read-through connectors that can report
// the timestamp of the latest block they currently serve. It lets the realtime cache age guard be
// enforced even for responses that carry no block timestamp of their own (eth_blockNumber,
// eth_gasPrice, eth_getLogs). Connectors that serve locally-written data do not implement it.
type CacheHeadReporter interface {
	// CacheLatestBlockTimestamp returns the unix timestamp (seconds) of the latest block this
	// connector can currently serve for networkId, and whether it is known.
	CacheLatestBlockTimestamp(networkId string) (unixSeconds int64, ok bool)
}

func NewConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	cfg *common.ConnectorConfig,
) (Connector, error) {
	var connector Connector
	var err error

	switch cfg.Driver {
	case common.DriverMemory:
		connector, err = NewMemoryConnector(ctx, logger, cfg.Id, cfg.Memory)
	case common.DriverRedis:
		connector, err = NewRedisConnector(ctx, logger, cfg.Id, cfg.Redis)
	case common.DriverDynamoDB:
		connector, err = NewDynamoDBConnector(ctx, logger, cfg.Id, cfg.DynamoDB)
	case common.DriverPostgreSQL:
		connector, err = NewPostgreSQLConnector(ctx, logger, cfg.Id, cfg.PostgreSQL)
	case common.DriverGrpc:
		connector, err = NewGrpcConnector(ctx, logger, cfg.Id, cfg.Grpc)
	default:
		if util.IsTest() && cfg.Driver == "mock" {
			connector, err = NewMockMemoryConnector(ctx, logger, "mock", cfg.Mock)
		} else {
			return nil, common.NewErrInvalidConnectorDriver(cfg.Driver)
		}
	}

	if err != nil {
		return nil, err
	}

	// Wrap with failsafe if configured
	if len(cfg.FailsafeForGets) > 0 || len(cfg.FailsafeForSets) > 0 {
		connector, err = NewFailsafeConnector(ctx, logger, connector, cfg.FailsafeForGets, cfg.FailsafeForSets)
		if err != nil {
			return nil, err
		}
	}

	return connector, nil
}
