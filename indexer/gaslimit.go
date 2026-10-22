package indexer

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog"
)

// logStaticGas is LOG0's static gas cost, the cheapest a log can be. A block
// can therefore hold at most gasLimit/logStaticGas logs, which is the
// protocol's own ceiling on how many notifications one block can produce.
const logStaticGas = 375

// DefaultFallbackGasLimit stands in for a network's gas limit until its
// first head arrives (and forever on a network whose heads carry none).
const DefaultFallbackGasLimit uint64 = 60_000_000

// GasLimitCeiling bounds what any header may set. It stops a broken upstream
// header from sizing every queue on a network to an absurd cap.
const GasLimitCeiling uint64 = 1_000_000_000

// GasLimitTracker holds the latest header gasLimit per network. A gas limit
// moves at most about 1/1024 per block, so the latest header is enough.
type GasLimitTracker struct {
	fallback uint64
	logger   *zerolog.Logger

	limits  sync.Map // networkId -> *atomic.Uint64
	clamped sync.Map // networkId -> struct{}, warned once
}

// NewGasLimitTracker returns a tracker that answers fallback until a
// network's first head. fallback is clamped to GasLimitCeiling.
func NewGasLimitTracker(fallback uint64, logger *zerolog.Logger) *GasLimitTracker {
	if fallback == 0 {
		fallback = DefaultFallbackGasLimit
	}
	if fallback > GasLimitCeiling {
		fallback = GasLimitCeiling
	}
	if logger == nil {
		nop := zerolog.Nop()
		logger = &nop
	}
	return &GasLimitTracker{fallback: fallback, logger: logger}
}

// Observe records the "gasLimit" of a newHeads payload. A payload without a
// parseable hex gasLimit leaves the value unchanged.
func (t *GasLimitTracker) Observe(networkID string, headerPayload json.RawMessage) {
	var h struct {
		GasLimit *string `json:"gasLimit"`
	}
	if json.Unmarshal(headerPayload, &h) != nil || h.GasLimit == nil {
		return
	}
	s := *h.GasLimit
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return
	}
	v, err := strconv.ParseUint(s[2:], 16, 64)
	if err != nil || v == 0 {
		return
	}
	if v > GasLimitCeiling {
		if _, warned := t.clamped.LoadOrStore(networkID, struct{}{}); !warned {
			t.logger.Warn().Str("networkId", networkID).Uint64("gasLimit", v).
				Uint64("ceiling", GasLimitCeiling).
				Msg("header gasLimit above ceiling; clamping notification queue cap")
		}
		v = GasLimitCeiling
	}
	cur, _ := t.limits.LoadOrStore(networkID, new(atomic.Uint64))
	cur.(*atomic.Uint64).Store(v)
}

// GasLimit returns the network's latest gas limit, else the fallback.
func (t *GasLimitTracker) GasLimit(networkID string) uint64 {
	if cur, ok := t.limits.Load(networkID); ok {
		if v := cur.(*atomic.Uint64).Load(); v != 0 {
			return v
		}
	}
	return t.fallback
}

// MaxLogsPerBlock is ceil(gasLimit / 375): the most logs one block can hold.
func MaxLogsPerBlock(gasLimit uint64) int {
	return int((gasLimit + logStaticGas - 1) / logStaticGas)
}
