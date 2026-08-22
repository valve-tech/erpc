package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
	yaml "gopkg.in/yaml.v3"
)

// The API key an operator puts in an endpoint URL. Every test that reads
// eRPC output looks for this string: it must never reach stdout, stderr
// or a log line.
const secretKey = "SUPERSECRETKEY123"

// cliRun is what an operator observes from one command: the two output
// streams, every exit code the process asked for, and the log level the
// command left behind.
type cliRun struct {
	stdout    string
	stderr    string
	exitCodes []int
	logLevel  zerolog.Level
}

// runCLI runs main() with the given argv and captures what an operator
// sees. It replaces util.OsExit, so a command that would end the process
// runs to completion and reports its code instead.
//
// Use it only for commands that return (validate, dump, and the failure
// paths of start). `start` on a healthy config blocks until a signal.
func runCLI(t *testing.T, args ...string) cliRun {
	t.Helper()
	mainMutex.Lock()
	defer mainMutex.Unlock()

	origArgs, origStdout, origStderr := os.Args, os.Stdout, os.Stderr
	origExit, origLogger := util.OsExit, log.Logger
	origLevel := zerolog.GlobalLevel()

	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)

	var outBuf, errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&outBuf, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errBuf, errR) }()

	var mu sync.Mutex
	codes := []int{}

	os.Stdout, os.Stderr = outW, errW
	os.Args = append([]string{"erpc-test"}, args...)
	util.OsExit = func(code int) {
		mu.Lock()
		defer mu.Unlock()
		codes = append(codes, code)
	}

	func() {
		defer func() {
			_ = outW.Close()
			_ = errW.Close()
			os.Stdout, os.Stderr = origStdout, origStderr
			os.Args = origArgs
			util.OsExit = origExit
			log.Logger = origLogger
		}()
		main()
	}()
	wg.Wait()

	leftLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(origLevel)

	mu.Lock()
	defer mu.Unlock()
	return cliRun{stdout: outBuf.String(), stderr: errBuf.String(), exitCodes: codes, logLevel: leftLevel}
}

// writeConfig writes body to a fresh temp file and returns its path.
func writeConfig(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// oneUpstreamConfig is a complete, loadable config with a single EVM
// upstream whose endpoint carries an API key.
func oneUpstreamConfig(endpoint string) string {
	return fmt.Sprintf(`
logLevel: debug
metrics:
  enabled: false
projects:
  - id: main
    upstreams:
      - id: local
        endpoint: %s
        evm:
          chainId: 1
    networks:
      - architecture: evm
        evm:
          chainId: 1
`, endpoint)
}

// deadEndpoint points at a port nothing listens on, so every upstream
// probe fails at once instead of reaching the network.
const deadEndpoint = "http://127.0.0.1:1/v1/" + secretKey

/* -------------------------------------------------------------------------- */
/*                                  validate                                  */
/* -------------------------------------------------------------------------- */

// `validate` on a loadable config prints a JSON report, counts the
// resources it found and exits zero. The upstream is unreachable here, so
// the report also carries the transport warning — and that warning must
// show the redaction placeholder in place of the endpoint.
func TestValidate_GoodConfig_ReportsResourcesAndDoesNotExit(t *testing.T) {
	cfgPath := writeConfig(t, "erpc.yaml", oneUpstreamConfig(deadEndpoint))
	run := runCLI(t, "validate", cfgPath)

	require.Empty(t, run.exitCodes, "a config that loads must not exit non-zero")

	var report struct {
		Errors    []string `json:"errors"`
		Warnings  []string `json:"warnings"`
		Resources struct {
			Totals struct {
				ProjectsTotal  int `json:"projectsTotal"`
				NetworksTotal  int `json:"networksTotal"`
				UpstreamsTotal int `json:"upstreamsTotal"`
			} `json:"totals"`
		} `json:"resources"`
	}
	require.NoError(t, json.Unmarshal([]byte(run.stdout), &report), "validate must print parseable JSON")
	require.Empty(t, report.Errors)
	require.Equal(t, 1, report.Resources.Totals.ProjectsTotal)
	require.Equal(t, 1, report.Resources.Totals.NetworksTotal)
	require.Equal(t, 1, report.Resources.Totals.UpstreamsTotal)

	require.NotEmpty(t, report.Warnings, "an unreachable upstream must produce a warning")
	require.Contains(t, run.stdout, "redacted",
		"the endpoint in a validate warning must show the redaction placeholder")
	require.NotContains(t, run.stdout, secretKey, "validate must never print the API key")
}

// A config file the operator names but that does not exist is a fatal
// error. The report says which path failed, and the command exits 1.
func TestValidate_MissingConfigFile_NamesPathAndExitsOne(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	run := runCLI(t, "--config", missing, "validate")

	require.Equal(t, []int{1}, run.exitCodes, "a missing config file must exit 1")

	var report struct {
		Errors []string `json:"errors"`
	}
	require.NoError(t, json.Unmarshal([]byte(run.stdout), &report))
	require.Len(t, report.Errors, 1)
	require.Contains(t, report.Errors[0], "config load error")
	require.Contains(t, report.Errors[0], missing, "the error must name the file the operator asked for")
}

// `--format md` renders the same report as markdown. An operator pastes
// this into a pull request, so the error must survive the format switch.
func TestValidate_MarkdownFormat_RendersErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	run := runCLI(t, "--config", missing, "validate", "--format", "md")

	require.Equal(t, []int{1}, run.exitCodes)
	require.Contains(t, run.stdout, "### Errors", "md format must render a markdown heading")
	require.Contains(t, run.stdout, "config load error")
	require.Contains(t, run.stdout, missing)
	require.NotContains(t, run.stdout, `"errors"`, "md format must not fall back to JSON")
}

// A config that LOADS can still be wrong. Here the upstream answers
// eth_chainId with a different chain than the config claims: validate
// reports it as an error and exits 1.
func TestValidate_ChainIdMismatch_ReportsErrorAndExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x2"}`))
	}))
	defer srv.Close()

	cfgPath := writeConfig(t, "erpc.yaml", oneUpstreamConfig(srv.URL+"/"+secretKey))
	run := runCLI(t, "validate", cfgPath)

	require.Equal(t, []int{1}, run.exitCodes, "a report with errors must exit 1")

	var report struct {
		Errors []string `json:"errors"`
	}
	require.NoError(t, json.Unmarshal([]byte(run.stdout), &report))
	require.NotEmpty(t, report.Errors)
	joined := strings.Join(report.Errors, "\n")
	require.Contains(t, joined, "chain ID 2", "the error must report the chain the upstream answered with")
	require.Contains(t, joined, "'local'", "the error must name the upstream by id")
	require.NotContains(t, run.stdout, secretKey, "validate must never print the API key")
}

/* -------------------------------------------------------------------------- */
/*                                    dump                                    */
/* -------------------------------------------------------------------------- */

// `dump` prints the config eRPC would run with. It is the command an
// operator pastes into a bug report, so the endpoint must arrive redacted
// — scheme and host only, plus the redaction placeholder.
func TestDump_YAML_RedactsEndpointSecret(t *testing.T) {
	cfgPath := writeConfig(t, "erpc.yaml", oneUpstreamConfig(deadEndpoint))
	run := runCLI(t, "dump", cfgPath)

	require.Empty(t, run.exitCodes)
	require.Contains(t, run.stdout, "redacted=", "the dumped endpoint must carry the redaction placeholder")
	require.NotContains(t, run.stdout, secretKey, "dump must never print the API key")

	var dumped map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(run.stdout), &dumped), "dump must print parseable YAML")
	require.Contains(t, dumped, "projects")
}

// The JSON form redacts the same way. Format must not change what a
// secret does.
func TestDump_JSON_RedactsEndpointSecret(t *testing.T) {
	cfgPath := writeConfig(t, "erpc.yaml", oneUpstreamConfig(deadEndpoint))
	run := runCLI(t, "dump", "--format", "json", cfgPath)

	require.Empty(t, run.exitCodes)
	require.NotContains(t, run.stdout, secretKey, "dump --format json must never print the API key")

	var dumped struct {
		Projects []struct {
			Upstreams []struct {
				Endpoint string `json:"endpoint"`
			} `json:"upstreams"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal([]byte(run.stdout), &dumped), "dump must print parseable JSON")
	require.Len(t, dumped.Projects, 1)
	require.Len(t, dumped.Projects[0].Upstreams, 1)
	require.Contains(t, dumped.Projects[0].Upstreams[0].Endpoint, "redacted=",
		"the dumped endpoint must carry the redaction placeholder")
}

// dump exists to show the EFFECTIVE config. A network that declares no
// selectionPolicy still gets one at runtime, so the dump must show it —
// otherwise the operator reads a config that is not the one eRPC runs.
func TestDump_FillsInEffectiveSelectionPolicy(t *testing.T) {
	cfgPath := writeConfig(t, "erpc.yaml", oneUpstreamConfig(deadEndpoint))
	run := runCLI(t, "dump", cfgPath)

	require.Empty(t, run.exitCodes)
	require.Contains(t, run.stdout, "selectionPolicy:",
		"dump must fill in the effective selectionPolicy the engine would apply")
	require.Contains(t, run.stdout, "evalFunc:",
		"the effective policy must include its eval source")
}

// An unsupported --format names the value the operator typed, writes to
// stderr (stdout stays machine-readable) and exits 1.
func TestDump_UnsupportedFormat_ExitsOne(t *testing.T) {
	cfgPath := writeConfig(t, "erpc.yaml", oneUpstreamConfig(deadEndpoint))
	run := runCLI(t, "dump", "--format", "xml", cfgPath)

	require.Equal(t, []int{1}, run.exitCodes)
	require.Contains(t, run.stderr, `unsupported format "xml"`, "the message must quote the format the operator typed")
	require.Contains(t, run.stderr, "use yaml or json", "the message must name the formats that do work")
	require.Empty(t, run.stdout, "a rejected format must print nothing to stdout")
}

// A missing config file fails the dump on stderr, naming the path.
func TestDump_MissingConfigFile_NamesPathAndExitsOne(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	run := runCLI(t, "--config", missing, "dump")

	require.Equal(t, []int{1}, run.exitCodes)
	require.Contains(t, run.stderr, "failed to load config")
	require.Contains(t, run.stderr, missing, "the error must name the file the operator asked for")
	require.Empty(t, run.stdout)
}

/* -------------------------------------------------------------------------- */
/*                             config discovery                               */
/* -------------------------------------------------------------------------- */

// With no --config flag eRPC probes a fixed list of paths in order. When
// a directory holds both erpc.yaml and erpc.yml, the .yaml file wins.
func TestConfigDiscovery_PrefersYamlOverYml(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "erpc.yaml"),
		[]byte(strings.Replace(oneUpstreamConfig(deadEndpoint), "id: main", "id: from-yaml", 1)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "erpc.yml"),
		[]byte(strings.Replace(oneUpstreamConfig(deadEndpoint), "id: main", "id: from-yml", 1)), 0o600))
	t.Chdir(dir)

	run := runCLI(t, "dump")
	require.Empty(t, run.exitCodes)
	require.Contains(t, run.stdout, "id: from-yaml", "erpc.yaml must win over erpc.yml")
	require.NotContains(t, run.stdout, "id: from-yml", "erpc.yml must not be loaded when erpc.yaml exists")
}

// --require-config with nothing to find must say WHERE it looked. The
// probe list is the only way an operator learns which paths eRPC checks.
func TestConfigDiscovery_RequireConfig_ListsEveryProbedPath(t *testing.T) {
	t.Chdir(t.TempDir())

	run := runCLI(t, "--require-config", "dump")
	require.Equal(t, []int{1}, run.exitCodes)
	require.Contains(t, run.stderr, "no valid configuration file found in")
	for _, path := range []string{"./erpc.yaml", "./erpc.yml", "./erpc.ts", "./erpc.js", "/root/erpc.yaml"} {
		require.Containsf(t, run.stderr, path, "the error must list the probed path %s", path)
	}
}

// Bug 127: `--config` is honoured only when the path is longer than one
// character (`len(configFile) > 1`). A one-character path is dropped
// SILENTLY — eRPC falls back to discovery and runs a different config
// than the operator named.
//
// This test pins today's behaviour. When the guard becomes a non-empty
// check, this test must flip to expect the named file.
func TestConfig_OneCharacterPathIsIgnored(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"),
		[]byte(strings.Replace(oneUpstreamConfig(deadEndpoint), "id: main", "id: from-flag", 1)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "erpc.yaml"),
		[]byte(strings.Replace(oneUpstreamConfig(deadEndpoint), "id: main", "id: from-discovery", 1)), 0o600))
	t.Chdir(dir)

	run := runCLI(t, "--config", "a", "dump")
	require.Empty(t, run.exitCodes)
	require.Contains(t, run.stdout, "id: from-discovery",
		"bug 127: a one-character --config path is ignored and discovery wins")
	require.NotContains(t, run.stdout, "id: from-flag")
}

/* -------------------------------------------------------------------------- */
/*                                 LOG_LEVEL                                  */
/* -------------------------------------------------------------------------- */

// LOG_LEVEL overrides the log level written in the config file, and the
// override is what the dumped config reports.
func TestLogLevelEnv_OverridesTheConfigFile(t *testing.T) {
	t.Setenv("LOG_LEVEL", "warn")
	cfgPath := writeConfig(t, "erpc.yaml", oneUpstreamConfig(deadEndpoint))

	run := runCLI(t, "dump", cfgPath)
	require.Empty(t, run.exitCodes)
	require.Contains(t, run.stdout, "logLevel: warn", "LOG_LEVEL must override the file's logLevel")
	require.Equal(t, zerolog.WarnLevel, run.logLevel, "LOG_LEVEL must set the global log level")
}

// loadConfigForTest calls getConfig the way a command does, and returns
// both the config and everything getConfig logged. It exists because the
// LOG_LEVEL block cannot be observed through a whole command: `dump` and
// `validate` silence logging before they load the config, and `start` on
// a loadable config never returns.
func loadConfigForTest(t *testing.T, cfgPath string) (*common.Config, string, error) {
	t.Helper()
	var logs bytes.Buffer
	logger := zerolog.New(&logs)

	var cfg *common.Config
	var cfgErr error
	app := &cli.Command{
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config"},
			&cli.BoolFlag{Name: "require-config"},
			&cli.StringSliceFlag{Name: "endpoint"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, cfgErr = getConfig(logger, cmd)
			return nil
		},
	}
	require.NoError(t, app.Run(context.Background(), []string{"erpc-test", "--config", cfgPath}))
	return cfg, logs.String(), cfgErr
}

// Bug 125. LOG_LEVEL is an override, and an override the process cannot
// read must cost the operator nothing but the override itself.
//
// It used to cost them every log line. zerolog.ParseLevel returns NoLevel
// with its error, NoLevel is 6, and the code installed it as the global
// floor on the failure path — above error, so debug, info, warn and error
// all stopped. The warning that explained it was suppressed by the level
// it had just installed.
//
// So this asserts the rule the whole family shares: a value the process
// cannot read must land where an ABSENT value lands, and must say so.
// Both halves matter. Without the fallback the typo is destructive;
// without the warning the operator cannot tell they made one.
func TestLogLevelEnv_AnUnreadableValueCostsNothingButTheOverride(t *testing.T) {
	cfgPath := writeConfig(t, "erpc.yaml", oneUpstreamConfig(deadEndpoint))

	// Pin the level the run starts from, and put it back afterwards. The
	// success path of the block under test sets the global level, so an
	// unrestored run would change what every later test measures.
	ambient := zerolog.GlobalLevel()
	defer zerolog.SetGlobalLevel(ambient)

	// The absent case, measured rather than assumed. It is the yardstick
	// for the unreadable one.
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	cfg, _, err := loadConfigForTest(t, cfgPath)
	require.NoError(t, err)
	absentLevel := zerolog.GlobalLevel()
	require.Equal(t, zerolog.InfoLevel, absentLevel,
		"no LOG_LEVEL must leave the level the process started with")
	require.Equal(t, "debug", cfg.LogLevel)

	t.Setenv("LOG_LEVEL", "not-a-level")
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	cfg, logs, err := loadConfigForTest(t, cfgPath)
	require.NoError(t, err)

	require.Equal(t, absentLevel, zerolog.GlobalLevel(),
		"an unreadable LOG_LEVEL must leave the level exactly where no LOG_LEVEL leaves it")
	require.NotEqual(t, zerolog.NoLevel, zerolog.GlobalLevel(),
		"NoLevel is what ParseLevel returns WITH its error; installing it is the bug")
	require.Equal(t, "debug", cfg.LogLevel,
		"an unreadable LOG_LEVEL must not overwrite the config value either")

	require.Contains(t, logs, "not-a-level",
		"the warning must quote the value the operator actually typed")
	require.NotContains(t, logs, "defaulting to 'debug'",
		"the message must not promise a level the code does not install")

	// The control. A LOG_LEVEL the process CAN read still takes effect, so
	// the fallback above is not the code ignoring the variable.
	t.Setenv("LOG_LEVEL", "warn")
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	cfg, _, err = loadConfigForTest(t, cfgPath)
	require.NoError(t, err)
	require.Equal(t, zerolog.WarnLevel, zerolog.GlobalLevel(),
		"a readable LOG_LEVEL must still set the global level")
	require.Equal(t, "warn", cfg.LogLevel,
		"a readable LOG_LEVEL must still override the config file")
}

/* -------------------------------------------------------------------------- */
/*                                 exit codes                                 */
/* -------------------------------------------------------------------------- */

// Bug 98. A Unix exit status is one byte: the kernel reports code & 0xFF.
// An exit code above 255 reaches the shell as a different number, so
// every documented code, systemd unit and CI gate reads the wrong value.
//
// The codes must also stay distinct from each other and from 1, which
// `validate` and `dump` already use for a bad config.
func TestExitCodes_FitInOneByte(t *testing.T) {
	codes := map[string]int{
		"ExitCodeERPCStartFailed":  util.ExitCodeERPCStartFailed,
		"ExitCodeHttpServerFailed": util.ExitCodeHttpServerFailed,
	}
	for name, code := range codes {
		require.Equalf(t, code, code&0xFF,
			"%s=%d does not survive the kernel's & 0xFF; the shell would see %d", name, code, code&0xFF)
		require.Greaterf(t, code, 1, "%s must not collide with the config-error code 1", name)
		require.Lessf(t, code, 126, "%s must stay below 126, which shells reserve for signals", name)
	}
	require.NotEqual(t, util.ExitCodeERPCStartFailed, util.ExitCodeHttpServerFailed,
		"a start failure and an http-server failure must stay distinguishable")
}

// The exit code an operator gets for a config eRPC cannot load.
func TestStart_MissingConfig_ExitsWithStartFailedCode(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	run := runCLI(t, "start", "--config", missing)

	require.Equal(t, []int{util.ExitCodeERPCStartFailed}, run.exitCodes)
}

// Bug 99 pin. erpc.Init is library code. It used to call util.OsExit from a
// goroutine the caller never started, so an embedder lost its whole process
// when the HTTP server could not bind.
//
// Init now sends the bind failure to cmd/erpc as ErrServerFailed, and the
// BINARY decides to exit. This test pins that split: the exit code still
// appears, but it comes from main, not from the library.
func TestStart_HttpServerCannotBind_InitReturnsTheErrorAndTheBinaryExits(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	cfgPath := writeConfig(t, "erpc.yaml", fmt.Sprintf(`
logLevel: warn
server:
  httpHostV4: "127.0.0.1"
  listenV4: true
  listenV6: false
  httpPort: %d
metrics:
  enabled: false
projects:
  - id: main
    upstreams:
      - id: local
        endpoint: %s
        evm:
          chainId: 1
    networks:
      - architecture: evm
        evm:
          chainId: 1
`, port, deadEndpoint))

	mainMutex.Lock()
	defer mainMutex.Unlock()

	origArgs, origExit := os.Args, util.OsExit
	defer func() {
		os.Args = origArgs
		util.OsExit = origExit
	}()

	exits := make(chan int, 4)
	util.OsExit = func(code int) { exits <- code }
	os.Args = []string{"erpc-test", "start", "--config", cfgPath}

	// main() never returns here: erpc.Init blocks on the context until a
	// signal arrives, so the process only ends via the exit call below.
	go main()

	select {
	case code := <-exits:
		require.Equal(t, util.ExitCodeHttpServerFailed, code,
			"bug 99: main must exit with the http-server code after Init returns the bind error")
	case <-time.After(60 * time.Second):
		t.Fatal("timed out waiting for the http server bind failure")
	}
}

/* -------------------------------------------------------------------------- */
/*                          more operator surfaces                            */
/* -------------------------------------------------------------------------- */

// The markdown report of a healthy config lists the resources eRPC found.
// This is the format an operator reads by eye, so it must carry the same
// counts as the JSON form and the same redaction.
func TestValidate_MarkdownFormat_GoodConfig(t *testing.T) {
	cfgPath := writeConfig(t, "erpc.yaml", oneUpstreamConfig(deadEndpoint))
	run := runCLI(t, "validate", "--format", "md", cfgPath)

	require.Empty(t, run.exitCodes)
	require.Contains(t, run.stdout, "### Resources")
	require.Contains(t, run.stdout, "projectsTotal: 1")
	require.Contains(t, run.stdout, "upstreamsTotal: 1")
	require.NotContains(t, run.stdout, secretKey, "validate --format md must never print the API key")
}

// `--endpoint` injects an upstream when the config declares none. The URL
// an operator passes on the command line carries an API key just like a
// config file does, so the dump must redact it the same way.
func TestEndpointFlag_InjectsRedactedUpstream(t *testing.T) {
	t.Chdir(t.TempDir())

	run := runCLI(t, "--endpoint", "https://eth.example.com/v1/"+secretKey, "dump")
	require.Empty(t, run.exitCodes)
	require.Contains(t, run.stdout, "endpoint: https://eth.example.com#redacted=",
		"an endpoint passed on the command line must be dumped redacted")
	require.NotContains(t, run.stdout, secretKey, "dump must never print the API key")
}

// An operator who runs eRPC in a directory with no config file gets the
// built-in default project rather than an error.
func TestNoConfigFile_FallsBackToDefaultProject(t *testing.T) {
	t.Chdir(t.TempDir())

	run := runCLI(t, "dump")
	require.Empty(t, run.exitCodes, "a missing config is only fatal with --require-config")
	require.Contains(t, run.stdout, "id: main", "the default config must carry the default project")
	require.Contains(t, run.stdout, "id: public", "the default project must carry the public provider")
}
