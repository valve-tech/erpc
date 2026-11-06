package erpc

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/rs/zerolog"
)

type raceTestNetwork struct{}

func (raceTestNetwork) Id() string                            { return "evm:1" }
func (raceTestNetwork) SuggestLatestBlock(string, int64) bool { return true }
func (raceTestNetwork) DeliveringHead(int64)                  {}
func (raceTestNetwork) DedupDropped(indexer.EventKind)        {}
func (raceTestNetwork) DedupEntriesAdded(int)                 {}
func (raceTestNetwork) HeadIndexed()                          {}
func (raceTestNetwork) Undelivered(indexer.EventKind)         {}

// Two eth_subscribe frames back to back (newHeads, then logs) race in
// getOrCreateConn. Every adapter for one connection has the same indexer
// name, so the losing subscribe used to overwrite the winner's
// registration and then delete it, leaving the connection with no egress:
// both subscribe replies went out, then no notification ever followed.
func TestSubscriptionManager_ConcurrentFirstSubscribesKeepEgress(t *testing.T) {
	lg := zerolog.New(io.Discard)
	for iter := 0; iter < 200; iter++ {
		idx := indexer.New(&lg, indexer.Options{})
		idx.RegisterNetwork(raceTestNetwork{})
		sm := NewSubscriptionManager(&lg, idx)

		wsc, sock := newFaultWsConnection(t)
		wsc.server = &HttpServer{serverCfg: &common.ServerConfig{WebSocket: &common.WebSocketServerConfig{}}}
		wsc.project = &PreparedProject{Config: &common.ProjectConfig{Id: "test"}}
		wsc.networkLabel = "evm:1"

		start := make(chan struct{})
		entries := make([]*connEntry, 2)
		var wg sync.WaitGroup
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				entries[g] = sm.getOrCreateConn(wsc, "evm:1")
			}(g)
		}
		close(start)
		wg.Wait()

		if entries[0] != entries[1] {
			t.Fatalf("iter %d: racing subscribes got different conn entries", iter)
		}
		if err := entries[0].adapter.AddSubscription("0xsub", "evm:1", indexer.KindNewHead, "", 10); err != nil {
			t.Fatalf("iter %d: AddSubscription: %v", iter, err)
		}

		idx.Ingest(indexer.StreamEvent{
			Kind:      indexer.KindNewHead,
			NetworkId: "evm:1",
			SourceId:  "ws:up",
			Block:     indexer.BlockRef{Number: 1, Hash: "0x01"},
			Payload:   json.RawMessage(`{"number":"0x1"}`),
		})

		deadline := time.Now().Add(2 * time.Second)
		for !strings.Contains(sock.Body(), "eth_subscription") {
			if time.Now().After(deadline) {
				t.Fatalf("iter %d: no notification reached the socket; the connection lost its egress", iter)
			}
			time.Sleep(time.Millisecond)
		}
		sm.CleanupConnection(wsc)
	}
}
