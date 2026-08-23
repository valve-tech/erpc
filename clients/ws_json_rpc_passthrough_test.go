package clients

import (
	"testing"
	"time"
)

// These cover the notification routing change behind generic (non-eth)
// subscriptions. handleNotification used to drop every frame whose method was
// not exactly "eth_subscription", so a subscription on any other namespace
// returned a live id and then delivered nothing for the life of the
// connection. Routing is now on the subscription id, and the method name rides
// along to whoever relays the frame onward.

type notification struct {
	method string
	params []byte
}

// A notification reaches its subscriber whatever the upstream calls it, and
// the name it used arrives intact.
func TestWsHandleNotification_RoutesByIdNotByMethodName(t *testing.T) {
	// Every name a real upstream has been observed to use for one namespace,
	// plus one nobody has invented yet. reth emitted `msgboard_subscribe` up
	// to v2.5.1-pulse-3 and `msgboard_subscription` from pulse-4 on, so a
	// relay that tests the name delivers on one build and silently stops on
	// the other.
	for _, method := range []string{
		"eth_subscription",
		"msgboard_subscription",
		"msgboard_subscribe",
		"some_futureNamespaceInvention",
	} {
		t.Run(method, func(t *testing.T) {
			srv := newFakeWsServer(t)
			c := newTestWsClient(t, srv.wsURL(t))

			got := make(chan notification, 1)
			c.RegisterSubscriptionHandler("0xaaa", func(m string, p []byte) {
				got <- notification{method: m, params: p}
			})

			c.handleMessage([]byte(`{"jsonrpc":"2.0","method":"` + method +
				`","params":{"subscription":"0xaaa","result":{"n":"0x1"}}}`))

			select {
			case n := <-got:
				if n.method != method {
					t.Fatalf("handler got method %q, want %q — the relay needs the name the upstream used", n.method, method)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("a notification named %q never reached its subscriber", method)
			}
		})
	}
}

// The counterweight: the id is what decides, so a frame for an id this client
// never registered is still dropped. Without this the change above would read
// as "deliver everything to everyone".
func TestWsHandleNotification_StillDropsAnUnknownSubscription(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	got := make(chan notification, 1)
	c.RegisterSubscriptionHandler("0xaaa", func(m string, p []byte) {
		got <- notification{method: m, params: p}
	})

	// Right shape, wrong id — another connection's subscription.
	c.handleMessage([]byte(`{"jsonrpc":"2.0","method":"msgboard_subscription","params":{"subscription":"0xnotours","result":{}}}`))
	// Right id, but not a notification at all: a reply carries an id.
	c.handleMessage([]byte(`{"jsonrpc":"2.0","id":7,"result":"0xaaa"}`))

	select {
	case n := <-got:
		t.Fatalf("handler fired for a frame it does not own: method=%q params=%s", n.method, n.params)
	case <-time.After(300 * time.Millisecond):
	}
}

// Unregistering must actually stop delivery. The passthrough path unregisters
// on unsubscribe and on client-connection close, and the upstream ws client
// outlives both — a handler that survived either would leak for the life of
// the process and keep writing to a connection that is gone.
func TestWsHandleNotification_StopsAfterUnregister(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	got := make(chan notification, 2)
	c.RegisterSubscriptionHandler("0xaaa", func(m string, p []byte) {
		got <- notification{method: m, params: p}
	})

	frame := []byte(`{"jsonrpc":"2.0","method":"msgboard_subscription","params":{"subscription":"0xaaa","result":{}}}`)
	c.handleMessage(frame)
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("test setup: the first notification never arrived")
	}

	c.UnregisterSubscriptionHandler("0xaaa")
	c.handleMessage(frame)

	select {
	case <-got:
		t.Fatal("a notification arrived after its handler was unregistered")
	case <-time.After(300 * time.Millisecond):
	}
}
