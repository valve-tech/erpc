package consensus

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureExporter records every line handed to the misbehavior exporter so a
// test can inspect the exported record instead of reading a file.
type captureExporter struct {
	mu       sync.Mutex
	lines    [][]byte
	methods  []string
	networks []string
	failWith error
}

func (c *captureExporter) AppendWithMetadata(line []byte, method string, networkId string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, append([]byte(nil), line...))
	c.methods = append(c.methods, method)
	c.networks = append(c.networks, networkId)
	return c.failWith
}

func (c *captureExporter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lines)
}

func (c *captureExporter) record(t *testing.T, i int) misbehaviorRecord {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Greater(t, len(c.lines), i, "exporter did not receive record %d", i)
	var rec misbehaviorRecord
	require.NoError(t, json.Unmarshal(c.lines[i], &rec))
	return rec
}

func punishTestLabels() metricsLabels {
	return metricsLabels{
		method:      "eth_getLogs",
		category:    "eth_getLogs",
		networkId:   "evm:123",
		projectId:   "punish-proj",
		userId:      "n/a",
		agentName:   "n/a",
		finalityStr: "unfinalized",
		finality:    common.DataFinalityStateUnfinalized,
	}
}

func newPunishExecutor(t *testing.T, cfg *config, exp misbehaviorExporter) *executor {
	t.Helper()
	lg := zerolog.New(zerolog.NewTestWriter(t))
	return &executor{
		consensusPolicy: &consensusPolicy{
			config:                          cfg,
			logger:                          &lg,
			misbehavingUpstreamsLimiter:     &sync.Map{},
			misbehavingUpstreamsSitoutTimer: &sync.Map{},
			disputeLogLevel:                 zerolog.WarnLevel,
			exporter:                        exp,
		},
	}
}

// resultFromUpstreamTagged builds an execResult whose NormalizedResponse also
// carries the upstream, so buildMisbehaviorRecord can resolve winner.UpstreamID.
func resultWithResponseUpstream(t *testing.T, ups common.Upstream, value string, index int) *execResult {
	t.Helper()
	r := resultFrom(t, ups, value, index)
	r.Result = r.Result.SetUpstream(ups)
	return r
}

// TestShouldPunishUpstream pins every gate that stands between a detected
// disagreement and an actual punishment.
func TestShouldPunishUpstream(t *testing.T) {
	t.Parallel()

	group := func(count int) *responseGroup { return &responseGroup{Count: count} }
	analysisWith := func(valid int) *consensusAnalysis { return &consensusAnalysis{validParticipants: valid} }

	cases := []struct {
		name  string
		cfg   *common.PunishMisbehaviorConfig
		group *responseGroup
		anal  *consensusAnalysis
		want  bool
	}{
		{
			name:  "punishment not configured",
			cfg:   nil,
			group: group(3), anal: analysisWith(3), want: false,
		},
		{
			name:  "dispute threshold zero disables punishment",
			cfg:   &common.PunishMisbehaviorConfig{DisputeThreshold: 0, DisputeWindow: common.Duration(time.Hour)},
			group: group(3), anal: analysisWith(3), want: false,
		},
		{
			name:  "zero dispute window disables punishment",
			cfg:   &common.PunishMisbehaviorConfig{DisputeThreshold: 2, DisputeWindow: common.Duration(0)},
			group: group(3), anal: analysisWith(3), want: false,
		},
		{
			name:  "negative dispute window disables punishment",
			cfg:   &common.PunishMisbehaviorConfig{DisputeThreshold: 2, DisputeWindow: common.Duration(-time.Second)},
			group: group(3), anal: analysisWith(3), want: false,
		},
		{
			name:  "clear majority punishes",
			cfg:   &common.PunishMisbehaviorConfig{DisputeThreshold: 2, DisputeWindow: common.Duration(time.Hour)},
			group: group(2), anal: analysisWith(3), want: true,
		},
		{
			name:  "exact half is not a majority",
			cfg:   &common.PunishMisbehaviorConfig{DisputeThreshold: 2, DisputeWindow: common.Duration(time.Hour)},
			group: group(2), anal: analysisWith(4), want: false,
		},
		{
			name:  "minority consensus group never punishes",
			cfg:   &common.PunishMisbehaviorConfig{DisputeThreshold: 2, DisputeWindow: common.Duration(time.Hour)},
			group: group(1), anal: analysisWith(5), want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPunishExecutor(t, &config{punishMisbehavior: tc.cfg}, nil)
			lg := zerolog.Nop()
			assert.Equal(t, tc.want, e.shouldPunishUpstream(&lg, tc.group, tc.anal))
		})
	}
}

// TestCreateRateLimiter pins the token budget: an upstream gets exactly
// DisputeThreshold free disagreements per window, the limiter is cached per
// upstream so the budget is not silently reset on the next round, and
// different upstreams get independent budgets.
func TestCreateRateLimiter(t *testing.T) {
	t.Parallel()
	lg := zerolog.Nop()

	t.Run("burst equals dispute threshold", func(t *testing.T) {
		e := newPunishExecutor(t, &config{punishMisbehavior: &common.PunishMisbehaviorConfig{
			DisputeThreshold: 3,
			DisputeWindow:    common.Duration(time.Hour),
		}}, nil)

		lim := e.createRateLimiter(&lg, "ups-a")
		require.NotNil(t, lim)
		assert.Equal(t, 3, lim.Burst(), "burst must equal the configured dispute threshold")

		for i := 0; i < 3; i++ {
			assert.True(t, lim.Allow(), "disagreement %d must stay inside the free budget", i+1)
		}
		assert.False(t, lim.Allow(), "the disagreement past the threshold must be denied")
	})

	t.Run("same upstream reuses the same limiter and its consumed budget", func(t *testing.T) {
		e := newPunishExecutor(t, &config{punishMisbehavior: &common.PunishMisbehaviorConfig{
			DisputeThreshold: 1,
			DisputeWindow:    common.Duration(time.Hour),
		}}, nil)

		first := e.createRateLimiter(&lg, "ups-a")
		require.True(t, first.Allow())

		second := e.createRateLimiter(&lg, "ups-a")
		assert.Same(t, first, second, "a second round must reuse the cached limiter")
		assert.False(t, second.Allow(), "the cached limiter must remember its spent budget")
	})

	t.Run("different upstreams get independent budgets", func(t *testing.T) {
		e := newPunishExecutor(t, &config{punishMisbehavior: &common.PunishMisbehaviorConfig{
			DisputeThreshold: 1,
			DisputeWindow:    common.Duration(time.Hour),
		}}, nil)

		a := e.createRateLimiter(&lg, "ups-a")
		b := e.createRateLimiter(&lg, "ups-b")
		assert.NotSame(t, a, b)
		require.True(t, a.Allow())
		assert.True(t, b.Allow(), "one upstream's disagreement must not spend another's budget")
	})

	t.Run("zero threshold still yields a usable one-token limiter", func(t *testing.T) {
		e := newPunishExecutor(t, &config{punishMisbehavior: &common.PunishMisbehaviorConfig{
			DisputeThreshold: 0,
			DisputeWindow:    common.Duration(time.Hour),
		}}, nil)

		lim := e.createRateLimiter(&lg, "ups-a")
		require.NotNil(t, lim)
		assert.Equal(t, 1, lim.Burst(), "burst must never drop below one")
	})
}

// TestHandleMisbehavingUpstream pins the sit-out lifecycle: cordon once,
// count once, ignore a re-entry while the upstream is already sitting out,
// and uncordon when the penalty expires.
func TestHandleMisbehavingUpstream(t *testing.T) {
	lg := zerolog.Nop()
	labels := punishTestLabels()
	labels.projectId = "punish-sitout"

	e := newPunishExecutor(t, &config{punishMisbehavior: &common.PunishMisbehaviorConfig{
		DisputeThreshold: 1,
		DisputeWindow:    common.Duration(time.Hour),
		SitOutPenalty:    common.Duration(150 * time.Millisecond),
	}}, nil)

	ups := common.NewFakeUpstream("ups-punished")
	fake := ups.(*common.FakeUpstream)

	counter := telemetry.MetricConsensusUpstreamPunished.WithLabelValues(
		labels.projectId, labels.networkId, "ups-punished", labels.userId, labels.agentName)
	before := testutil.ToFloat64(counter)

	e.handleMisbehavingUpstream(&lg, ups, "ups-punished", labels)

	reason, cordoned := fake.CordonedReason()
	require.True(t, cordoned, "a punished upstream must be cordoned")
	assert.Equal(t, "misbehaving in consensus", reason)
	assert.Equal(t, before+1, testutil.ToFloat64(counter), "punishment must be counted exactly once")

	// A second punishment while the upstream is already sitting out must be
	// ignored — otherwise every later round restarts the penalty timer.
	e.handleMisbehavingUpstream(&lg, ups, "ups-punished", labels)
	assert.Equal(t, before+1, testutil.ToFloat64(counter), "re-entry during sit-out must not be counted")
	_, stillCordoned := fake.CordonedReason()
	assert.True(t, stillCordoned)

	require.Eventually(t, func() bool {
		_, c := fake.CordonedReason()
		return !c
	}, 5*time.Second, 10*time.Millisecond, "the sit-out penalty must expire and uncordon the upstream")

	require.Eventually(t, func() bool {
		_, present := e.misbehavingUpstreamsSitoutTimer.Load("ups-punished")
		return !present
	}, 5*time.Second, 10*time.Millisecond, "the sit-out entry must be removed so the upstream can be punished again")
}

// TestBuildMisbehaviorRecord pins the exported JSONL shape. The record is what
// an operator reads after the fact, so every field a dispute investigation
// needs must be present, not merely non-empty.
func TestBuildMisbehaviorRecord(t *testing.T) {
	t.Parallel()

	cfg := &config{
		maxParticipants:       3,
		agreementThreshold:    2,
		disputeBehavior:       common.ConsensusDisputeBehaviorReturnError,
		preferNonEmpty:        true,
		preferLargerResponses: true,
		ignoreFields:          map[string][]string{"eth_getLogs": {"logIndex"}},
	}

	u1 := common.NewFakeUpstream("ups-1")
	u2 := common.NewFakeUpstream("ups-2")
	u3 := common.NewFakeUpstream("ups-3")
	u3.Config().VendorName = "acme"

	r1 := resultWithResponseUpstream(t, u1, "0xaa", 0)
	r2 := resultWithResponseUpstream(t, u2, "0xaa", 1)
	r3 := resultWithResponseUpstream(t, u3, "0xbb", 2)

	analysis := analyze(cfg, []*execResult{r1, r2, r3})
	consensusGroup := analysis.groups[r1.CachedHash]
	require.NotNil(t, consensusGroup)

	participants := []participantInfo{
		{upstreamId: "ups-1", upstream: u1, responseType: ResponseTypeNonEmpty, responseHash: r1.CachedHash, responseSize: r1.CachedResponseSize, responseBody: []byte(`"0xaa"`), agreesWithConsensus: true},
		{upstreamId: "ups-2", upstream: u2, responseType: ResponseTypeNonEmpty, responseHash: r2.CachedHash, responseSize: r2.CachedResponseSize, responseBody: []byte(`"0xaa"`), agreesWithConsensus: true},
		{upstreamId: "ups-3", upstream: u3, responseType: ResponseTypeNonEmpty, responseHash: r3.CachedHash, responseSize: r3.CachedResponseSize, responseBody: []byte(`"0xbb"`), agreesWithConsensus: false},
	}

	e := newPunishExecutor(t, cfg, nil)
	labels := punishTestLabels()
	req := newTestRequest()

	raw, err := e.buildMisbehaviorRecord(labels, req, &slotResult{Result: r1.Result}, analysis, consensusGroup, participants)
	require.NoError(t, err)

	var rec misbehaviorRecord
	require.NoError(t, json.Unmarshal(raw, &rec))

	assert.Equal(t, "punish-proj", rec.ProjectID)
	assert.Equal(t, "evm:123", rec.NetworkID)
	assert.Equal(t, "eth_getLogs", rec.Method)
	assert.Equal(t, "unfinalized", rec.Finality)
	assert.Greater(t, rec.TimestampMs, int64(0))

	// The request must be replayable from the record.
	var reqObj map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Request, &reqObj))
	assert.Equal(t, "eth_getLogs", reqObj["method"])

	// Policy snapshot: an investigator must see the thresholds in force.
	assert.Equal(t, 3, rec.Policy.MaxParticipants)
	assert.Equal(t, 2, rec.Policy.AgreementThreshold)
	assert.Equal(t, common.ConsensusDisputeBehaviorReturnError, rec.Policy.DisputeBehavior)
	assert.True(t, rec.Policy.PreferNonEmpty)
	assert.True(t, rec.Policy.PreferLargerResponses)
	assert.Equal(t, map[string][]string{"eth_getLogs": {"logIndex"}}, rec.Policy.IgnoreFields)

	// Winner snapshot: which upstream's answer was actually served.
	assert.Equal(t, "non_empty", rec.Winner.ResponseType)
	assert.Equal(t, "ups-1", rec.Winner.UpstreamID)
	assert.NotEmpty(t, rec.Winner.Hash)
	assert.Greater(t, rec.Winner.Size, 0)

	// Analysis snapshot: the vote split.
	assert.Equal(t, 3, rec.Analysis.TotalParticipants)
	assert.Equal(t, 3, rec.Analysis.ValidParticipants)
	assert.Len(t, rec.Analysis.Groups, 2)
	require.NotNil(t, rec.Analysis.BestByCount)
	assert.Equal(t, 2, rec.Analysis.BestByCount.Count)
	assert.Equal(t, consensusGroup.Hash, rec.Analysis.BestByCount.Hash)

	// Participants: every upstream plus its body, and the vendor of the
	// dissenter (the field a vendor-level investigation starts from).
	require.Len(t, rec.Participants, 3)
	byId := map[string]participantSnapshot{}
	for _, p := range rec.Participants {
		byId[p.UpstreamID] = p
	}
	require.Contains(t, byId, "ups-3")
	assert.Equal(t, "acme", byId["ups-3"].Vendor)
	assert.JSONEq(t, `"0xbb"`, string(byId["ups-3"].Response))
	assert.JSONEq(t, `"0xaa"`, string(byId["ups-1"].Response))
	assert.Equal(t, r3.CachedHash, byId["ups-3"].ResponseHash)
	assert.NotEqual(t, byId["ups-1"].ResponseHash, byId["ups-3"].ResponseHash,
		"the dissenter's hash must differ from the consensus hash")
}

// TestBuildMisbehaviorRecord_ErrorWinnerAndErrorParticipant covers the two
// error-shaped branches: a winner that is an error, and a participant whose
// contribution is an error summary rather than a body.
func TestBuildMisbehaviorRecord_ErrorWinnerAndErrorParticipant(t *testing.T) {
	t.Parallel()

	cfg := &config{maxParticipants: 2, agreementThreshold: 2}
	u1 := common.NewFakeUpstream("ups-1")
	u2 := common.NewFakeUpstream("ups-2")

	boom := common.NewErrEndpointServerSideException(nil, nil, 500)
	r1 := errorFrom(u1, boom, 0)
	r2 := resultWithResponseUpstream(t, u2, "0xaa", 1)
	analysis := analyze(cfg, []*execResult{r1, r2})

	e := newPunishExecutor(t, cfg, nil)
	participants := []participantInfo{
		{upstreamId: "ups-1", upstream: u1, responseType: ResponseTypeInfrastructureError, errorMessage: common.ErrorSummary(boom)},
		{upstreamId: "ups-2", upstream: u2, responseType: ResponseTypeNonEmpty, responseBody: []byte(`"0xaa"`)},
	}

	raw, err := e.buildMisbehaviorRecord(punishTestLabels(), newTestRequest(),
		&slotResult{Error: boom}, analysis, analysis.groups[r2.CachedHash], participants)
	require.NoError(t, err)

	var rec misbehaviorRecord
	require.NoError(t, json.Unmarshal(raw, &rec))

	assert.Equal(t, "consensus_error", rec.Winner.ResponseType, "an error winner must be labelled as such")
	assert.NotEmpty(t, rec.Winner.Hash, "the error winner must still carry a hash for grouping")

	byId := map[string]participantSnapshot{}
	for _, p := range rec.Participants {
		byId[p.UpstreamID] = p
	}
	require.Contains(t, byId, "ups-1")
	assert.NotEmpty(t, byId["ups-1"].Error, "an erroring participant must carry its error summary")
	assert.Empty(t, byId["ups-1"].Response)
	assert.JSONEq(t, `"0xaa"`, string(byId["ups-2"].Response))
}

// TestTrackAndPunish_RecordsExportsAndPunishes drives the whole punishment
// path end to end: two upstreams agree, one dissents. The dissenter must be
// recorded on its health tracker, counted in the misbehavior metric, exported
// once, and — only after its free budget is spent — cordoned.
func TestTrackAndPunish_RecordsExportsAndPunishes(t *testing.T) {
	cfg := &config{
		maxParticipants:    3,
		agreementThreshold: 2,
		punishMisbehavior: &common.PunishMisbehaviorConfig{
			DisputeThreshold: 1,
			DisputeWindow:    common.Duration(time.Hour),
			SitOutPenalty:    common.Duration(time.Hour),
		},
	}
	exp := &captureExporter{}
	e := newPunishExecutor(t, cfg, exp)

	labels := punishTestLabels()
	labels.projectId = "punish-e2e"

	u1 := common.NewFakeUpstream("e2e-1")
	u2 := common.NewFakeUpstream("e2e-2")
	u3 := common.NewFakeUpstream("e2e-3")
	dissenter := u3.(*common.FakeUpstream)

	detected := telemetry.MetricConsensusMisbehaviorDetected.WithLabelValues(
		labels.projectId, labels.networkId, "e2e-3", labels.category, labels.finalityStr,
		ResponseTypeNonEmpty.String(), "false", labels.userId, labels.agentName)
	detectedBefore := testutil.ToFloat64(detected)

	round := func() {
		r1 := resultWithResponseUpstream(t, u1, "0xaa", 0)
		r2 := resultWithResponseUpstream(t, u2, "0xaa", 1)
		r3 := resultWithResponseUpstream(t, u3, "0xbb", 2)
		analysis := analyze(cfg, []*execResult{r1, r2, r3})
		e.trackAndPunishMisbehavingUpstreams(e.logger, newTestRequest(), labels,
			&slotResult{Result: r1.Result}, analysis)
	}

	round()

	// The dissenter, and only the dissenter, is recorded as misbehaving.
	require.Equal(t, 1, u3.Tracker().(*common.FakeHealthTracker).MisbehaviorCount)
	assert.Equal(t, 0, u1.Tracker().(*common.FakeHealthTracker).MisbehaviorCount)
	assert.Equal(t, 0, u2.Tracker().(*common.FakeHealthTracker).MisbehaviorCount)
	assert.Equal(t, detectedBefore+1, testutil.ToFloat64(detected))

	// One export, tagged with the method and network so the file/S3 exporter
	// can partition it.
	require.Equal(t, 1, exp.count())
	assert.Equal(t, "eth_getLogs", exp.methods[0])
	assert.Equal(t, "evm:123", exp.networks[0])
	rec := exp.record(t, 0)
	assert.Equal(t, "punish-e2e", rec.ProjectID)
	require.Len(t, rec.Participants, 3)

	// The first disagreement sits inside the free budget (DisputeThreshold=1),
	// so the upstream keeps serving.
	_, cordoned := dissenter.CordonedReason()
	require.False(t, cordoned, "the first disagreement must not cordon the upstream")

	// The second exhausts the budget and triggers the sit-out.
	round()
	reason, cordoned := dissenter.CordonedReason()
	assert.True(t, cordoned, "the disagreement past the threshold must cordon the upstream")
	assert.Equal(t, "misbehaving in consensus", reason)
	assert.Equal(t, 2, u3.Tracker().(*common.FakeHealthTracker).MisbehaviorCount)
	assert.Equal(t, 2, exp.count(), "each round with a dissenter exports one record")
}

// TestTrackAndPunish_NoPunishmentWhenUnconfigured shows the punishment gate is
// what stops the cordon: the same disagreement, with punishMisbehavior unset,
// is still tracked and exported but never cordons.
func TestTrackAndPunish_NoPunishmentWhenUnconfigured(t *testing.T) {
	cfg := &config{maxParticipants: 3, agreementThreshold: 2}
	exp := &captureExporter{}
	e := newPunishExecutor(t, cfg, exp)

	labels := punishTestLabels()
	labels.projectId = "punish-off"

	u1 := common.NewFakeUpstream("off-1")
	u2 := common.NewFakeUpstream("off-2")
	u3 := common.NewFakeUpstream("off-3")

	for i := 0; i < 5; i++ {
		r1 := resultWithResponseUpstream(t, u1, "0xaa", 0)
		r2 := resultWithResponseUpstream(t, u2, "0xaa", 1)
		r3 := resultWithResponseUpstream(t, u3, "0xbb", 2)
		analysis := analyze(cfg, []*execResult{r1, r2, r3})
		e.trackAndPunishMisbehavingUpstreams(e.logger, newTestRequest(), labels,
			&slotResult{Result: r1.Result}, analysis)
	}

	assert.Equal(t, 5, u3.Tracker().(*common.FakeHealthTracker).MisbehaviorCount,
		"misbehavior must still be tracked when punishment is off")
	assert.Equal(t, 5, exp.count())
	_, cordoned := u3.(*common.FakeUpstream).CordonedReason()
	assert.False(t, cordoned, "punishment must stay off when it is not configured")
}

// TestTrackAndPunish_ErrorParticipantIsNotMisbehavior pins the split between a
// wrong answer and a failed one: an upstream that returns an error while the
// rest agree on data is counted as an upstream error, never as misbehavior,
// and is never punished.
func TestTrackAndPunish_ErrorParticipantIsNotMisbehavior(t *testing.T) {
	cfg := &config{
		maxParticipants:    3,
		agreementThreshold: 2,
		punishMisbehavior: &common.PunishMisbehaviorConfig{
			DisputeThreshold: 1,
			DisputeWindow:    common.Duration(time.Hour),
			SitOutPenalty:    common.Duration(time.Hour),
		},
	}
	exp := &captureExporter{}
	e := newPunishExecutor(t, cfg, exp)

	labels := punishTestLabels()
	labels.projectId = "punish-errsplit"

	u1 := common.NewFakeUpstream("err-1")
	u2 := common.NewFakeUpstream("err-2")
	u3 := common.NewFakeUpstream("err-3")

	boom := common.NewErrEndpointServerSideException(nil, nil, 500)
	r1 := resultWithResponseUpstream(t, u1, "0xaa", 0)
	r2 := resultWithResponseUpstream(t, u2, "0xaa", 1)
	r3 := errorFrom(u3, boom, 2)
	analysis := analyze(cfg, []*execResult{r1, r2, r3})

	errCounter := telemetry.MetricConsensusUpstreamErrors.WithLabelValues(
		labels.projectId, labels.networkId, "err-3", labels.category, labels.finalityStr,
		analysis.groups[r3.CachedHash].ResponseType.String(),
		string(common.ErrCodeEndpointServerSideException), labels.userId, labels.agentName)
	errBefore := testutil.ToFloat64(errCounter)

	for i := 0; i < 3; i++ {
		e.trackAndPunishMisbehavingUpstreams(e.logger, newTestRequest(), labels,
			&slotResult{Result: r1.Result}, analysis)
	}

	assert.Equal(t, 0, u3.Tracker().(*common.FakeHealthTracker).MisbehaviorCount,
		"a failing upstream is not a lying upstream")
	assert.Equal(t, errBefore+3, testutil.ToFloat64(errCounter),
		"the failure must be counted as an upstream error instead")
	_, cordoned := u3.(*common.FakeUpstream).CordonedReason()
	assert.False(t, cordoned, "an erroring upstream must never be cordoned by consensus punishment")
	assert.Equal(t, 0, exp.count(), "no data disagreement means nothing to export")
}

// TestTrackAndPunish_SkipsWhenNoValidParticipants covers the guard that keeps
// an all-infrastructure-failure round from punishing anyone.
func TestTrackAndPunish_SkipsWhenNoValidParticipants(t *testing.T) {
	cfg := &config{
		maxParticipants:    2,
		agreementThreshold: 2,
		punishMisbehavior: &common.PunishMisbehaviorConfig{
			DisputeThreshold: 1,
			DisputeWindow:    common.Duration(time.Hour),
			SitOutPenalty:    common.Duration(time.Hour),
		},
	}
	exp := &captureExporter{}
	e := newPunishExecutor(t, cfg, exp)

	u1 := common.NewFakeUpstream("infra-1")
	u2 := common.NewFakeUpstream("infra-2")
	infra := common.NewErrEndpointServerSideException(nil, nil, 500)

	analysis := analyze(cfg, []*execResult{errorFrom(u1, infra, 0), errorFrom(u2, infra, 1)})
	require.Equal(t, 0, analysis.validParticipants, "test setup: both results must be infrastructure errors")

	e.trackAndPunishMisbehavingUpstreams(e.logger, newTestRequest(), punishTestLabels(), nil, analysis)

	assert.Equal(t, 0, exp.count())
	assert.Equal(t, 0, u1.Tracker().(*common.FakeHealthTracker).MisbehaviorCount)
	assert.Equal(t, 0, u2.Tracker().(*common.FakeHealthTracker).MisbehaviorCount)
}

// TestTrackAndPunish_CompositionDisputeInvertsNothing pins the trust-boundary
// guard: when the count-majority was rejected as untrustworthy, the
// quota-tagged dissenters must not be punished for disagreeing with it.
func TestTrackAndPunish_CompositionDisputeInvertsNothing(t *testing.T) {
	cfg := &config{
		maxParticipants:    3,
		agreementThreshold: 2,
		punishMisbehavior: &common.PunishMisbehaviorConfig{
			DisputeThreshold: 1,
			DisputeWindow:    common.Duration(time.Hour),
			SitOutPenalty:    common.Duration(time.Hour),
		},
	}
	exp := &captureExporter{}
	e := newPunishExecutor(t, cfg, exp)

	u1 := common.NewFakeUpstream("comp-1")
	u2 := common.NewFakeUpstream("comp-2")
	u3 := common.NewFakeUpstream("comp-3")

	analysis := analyze(cfg, []*execResult{
		resultWithResponseUpstream(t, u1, "0xaa", 0),
		resultWithResponseUpstream(t, u2, "0xaa", 1),
		resultWithResponseUpstream(t, u3, "0xbb", 2),
	})

	disputeErr := common.NewErrConsensusCompositionDispute("quota not met", nil, nil)
	require.True(t, common.HasErrorCode(disputeErr, common.ErrCodeConsensusCompositionDispute),
		"test setup: the winner error must carry the composition-dispute code")

	e.trackAndPunishMisbehavingUpstreams(e.logger, newTestRequest(), punishTestLabels(),
		&slotResult{Error: disputeErr}, analysis)

	assert.Equal(t, 0, exp.count(), "a composition dispute must export nothing")
	for _, u := range []common.Upstream{u1, u2, u3} {
		assert.Equal(t, 0, u.Tracker().(*common.FakeHealthTracker).MisbehaviorCount,
			"no one is punishable when the majority itself was rejected")
		_, cordoned := u.(*common.FakeUpstream).CordonedReason()
		assert.False(t, cordoned)
	}
}

// TestTrackAndPunish_ExporterFailureIsNotFatal keeps a broken export sink from
// taking the request path down with it.
func TestTrackAndPunish_ExporterFailureIsNotFatal(t *testing.T) {
	cfg := &config{maxParticipants: 3, agreementThreshold: 2}
	exp := &captureExporter{failWith: context.DeadlineExceeded}
	e := newPunishExecutor(t, cfg, exp)

	u1 := common.NewFakeUpstream("fail-1")
	u2 := common.NewFakeUpstream("fail-2")
	u3 := common.NewFakeUpstream("fail-3")

	r1 := resultWithResponseUpstream(t, u1, "0xaa", 0)
	analysis := analyze(cfg, []*execResult{
		r1,
		resultWithResponseUpstream(t, u2, "0xaa", 1),
		resultWithResponseUpstream(t, u3, "0xbb", 2),
	})

	require.NotPanics(t, func() {
		e.trackAndPunishMisbehavingUpstreams(e.logger, newTestRequest(), punishTestLabels(),
			&slotResult{Result: r1.Result}, analysis)
	})

	// The sink was still called — the failure is swallowed, not skipped — and
	// the misbehavior is still recorded on the tracker.
	assert.Equal(t, 1, exp.count())
	assert.Equal(t, 1, u3.Tracker().(*common.FakeHealthTracker).MisbehaviorCount)
}
