//go:build integration

package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/codespacesh/codewire/internal/relay"
	"github.com/codespacesh/codewire/internal/store"
)

func TestNodeConnect(t *testing.T) {
	st, _ := store.NewSQLiteStore(t.TempDir())
	defer st.Close()
	ctx := context.Background()
	_ = st.NodeRegister(ctx, store.NodeRecord{Name: "n1", Token: "tok1", AuthorizedAt: time.Now(), LastSeenAt: time.Now()})

	hub := relay.NewNodeHub()
	mux := http.NewServeMux()
	relay.RegisterNodeConnectHandler(mux, hub, st)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + srv.URL[4:] + "/node/connect"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer tok1"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// Poll briefly — handler goroutine registers asynchronously.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !hub.Has("n1") {
		time.Sleep(10 * time.Millisecond)
	}
	if !hub.Has("n1") {
		t.Fatal("expected n1 in hub")
	}

	conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(100 * time.Millisecond)
	if hub.Has("n1") {
		t.Fatal("expected n1 removed from hub after disconnect")
	}
}

func TestBackConnect(t *testing.T) {
	st, _ := store.NewSQLiteStore(t.TempDir())
	defer st.Close()
	ctx := context.Background()
	_ = st.NodeRegister(ctx, store.NodeRecord{Name: "n1", Token: "tok1", AuthorizedAt: time.Now(), LastSeenAt: time.Now()})

	sessions := relay.NewPendingSessions()
	mux := http.NewServeMux()
	relay.RegisterBackHandler(mux, sessions, st)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Pre-register a pending session channel.
	ch := sessions.Expect("sess1")

	wsURL := "ws" + srv.URL[4:] + "/node/back/sess1"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer tok1"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// Wait for the back-connection to be delivered to the pending channel.
	select {
	case nc := <-ch:
		if nc == nil {
			t.Fatal("nil connection")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for back connection")
	}
}
