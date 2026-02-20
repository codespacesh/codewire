//go:build integration

package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	localrelay "github.com/codespacesh/codewire/internal/relay"
	"github.com/codespacesh/codewire/internal/store"
)

func TestAgentConnectsToHub(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, _ := store.NewSQLiteStore(t.TempDir())
	_ = st.NodeRegister(ctx, store.NodeRecord{Name: "n1", Token: "tok1", AuthorizedAt: time.Now(), LastSeenAt: time.Now()})

	hub := localrelay.NewNodeHub()
	sessions := localrelay.NewPendingSessions()

	mux := http.NewServeMux()
	localrelay.RegisterNodeConnectHandler(mux, hub, st)
	localrelay.RegisterBackHandler(mux, sessions, st)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Start node agent.
	go localrelay.RunAgent(ctx, localrelay.AgentConfig{
		RelayURL:  srv.URL,
		NodeName:  "n1",
		NodeToken: "tok1",
	})

	// Wait for agent to connect.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Has("n1") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !hub.Has("n1") {
		t.Fatal("agent did not connect to hub")
	}
}
