package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore implements Store using an embedded SQLite database.
// It uses modernc.org/sqlite which is pure Go (no CGO).
type SQLiteStore struct {
	db      *sql.DB
	mu      sync.RWMutex // serializes writes (SQLite is single-writer)
	closeCh chan struct{}
}

// NewSQLiteStore opens or creates a SQLite database at dataDir/relay.db
// and runs schema migrations.
func NewSQLiteStore(dataDir string) (*SQLiteStore, error) {
	dbPath := filepath.Join(dataDir, "relay.db")
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}

	// Single connection for writes to avoid SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	s := &SQLiteStore{
		db:      db,
		closeCh: make(chan struct{}),
	}

	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating sqlite: %w", err)
	}

	// Start background cleanup goroutine.
	go s.cleanupLoop()

	return s, nil
}

func (s *SQLiteStore) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS nodes (
			name TEXT PRIMARY KEY,
			public_key TEXT NOT NULL UNIQUE,
			tunnel_url TEXT NOT NULL,
			authorized_at DATETIME NOT NULL,
			last_seen_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS kv (
			namespace TEXT NOT NULL,
			key TEXT NOT NULL,
			value BLOB NOT NULL,
			expires_at DATETIME,
			PRIMARY KEY (namespace, key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kv_expires ON kv(expires_at) WHERE expires_at IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS device_codes (
			code TEXT PRIMARY KEY,
			public_key TEXT NOT NULL,
			node_name TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			created_at DATETIME NOT NULL,
			expires_at DATETIME NOT NULL
		)`,
	}

	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("executing migration: %w", err)
		}
	}
	return nil
}

// cleanupLoop periodically removes expired KV entries and device codes.
func (s *SQLiteStore) cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.closeCh:
			return
		case <-ticker.C:
			now := time.Now().UTC()
			s.mu.Lock()
			s.db.Exec("DELETE FROM kv WHERE expires_at IS NOT NULL AND expires_at < ?", now)
			s.db.Exec("DELETE FROM device_codes WHERE expires_at < ?", now)
			s.mu.Unlock()
		}
	}
}

// --- KV Store ---

func (s *SQLiteStore) KVSet(_ context.Context, namespace, key string, value []byte, ttl *time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var expiresAt *time.Time
	if ttl != nil {
		t := time.Now().UTC().Add(*ttl)
		expiresAt = &t
	}

	_, err := s.db.Exec(
		`INSERT INTO kv (namespace, key, value, expires_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (namespace, key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at`,
		namespace, key, value, expiresAt,
	)
	return err
}

func (s *SQLiteStore) KVGet(_ context.Context, namespace, key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var value []byte
	err := s.db.QueryRow(
		"SELECT value FROM kv WHERE namespace = ? AND key = ? AND (expires_at IS NULL OR expires_at > ?)",
		namespace, key, time.Now().UTC(),
	).Scan(&value)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return value, err
}

func (s *SQLiteStore) KVDelete(_ context.Context, namespace, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM kv WHERE namespace = ? AND key = ?", namespace, key)
	return err
}

func (s *SQLiteStore) KVList(_ context.Context, namespace, prefix string) ([]KVEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		"SELECT key, value, expires_at FROM kv WHERE namespace = ? AND key LIKE ? AND (expires_at IS NULL OR expires_at > ?)",
		namespace, prefix+"%", time.Now().UTC(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []KVEntry
	for rows.Next() {
		var e KVEntry
		if err := rows.Scan(&e.Key, &e.Value, &e.ExpiresAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// --- Node Registry ---

func (s *SQLiteStore) NodeRegister(_ context.Context, node NodeRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT INTO nodes (name, public_key, tunnel_url, authorized_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (name) DO UPDATE SET
		   public_key = excluded.public_key,
		   tunnel_url = excluded.tunnel_url,
		   last_seen_at = excluded.last_seen_at`,
		node.Name, node.PublicKey, node.TunnelURL, node.AuthorizedAt, node.LastSeenAt,
	)
	return err
}

func (s *SQLiteStore) NodeList(_ context.Context) ([]NodeRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT name, public_key, tunnel_url, authorized_at, last_seen_at FROM nodes ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []NodeRecord
	for rows.Next() {
		var n NodeRecord
		if err := rows.Scan(&n.Name, &n.PublicKey, &n.TunnelURL, &n.AuthorizedAt, &n.LastSeenAt); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func (s *SQLiteStore) NodeGet(_ context.Context, name string) (*NodeRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var n NodeRecord
	err := s.db.QueryRow(
		"SELECT name, public_key, tunnel_url, authorized_at, last_seen_at FROM nodes WHERE name = ?",
		name,
	).Scan(&n.Name, &n.PublicKey, &n.TunnelURL, &n.AuthorizedAt, &n.LastSeenAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *SQLiteStore) NodeDelete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM nodes WHERE name = ?", name)
	return err
}

func (s *SQLiteStore) NodeUpdateLastSeen(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("UPDATE nodes SET last_seen_at = ? WHERE name = ?", time.Now().UTC(), name)
	return err
}

// --- Device Codes ---

func (s *SQLiteStore) DeviceCodeCreate(_ context.Context, dc DeviceCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT INTO device_codes (code, public_key, node_name, status, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)",
		dc.Code, dc.PublicKey, dc.NodeName, dc.Status, dc.CreatedAt, dc.ExpiresAt,
	)
	return err
}

func (s *SQLiteStore) DeviceCodeGet(_ context.Context, code string) (*DeviceCode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var dc DeviceCode
	err := s.db.QueryRow(
		"SELECT code, public_key, node_name, status, created_at, expires_at FROM device_codes WHERE code = ? AND expires_at > ?",
		code, time.Now().UTC(),
	).Scan(&dc.Code, &dc.PublicKey, &dc.NodeName, &dc.Status, &dc.CreatedAt, &dc.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &dc, nil
}

func (s *SQLiteStore) DeviceCodeConfirm(_ context.Context, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(
		"UPDATE device_codes SET status = 'authorized' WHERE code = ? AND status = 'pending' AND expires_at > ?",
		code, time.Now().UTC(),
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("device code not found or already confirmed")
	}
	return nil
}

func (s *SQLiteStore) DeviceCodeCleanup(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM device_codes WHERE expires_at < ?", time.Now().UTC())
	return err
}

// Close shuts down the cleanup goroutine and closes the database.
func (s *SQLiteStore) Close() error {
	close(s.closeCh)
	return s.db.Close()
}
