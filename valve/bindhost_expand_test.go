// Package valve holds fork-owned checks that do not belong in an
// upstream-owned directory.
//
// This file answers ONE question, because the answer decides whether a
// deployed config may safely carry `httpHostV4: '${ERPC_BIND_HOST}'`:
//
//	When the environment variable is UNSET, what does eRPC bind?
//
// It matters because Go's os.ExpandEnv has no shell-style `:-default`. An
// unset variable expands to the empty string, and common/defaults.go only
// substitutes a default when HttpHostV4 is NIL. An empty string is not nil.
// If that combination yields ":4000", eRPC listens on EVERY interface, and
// the only thing between a typo and a publicly reachable eRPC is a firewall
// rule -- which decides who may connect, never who may read the wire.
package valve

import (
	"fmt"
	"os"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/spf13/afero"
)

// writeCfg puts a minimal but VALID config on an in-memory filesystem.
func writeCfg(t *testing.T, body string) (afero.Fs, string) {
	t.Helper()
	fs := afero.NewMemMapFs()
	const name = "/erpc.yaml"
	if err := afero.WriteFile(fs, name, []byte(body), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return fs, name
}

const cfgTemplate = `
logLevel: warn
server:
  httpHostV4: '%s'
  httpPortV4: 4000
projects:
  - id: main
    upstreams:
      - endpoint: http://127.0.0.1:8545
`

// TestUnsetBindHostExpandsToEmpty is the whole point of the file. It asserts
// the DANGEROUS behaviour so that if upstream ever fixes it, this test fails
// loudly and the deployed config can drop its guard -- rather than the guard
// quietly outliving its reason.
func TestUnsetBindHostExpandsToEmpty(t *testing.T) {
	const varName = "VALVE_TEST_BIND_HOST_DEFINITELY_UNSET"
	// Checked, because an Unsetenv that silently failed would leave the
	// variable SET and this test would then measure the control case while
	// claiming to measure the dangerous one.
	if err := os.Unsetenv(varName); err != nil {
		t.Fatalf("could not unset %s, so this test cannot measure what it claims: %v", varName, err)
	}

	fs, name := writeCfg(t, fmt.Sprintf(cfgTemplate, "${"+varName+"}"))
	cfg, err := common.LoadConfig(fs, name, nil)
	if err != nil {
		t.Fatalf("LoadConfig with an unset variable failed outright: %v\n"+
			"That would be SAFE behaviour and this test's premise is wrong; "+
			"re-read it before relaxing the deployed config.", err)
	}

	got := cfg.Server.HttpHostV4
	if got == nil {
		t.Fatalf("HttpHostV4 is nil, so defaults.go would substitute 0.0.0.0 -- " +
			"still bind-everything, but by a different route than this test describes")
	}

	// fmt matches erpc/http_server.go:1955 exactly.
	addr := fmt.Sprintf("%s:%d", *got, 4000)
	t.Logf("unset ${%s} -> httpHostV4=%q -> listen address %q", varName, *got, addr)

	if *got != "" {
		t.Fatalf("expected the unset variable to expand to an EMPTY host, got %q. "+
			"If Go or eRPC gained a default here, the deployed config's guard can "+
			"be reconsidered -- but read why it exists first.", *got)
	}
	if addr != ":4000" {
		t.Fatalf("expected listen address %q, got %q", ":4000", addr)
	}
	t.Log("CONFIRMED: an unset variable yields \":4000\", which binds every interface.")
}

// TestSetBindHostIsHonoured is the control. Without it the test above could
// pass because the whole expansion is broken rather than because an unset
// variable specifically yields empty.
func TestSetBindHostIsHonoured(t *testing.T) {
	const varName = "VALVE_TEST_BIND_HOST_SET"
	t.Setenv(varName, "10.44.0.2")

	fs, name := writeCfg(t, fmt.Sprintf(cfgTemplate, "${"+varName+"}"))
	cfg, err := common.LoadConfig(fs, name, nil)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server.HttpHostV4 == nil || *cfg.Server.HttpHostV4 != "10.44.0.2" {
		t.Fatalf("expected the set variable to be honoured, got %v", cfg.Server.HttpHostV4)
	}
	t.Log("CONFIRMED: a set variable reaches httpHostV4, so per-host binding via env works.")
}
