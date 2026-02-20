//go:build integration

package tests

import (
	"bytes"
	"context"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	localrelay "github.com/codespacesh/codewire/internal/relay"
	"github.com/codespacesh/codewire/internal/store"
)

func TestRelayE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Setup relay components.
	st, _ := store.NewSQLiteStore(t.TempDir())
	_ = st.NodeRegister(ctx, store.NodeRecord{Name: "n1", Token: "tok1", AuthorizedAt: time.Now(), LastSeenAt: time.Now()})

	hub := localrelay.NewNodeHub()
	sessions := localrelay.NewPendingSessions()

	// HTTP server (node connect + back endpoints).
	httpMux := localrelay.BuildRelayMux(hub, sessions, st)
	httpSrv := httptest.NewServer(httpMux)
	defer httpSrv.Close()

	// SSH server.
	sshSrv, err := localrelay.NewSSHServer(st, hub, sessions)
	if err != nil {
		t.Fatalf("creating SSH server: %v", err)
	}
	sshLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go sshSrv.Serve(ctx, sshLn)

	// Node agent connects to HTTP server.
	go localrelay.RunAgent(ctx, localrelay.AgentConfig{
		RelayURL:  httpSrv.URL,
		NodeName:  "n1",
		NodeToken: "tok1",
	})

	// Wait for agent to appear in hub.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !hub.Has("n1") {
		time.Sleep(20 * time.Millisecond)
	}
	if !hub.Has("n1") {
		t.Fatal("agent did not connect")
	}

	// SSH client connects to SSH server.
	sshCfg := &ssh.ClientConfig{
		User:            "n1",
		Auth:            []ssh.AuthMethod{ssh.Password("tok1")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	client, err := ssh.Dial("tcp", sshLn.Addr().String(), sshCfg)
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("ssh session: %v", err)
	}
	defer sess.Close()

	if err := sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatalf("pty request: %v", err)
	}

	// Pipe stdin/stdout through the session — send a command and exit.
	var stdout bytes.Buffer
	sess.Stdout = &stdout
	sess.Stdin = bytes.NewBufferString("echo hello-relay\nexit\n")

	if err := sess.Shell(); err != nil {
		t.Fatalf("ssh shell: %v", err)
	}

	// Wait for session to finish.
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for shell to exit")
	}

	if !bytes.Contains(stdout.Bytes(), []byte("hello-relay")) {
		t.Fatalf("expected 'hello-relay' in output, got: %q", stdout.String())
	}
}
