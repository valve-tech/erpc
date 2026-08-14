package upstream

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
)

// What upstream bootstrap owes a chain family, proved on the real gate rather
// than on btc: a fake family isolates detectChainFamilyFeatures from anything
// bitcoind-specific, so these tests keep answering for the next chain added.

const fakeFamilyName = common.NetworkArchitecture("fakechain")

// fakeFamily is a minimal common.ChainFamily whose probe answer is canned.
type fakeFamily struct {
	probe common.ChainProbe
	// gotEndpoint records what NewProbeCaller was asked to build for.
	gotEndpoint string
	// rejectChain makes ValidateNetworkId refuse everything.
	rejectChain bool
}

func (f *fakeFamily) Family() common.NetworkArchitecture { return fakeFamilyName }
func (f *fakeFamily) Transport() common.ChainTransport   { return common.TransportJsonRpc }
func (f *fakeFamily) ValidateNetworkId(body string) bool {
	return !f.rejectChain && body != "" && !strings.Contains(body, ":")
}
func (f *fakeFamily) Probe(context.Context, common.ProbeCaller) common.ChainProbe { return f.probe }
func (f *fakeFamily) Classify(common.ClassifyInput) common.RotateVerdict {
	return common.VerdictServe
}

// probingFakeFamily also provides a probe transport, which is what a family
// must do to be routable.
type probingFakeFamily struct{ *fakeFamily }

func (f *probingFakeFamily) NewProbeCaller(endpoint string, _ *http.Client) common.ProbeCaller {
	f.gotEndpoint = endpoint
	return nopProbeCaller{}
}

type nopProbeCaller struct{}

func (nopProbeCaller) CallJsonRpc(context.Context, string, []interface{}) ([]byte, error) {
	return nil, nil
}
func (nopProbeCaller) CallREST(context.Context, string, string) (int, []byte, error) {
	return 0, nil, nil
}

// registerFakeFamily installs `f` and removes it again after the test. The
// registries are process-global, so a leaked fake decides the next test's
// answers.
func registerFakeFamily(t *testing.T, f common.ChainFamily) {
	t.Helper()
	if err := common.RegisterChainFamily(f); err != nil {
		t.Fatalf("RegisterChainFamily: %v", err)
	}
	t.Cleanup(func() { common.UnregisterChainFamilyForTest(fakeFamilyName) })
}

func newTestUpstream(t *testing.T, ctx context.Context, cfg *common.UpstreamConfig) *Upstream {
	t.Helper()
	lg := zerolog.Nop()
	return &Upstream{
		ProjectId:      "test",
		appCtx:         ctx,
		logger:         &lg,
		config:         cfg,
		metricsTracker: health.NewTracker(&lg, "test", 2*time.Second),
	}
}

func isFatal(err error) bool {
	var fatal interface{ IsTaskFatal() bool }
	for e := err; e != nil; {
		if f, ok := e.(interface{ IsTaskFatal() bool }); ok {
			fatal = f
			break
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	return fatal != nil && fatal.IsTaskFatal()
}

func TestChainFamilyBootstrap_HealthyUpstreamLearnsItsNetworkId(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fam := &probingFakeFamily{&fakeFamily{probe: common.ChainProbe{
		Liveness: common.ChainHealthy, Tip: 812345, Detail: "height 812345",
	}}}
	registerFakeFamily(t, fam)

	u := newTestUpstream(t, ctx, &common.UpstreamConfig{
		Id: "u1", Type: common.UpstreamType(fakeFamilyName),
		Endpoint: "http://node.localhost:1234", Chain: "mainnet",
	})

	if err := u.detectFeatures(ctx); err != nil {
		t.Fatalf("detectFeatures: %v", err)
	}
	// The networkId is the ONLY thing binding an upstream to a network. Without
	// it the upstream bootstraps and is never selected for anything.
	if got := u.NetworkId(); got != "fakechain:mainnet" {
		t.Fatalf("networkId = %q, want fakechain:mainnet", got)
	}
	if fam.gotEndpoint != "http://node.localhost:1234" {
		t.Fatalf("probe caller was built for %q, want the upstream's endpoint", fam.gotEndpoint)
	}
	if _, cordoned := u.CordonedReason("*"); cordoned {
		t.Fatal("a healthy upstream was cordoned at bootstrap")
	}

	// The bootstrap probe must publish the tip, or the first request ranks an
	// unmeasured pool. A second upstream 45 blocks behind is what makes that
	// observable: head lag is derived from the highest tip in the network, so a
	// lag of 45 can only appear if BOTH probes reached the tracker.
	fam.probe = common.ChainProbe{Liveness: common.ChainHealthy, Tip: 812300}
	behind := newTestUpstream(t, ctx, &common.UpstreamConfig{
		Id: "u2", Type: common.UpstreamType(fakeFamilyName),
		Endpoint: "http://slow.localhost:1234", Chain: "mainnet",
	})
	behind.metricsTracker = u.metricsTracker
	if err := behind.detectFeatures(ctx); err != nil {
		t.Fatalf("detectFeatures for the second upstream: %v", err)
	}
	got := behind.metricsTracker.
		GetUpstreamMethodMetrics(behind, "*", common.DataFinalityStateAll).
		BlockHeadLag.Load()
	if got != 45 {
		t.Fatalf("blockHeadLag = %d for an upstream 45 blocks behind, want 45; "+
			"the bootstrap probe did not reach the health tracker", got)
	}
}

func TestChainFamilyBootstrap_SyncingUpstreamJoinsButIsCordoned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fam := &probingFakeFamily{&fakeFamily{probe: common.ChainProbe{
		Liveness: common.ChainSyncing, Tip: 700000, Detail: "initial block download",
	}}}
	registerFakeFamily(t, fam)

	u := newTestUpstream(t, ctx, &common.UpstreamConfig{
		Id: "u1", Type: common.UpstreamType(fakeFamilyName),
		Endpoint: "http://node.localhost:1234", Chain: "mainnet",
	})

	if err := u.detectFeatures(ctx); err != nil {
		t.Fatalf("a syncing upstream was refused at bootstrap: %v", err)
	}
	// It joins — an operator has to be able to see it and watch its tip climb.
	if got := u.NetworkId(); got != "fakechain:mainnet" {
		t.Fatalf("networkId = %q, want fakechain:mainnet", got)
	}
	// But it takes no traffic.
	reason, cordoned := u.CordonedReason("*")
	if !cordoned {
		t.Fatal("a syncing upstream joined the pool uncordoned; it will serve stale reads")
	}
	if !strings.Contains(reason, "initial block download") {
		t.Fatalf("cordon reason %q does not carry the probe's detail", reason)
	}
}

func TestChainFamilyBootstrap_DownUpstreamIsRefusedButRetryable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fam := &probingFakeFamily{&fakeFamily{probe: common.ChainProbe{
		Liveness: common.ChainDown, Detail: "connection refused",
		Err: context.DeadlineExceeded,
	}}}
	registerFakeFamily(t, fam)

	u := newTestUpstream(t, ctx, &common.UpstreamConfig{
		Id: "u1", Type: common.UpstreamType(fakeFamilyName),
		Endpoint: "http://node.localhost:1234", Chain: "mainnet",
	})

	err := u.detectFeatures(ctx)
	if err == nil {
		t.Fatal("an unreachable node bootstrapped; eRPC would route to it")
	}
	// Retryable on purpose: a refused dial is usually a restart, and the
	// initializer keeps trying. Marking it fatal would need an operator to
	// restart eRPC after every node reboot.
	if isFatal(err) {
		t.Fatalf("an unreachable node produced a FATAL error (%v); it can never self-heal", err)
	}
	if got := u.NetworkId(); got != "n/a" {
		t.Fatalf("networkId = %q for a node that never answered; it must not be routable", got)
	}
}

func TestChainFamilyBootstrap_ConfigErrorsAreFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	healthy := common.ChainProbe{Liveness: common.ChainHealthy, Tip: 1}

	for _, tc := range []struct {
		name   string
		family common.ChainFamily
		cfg    *common.UpstreamConfig
		want   string
	}{
		{
			name:   "no chain named",
			family: &probingFakeFamily{&fakeFamily{probe: healthy}},
			cfg: &common.UpstreamConfig{Id: "u1", Type: common.UpstreamType(fakeFamilyName),
				Endpoint: "http://node.localhost:1234"},
			want: "chain",
		},
		{
			name:   "chain the family rejects",
			family: &probingFakeFamily{&fakeFamily{probe: healthy, rejectChain: true}},
			cfg: &common.UpstreamConfig{Id: "u1", Type: common.UpstreamType(fakeFamilyName),
				Endpoint: "http://node.localhost:1234", Chain: "main:net"},
			want: "rejects",
		},
		{
			name:   "family with no probe transport",
			family: &fakeFamily{probe: healthy},
			cfg: &common.UpstreamConfig{Id: "u1", Type: common.UpstreamType(fakeFamilyName),
				Endpoint: "http://node.localhost:1234", Chain: "mainnet"},
			want: "probe transport",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registerFakeFamily(t, tc.family)
			u := newTestUpstream(t, ctx, tc.cfg)

			err := u.detectFeatures(ctx)
			if err == nil {
				t.Fatal("bootstrap accepted an upstream it cannot route to")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not say what is wrong (want it to mention %q)", err, tc.want)
			}
			// Config cannot fix itself. A retryable error here makes the
			// initializer retry a typo until the process dies.
			if !isFatal(err) {
				t.Fatalf("a config error was reported as retryable: %v", err)
			}
		})
	}
}

func TestChainFamilyBootstrap_UnregisteredTypeIsStillRefused(t *testing.T) {
	// The gate detectFeatures used to be. An upstream type nobody registered
	// must not become routable just because the registry lookup replaced a
	// hardcoded switch.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	u := newTestUpstream(t, ctx, &common.UpstreamConfig{
		Id: "u1", Type: common.UpstreamType("nosuchchain"),
		Endpoint: "http://node.localhost:1234", Chain: "mainnet",
	})

	err := u.detectFeatures(ctx)
	if err == nil {
		t.Fatal("an unregistered upstream type bootstrapped")
	}
	if !strings.Contains(err.Error(), "upstream type not supported") {
		t.Fatalf("error = %v, want it to name the unsupported type", err)
	}
}
