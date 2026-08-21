package test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/erpc"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
)

type ServerConfig struct {
	Port             int                    `yaml:"port"`
	FailureRate      float64                `yaml:"failureRate"`
	LimitedRate      float64                `yaml:"limitedRate"`
	MinDelay         time.Duration          `yaml:"minDelay"`
	MaxDelay         time.Duration          `yaml:"maxDelay"`
	SampleFile       string                 `yaml:"sampleFile"`
	AdditionalConfig *common.UpstreamConfig `yaml:"additionalConfig"`
}

type StressTestConfig struct {
	ServicePort             int
	MetricsPort             int
	MaxRPS                  int
	ServerConfigs           []ServerConfig
	AdditionalProjectConfig *common.ProjectConfig
	AdditionalNetworkConfig *common.NetworkConfig
	AdditionalConfig        *common.Config
	Duration                string
	VUs                     int
}

type ServerStats struct {
	RequestsHandled int64
	RequestsSuccess int64
	RequestsFailed  int64
}

type CounterMetric struct {
	Name   string
	Labels map[string]string
	Value  float64
}

type StressTestResult struct {
	CounterMetrics []*CounterMetric
}

func (s *StressTestResult) SumCounter(name string, groupBy []string) []*CounterMetric {
	result := []*CounterMetric{}
	groupMap := make(map[string]*CounterMetric)

	for _, metric := range s.CounterMetrics {
		if metric.Name != name {
			continue
		}

		groupKey := ""
		groupLabels := make(map[string]string)

		if len(groupBy) == 0 {
			groupKey = "overall"
		} else {
			keyParts := []string{}
			for _, label := range groupBy {
				if value, exists := metric.Labels[label]; exists {
					keyParts = append(keyParts, value)
					groupLabels[label] = value
				}
			}
			groupKey = strings.Join(keyParts, "|")
		}

		if existingMetric, exists := groupMap[groupKey]; exists {
			existingMetric.Value += metric.Value
		} else {
			newMetric := &CounterMetric{
				Name:   name,
				Labels: groupLabels,
				Value:  metric.Value,
			}
			groupMap[groupKey] = newMetric
			result = append(result, newMetric)
		}
	}

	return result
}

func CreateFakeServers(configs []ServerConfig) []*FakeServer {
	var fakeServers []*FakeServer
	for _, config := range configs {
		server, err := NewFakeServer(
			config.Port,
			config.FailureRate,
			config.LimitedRate,
			config.MinDelay,
			config.MaxDelay,
			config.SampleFile,
		)
		if err != nil {
			log.Error().Err(err).Int("port", config.Port).Msg("Error creating fake server")
			continue
		}
		fakeServers = append(fakeServers, server)
	}
	return fakeServers
}

func startFakeServer(wg *sync.WaitGroup, server *FakeServer) {
	defer wg.Done()
	log.Info().Int("port", server.Port).Msg("Starting fake server")
	if err := server.Start(); err != nil && !strings.Contains(err.Error(), "Fake server closed") {
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Int("port", server.Port).Msg("Error starting fake server")
		}
	}
}

func loadSamples(filename string) ([]RequestResponseSample, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read sample file: %w", err)
	}

	var samples []RequestResponseSample
	if err := sonic.Unmarshal(data, &samples); err != nil {
		return nil, fmt.Errorf("failed to unmarshal samples: %w", err)
	}

	return samples, nil
}

// libraryExitCode reports the code the erpc library asked the process to exit
// with, or -1 when it never asked.
//
// erpc.Init starts its HTTP, gRPC and metrics servers in goroutines that call
// util.OsExit when a listener fails (upstream bug 99), which ends the whole
// process from inside a library. The test binary replaces this function with a
// recorder and neuters the exit; test/cmd, which never starts eRPC, keeps the
// default and keeps normal os.Exit behaviour.
var libraryExitCode = func() int64 { return -1 }

// stressHarness holds one booted harness: the fake upstreams and one eRPC
// instance in front of them. Close stops both.
type stressHarness struct {
	config      StressTestConfig
	fakeServers []*FakeServer
	baseUrl     string
	cancel      context.CancelFunc
	initErr     chan error
	wg          sync.WaitGroup
	closeOnce   sync.Once
}

// bootStressHarness starts the fake upstreams and eRPC, then waits until eRPC
// accepts connections on its service port.
//
// It returns an error for every failure the caller can act on. The old version
// started eRPC in a goroutine, ignored the result and slept one second, so an
// eRPC that never came up looked the same as one that did.
func bootStressHarness(ctx context.Context, config StressTestConfig) (*stressHarness, error) {
	erpcConfig, localBaseUrl, err := prepareERPCConfig(config)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	h := &stressHarness{
		config:      config,
		fakeServers: CreateFakeServers(config.ServerConfigs),
		baseUrl:     localBaseUrl,
		cancel:      cancel,
		initErr:     make(chan error, 1),
	}
	if len(h.fakeServers) != len(config.ServerConfigs) {
		cancel()
		return nil, fmt.Errorf("only %d of %d fake servers were created", len(h.fakeServers), len(config.ServerConfigs))
	}

	for _, server := range h.fakeServers {
		h.wg.Add(1)
		go startFakeServer(&h.wg, server)
	}

	go func() {
		// erpc.Init blocks until the context is cancelled, so a nil error here
		// means a clean shutdown, not a successful boot.
		h.initErr <- erpc.Init(runCtx, erpcConfig, log.With().Logger())
	}()

	if err := h.waitReady(15 * time.Second); err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

// waitReady polls the eRPC service port until it accepts a connection.
func (h *stressHarness) waitReady(timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", h.config.ServicePort)
	deadline := time.Now().Add(timeout)
	for {
		select {
		case err := <-h.initErr:
			return fmt.Errorf("eRPC exited before it served on %s: %w", addr, err)
		default:
		}
		if code := libraryExitCode(); code != -1 {
			return fmt.Errorf("the erpc library called util.OsExit(%d) instead of serving on %s — see valve/upstream-bug-log.md bug 99; run with LOG_LEVEL=error to see the reason", code, addr)
		}
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("eRPC did not listen on %s within %s: %w", addr, timeout, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Close stops eRPC and every fake upstream. It is safe to call twice.
func (h *stressHarness) Close() {
	h.closeOnce.Do(func() {
		h.cancel()
		for _, server := range h.fakeServers {
			if err := server.Stop(); err != nil {
				log.Error().Err(err).Int("port", server.Port).Msg("Error stopping server")
			}
		}
		h.wg.Wait()
		select {
		case err := <-h.initErr:
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Error().Err(err).Msg("eRPC returned an error on shutdown")
			}
		case <-time.After(10 * time.Second):
			log.Warn().Msg("eRPC did not return within 10s of shutdown")
		}
	})
}

func executeStressTest(config StressTestConfig) (*StressTestResult, error) {
	h, err := bootStressHarness(context.Background(), config)
	if err != nil {
		return nil, err
	}
	defer h.Close()

	if err := runK6StressTest(afero.NewOsFs(), h.baseUrl, config); err != nil {
		return nil, err
	}

	// Wait for 5 seconds to ensure all metrics are collected
	time.Sleep(5 * time.Second)

	// Fetch prometheus metrics used for assertions. eRPC must still be up here,
	// because the metrics endpoint dies with it.
	return fetchPrometheusMetrics(config.MetricsPort)
}

func prepareERPCConfig(config StressTestConfig) (*common.Config, string, error) {
	localBaseUrl := fmt.Sprintf("http://localhost:%d", config.ServicePort)

	upsList := []*common.UpstreamConfig{}
	for _, serverConfig := range config.ServerConfigs {
		ucfg := &common.UpstreamConfig{
			Id:       fmt.Sprintf("server-%d", serverConfig.Port),
			Endpoint: fmt.Sprintf("http://localhost:%d", serverConfig.Port),
			Type:     "evm",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
		if serverConfig.AdditionalConfig != nil {
			ucfg = MergeStructs(ucfg, serverConfig.AdditionalConfig)
		}
		upsList = append(upsList, ucfg)
	}

	nwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
	}

	if config.AdditionalNetworkConfig != nil {
		if config.AdditionalNetworkConfig.Failsafe != nil {
			nwCfg.Failsafe = config.AdditionalNetworkConfig.Failsafe
		}
	}

	prjCfg := &common.ProjectConfig{
		Id:        "main",
		Upstreams: upsList,
		Networks:  []*common.NetworkConfig{nwCfg},
	}
	if config.AdditionalProjectConfig != nil {
		prjCfg = MergeStructs(prjCfg, config.AdditionalProjectConfig)
	}

	mergedConfig := &common.Config{
		LogLevel: "ERROR",
		Server: &common.ServerConfig{
			// ListenV4 must be true. HttpServer.Start rejects a config that
			// enables neither listener, and erpc.Init answers that rejection by
			// ending the process (see bug 99), so omitting it killed the whole
			// test binary with no output.
			ListenV4:   util.BoolPtr(true),
			HttpHostV4: util.StringPtr("0.0.0.0"),
			HttpHostV6: util.StringPtr("[::]"),
			HttpPortV4: util.IntPtr(config.ServicePort),
		},
		Metrics: &common.MetricsConfig{
			Enabled: util.BoolPtr(true),
			HostV4:  util.StringPtr("0.0.0.0"),
			HostV6:  util.StringPtr("[::]"),
			Port:    util.IntPtr(config.MetricsPort),
		},
		Projects: []*common.ProjectConfig{prjCfg},
	}

	if config.AdditionalConfig != nil {
		mergedConfig = MergeStructs(mergedConfig, config.AdditionalConfig)
	}

	return mergedConfig, localBaseUrl, nil
}

// func generateUpstreamConfig(configs []ServerConfig) string {
// 	var upstreamsCfg string
// 	for _, config := range configs {
// 		upstreamsCfg += fmt.Sprintf(`
//     - id: server-%d
//       endpoint: http://localhost:%d
//       type: evm
//       evm:
//         chainId: 123
// `, config.Port, config.Port)
// 	}
// 	return upstreamsCfg
// }

func runK6StressTest(fs afero.Fs, baseUrl string, config StressTestConfig) error {
	// Load all samples
	allSamples, err := loadAllSamples(config.ServerConfigs)
	if err != nil {
		return err
	}

	// Create k6 script
	script := createK6Script(baseUrl, allSamples, config)

	// Write script to temporary file
	tmpfile, err := createTempFile(fs, "k6script*.js", script)
	if err != nil {
		return err
	}
	defer fs.Remove(tmpfile.Name())

	// Execute k6
	// resultsFile, err := createTempFile(fs, "k6results*.json", "")
	// if err != nil {
	// 	return StressTestResult{}, fmt.Errorf("failed to create results file: %w", err)
	// }
	// defer fs.Remove(resultsFile.Name())

	cmd := exec.Command("k6", "run", tmpfile.Name()) //, "--out", "json="+resultsFile.Name()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("k6 execution failed: %w", err)
	}

	// Parse k6 output and create StressTestResult
	// return parseK6Results(fs, resultsFile)
	return nil
}

func loadAllSamples(configs []ServerConfig) ([]RequestResponseSample, error) {
	var allSamples []RequestResponseSample
	for _, config := range configs {
		samples, err := loadSamples(config.SampleFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load samples from %s: %w", config.SampleFile, err)
		}
		allSamples = append(allSamples, samples...)
	}
	return allSamples, nil
}

func createK6Script(baseUrl string, samples []RequestResponseSample, config StressTestConfig) string {
	samplesJSON, _ := sonic.Marshal(samples)
	return fmt.Sprintf(`
		import http from 'k6/http';
		import { check, sleep } from 'k6';
		import { Rate } from 'k6/metrics';

		const baseUrl = '%s/main/evm/123';
		const samples = %s;

		const errorRate = new Rate('errors');

		export let options = {
			vus: %d,
			duration: '%s',
			rps: %d
		};

		export default function() {
			const sample = samples[Math.floor(Math.random() * samples.length)];
			const payload = JSON.stringify(sample.request);
			const params = {
				headers: { 'Content-Type': 'application/json' },
			};

			const res = http.post(baseUrl, payload, params);

			check(res, {
				'status is 200': (r) => r.status === 200,
				'response has no error': (r) => {
					const body = JSON.parse(r.body);
					return body && (body.error === undefined || body.error === null);
				},
			});

			errorRate.add(res.status !== 200);

			sleep(1);
		}
	`, baseUrl, samplesJSON, config.VUs, config.Duration, config.MaxRPS)
}

func createTempFile(fs afero.Fs, pattern, content string) (afero.File, error) {
	tmpfile, err := afero.TempFile(fs, "", pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}

	if _, err := tmpfile.Write([]byte(content)); err != nil {
		return nil, fmt.Errorf("failed to write to temp file: %w", err)
	}

	if err := tmpfile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp file: %w", err)
	}

	return tmpfile, nil
}

func fetchPrometheusMetrics(port int) (*StressTestResult, error) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/metrics", port))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch prometheus metrics: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	os.Stdout.Write(body)

	testResult := &StressTestResult{
		CounterMetrics: []*CounterMetric{},
	}

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return testResult, fmt.Errorf("failed to gather metrics: %w", err)
	}

	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			labels := m.GetLabel()
			var project, network, upstream, category, errorType string
			for _, label := range labels {
				if label.GetName() == "project" {
					project = label.GetValue()
				}
				if label.GetName() == "network" {
					network = label.GetValue()
				}
				if label.GetName() == "upstream" {
					upstream = label.GetValue()
				}
				if label.GetName() == "category" {
					category = label.GetValue()
				}
				if label.GetName() == "error" {
					errorType = label.GetValue()
				}
			}

			if strings.HasSuffix(mf.GetName(), "total") {
				var value float64
				if m.GetCounter().GetValue() > 0 {
					value = m.GetCounter().GetValue()
				} else if m.GetGauge().GetValue() > 0 {
					value = m.GetGauge().GetValue()
				}
				mt := &CounterMetric{
					Name:  mf.GetName(),
					Value: value,
					Labels: map[string]string{
						"project":   project,
						"network":   network,
						"upstream":  upstream,
						"category":  category,
						"errorType": errorType,
					},
				}

				testResult.CounterMetrics = append(testResult.CounterMetrics, mt)
			}
		}
	}

	return testResult, nil
}
