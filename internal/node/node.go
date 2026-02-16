package node

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"nhooyr.io/websocket"

	"github.com/codespacesh/codewire/internal/auth"
	"github.com/codespacesh/codewire/internal/config"
	"github.com/codespacesh/codewire/internal/connection"
	"github.com/codespacesh/codewire/internal/session"
)

// Node is the daemon that manages PTY sessions, accepting connections over
// a Unix domain socket and optionally a WebSocket listener.
type Node struct {
	Manager    *session.SessionManager
	socketPath string
	pidPath    string
	config     *config.Config
	dataDir    string
}

// NewNode creates a Node rooted at dataDir. It loads the configuration,
// initialises the session manager, and ensures an auth token exists on disk.
func NewNode(dataDir string) (*Node, error) {
	cfg, err := config.LoadConfig(dataDir)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	mgr, err := session.NewSessionManager(dataDir)
	if err != nil {
		return nil, fmt.Errorf("creating session manager: %w", err)
	}

	token, err := auth.LoadOrGenerateToken(dataDir)
	if err != nil {
		return nil, fmt.Errorf("loading auth token: %w", err)
	}
	slog.Info("auth token ready", "token", token)

	return &Node{
		Manager:    mgr,
		socketPath: filepath.Join(dataDir, "codewire.sock"),
		pidPath:    filepath.Join(dataDir, "codewire.pid"),
		config:     cfg,
		dataDir:    dataDir,
	}, nil
}

// Run starts the node daemon. It writes a PID file, listens on a Unix socket,
// and optionally starts a WebSocket server. It blocks until ctx is cancelled.
func (n *Node) Run(ctx context.Context) error {
	// Write PID file.
	pid := os.Getpid()
	if err := os.WriteFile(n.pidPath, []byte(fmt.Sprintf("%d", pid)), 0o644); err != nil {
		return fmt.Errorf("writing pid file: %w", err)
	}

	// Remove stale socket if it exists.
	_ = os.Remove(n.socketPath)

	ln, err := net.Listen("unix", n.socketPath)
	if err != nil {
		return fmt.Errorf("listening on unix socket: %w", err)
	}
	slog.Info("listening on unix socket", "path", n.socketPath)

	defer n.Cleanup()

	// Start WebSocket server if configured.
	if n.config.Node.Listen != nil {
		addr := *n.config.Node.Listen
		go func() {
			if wsErr := n.runWSServer(ctx, addr); wsErr != nil {
				slog.Error("websocket server error", "err", wsErr)
			}
		}()
	}

	// Start periodic status refresh (every 5 seconds).
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n.Manager.RefreshStatuses()
			}
		}
	}()

	// Start persistence manager.
	go persistenceManager(n.Manager)

	// Close the listener when ctx is cancelled so Accept unblocks.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	// Accept loop.
	for {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			// Check if we were shut down.
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			slog.Error("accept error", "err", acceptErr)
			continue
		}
		go handleClient(
			connection.NewUnixReader(conn),
			connection.NewUnixWriter(conn),
			n.Manager,
		)
	}
}

// Cleanup removes the Unix socket and PID files.
func (n *Node) Cleanup() {
	_ = os.Remove(n.socketPath)
	_ = os.Remove(n.pidPath)
}

// runWSServer starts an HTTP server that upgrades /ws connections to WebSocket
// and dispatches them through the standard client handler after validating the
// auth token.
func (n *Node) runWSServer(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if !auth.ValidateToken(n.dataDir, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		wsConn, err := websocket.Accept(w, r, nil)
		if err != nil {
			slog.Error("websocket accept error", "err", err)
			return
		}

		wsCtx := r.Context()
		reader := connection.NewWSReader(wsCtx, wsConn)
		writer := connection.NewWSWriter(wsCtx, wsConn)
		handleClient(reader, writer, n.Manager)
	})

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	slog.Info("websocket server listening", "addr", addr)

	// Shut down gracefully when ctx is cancelled.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("websocket server: %w", err)
	}
	return nil
}

// persistenceManager debounces persist signals from the session manager.
// After receiving a signal it waits 500ms for additional signals before
// flushing metadata to disk.
func persistenceManager(manager *session.SessionManager) {
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}

	pending := false

	for {
		select {
		case _, ok := <-manager.PersistCh:
			if !ok {
				// Channel closed — flush any pending write and exit.
				if pending {
					manager.PersistMeta()
				}
				return
			}
			// Reset the debounce timer. If it was already running, stop it first.
			if !timer.Stop() && pending {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(500 * time.Millisecond)
			pending = true

		case <-timer.C:
			if pending {
				manager.PersistMeta()
				pending = false
			}
		}
	}
}
