package evm

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// A -32602 means two different things. When reth's JSON decoder rejects the
// request text itself, every node gives the same answer, so a retry only
// multiplies load. Other -32602 answers can depend on the node, so they keep
// their retry.

func TestExtractJsonRpcError_ADecoderRejectionIsNotRetried(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data string
	}{
		{"odd-length hex (reth, live 2026-10-07)", `invalid value: string "0x70a082310", expected a valid hex string at line 1 column 72`},
		{"wrong type", `invalid type: integer ` + "`5`" + `, expected a string at line 1 column 9`},
		{"wrong length", `invalid length 19, expected a 20 byte address at line 1 column 50`},
		{"missing field", `missing field ` + "`to`" + ` at line 1 column 30`},
		{"unknown variant", `unknown variant ` + "`foo`" + `, expected one of ` + "`latest`" + ` at line 1 column 12`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classify(t, 200, -32602, "Invalid params", tc.data, "eth_call")
			require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException), "got %v", err)
			require.False(t, common.IsRetryableTowardNetwork(err),
				"every node decodes the same bytes the same way; a retry cannot help")
		})
	}
}

func TestExtractJsonRpcError_ANodeSpecificInvalidParamsKeepsItsRetry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		message string
		data    interface{}
	}{
		{"bare invalid params", "Invalid params", nil},
		{"range limit worded as params", "Invalid params", "block range exceeds the node's limit of 1000"},
		{"position without a decoder phrase", "Invalid params", "something at line 1 column 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classify(t, 200, -32602, tc.message, tc.data, "eth_call")
			require.True(t, common.IsRetryableTowardNetwork(err),
				"another node may answer this one differently, got %v", err)
		})
	}
}

func TestExtractJsonRpcError_EnvioBlockObjectStillRetries(t *testing.T) {
	t.Parallel()
	// Envio rejects a {blockNumber, blockHash} object that other nodes accept.
	// It reads like a decoder error, so pin that it still fails over.
	err := classify(t, 200, -32602, "Invalid params",
		"invalid type: map, expected BlockNumber, 'latest', or 'earliest' at line 1 column 40", "eth_getBlockReceipts")
	require.True(t, common.IsRetryableTowardNetwork(err), "got %v", err)
}
