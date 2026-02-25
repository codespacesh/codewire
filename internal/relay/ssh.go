package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/codewiresh/codewire/internal/store"
)

// SSHServer wraps an ssh.ServerConfig with relay-specific auth and routing.
type SSHServer struct {
	config   *ssh.ServerConfig
	hub      *NodeHub
	sessions *PendingSessions
}

// NewSSHServer creates an SSH server that authenticates via node tokens.
func NewSSHServer(st store.Store, hub *NodeHub, sessions *PendingSessions) (*SSHServer, error) {
	hostKey, err := generateEd25519Key()
	if err != nil {
		return nil, fmt.Errorf("generating host key: %w", err)
	}

	srv := &SSHServer{hub: hub, sessions: sessions}

	srv.config = &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			node, err := st.NodeGetByToken(ctx, string(pass))
			if err != nil || node == nil {
				return nil, fmt.Errorf("authentication failed")
			}
			if subtle.ConstantTimeCompare([]byte(c.User()), []byte(node.Name)) != 1 {
				return nil, fmt.Errorf("username does not match node name")
			}
			return &ssh.Permissions{
				Extensions: map[string]string{"node_name": node.Name},
			}, nil
		},
	}
	srv.config.AddHostKey(hostKey)
	return srv, nil
}

// Serve accepts SSH connections on ln until ctx is cancelled.
func (s *SSHServer) Serve(ctx context.Context, ln net.Listener) {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		tc, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(ctx, tc)
	}
}

func (s *SSHServer) handleConn(ctx context.Context, tc net.Conn) {
	defer tc.Close()

	sshConn, chans, reqs, err := ssh.NewServerConn(tc, s.config)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	nodeName := sshConn.Permissions.Extensions["node_name"]

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "only session channels supported")
			continue
		}
		ch, reqs, err := newChan.Accept()
		if err != nil {
			return
		}
		go s.handleSession(ctx, ch, reqs, nodeName)
	}
}

func (s *SSHServer) handleSession(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, nodeName string) {
	defer ch.Close()

	sessionID := generateSessionID()
	var cols, rows uint32 = 80, 24

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			// pty-req payload: uint32(termLen), term string, uint32(cols), uint32(rows), ...
			if len(req.Payload) >= 4 {
				termLen := binary.BigEndian.Uint32(req.Payload[0:4])
				offset := 4 + termLen
				if int(offset+8) <= len(req.Payload) {
					cols = binary.BigEndian.Uint32(req.Payload[offset:])
					rows = binary.BigEndian.Uint32(req.Payload[offset+4:])
				}
			}
			if req.WantReply {
				req.Reply(true, nil)
			}
		case "window-change":
			// Phase 1: resize not forwarded.
			if req.WantReply {
				req.Reply(true, nil)
			}
		case "shell", "exec":
			if req.WantReply {
				req.Reply(true, nil)
			}
			s.bridgeToNode(ctx, ch, nodeName, sessionID, int(cols), int(rows))
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func (s *SSHServer) bridgeToNode(ctx context.Context, ch ssh.Channel, nodeName, sessionID string, cols, rows int) {
	// Register pending back-connection channel before signalling node.
	backCh := s.sessions.Expect(sessionID)
	defer s.sessions.Cancel(sessionID)

	// Signal node via hub.
	err := s.hub.Send(nodeName, HubMessage{
		Type:      "SSHRequest",
		SessionID: sessionID,
		Cols:      cols,
		Rows:      rows,
	})
	if err != nil {
		slog.Error("SSH: node not connected", "node", nodeName, "err", err)
		ch.Stderr().Write([]byte("node not connected\r\n"))
		return
	}

	// Wait for node's back-connection.
	var backConn net.Conn
	select {
	case conn, ok := <-backCh:
		if !ok || conn == nil {
			slog.Error("SSH: back-connection channel closed", "node", nodeName)
			return
		}
		backConn = conn
	case <-time.After(10 * time.Second):
		ch.Stderr().Write([]byte("node connection timed out\r\n"))
		return
	case <-ctx.Done():
		return
	}
	defer backConn.Close()

	slog.Info("SSH: bridging session", "node", nodeName, "session", sessionID)

	// Pipe SSH channel ↔ back-connection.
	// Wait for BOTH directions: stdin EOF fires first, then node output drains.
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(backConn, ch)
		// Signal stdin EOF to the node via PTY Ctrl-D so bash exits gracefully.
		backConn.Write([]byte{0x04})
		done <- struct{}{}
	}()
	go func() { io.Copy(ch, backConn); done <- struct{}{} }()
	select {
	case <-done:
		// One direction finished; wait for the other (with ctx as safety valve).
		select {
		case <-done:
		case <-ctx.Done():
		}
	case <-ctx.Done():
	}
}

func generateSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func generateEd25519Key() (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}
