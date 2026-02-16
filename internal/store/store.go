// Package store provides a pluggable storage interface for the CodeWire relay.
// The default implementation uses SQLite (pure Go, no CGO). A PostgreSQL
// backend can be added later behind the same interface.
package store

import (
	"context"
	"time"
)

// KVEntry is a single key-value pair returned by KVList.
type KVEntry struct {
	Key       string     `json:"key"`
	Value     []byte     `json:"value"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// NodeRecord is a registered relay node.
type NodeRecord struct {
	Name         string    `json:"name"`
	PublicKey    string    `json:"public_key"`
	TunnelURL    string    `json:"tunnel_url"`
	AuthorizedAt time.Time `json:"authorized_at"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

// DeviceCode is used for the device authorization flow.
type DeviceCode struct {
	Code      string    `json:"code"`
	PublicKey string    `json:"public_key"`
	NodeName  string    `json:"node_name"`
	Status    string    `json:"status"` // "pending", "authorized"
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Store is the relay's storage interface. All methods are safe for concurrent use.
type Store interface {
	// KV store — shared across all nodes.
	KVSet(ctx context.Context, namespace, key string, value []byte, ttl *time.Duration) error
	KVGet(ctx context.Context, namespace, key string) ([]byte, error)
	KVDelete(ctx context.Context, namespace, key string) error
	KVList(ctx context.Context, namespace, prefix string) ([]KVEntry, error)

	// Node registry — internal to relay.
	NodeRegister(ctx context.Context, node NodeRecord) error
	NodeList(ctx context.Context) ([]NodeRecord, error)
	NodeGet(ctx context.Context, name string) (*NodeRecord, error)
	NodeDelete(ctx context.Context, name string) error
	NodeUpdateLastSeen(ctx context.Context, name string) error

	// Device authorization flow.
	DeviceCodeCreate(ctx context.Context, dc DeviceCode) error
	DeviceCodeGet(ctx context.Context, code string) (*DeviceCode, error)
	DeviceCodeConfirm(ctx context.Context, code string) error
	DeviceCodeCleanup(ctx context.Context) error

	// Close releases resources (e.g. closes the database).
	Close() error
}
