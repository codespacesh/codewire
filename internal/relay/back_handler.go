package relay

import (
	"net"
	"net/http"
	"strings"
	"sync"

	"nhooyr.io/websocket"

	"github.com/codewiresh/codewire/internal/store"
)

// PendingSessions tracks back-connections that SSH sessions are waiting for.
type PendingSessions struct {
	mu    sync.Mutex
	waits map[string]chan net.Conn
}

// NewPendingSessions returns an empty PendingSessions registry.
func NewPendingSessions() *PendingSessions {
	return &PendingSessions{waits: make(map[string]chan net.Conn)}
}

// Expect registers a channel that will receive the back-connection for sessionID.
// The caller must call this before signalling the node.
func (p *PendingSessions) Expect(sessionID string) <-chan net.Conn {
	ch := make(chan net.Conn, 1)
	p.mu.Lock()
	p.waits[sessionID] = ch
	p.mu.Unlock()
	return ch
}

func (p *PendingSessions) deliver(sessionID string, conn net.Conn) bool {
	p.mu.Lock()
	ch, ok := p.waits[sessionID]
	if ok {
		delete(p.waits, sessionID)
	}
	p.mu.Unlock()
	if ok {
		ch <- conn
	}
	return ok
}

// Cancel removes a pending session and closes its channel (unblocks any waiter).
func (p *PendingSessions) Cancel(sessionID string) {
	p.mu.Lock()
	ch, ok := p.waits[sessionID]
	if ok {
		delete(p.waits, sessionID)
		close(ch)
	}
	p.mu.Unlock()
}

// DeliverForTest allows tests to inject a back-connection directly.
func (p *PendingSessions) DeliverForTest(sessionID string, conn net.Conn) {
	p.deliver(sessionID, conn)
}

// RegisterBackHandler adds GET /node/back/{session_id} to mux.
// Node agents dial here to bridge an SSH session.
func RegisterBackHandler(mux *http.ServeMux, sessions *PendingSessions, st store.Store) {
	mux.HandleFunc("GET /node/back/{session_id}", func(w http.ResponseWriter, r *http.Request) {
		// Authenticate node.
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		node, err := st.NodeGetByToken(r.Context(), token)
		if err != nil || node == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		sessionID := r.PathValue("session_id")

		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}

		// Wrap as net.Conn and deliver to waiting SSH session.
		nc := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
		if !sessions.deliver(sessionID, nc) {
			// No one waiting — session may have timed out.
			ws.Close(websocket.StatusNormalClosure, "no waiter")
			return
		}

		// nc is now owned by the SSH bridge. Block until context done
		// so the HTTP handler doesn't return (which would close the conn).
		<-r.Context().Done()
	})
}
