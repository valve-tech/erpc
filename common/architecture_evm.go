package common

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	UpstreamTypeEvm UpstreamType = "evm"
)

type EvmUpstream interface {
	Upstream
	EvmGetChainId(ctx context.Context) (string, error)
	EvmIsBlockFinalized(ctx context.Context, blockNumber int64, forceFreshIfStale bool) (bool, error)
	EvmAssertBlockAvailability(ctx context.Context, forMethod string, confidence AvailbilityConfidence, forceFreshIfStale bool, blockNumber int64) (bool, error)
	EvmSyncingState() EvmSyncingState
	EvmStatePoller() EvmStatePoller
	// EvmEffectiveLatestBlock returns the latest block adjusted for the upstream's upper availability bound.
	// If the upstream has a blockAvailability.upper config (e.g., latestBlockMinus: 5), this returns
	// min(latestBlock, upperBound) instead of the raw latest block.
	EvmEffectiveLatestBlock() int64
	// EvmEffectiveFinalizedBlock returns the finalized block adjusted for the upstream's upper availability bound.
	// If the upstream has a blockAvailability.upper config, this returns min(finalizedBlock, upperBound).
	EvmEffectiveFinalizedBlock() int64
	// EvmBlockAvailabilityBounds returns the resolved [min, max] block range this upstream
	// is configured to serve. Returns (math.MinInt64, math.MaxInt64) for unbounded sides.
	EvmBlockAvailabilityBounds() (int64, int64)
}

// EvmStateProvenWriter is the OPTIONAL, separately-asserted surface the
// integrity state prober records proofs through. The proven head is telemetry
// (the state-proven block / proven-lag gauges), never a routing input.
// Deliberately NOT part of EvmUpstream: that interface is implemented outside
// this repo, and widening it broke every existing implementor — the chainId
// suggest-gate silently degraded when its upstream stopped satisfying the
// assertion. Optional capabilities are asserted narrowly, never added to the
// core interface.
type EvmStateProvenWriter interface {
	// EvmSetStateProvenBlock records a successful state proof at a height.
	// Monotonic: a lower value than the current one is ignored.
	EvmSetStateProvenBlock(int64)
}

type AvailbilityConfidence int

const (
	AvailbilityConfidenceBlockHead AvailbilityConfidence = 1
	AvailbilityConfidenceFinalized AvailbilityConfidence = 2
	// NOTE: there is deliberately no "state-proven" confidence. The proven head
	// (see the integrity state prober) advances at probe cadence, so on any
	// chain whose block time is shorter than that cadence it sits structurally
	// behind every honest upstream's claimed head — a routing bound built on it
	// would refuse the entire network's tip traffic. Absence of proof is not
	// evidence; only DISPROOF is, and the prober publishes that as upstream
	// misbehavior on the health tracker for selection policies to act on
	// (misbehaviorRateAbove), not as an availability confidence.
)

func (c AvailbilityConfidence) String() string {
	switch c {
	case AvailbilityConfidenceBlockHead:
		return "blockHead"
	case AvailbilityConfidenceFinalized:
		return "finalizedBlock"
	default:
		return fmt.Sprintf("unknown(%d)", c)
	}
}

func (c AvailbilityConfidence) MarshalYAML() (interface{}, error) {
	return c.String(), nil
}

func (c AvailbilityConfidence) MarshalJSON() ([]byte, error) {
	return SonicCfg.Marshal(c.String())
}

// availbilityConfidences names every value of the type. UnmarshalYAML reads
// this list and matches against each value's own String(), so the parser
// accepts exactly what the printer emits and the two cannot drift.
//
// They used to drift. The parser held its own hand-written table of four
// spellings while String() emitted three, so `stateProven` marshalled out
// and failed to parse back: an operator who dumped the effective config and
// fed it in got "invalid availability confidence: stateProven". Adding a
// value to the const block above is now enough — add it here and both
// directions follow.
var availbilityConfidences = []AvailbilityConfidence{
	AvailbilityConfidenceBlockHead,
	AvailbilityConfidenceFinalized,
}

func (c *AvailbilityConfidence) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)

	names := make([]string, 0, len(availbilityConfidences))
	for _, known := range availbilityConfidences {
		name := known.String()
		// The number is accepted too, because the config surface has
		// always taken it and a dropped quote in YAML reads as one.
		if strings.EqualFold(s, name) || s == strconv.Itoa(int(known)) {
			*c = known
			return nil
		}
		names = append(names, name)
	}

	// Name what is allowed. Rejecting a value without saying what the
	// alternatives are leaves the operator guessing at a closed set.
	return fmt.Errorf("invalid availability confidence %q, expected one of: %s",
		s, strings.Join(names, ", "))
}

type EvmNodeType string

const (
	EvmNodeTypeUnknown EvmNodeType = "unknown"
	EvmNodeTypeFull    EvmNodeType = "full"
	EvmNodeTypeArchive EvmNodeType = "archive"
)

type EvmSyncingState int

const (
	EvmSyncingStateUnknown EvmSyncingState = iota
	EvmSyncingStateSyncing
	EvmSyncingStateNotSyncing
)

func (s EvmSyncingState) String() string {
	switch s {
	case EvmSyncingStateSyncing:
		return "syncing"
	case EvmSyncingStateNotSyncing:
		return "not_syncing"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

type EvmStatePoller interface {
	Bootstrap(ctx context.Context) error
	Poll(ctx context.Context) error
	PollLatestBlockNumber(ctx context.Context) (int64, error)
	PollFinalizedBlockNumber(ctx context.Context) (int64, error)
	PollEarliestBlockNumber(ctx context.Context, probe EvmAvailabilityProbeType, staleness time.Duration) (int64, error)
	SyncingState() EvmSyncingState
	SetSyncingState(state EvmSyncingState)
	LatestBlock() int64
	FinalizedBlock() int64
	IsBlockFinalized(blockNumber int64) (bool, error)
	SuggestFinalizedBlock(blockNumber int64)
	SuggestLatestBlock(blockNumber int64)
	SetNetworkConfig(cfg *NetworkConfig)
	IsObjectNull() bool
	EarliestBlock(probe EvmAvailabilityProbeType) int64
	GetDiagnostics() *EvmStatePollerDiagnostics
}

// EvmStatePollerDiagnostics contains diagnostic information about the state poller
// including block bounds, probe status, and any detection issues.
type EvmStatePollerDiagnostics struct {
	Enabled bool `json:"enabled"`

	// Block head information
	LatestBlock    int64 `json:"latestBlock"`
	FinalizedBlock int64 `json:"finalizedBlock"`

	// Syncing state
	SyncingState      string `json:"syncingState"`
	SkipSyncingCheck  bool   `json:"skipSyncingCheck,omitempty"`
	SyncingCheckError string `json:"syncingCheckError,omitempty"`

	// Latest block detection status
	SkipLatestBlockCheck      bool   `json:"skipLatestBlockCheck,omitempty"`
	LatestBlockFailureCount   int    `json:"latestBlockFailureCount,omitempty"`
	LatestBlockSuccessfulOnce bool   `json:"latestBlockSuccessfulOnce,omitempty"`
	LatestBlockDetectionIssue string `json:"latestBlockDetectionIssue,omitempty"`

	// Finalized block detection status
	SkipFinalizedCheck           bool   `json:"skipFinalizedCheck,omitempty"`
	FinalizedBlockFailureCount   int    `json:"finalizedBlockFailureCount,omitempty"`
	FinalizedBlockSuccessfulOnce bool   `json:"finalizedBlockSuccessfulOnce,omitempty"`
	FinalizedBlockDetectionIssue string `json:"finalizedBlockDetectionIssue,omitempty"`

	// Earliest block bounds per probe type
	EarliestByProbe map[EvmAvailabilityProbeType]*EvmProbeEarliestInfo `json:"earliestByProbe,omitempty"`
}

// EvmProbeEarliestInfo contains information about earliest block detection for a specific probe type
type EvmProbeEarliestInfo struct {
	ProbeType        EvmAvailabilityProbeType `json:"probeType"`
	EarliestBlock    int64                    `json:"earliestBlock"`
	SchedulerRunning bool                     `json:"schedulerRunning,omitempty"`
}
