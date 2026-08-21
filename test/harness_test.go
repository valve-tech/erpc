package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
)

// recordedExitCode holds the code the erpc library asked the process to exit
// with. -1 means the library never asked.
var recordedExitCode atomic.Int64

func init() {
	recordedExitCode.Store(-1)
	libraryExitCode = recordedExitCode.Load

	// Work around upstream bug 99: erpc.Init ends the process from inside a
	// goroutine the caller never started, so a bind failure printed no
	// assertion, no panic and no reason — the test binary was simply gone.
	//
	// This is a test-level workaround, not a fix. The fix moves the exit out of
	// the library and lets cmd/erpc own the process; that change is wider and
	// needs its own pin. Here the test binary records the code and stops only
	// the calling goroutine, so the test that caused it can report it.
	util.OsExit = func(code int) {
		recordedExitCode.CompareAndSwap(-1, int64(code))
		runtime.Goexit()
	}
}

// requireNoLibraryExit fails the test when the library asked to end the process
// during it. The guard that records the code lives in fake_erpc.go. Without
// this check the harness would swallow the very failure that used to kill the
// test binary.
func requireNoLibraryExit(t *testing.T) {
	t.Helper()
	if code := libraryExitCode(); code != -1 {
		t.Fatalf("the erpc library called util.OsExit(%d) — see valve/upstream-bug-log.md bug 99; run with LOG_LEVEL=error to see the reason", code)
	}
}

// requireK6 skips the test when the k6 binary is absent.
//
// The stress harness drives load with k6. k6 is a separate install, so a
// machine without it cannot run these tests, and a machine without it must say
// so rather than fail on something unrelated.
func requireK6(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("k6"); err != nil {
		t.Skip("k6 is not installed; install it from https://grafana.com/docs/k6/latest/set-up/install-k6/ and re-run, or run TestStressHarness_BootsAndServesWithoutK6 for the non-k6 coverage")
	}
}

// requireFreePorts skips the test when a port the harness needs is taken.
//
// The harness binds fixed ports. A taken port is a fact about the machine, not
// a defect in eRPC, so the test says which port and stops.
func requireFreePorts(t *testing.T, ports ...int) {
	t.Helper()
	for _, port := range ports {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Skipf("port %d is already in use (%v); the harness binds fixed ports, so free it and re-run", port, err)
		}
		ln.Close()
	}
}

func stressPorts(config StressTestConfig) []int {
	ports := []int{config.ServicePort, config.MetricsPort}
	for _, sc := range config.ServerConfigs {
		ports = append(ports, sc.Port)
	}
	return ports
}

// TestStressHarness_BootsAndServesWithoutK6 covers every part of the stress
// harness except the load generator: the fake upstreams start, eRPC reads the
// generated config, binds its service port, and answers a JSON-RPC request
// through an upstream.
//
// This test needs no k6, so it runs everywhere. It is the test that catches a
// harness config the library refuses — the failure that used to end the test
// binary with no output at all.
func TestStressHarness_BootsAndServesWithoutK6(t *testing.T) {
	config := StressTestConfig{
		ServicePort: 4291,
		MetricsPort: 5291,
		ServerConfigs: []ServerConfig{
			{Port: 8191, FailureRate: 0, MinDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, SampleFile: "samples/evm-json-rpc.json"},
		},
	}
	requireFreePorts(t, stressPorts(config)...)

	h, err := bootStressHarness(context.Background(), config)
	if err != nil {
		t.Fatalf("stress harness failed to boot: %v", err)
	}
	defer h.Close()
	requireNoLibraryExit(t)

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)
	req, err := http.NewRequest(http.MethodPost, h.baseUrl+"/main/evm/123", body)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("eRPC did not answer on %s: %v", h.baseUrl, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("eRPC answered %s, want 200", resp.Status)
	}

	// A 200 alone is not proof of routing: eRPC returns 200 with a JSON-RPC
	// error body for some failures. Require a result the fake upstream produced.
	var rpcResp struct {
		Result interface{}     `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("eRPC answered with a body that is not JSON: %v", err)
	}
	if len(rpcResp.Error) > 0 && string(rpcResp.Error) != "null" {
		t.Fatalf("eRPC returned a JSON-RPC error: %s", rpcResp.Error)
	}
	if rpcResp.Result == nil {
		t.Fatal("eRPC returned no result, so the request never reached the fake upstream")
	}

	// The metrics endpoint is the source of every stress-test assertion, so the
	// harness is only useful if it is up too.
	metricsResp, err := client.Get(fmt.Sprintf("http://localhost:%d/metrics", config.MetricsPort))
	if err != nil {
		t.Fatalf("metrics endpoint on port %d did not answer: %v", config.MetricsPort, err)
	}
	defer metricsResp.Body.Close()
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("metrics endpoint answered %s, want 200", metricsResp.Status)
	}

	requireNoLibraryExit(t)
}
