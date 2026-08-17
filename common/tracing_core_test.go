package common

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// InitializeTracing
// ---------------------------------------------------------------------------

// A nil or disabled config must leave tracing off. If it did not, every request
// would build spans and ship them nowhere.
func TestInitializeTracing_DisabledLeavesTracingOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *TracingConfig
	}{
		{"nil config", nil},
		{"enabled false", &TracingConfig{Enabled: false, Protocol: TracingProtocolHttp}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			saveTracingGlobals(t)
			resetTracingInitOnce(t)
			IsTracingEnabled = true
			IsTracingDetailed = true

			logger := zerolog.Nop()
			require.NoError(t, InitializeTracing(ctx, &logger, tc.cfg))

			assert.False(t, IsTracingEnabled, "a disabled config must turn tracing off")
			assert.False(t, IsTracingDetailed, "a disabled config must turn detailed tracing off")
		})
	}
}

// An unknown protocol must surface an error to the operator rather than
// silently starting with no exporter.
func TestInitializeTracing_UnsupportedProtocolReturnsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	saveTracingGlobals(t)
	resetTracingInitOnce(t)
	IsTracingEnabled = false

	logger := zerolog.Nop()
	err := InitializeTracing(ctx, &logger, &TracingConfig{
		Enabled:  true,
		Protocol: TracingProtocol("carrier-pigeon"),
		Endpoint: "127.0.0.1:4318",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported tracing protocol")
	assert.Contains(t, err.Error(), "carrier-pigeon")
	assert.False(t, IsTracingEnabled, "a failed init must not claim tracing is on")
}

// The happy path must set the globals the rest of the process reads, and must
// publish the force-trace matchers so ShouldForceTrace can see them.
func TestInitializeTracing_EnabledSetsGlobalsAndMatchers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	saveTracingGlobals(t)
	resetTracingInitOnce(t)
	IsTracingEnabled = false
	IsTracingDetailed = false
	forceTraceMatchers = nil

	logger := zerolog.Nop()
	matcher := &ForceTraceMatcher{Network: "evm:1", Method: "eth_call"}
	require.NoError(t, InitializeTracing(ctx, &logger, &TracingConfig{
		Enabled:            true,
		Protocol:           TracingProtocolHttp,
		Endpoint:           "127.0.0.1:14318",
		ServiceName:        "erpc-under-test",
		SampleRate:         1.0,
		Detailed:           true,
		ResourceAttributes: map[string]string{"deployment": "test", "skipped": ""},
		ForceTraceMatchers: []*ForceTraceMatcher{matcher},
	}))

	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = ShutdownTracing(shutdownCtx)
	})

	assert.True(t, IsTracingEnabled, "an enabled config must turn tracing on")
	assert.True(t, IsTracingDetailed, "detailed:true must reach IsTracingDetailed")
	assert.NotNil(t, tracer, "the package tracer must be installed")
	assert.NotNil(t, tracerProvider, "the package tracer provider must be installed")
	assert.Equal(t, []*ForceTraceMatcher{matcher}, forceTraceMatchers,
		"configured matchers must reach the package global ShouldForceTrace reads")

	// The matchers really are live, not just stored.
	forced, reason := ShouldForceTrace("evm:1", "eth_call")
	assert.True(t, forced)
	assert.Equal(t, "network:evm:1,method:eth_call", reason)
}

// ShutdownTracing must be safe when tracing was never initialized — the
// shutdown path runs on every exit, including a config-error exit.
func TestShutdownTracing_NoProviderIsNoError(t *testing.T) {
	saveTracingGlobals(t)
	tracerProvider = nil

	require.NoError(t, ShutdownTracing(t.Context()))
}

// ---------------------------------------------------------------------------
// Exporter construction
// ---------------------------------------------------------------------------

// A broken TLS config must fail loudly at construction. Both exporters share
// the shape, so both are checked.
func TestCreateTracingExporters_BrokenTLSFails(t *testing.T) {
	cfg := &TracingConfig{
		Endpoint: "127.0.0.1:14318",
		TLS: &TLSConfig{
			Enabled:  true,
			CertFile: "/nonexistent/cert.pem",
			KeyFile:  "/nonexistent/key.pem",
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	grpcExp, err := createTracingGRPCExporter(ctx, cfg)
	require.Error(t, err, "a missing cert/key pair must fail the gRPC exporter")
	assert.Nil(t, grpcExp)

	httpExp, err := createTracingHTTPExporter(ctx, cfg)
	require.Error(t, err, "a missing cert/key pair must fail the HTTP exporter")
	assert.Nil(t, httpExp)
}

// Headers and an insecure endpoint must build an exporter without error. This
// is the configuration most operators ship behind a local collector.
func TestCreateTracingExporters_InsecureWithHeaders(t *testing.T) {
	cfg := &TracingConfig{
		Endpoint: "127.0.0.1:14318",
		Headers:  map[string]string{"x-api-key": "secret"},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	httpExp, err := createTracingHTTPExporter(ctx, cfg)
	require.NoError(t, err)
	require.NotNil(t, httpExp)
	shutdownExporter(t, httpExp.Shutdown)

	grpcExp, err := createTracingGRPCExporter(ctx, cfg)
	require.NoError(t, err)
	require.NotNil(t, grpcExp)
	shutdownExporter(t, grpcExp.Shutdown)
}

// A working TLS config must reach the exporter as real transport credentials.
// An operator who enables TLS and silently gets an insecure exporter would ship
// trace data in the clear with no warning at all.
//
// The test exports to a live TLS collector, so it fails if the credentials are
// dropped: the same call over plaintext cannot complete the handshake.
func TestCreateTracingHTTPExporter_TLSReachesTheTransport(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: srv.Certificate().Raw,
	}), 0o600))

	spans := tracetest.SpanStubs{{Name: "Test.Export"}}.Snapshots()

	t.Run("TLS enabled reaches the collector", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()

		exp, err := createTracingHTTPExporter(ctx, &TracingConfig{
			Endpoint: srv.Listener.Addr().String(),
			TLS:      &TLSConfig{Enabled: true, CAFile: caFile},
		})
		require.NoError(t, err)
		shutdownExporter(t, exp.Shutdown)

		require.NoError(t, exp.ExportSpans(ctx, spans),
			"the configured CA must let the exporter complete the TLS handshake")
		assert.Positive(t, hits.Load(), "the collector must have received the export")
	})

	t.Run("TLS disabled cannot reach a TLS collector", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		exp, err := createTracingHTTPExporter(ctx, &TracingConfig{
			Endpoint: srv.Listener.Addr().String(),
		})
		require.NoError(t, err)
		shutdownExporter(t, exp.Shutdown)

		assert.Error(t, exp.ExportSpans(ctx, spans),
			"a plaintext exporter must fail against a TLS collector")
	})
}

// The gRPC exporter takes the same TLS config. Construction alone is checked
// here; the HTTP test above proves the credentials are honoured.
func TestCreateTracingGRPCExporter_TLSEnabled(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, []byte("-- not a real certificate --\n"), 0o600))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	exp, err := createTracingGRPCExporter(ctx, &TracingConfig{
		Endpoint: "127.0.0.1:14317",
		TLS:      &TLSConfig{Enabled: true, CAFile: caFile},
	})
	require.NoError(t, err)
	require.NotNil(t, exp)
	shutdownExporter(t, exp.Shutdown)
}

func shutdownExporter(t *testing.T, shutdown func(context.Context) error) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})
}

// ---------------------------------------------------------------------------
// Sampler
// ---------------------------------------------------------------------------

// The sample rate picks the base sampler. Getting this branch wrong either
// drops every trace or floods the collector, and neither shows up as an error.
func TestCreateTracingSampler_RateSelectsBaseSampler(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rate     float64
		wantBase string
	}{
		{"zero rate never samples", 0, "AlwaysOffSampler"},
		{"negative rate never samples", -1, "AlwaysOffSampler"},
		{"rate of one always samples", 1.0, "AlwaysOnSampler"},
		{"rate above one always samples", 5.0, "AlwaysOnSampler"},
		{"fractional rate is parent based", 0.25, "ParentBased"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := createTracingSampler(&TracingConfig{SampleRate: tc.rate})
			desc := s.Description()

			assert.Contains(t, desc, "ForceTraceSampler{",
				"every sampler must be wrapped so force-tracing still works")
			assert.Contains(t, desc, tc.wantBase)
		})
	}
}

// The force-trace attribute must override the base decision. This is what an
// operator relies on when they add the header to debug one live request.
func TestForceTraceSampler_AttributeOverridesNeverSample(t *testing.T) {
	s := createTracingSampler(&TracingConfig{SampleRate: 0})

	plain := s.ShouldSample(sdktrace.SamplingParameters{
		ParentContext: context.Background(),
		Name:          "Http.ReceivedRequest",
	})
	assert.Equal(t, sdktrace.Drop, plain.Decision,
		"a zero sample rate must drop an unmarked span")

	forced := s.ShouldSample(sdktrace.SamplingParameters{
		ParentContext: context.Background(),
		Name:          "Http.ReceivedRequest",
		Attributes:    []attribute.KeyValue{attribute.Bool(forceTraceAttributeKey, true)},
	})
	assert.Equal(t, sdktrace.RecordAndSample, forced.Decision,
		"the force-trace attribute must bypass the base sampler")

	// An attribute set to false must NOT force. Otherwise merely naming the key
	// would defeat sampling.
	notForced := s.ShouldSample(sdktrace.SamplingParameters{
		ParentContext: context.Background(),
		Name:          "Http.ReceivedRequest",
		Attributes:    []attribute.KeyValue{attribute.Bool(forceTraceAttributeKey, false)},
	})
	assert.Equal(t, sdktrace.Drop, notForced.Decision,
		"force_trace=false must not force sampling")

	// An unrelated attribute must not force either.
	unrelated := s.ShouldSample(sdktrace.SamplingParameters{
		ParentContext: context.Background(),
		Name:          "Http.ReceivedRequest",
		Attributes:    []attribute.KeyValue{attribute.Bool("some.other.flag", true)},
	})
	assert.Equal(t, sdktrace.Drop, unrelated.Decision)
}

// ---------------------------------------------------------------------------
// Force-trace matchers
// ---------------------------------------------------------------------------

func TestShouldForceTrace(t *testing.T) {
	for _, tc := range []struct {
		name       string
		matchers   []*ForceTraceMatcher
		network    string
		method     string
		wantForced bool
		wantReason string
	}{
		{
			name:     "no matchers configured",
			matchers: nil,
			network:  "evm:1", method: "eth_call",
			wantForced: false, wantReason: "",
		},
		{
			name:     "network and method both match",
			matchers: []*ForceTraceMatcher{{Network: "evm:1", Method: "eth_call"}},
			network:  "evm:1", method: "eth_call",
			wantForced: true, wantReason: "network:evm:1,method:eth_call",
		},
		{
			name:     "both specified but method differs",
			matchers: []*ForceTraceMatcher{{Network: "evm:1", Method: "eth_call"}},
			network:  "evm:1", method: "eth_getBalance",
			wantForced: false, wantReason: "",
		},
		{
			name:     "network only",
			matchers: []*ForceTraceMatcher{{Network: "evm:137"}},
			network:  "evm:137", method: "anything",
			wantForced: true, wantReason: "network:evm:137",
		},
		{
			name:     "method only",
			matchers: []*ForceTraceMatcher{{Method: "debug_*"}},
			network:  "evm:42161", method: "debug_traceTransaction",
			wantForced: true, wantReason: "method:debug_traceTransaction",
		},
		{
			name:     "or pattern in method",
			matchers: []*ForceTraceMatcher{{Method: "debug_*|trace_*"}},
			network:  "evm:1", method: "trace_block",
			wantForced: true, wantReason: "method:trace_block",
		},
		{
			name:     "empty matcher matches nothing",
			matchers: []*ForceTraceMatcher{{}},
			network:  "evm:1", method: "eth_call",
			wantForced: false, wantReason: "",
		},
		{
			name:     "nil matcher in the list is skipped",
			matchers: []*ForceTraceMatcher{nil, {Method: "eth_call"}},
			network:  "evm:1", method: "eth_call",
			wantForced: true, wantReason: "method:eth_call",
		},
		{
			name:     "second matcher wins when the first misses",
			matchers: []*ForceTraceMatcher{{Network: "evm:10"}, {Network: "evm:1"}},
			network:  "evm:1", method: "eth_call",
			wantForced: true, wantReason: "network:evm:1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saveTracingGlobals(t)
			forceTraceMatchers = tc.matchers

			forced, reason := ShouldForceTrace(tc.network, tc.method)
			assert.Equal(t, tc.wantForced, forced)
			assert.Equal(t, tc.wantReason, reason)
		})
	}
}

// SetForceTraceNetwork must be a no-op when tracing is off or the network is
// empty, so the context does not grow a useless value on every request.
func TestSetAndGetForceTraceNetwork(t *testing.T) {
	t.Run("stores the network when tracing is on", func(t *testing.T) {
		saveTracingGlobals(t)
		IsTracingEnabled = true

		ctx := SetForceTraceNetwork(context.Background(), "evm:1")
		assert.Equal(t, "evm:1", GetForceTraceNetwork(ctx))
	})

	t.Run("stores nothing when tracing is off", func(t *testing.T) {
		saveTracingGlobals(t)
		IsTracingEnabled = false

		ctx := SetForceTraceNetwork(context.Background(), "evm:1")
		assert.Equal(t, "", GetForceTraceNetwork(ctx))
	})

	t.Run("stores nothing for an empty network", func(t *testing.T) {
		saveTracingGlobals(t)
		IsTracingEnabled = true

		ctx := SetForceTraceNetwork(context.Background(), "")
		assert.Equal(t, "", GetForceTraceNetwork(ctx))
	})

	t.Run("returns empty for a bare context", func(t *testing.T) {
		assert.Equal(t, "", GetForceTraceNetwork(context.Background()))
	})
}

// ---------------------------------------------------------------------------
// SetTraceSpanError
// ---------------------------------------------------------------------------

// A StandardError must land on the span as its code chain plus a recorded
// error event, and the status description must be the base code. An operator
// filters on error.code, so a wrong variable here hides the failure.
func TestSetTraceSpanError_StandardError(t *testing.T) {
	h := newTracingHarness(t, false)

	ctx, span := StartSpan(context.Background(), "Test.Op")
	inner := NewErrEndpointServerSideException(errors.New("boom"), nil, 500)
	outer := NewErrUpstreamRequest(inner, NewFakeUpstream("up1"), "evm:1", "eth_call", time.Second, 1, 0, 0)
	SetTraceSpanError(span, outer)
	span.End()
	_ = ctx

	rec := h.endedNamed("Test.Op")
	attrs := spanAttrs(rec)

	codeChain, ok := attrs[attribute.Key("error.code")]
	require.True(t, ok, "error.code must be set for a StandardError")
	assert.Equal(t, outer.(StandardError).CodeChain(), codeChain.AsString(),
		"error.code must carry the full code chain, not just the outer code")

	assert.Equal(t, codes.Error, rec.Status().Code)
	assert.Equal(t, string(ErrCodeUpstreamRequest), rec.Status().Description,
		"status description must be the outer base code")
	require.Len(t, rec.Events(), 1, "the error must be recorded as one span event")
	assert.Equal(t, "exception", rec.Events()[0].Name)
}

// A plain error has no code chain, so the span gets only the summary status.
func TestSetTraceSpanError_PlainError(t *testing.T) {
	h := newTracingHarness(t, false)

	_, span := StartSpan(context.Background(), "Test.Plain")
	SetTraceSpanError(span, errors.New("plain failure"))
	span.End()

	rec := h.endedNamed("Test.Plain")
	attrs := spanAttrs(rec)

	_, hasCode := attrs[attribute.Key("error.code")]
	assert.False(t, hasCode, "a plain error carries no code chain")
	assert.Equal(t, codes.Error, rec.Status().Code)
	assert.Equal(t, ErrorSummary(errors.New("plain failure")), rec.Status().Description)
	require.Len(t, rec.Events(), 1)
}

// The guards must hold: a nil span, a non-recording span, and a non-error
// argument must all be ignored instead of panicking on a live request path.
func TestSetTraceSpanError_Guards(t *testing.T) {
	assert.NotPanics(t, func() { SetTraceSpanError(nil, errors.New("x")) })
	assert.NotPanics(t, func() { SetTraceSpanError(defaultNoopSpan, errors.New("x")) })

	h := newTracingHarness(t, false)
	_, span := StartSpan(context.Background(), "Test.NonError")
	SetTraceSpanError(span, "this is a string, not an error")
	span.End()

	rec := h.endedNamed("Test.NonError")
	assert.Equal(t, codes.Unset, rec.Status().Code,
		"a non-error argument must leave the span status alone")
	assert.Empty(t, rec.Events())
}

// SpanFromContext must round-trip the span the tracer put in the context.
func TestSpanFromContext(t *testing.T) {
	h := newTracingHarness(t, false)

	ctx, span := StartSpan(context.Background(), "Test.Ctx")
	got := SpanFromContext(ctx)
	assert.Equal(t, span.SpanContext().SpanID(), got.SpanContext().SpanID())
	span.End()

	assert.False(t, trace.SpanFromContext(context.Background()).SpanContext().IsValid(),
		"a bare context must yield an invalid span context")
	_ = h
}
