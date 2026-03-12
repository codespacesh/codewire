package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"nhooyr.io/websocket"

	"github.com/codewiresh/codewire/internal/platform"
	"github.com/codewiresh/codewire/internal/terminal"
)

func sshCmd() *cobra.Command {
	var stdio bool

	cmd := &cobra.Command{
		Use:   "ssh <env-id-or-name>",
		Short: "SSH into a running environment",
		Long: `Connect to a running sandbox environment via SSH.

Interactive mode (default):
  Connects via SSH with PTY, resize support, and Ctrl+B d to detach.

Stdio mode (--stdio):
  For use as SSH ProxyCommand. Pipes stdin/stdout directly to the SSH proxy.
  Used by: ssh cw-<envid> (via ~/.ssh/config ProxyCommand)

For VS Code Remote-SSH, run 'cw setup' to configure ~/.ssh/config.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: envCompletionFunc,
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]

			// Strip "cw-" prefix for ProxyCommand use (Host cw-*)
			if strings.HasPrefix(ref, "cw-") {
				ref = ref[3:]
			}

			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			envID, err := resolveEnvID(client, orgID, ref)
			if err != nil {
				return err
			}

			if stdio {
				return sshStdio(client, orgID, envID)
			}
			return sshInteractive(client, orgID, envID)
		},
	}

	cmd.Flags().BoolVar(&stdio, "stdio", false, "Stdio mode for ProxyCommand (pipe stdin/stdout to SSH proxy)")
	return cmd
}

// sshStdio connects to the SSH proxy WebSocket and pipes stdin/stdout.
// Used as ProxyCommand for VS Code and regular ssh client.
func sshStdio(client *platform.Client, orgID, envID string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wsURL := strings.Replace(client.ServerURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL += fmt.Sprintf("/api/v1/organizations/%s/environments/%s/ssh-proxy", orgID, envID)

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"ssh"},
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + client.SessionToken},
		},
	})
	if err != nil {
		return fmt.Errorf("ssh proxy connect: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Set up signal handler
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		cancel()
	}()

	done := make(chan error, 2)

	// stdin -> WebSocket
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if wErr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); wErr != nil {
					done <- wErr
					return
				}
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()

	// WebSocket -> stdout
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				done <- err
				return
			}
			if _, err := os.Stdout.Write(data); err != nil {
				done <- err
				return
			}
		}
	}()

	err = <-done
	if err == io.EOF {
		return nil
	}
	return err
}

// sshInteractive connects to the environment via SSH over WebSocket with a PTY.
func sshInteractive(client *platform.Client, orgID, envID string) error {
	// Check if SSH proxy is available
	available, _ := client.CheckSSHProxy(orgID, envID)

	if !available {
		// Fall back to terminal WebSocket
		fmt.Fprintln(os.Stderr, "sshd not available — using terminal fallback")
		fmt.Fprintln(os.Stderr, "For full SSH support, use a codewire base image")
		return terminalFallback(client, orgID, envID)
	}

	return sshOverWebSocket(client, orgID, envID)
}

// sshOverWebSocket establishes an SSH connection through the WebSocket proxy.
func sshOverWebSocket(client *platform.Client, orgID, envID string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wsURL := strings.Replace(client.ServerURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL += fmt.Sprintf("/api/v1/organizations/%s/environments/%s/ssh-proxy", orgID, envID)

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"ssh"},
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + client.SessionToken},
		},
	})
	if err != nil {
		return fmt.Errorf("ssh proxy connect: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Create a net.Conn-like wrapper over the WebSocket for the SSH client
	wsConn := &wsNetConn{conn: conn, ctx: ctx}

	// Find the user's SSH key
	keyPath := defaultSSHKeyPath()
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read SSH key %s: %w (run 'cw setup' to generate one)", keyPath, err)
	}

	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return fmt.Errorf("parse SSH key: %w", err)
	}

	sshConfig := &ssh.ClientConfig{
		User:            "coder",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// SSH handshake over the WebSocket
	sshConn, chans, reqs, err := ssh.NewClientConn(wsConn, "cw-"+envID, sshConfig)
	if err != nil {
		return fmt.Errorf("ssh handshake: %w", err)
	}
	defer sshConn.Close()

	sshClient := ssh.NewClient(sshConn, chans, reqs)
	defer sshClient.Close()

	session, err := sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session: %w", err)
	}
	defer session.Close()

	// Get terminal size
	cols, rows, err := terminal.TerminalSize()
	if err != nil {
		cols, rows = 80, 24
	}

	// Request PTY
	if err := session.RequestPty("xterm-256color", int(rows), int(cols), ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}); err != nil {
		return fmt.Errorf("request pty: %w", err)
	}

	// Enable raw mode
	rawGuard, err := terminal.EnableRawMode()
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer rawGuard.Restore()

	// Set up I/O with detach detection
	stdinPipe, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	// Start shell
	if err := session.Shell(); err != nil {
		return fmt.Errorf("start shell: %w", err)
	}

	// Handle SIGWINCH
	resizeCh, resizeCleanup := terminal.ResizeSignal()
	defer resizeCleanup()

	// Stdin reader with detach detection
	detach := terminal.NewDetachDetector()
	done := make(chan error, 1)

	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				detached, fwd := detach.FeedBuf(buf[:n])
				if detached {
					rawGuard.Restore()
					fmt.Fprintln(os.Stderr, "\nDetached.")
					done <- nil
					return
				}
				if len(fwd) > 0 {
					if _, wErr := stdinPipe.Write(fwd); wErr != nil {
						done <- wErr
						return
					}
				}
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()

	// Handle resize signals
	go func() {
		for range resizeCh {
			c, r, err := terminal.TerminalSize()
			if err == nil {
				session.WindowChange(int(r), int(c))
			}
		}
	}()

	// Wait for session to finish or detach
	sessionDone := make(chan error, 1)
	go func() {
		sessionDone <- session.Wait()
	}()

	select {
	case err := <-done:
		return err
	case err := <-sessionDone:
		rawGuard.Restore()
		if err != nil {
			if exitErr, ok := err.(*ssh.ExitError); ok {
				os.Exit(exitErr.ExitStatus())
			}
		}
		return nil
	}
}

// terminalFallback uses the existing terminal WebSocket for environments without sshd.
func terminalFallback(client *platform.Client, orgID, envID string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wsURL := strings.Replace(client.ServerURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL += fmt.Sprintf("/api/v1/organizations/%s/environments/%s/terminal", orgID, envID)

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"terminal"},
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + client.SessionToken},
		},
	})
	if err != nil {
		return fmt.Errorf("terminal connect: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Enable raw mode
	rawGuard, err := terminal.EnableRawMode()
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer rawGuard.Restore()

	// Send initial resize
	cols, rows, _ := terminal.TerminalSize()
	if cols > 0 && rows > 0 {
		resizeMsg := make([]byte, 5)
		resizeMsg[0] = 0x01 // msgTypeResize
		resizeMsg[1] = byte(cols >> 8)
		resizeMsg[2] = byte(cols)
		resizeMsg[3] = byte(rows >> 8)
		resizeMsg[4] = byte(rows)
		conn.Write(ctx, websocket.MessageBinary, resizeMsg)
	}

	// Handle SIGWINCH
	resizeCh, resizeCleanup := terminal.ResizeSignal()
	defer resizeCleanup()

	go func() {
		for range resizeCh {
			c, r, err := terminal.TerminalSize()
			if err == nil {
				msg := make([]byte, 5)
				msg[0] = 0x01
				msg[1] = byte(c >> 8)
				msg[2] = byte(c)
				msg[3] = byte(r >> 8)
				msg[4] = byte(r)
				conn.Write(ctx, websocket.MessageBinary, msg)
			}
		}
	}()

	done := make(chan error, 2)
	detach := terminal.NewDetachDetector()

	// stdin -> WebSocket (with terminal framing: 0x00 prefix for stdin)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				detached, fwd := detach.FeedBuf(buf[:n])
				if detached {
					rawGuard.Restore()
					fmt.Fprintln(os.Stderr, "\nDetached.")
					done <- nil
					return
				}
				if len(fwd) > 0 {
					// Prepend stdin message type
					msg := make([]byte, 1+len(fwd))
					msg[0] = 0x00 // msgTypeStdin
					copy(msg[1:], fwd)
					if wErr := conn.Write(ctx, websocket.MessageBinary, msg); wErr != nil {
						done <- wErr
						return
					}
				}
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()

	// WebSocket -> stdout (raw bytes, no framing prefix on output)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				done <- err
				return
			}
			os.Stdout.Write(data)
		}
	}()

	<-done
	return nil
}

// wsNetConn wraps a nhooyr.io/websocket.Conn to implement io.ReadWriteCloser
// for use with golang.org/x/crypto/ssh.NewClientConn.
type wsNetConn struct {
	conn   *websocket.Conn
	ctx    context.Context
	reader io.Reader
}

func (w *wsNetConn) Read(p []byte) (int, error) {
	for {
		if w.reader != nil {
			n, err := w.reader.Read(p)
			if n > 0 {
				return n, nil
			}
			if err != io.EOF {
				return 0, err
			}
			w.reader = nil
		}
		_, reader, err := w.conn.Reader(w.ctx)
		if err != nil {
			return 0, err
		}
		w.reader = reader
	}
}

func (w *wsNetConn) Write(p []byte) (int, error) {
	err := w.conn.Write(w.ctx, websocket.MessageBinary, p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *wsNetConn) Close() error {
	return w.conn.Close(websocket.StatusNormalClosure, "")
}

func (w *wsNetConn) LocalAddr() net.Addr  { return wsAddr{} }
func (w *wsNetConn) RemoteAddr() net.Addr { return wsAddr{} }

func (w *wsNetConn) SetDeadline(t time.Time) error      { return nil }
func (w *wsNetConn) SetReadDeadline(t time.Time) error   { return nil }
func (w *wsNetConn) SetWriteDeadline(t time.Time) error  { return nil }

type wsAddr struct{}

func (wsAddr) Network() string { return "websocket" }
func (wsAddr) String() string  { return "websocket" }

// defaultSSHKeyPath returns the path to the user's default SSH private key.
func defaultSSHKeyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "id_ed25519")
}
