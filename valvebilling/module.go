package valvebilling

import (
	"context"
	"fmt"
	"math/big"

	"github.com/redis/go-redis/v9"
)

// Module is the billing path, assembled. A DISABLED module is a nil *Module.
//
// That is the whole point of the type. "Off" is not a branch inside the module
// that could be reached with the wrong value, or a field somebody forgets to
// check — it is the absence of the object. Enabled() is nil-safe, so a caller
// holding a nil *Module asks one question and takes the stock path, and there
// is no state left running to leak, no Redis connection open, and nothing to
// go wrong. New returns nil when the flag is clear.
type Module struct {
	cfg    Config
	rdb    *redis.Client
	prices *PriceTable
}

// New builds the module, or returns (nil, nil) when the flag is clear.
//
// A nil module with a nil error is the SUCCESS case for a deployment that does
// not use billing. Callers must test Enabled(), not err.
func New(ctx context.Context, cfg Config, prices *PriceTable) (*Module, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if prices == nil {
		return nil, fmt.Errorf("valvebilling: enabled without a price table; costs cannot be resolved")
	}

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		// Fail loud and fail at boot. A wrong Redis URL that degraded into
		// "billing quietly does nothing" would serve every request free and
		// look completely healthy.
		return nil, fmt.Errorf("valvebilling: %s is not a valid Redis URL: %w", EnvRedisURL, err)
	}
	rdb := redis.NewClient(opts)
	// A successful PING proves the socket and, usually, the credentials. It
	// does NOT prove the next command will work.
	//
	// Observed on 2026-08-24 with redis-cli: against a Redis that wants no
	// password, REDISCLI_AUTH set makes `ping` answer PONG while `--scan`
	// returns nothing at all, with the error only on stderr. That failed a
	// deploy gate which counted zero keys against a Redis holding 32.
	//
	// That specific asymmetry is redis-cli's, not go-redis's, so nothing
	// heavier is done here — a richer probe would be machinery guarding a
	// failure this client has not been shown to have. It is recorded because
	// the shape generalises: if billing ever reports an empty or impossible
	// reading while the connection looks healthy, distrust the health check
	// before distrusting the data.
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("valvebilling: cannot reach Redis: %w", err)
	}
	return &Module{cfg: cfg, rdb: rdb, prices: prices}, nil
}

// Enabled reports whether billing runs. It is safe on a nil receiver, which is
// what makes the disabled case free of nil checks at every call site.
func (m *Module) Enabled() bool { return m != nil }

// Close releases the Redis connection. Safe on a nil receiver.
func (m *Module) Close() error {
	if m == nil {
		return nil
	}
	return m.rdb.Close()
}

// HashKey derives the Redis identifier for an API key.
func (m *Module) HashKey(apiKey string) (string, error) {
	if m == nil {
		return "", errDisabled("HashKey")
	}
	return HashAPIKey(m.cfg.Pepper, apiKey)
}

// ResolveCost prices one call. See cost.go for the three tiers and the three
// hazards.
func (m *Module) ResolveCost(chainID int64, method, tokenAddress string) (Cost, error) {
	if m == nil {
		return Cost{}, errDisabled("ResolveCost")
	}
	return m.prices.Resolve(chainID, method, tokenAddress), nil
}

// Authorize runs the metering decision inside Redis.
func (m *Module) Authorize(ctx context.Context, in AuthorizeInput) (Verdict, error) {
	if m == nil {
		return Verdict{}, errDisabled("Authorize")
	}
	return Authorize(ctx, m.rdb, in)
}

// Capture debits the cost after the upstream answered. It is a separate step
// from Authorize and must stay one — see capture.go.
func (m *Module) Capture(ctx context.Context, accountID string, cost *big.Int) error {
	if m == nil {
		return errDisabled("Capture")
	}
	return Capture(ctx, m.rdb, accountID, cost)
}

// Prices exposes the table so a caller can refresh it from Postgres on its own
// schedule. The module deliberately owns no timer: what refreshes pricing, and
// how often, is the host's decision, and a package that started its own
// goroutine would be doing something the flag could not fully switch off.
func (m *Module) Prices() *PriceTable {
	if m == nil {
		return nil
	}
	return m.prices
}

// errDisabled is returned when a disabled module is used anyway. It is an
// error rather than a neutral value on purpose: answering "allowed" would bill
// nobody while looking healthy, and answering "denied" would break a
// deployment that never wanted billing. Both are silent. A caller that reaches
// here has skipped its Enabled() check, and that is a bug worth seeing.
func errDisabled(op string) error {
	return fmt.Errorf("valvebilling: %s called on a disabled module; test Enabled() first", op)
}
