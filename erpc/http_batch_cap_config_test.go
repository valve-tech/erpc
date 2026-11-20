package erpc

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The batch cap runs before authentication. It must read a network's limit
// from config and never build a network for an id the caller chose: building
// one calls every provider's SupportsNetwork and caches a client per chain,
// so an unauthenticated batch of unknown ids would fan out to providers.
func TestClientBatchCap_DoesNotBuildNetworksBeforeAuth(t *testing.T) {
	batchOf := func(ids ...string) string {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_chainId","params":[],"networkId":%q}`, i+1, id)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	networkCount := func(t *testing.T, e *ERPC) int {
		prj, err := e.GetProject("test_project")
		require.NoError(t, err)
		// A bootstrap attempt registers a task even when it fails, so the
		// task count sees attempts that build nothing.
		tasks := 0
		prj.networksRegistry.initializer.RangeTaskStates(func(string, util.TaskState) bool { tasks++; return true })
		return len(prj.GetNetworks())*1000 + tasks
	}

	t.Run("UnauthenticatedPathlessBatch", func(t *testing.T) {
		calls := countEvmGetBalanceCalls(t)
		cfg := clientBatchCapTestConfig()
		cfg.Projects[0].NetworkDefaults = &common.NetworkDefaults{MaxClientBatchSize: util.IntPtr(100)}
		cfg.Projects[0].Auth = &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
			{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Value: "fixture-not-a-credential"}},
		}}
		_, _, baseURL, shutdown, e := createServerTestFixtures(cfg, t)
		defer shutdown()
		time.Sleep(1500 * time.Millisecond)
		before := networkCount(t, e)

		ids := make([]string, 10)
		for i := range ids {
			ids[i] = fmt.Sprintf("evm:%d", 900001+i)
		}
		statusCode, respBody := postBody(t, baseURL+"/test_project", batchOf(ids...), nil)
		assert.Less(t, statusCode, 500, "body: %s", respBody)
		assert.Equal(t, before, networkCount(t, e), "an unauthenticated batch must build no network; body: %s", respBody)
		assert.EqualValues(t, 0, calls.Load())
	})

	t.Run("ColdNetworkInheritsNetworkDefaults", func(t *testing.T) {
		cfg := clientBatchCapTestConfig()
		cfg.Projects[0].NetworkDefaults = &common.NetworkDefaults{MaxClientBatchSize: util.IntPtr(2)}
		_, _, baseURL, shutdown, e := createServerTestFixtures(cfg, t)
		defer shutdown()
		time.Sleep(1500 * time.Millisecond)
		before := networkCount(t, e)

		statusCode, respBody := postBody(t, baseURL+"/test_project", batchOf("evm:777", "evm:777", "evm:777"), nil)
		assert.Equal(t, http.StatusBadRequest, statusCode, "body: %s", respBody)
		assertSingleBatchCapError(t, respBody, 3, 2)

		statusCode, respBody = postBody(t, baseURL+"/test_project/evm/4242", batchOf("", "", ""), nil)
		assert.Equal(t, http.StatusBadRequest, statusCode, "body: %s", respBody)
		assertSingleBatchCapError(t, respBody, 3, 2)

		assert.Equal(t, before, networkCount(t, e), "the cap check must build no network")
	})
}
