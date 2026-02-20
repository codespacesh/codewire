package relay

import (
	"fmt"
	"sync"
)

// HubMessage is a control message sent to a connected node agent.
type HubMessage struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
}

// NodeHub tracks connected node agents (in-memory).
type NodeHub struct {
	mu    sync.RWMutex
	nodes map[string]chan<- HubMessage
}

func NewNodeHub() *NodeHub {
	return &NodeHub{nodes: make(map[string]chan<- HubMessage)}
}

func (h *NodeHub) Register(name string, ch chan<- HubMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nodes[name] = ch
}

func (h *NodeHub) Unregister(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.nodes, name)
}

func (h *NodeHub) Has(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.nodes[name]
	return ok
}

// Send delivers a message to the named node. Returns error if node not connected.
func (h *NodeHub) Send(name string, msg HubMessage) error {
	h.mu.RLock()
	ch, ok := h.nodes[name]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("node %q not connected", name)
	}
	select {
	case ch <- msg:
		return nil
	default:
		return fmt.Errorf("node %q message buffer full", name)
	}
}
