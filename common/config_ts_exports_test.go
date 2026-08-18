package common

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTsConfig writes a TypeScript config to a real temp file. esbuild reads
// the OS filesystem, so the test cannot use an in-memory afero fs here.
func writeTsConfig(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "erpc.ts")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
	return path
}

// An operator who forgets `export default` gets no `exports` global at all.
// Runtime.Exports called ToObject on that null, and sobek raised a TypeError
// that unwound out of the Go call as a panic — so the very next check, the one
// that tells the operator what to type, never ran.
func TestLoadConfig_TypeScriptWithNoExportsExplainsItself(t *testing.T) {
	// A file that declares the config and then forgets to export it.
	path := writeTsConfig(t, `
const config = { logLevel: "warn" };
`)

	var cfg *Config
	var err error
	require.NotPanics(t, func() {
		cfg, err = LoadConfig(afero.NewOsFs(), path, nil)
	}, "a config with no exports must report the mistake, not crash")

	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "must be default exported from TypeScript code",
		"the operator must read the sentence that names the fix")
}

// `export default undefined` and `export default null` leave a real exports
// object behind, so they always reached the friendly error. They must keep
// reaching it, and they must report the same thing as the no-export file.
func TestLoadConfig_TypeScriptWithAnEmptyDefaultExportExplainsItself(t *testing.T) {
	for name, source := range map[string]string{
		"undefined": "export default undefined;\n",
		"null":      "export default null;\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTsConfig(t, source)

			cfg, err := LoadConfig(afero.NewOsFs(), path, nil)
			require.Error(t, err)
			assert.Nil(t, cfg)
			assert.Contains(t, err.Error(), "must be default exported from TypeScript code")
		})
	}
}

// The guard must not cost a working config its exports.
func TestRuntime_Exports_ReturnsTheObjectWhenTheScriptExportsOne(t *testing.T) {
	runtime, err := NewRuntime()
	require.NoError(t, err)

	_, err = runtime.Evaluate(`var exports = { default: { logLevel: "warn" } };`)
	require.NoError(t, err)

	exports := runtime.Exports()
	require.NotNil(t, exports)
	def := exports.Get("default")
	require.NotNil(t, def)
	assert.Equal(t, "warn", def.ToObject(runtime.VM()).Get("logLevel").String())
}

// Exports answers for a runtime whose script left no exports global at all,
// and for one that set it to null.
func TestRuntime_Exports_ReturnsNilWhenThereAreNoExports(t *testing.T) {
	for name, script := range map[string]string{
		"absent":    `var somethingElse = 1;`,
		"null":      `var exports = null;`,
		"undefined": `var exports = undefined;`,
	} {
		t.Run(name, func(t *testing.T) {
			runtime, err := NewRuntime()
			require.NoError(t, err)
			_, err = runtime.Evaluate(script)
			require.NoError(t, err)

			var exports interface{}
			require.NotPanics(t, func() {
				exports = runtime.Exports()
			})
			assert.Nil(t, exports)
		})
	}
}
