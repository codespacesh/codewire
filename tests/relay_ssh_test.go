//go:build integration

package tests

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	localrelay "github.com/codewiresh/codewire/internal/relay"
	"github.com/codewiresh/codewire/internal/store"
)

func TestSSHConnectAndShell(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Setup store with a node.
	st, _ := store.NewSQLiteStore(t.TempDir())
	_ = st.NodeRegister(ctx, store.NodeRecord{Name: "n1", Token: "tok1", AuthorizedAt: time.Now(), LastSeenAt: time.Now()})

	hub := localrelay.NewNodeHub()
	sessions := localrelay.NewPendingSessions()

	// Start the SSH server.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	sshSrv, err := localrelay.NewSSHServer(st, hub, sessions)
	if err != nil {
		t.Fatal(err)
	}
	go sshSrv.Serve(ctx, ln)

	// Simulate a node back-connecting (echo server).
	// When hub receives SSHRequest for n1, the node dials back.
	msgCh := make(chan localrelay.HubMessage, 4)
	hub.Register("n1", msgCh)
	go func() {
		for msg := range msgCh {
			if msg.Type == "SSHRequest" {
				// Simulate node: create a pipe and deliver as back-connection.
				client, server := net.Pipe()
				sessions.DeliverForTest(msg.SessionID, server)
				// Echo everything back.
				go func() {
					buf := make([]byte, 4096)
					for {
						n, err := client.Read(buf)
						if err != nil {
							return
						}
						client.Write(buf[:n])
					}
				}()
			}
		}
	}()

	// SSH client connects.
	sshCfg := &ssh.ClientConfig{
		User:            "n1",
		Auth:            []ssh.AuthMethod{ssh.Password("tok1")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	client, err := ssh.Dial("tcp", ln.Addr().String(), sshCfg)
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("ssh session: %v", err)
	}
	defer sess.Close()

	sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{})
	if err := sess.Shell(); err != nil {
		t.Fatalf("ssh shell: %v", err)
	}
	t.Log("SSH shell started successfully")
}
