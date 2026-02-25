package relay

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"nhooyr.io/websocket"

	"github.com/codewiresh/codewire/internal/store"
)

// RegisterNodeConnectHandler adds GET /node/connect to mux.
// Nodes connect here with Authorization: Bearer <node-token>.
// The handler registers them in the hub and streams HubMessages to the node.
func RegisterNodeConnectHandler(mux *http.ServeMux, hub *NodeHub, st store.Store) {
	mux.HandleFunc("GET /node/connect", func(w http.ResponseWriter, r *http.Request) {
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

		// Upgrade to WebSocket.
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // origin check done by token auth
		})
		if err != nil {
			return
		}
		defer ws.CloseNow()

		slog.Info("node agent connected", "node", node.Name)

		// Register in hub — messages from SSH handler flow here.
		msgCh := make(chan HubMessage, 16)
		hub.Register(node.Name, msgCh)
		defer hub.Unregister(node.Name)

		_ = st.NodeUpdateLastSeen(r.Context(), node.Name)

		ctx := r.Context()

		// Write loop: relay hub messages to node.
		go func() {
			for {
				select {
				case msg, ok := <-msgCh:
					if !ok {
						return
					}
					data, _ := json.Marshal(msg)
					if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()

		// Read loop: keep connection alive (nodes may send pings or status).
		for {
			_, _, err := ws.Read(ctx)
			if err != nil {
				slog.Info("node agent disconnected", "node", node.Name, "err", err)
				return
			}
		}
	})
}

// nodeAuthFromRequest extracts and validates the node token from the
// Authorization header. Returns the NodeRecord or nil if unauthorized.
func nodeAuthFromRequest(r *http.Request, st store.Store) (*store.NodeRecord, error) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		return nil, nil
	}
	return st.NodeGetByToken(r.Context(), token)
}

// nodeAuthMiddleware wraps h with node token authentication.
// It sets a node name context value and calls h if authenticated.
func nodeAuthMiddleware(st store.Store, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		node, err := nodeAuthFromRequest(r, st)
		if err != nil || node == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), nodeContextKey{}, node.Name)))
	}
}

type nodeContextKey struct{}
