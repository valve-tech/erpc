package consensus

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuilder_EverySetterLandsOnItsOwnField sets every option to a distinct
// value in one build and checks each one arrived. The builder is thirteen
// near-identical one-line setters, so the failure worth catching is
// cross-wiring: a copy-pasted setter writing a neighbour's field. That loses
// the operator's setting with nothing logged — preferNonEmpty silently off,
// for instance, changes which answer erpc serves. Asserting the fields one at
// a time would not catch it; asserting all of them from one build does.
func TestBuilder_EverySetterLandsOnItsOwnField(t *testing.T) {
	lg := zerolog.Nop()
	punish := &common.PunishMisbehaviorConfig{SitOutPenalty: common.Duration(30 * time.Second)}
	dest := &common.MisbehaviorsDestinationConfig{Type: common.MisbehaviorsDestinationTypeFile}
	ignore := map[string][]string{"eth_getBlockByNumber": {"totalDifficulty"}}
	highest := map[string][]string{"eth_blockNumber": {"result"}}
	required := []*common.ConsensusRequiredParticipant{{Tag: "archive"}}
	onResult := &common.AdaptiveDuration{Base: common.Duration(700 * time.Millisecond)}
	onEmpty := &common.AdaptiveDuration{Base: common.Duration(900 * time.Millisecond)}

	pol := NewConsensusPolicyBuilder().
		WithLogger(&lg).
		WithMaxParticipants(7).
		WithAgreementThreshold(4).
		WithDisputeBehavior(common.ConsensusDisputeBehaviorPreferBlockHeadLeader).
		WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult).
		WithPunishMisbehavior(punish).
		WithMisbehaviorsDestination(dest).
		WithDisputeLogLevel(zerolog.ErrorLevel).
		WithIgnoreFields(ignore).
		WithPreferNonEmpty(true).
		WithPreferLargerResponses(true).
		WithPreferHighestValueFor(highest).
		WithFireAndForget(true).
		WithMaxWaitOnResult(onResult).
		WithMaxWaitOnEmpty(onEmpty).
		WithRequiredParticipants(required).
		Build()

	require.NotNil(t, pol.policy)
	cfg := pol.policy.config

	assert.Equal(t, 7, cfg.maxParticipants)
	assert.Equal(t, 4, cfg.agreementThreshold)
	assert.Equal(t, common.ConsensusDisputeBehaviorPreferBlockHeadLeader, cfg.disputeBehavior)
	assert.Equal(t, common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult, cfg.lowParticipantsBehavior)
	assert.Same(t, punish, cfg.punishMisbehavior)
	assert.Same(t, dest, cfg.misbehaviorsDestination)
	assert.Equal(t, zerolog.ErrorLevel, cfg.disputeLogLevel)
	assert.Equal(t, ignore, cfg.ignoreFields)
	assert.True(t, cfg.preferNonEmpty)
	assert.True(t, cfg.preferLargerResponses)
	assert.Equal(t, highest, cfg.preferHighestValueFor)
	assert.True(t, cfg.fireAndForget)
	assert.Same(t, onResult, cfg.maxWaitOnResult)
	assert.Same(t, onEmpty, cfg.maxWaitOnEmpty)
	assert.Equal(t, required, cfg.requiredParticipants)

	assert.Equal(t, zerolog.ErrorLevel, pol.policy.disputeLogLevel,
		"the runtime policy must carry the operator's dispute log level, not the default")
}

// TestNewConsensus_CarriesTheWholeOperatorConfig covers the real production
// path from YAML config to runtime policy. Every consensus setting an operator
// writes passes through here, and the optional pointer fields
// (preferNonEmpty, preferLargerResponses) are exactly where a setting goes
// missing without a word in the logs.
func TestNewConsensus_CarriesTheWholeOperatorConfig(t *testing.T) {
	lg := zerolog.Nop()
	yes, no := true, false
	cfg := &common.ConsensusPolicyConfig{
		MaxParticipants:         6,
		AgreementThreshold:      3,
		DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		PunishMisbehavior:       &common.PunishMisbehaviorConfig{SitOutPenalty: common.Duration(time.Minute)},
		DisputeLogLevel:         "error",
		IgnoreFields:            map[string][]string{"eth_getBlockByNumber": {"totalDifficulty"}},
		PreferNonEmpty:          &yes,
		PreferLargerResponses:   &no,
		PreferHighestValueFor:   map[string][]string{"eth_blockNumber": {"result"}},
		FireAndForget:           true,
		MaxWaitOnResult:         &common.AdaptiveDuration{Base: common.Duration(200 * time.Millisecond)},
		MaxWaitOnEmpty:          &common.AdaptiveDuration{Base: common.Duration(300 * time.Millisecond)},
		RequiredParticipants:    []*common.ConsensusRequiredParticipant{{Tag: "archive", MinParticipants: 2}},
	}

	c, err := NewConsensus(cfg, &lg)
	require.NoError(t, err)
	require.NotNil(t, c.policy)
	got := c.policy.config

	assert.Equal(t, 6, got.maxParticipants)
	assert.Equal(t, 3, got.agreementThreshold)
	assert.Equal(t, common.ConsensusDisputeBehaviorAcceptMostCommonValidResult, got.disputeBehavior)
	assert.Equal(t, common.ConsensusLowParticipantsBehaviorReturnError, got.lowParticipantsBehavior)
	assert.Same(t, cfg.PunishMisbehavior, got.punishMisbehavior)
	assert.Equal(t, zerolog.ErrorLevel, c.policy.disputeLogLevel)
	assert.Equal(t, cfg.IgnoreFields, got.ignoreFields)
	assert.True(t, got.preferNonEmpty)
	assert.False(t, got.preferLargerResponses,
		"an explicit false must stay false, not fall back to a default")
	assert.Equal(t, cfg.PreferHighestValueFor, got.preferHighestValueFor)
	assert.True(t, got.fireAndForget)
	assert.Same(t, cfg.MaxWaitOnResult, got.maxWaitOnResult)
	assert.Same(t, cfg.MaxWaitOnEmpty, got.maxWaitOnEmpty)
	assert.Equal(t, cfg.RequiredParticipants, got.requiredParticipants)
}

// TestNewConsensus_OptionalSettings covers what an operator gets when they
// leave the optional settings out or write one wrong.
func TestNewConsensus_OptionalSettings(t *testing.T) {
	lg := zerolog.Nop()

	t.Run("a nil config is refused", func(t *testing.T) {
		c, err := NewConsensus(nil, &lg)
		require.Error(t, err)
		assert.Nil(t, c)
	})

	t.Run("omitted preferences stay off and no exporter is built", func(t *testing.T) {
		c, err := NewConsensus(&common.ConsensusPolicyConfig{
			MaxParticipants:    3,
			AgreementThreshold: 2,
		}, &lg)
		require.NoError(t, err)

		assert.False(t, c.policy.config.preferNonEmpty)
		assert.False(t, c.policy.config.preferLargerResponses)
		assert.Nil(t, c.policy.config.ignoreFields)
		assert.Nil(t, c.policy.config.preferHighestValueFor)
		assert.Equal(t, zerolog.WarnLevel, c.policy.disputeLogLevel)
		assert.Nil(t, c.policy.exporter)
	})

	t.Run("an unparsable dispute log level keeps the default", func(t *testing.T) {
		// A typo in the config must not silence dispute logging, which is
		// what a level of 0 (debug) would do behind a production log filter.
		c, err := NewConsensus(&common.ConsensusPolicyConfig{
			MaxParticipants:    3,
			AgreementThreshold: 2,
			DisputeLogLevel:    "verbose",
		}, &lg)
		require.NoError(t, err)
		assert.Equal(t, zerolog.WarnLevel, c.policy.disputeLogLevel)
	})

	t.Run("a valid dispute log level is applied", func(t *testing.T) {
		c, err := NewConsensus(&common.ConsensusPolicyConfig{
			MaxParticipants:    3,
			AgreementThreshold: 2,
			DisputeLogLevel:    "info",
		}, &lg)
		require.NoError(t, err)
		assert.Equal(t, zerolog.InfoLevel, c.policy.disputeLogLevel)
	})
}

// TestConsensus_RunWithoutAPolicyStillServes: a nil *Consensus means the
// network has no consensus policy configured, and the request must go straight
// through rather than fail.
func TestConsensus_RunWithoutAPolicyStillServes(t *testing.T) {
	var c *Consensus

	called := false
	resp, err := c.Run(context.Background(), nil,
		func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			called = true
			return nil, nil
		})

	require.NoError(t, err)
	assert.Nil(t, resp)
	assert.True(t, called, "with no policy the inner call must still run")
}

// TestBuilder_DisputeLogLevelDefaultsToWarn pins the default. Level 0 is
// zerolog's Debug, so leaving it unset would bury every dispute below a
// production log level and hide the exact events an operator watches for.
func TestBuilder_DisputeLogLevelDefaultsToWarn(t *testing.T) {
	lg := zerolog.Nop()
	pol := NewConsensusPolicyBuilder().
		WithLogger(&lg).
		WithMaxParticipants(3).
		WithAgreementThreshold(2).
		Build()

	assert.Equal(t, zerolog.WarnLevel, pol.policy.disputeLogLevel)
	assert.Nil(t, pol.policy.exporter, "no destination configured means no exporter")
}
