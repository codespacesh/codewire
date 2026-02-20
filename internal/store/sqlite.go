package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
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
			token TEXT NOT NULL UNIQUE,
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
		`CREATE TABLE IF NOT EXISTS github_app (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			app_id INTEGER NOT NULL,
			client_id TEXT NOT NULL,
			client_secret TEXT NOT NULL,
			pem TEXT NOT NULL,
			webhook_secret TEXT NOT NULL DEFAULT '',
			owner TEXT NOT NULL,
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			github_id INTEGER PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			avatar_url TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			last_login_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			github_id INTEGER NOT NULL REFERENCES users(github_id),
			created_at DATETIME NOT NULL,
			expires_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS oauth_state (
			state TEXT PRIMARY KEY,
			created_at DATETIME NOT NULL,
			expires_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS invites (
			token TEXT PRIMARY KEY,
			created_by INTEGER REFERENCES users(github_id),
			uses_remaining INTEGER NOT NULL DEFAULT 1,
			expires_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS revoked_keys (
			public_key TEXT PRIMARY KEY,
			revoked_at DATETIME NOT NULL,
			reason TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS oidc_users (
			sub          TEXT PRIMARY KEY,
			username     TEXT NOT NULL,
			avatar_url   TEXT NOT NULL DEFAULT '',
			created_at   DATETIME NOT NULL,
			last_login_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS oidc_sessions (
			token      TEXT PRIMARY KEY,
			sub        TEXT NOT NULL REFERENCES oidc_users(sub) ON DELETE CASCADE,
			created_at DATETIME NOT NULL,
			expires_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS oidc_device_flows (
			poll_token  TEXT PRIMARY KEY,
			device_code TEXT NOT NULL UNIQUE,
			node_name   TEXT NOT NULL DEFAULT '',
			node_token  TEXT NOT NULL DEFAULT '',
			expires_at  DATETIME NOT NULL
		)`,
	}

	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("executing migration: %w", err)
		}
	}

	// Add columns that may not exist in older databases.
	s.addColumnIfNotExists("nodes", "github_id", "INTEGER REFERENCES users(github_id)")
	// token column replaces public_key/tunnel_url in the new relay architecture.
	s.addColumnIfNotExists("nodes", "token", "TEXT NOT NULL DEFAULT ''")

	// Ensure unique index on token for NodeGetByToken.
	s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_token ON nodes(token) WHERE token != ''`)

	return nil
}

// addColumnIfNotExists attempts to add a column to a table, ignoring the error
// if the column already exists (SQLite returns "duplicate column name").
func (s *SQLiteStore) addColumnIfNotExists(table, column, colType string) {
	_, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, colType))
	if err != nil && strings.Contains(err.Error(), "duplicate column") {
		return
	}
}

// cleanupLoop periodically removes expired KV entries, device codes, sessions,
// OAuth state parameters, and invites.
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
			s.db.Exec("DELETE FROM sessions WHERE expires_at < ?", now)
			s.db.Exec("DELETE FROM oauth_state WHERE expires_at < ?", now)
			s.db.Exec("DELETE FROM invites WHERE expires_at < ?", now)
			s.db.Exec("DELETE FROM oidc_sessions WHERE expires_at < ?", now)
			s.db.Exec("DELETE FROM oidc_device_flows WHERE expires_at < ?", now)
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
		`INSERT INTO nodes (name, token, github_id, authorized_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (name) DO UPDATE SET
		   token = excluded.token,
		   github_id = excluded.github_id,
		   last_seen_at = excluded.last_seen_at`,
		node.Name, node.Token, node.GitHubID, node.AuthorizedAt, node.LastSeenAt,
	)
	return err
}

func (s *SQLiteStore) NodeList(_ context.Context) ([]NodeRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT name, token, github_id, authorized_at, last_seen_at FROM nodes ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []NodeRecord
	for rows.Next() {
		var n NodeRecord
		if err := rows.Scan(&n.Name, &n.Token, &n.GitHubID, &n.AuthorizedAt, &n.LastSeenAt); err != nil {
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
		"SELECT name, token, github_id, authorized_at, last_seen_at FROM nodes WHERE name = ?",
		name,
	).Scan(&n.Name, &n.Token, &n.GitHubID, &n.AuthorizedAt, &n.LastSeenAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *SQLiteStore) NodeGetByToken(_ context.Context, token string) (*NodeRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var n NodeRecord
	err := s.db.QueryRow(
		"SELECT name, token, github_id, authorized_at, last_seen_at FROM nodes WHERE token = ?",
		token,
	).Scan(&n.Name, &n.Token, &n.GitHubID, &n.AuthorizedAt, &n.LastSeenAt)
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

// --- GitHub App (singleton) ---

func (s *SQLiteStore) GitHubAppGet(_ context.Context) (*GitHubApp, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var app GitHubApp
	err := s.db.QueryRow(
		"SELECT app_id, client_id, client_secret, pem, webhook_secret, owner, created_at FROM github_app WHERE id = 1",
	).Scan(&app.AppID, &app.ClientID, &app.ClientSecret, &app.PEM, &app.WebhookSecret, &app.Owner, &app.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &app, nil
}

func (s *SQLiteStore) GitHubAppSet(_ context.Context, app GitHubApp) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT INTO github_app (id, app_id, client_id, client_secret, pem, webhook_secret, owner, created_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
		   app_id = excluded.app_id,
		   client_id = excluded.client_id,
		   client_secret = excluded.client_secret,
		   pem = excluded.pem,
		   webhook_secret = excluded.webhook_secret,
		   owner = excluded.owner`,
		app.AppID, app.ClientID, app.ClientSecret, app.PEM, app.WebhookSecret, app.Owner, app.CreatedAt,
	)
	return err
}

// --- Users ---

func (s *SQLiteStore) UserUpsert(_ context.Context, user User) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT INTO users (github_id, username, avatar_url, created_at, last_login_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (github_id) DO UPDATE SET
		   username = excluded.username,
		   avatar_url = excluded.avatar_url,
		   last_login_at = excluded.last_login_at`,
		user.GitHubID, user.Username, user.AvatarURL, user.CreatedAt, user.LastLoginAt,
	)
	return err
}

func (s *SQLiteStore) UserGetByID(_ context.Context, githubID int64) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var u User
	err := s.db.QueryRow(
		"SELECT github_id, username, avatar_url, created_at, last_login_at FROM users WHERE github_id = ?",
		githubID,
	).Scan(&u.GitHubID, &u.Username, &u.AvatarURL, &u.CreatedAt, &u.LastLoginAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *SQLiteStore) UserGetByUsername(_ context.Context, username string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var u User
	err := s.db.QueryRow(
		"SELECT github_id, username, avatar_url, created_at, last_login_at FROM users WHERE username = ?",
		username,
	).Scan(&u.GitHubID, &u.Username, &u.AvatarURL, &u.CreatedAt, &u.LastLoginAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// --- Sessions ---

func (s *SQLiteStore) SessionCreate(_ context.Context, sess Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT INTO sessions (token, github_id, created_at, expires_at) VALUES (?, ?, ?, ?)",
		sess.Token, sess.GitHubID, sess.CreatedAt, sess.ExpiresAt,
	)
	return err
}

func (s *SQLiteStore) SessionGet(_ context.Context, token string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var sess Session
	err := s.db.QueryRow(
		"SELECT token, github_id, created_at, expires_at FROM sessions WHERE token = ? AND expires_at > ?",
		token, time.Now().UTC(),
	).Scan(&sess.Token, &sess.GitHubID, &sess.CreatedAt, &sess.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *SQLiteStore) SessionDelete(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM sessions WHERE token = ?", token)
	return err
}

func (s *SQLiteStore) SessionDeleteByUser(_ context.Context, githubID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM sessions WHERE github_id = ?", githubID)
	return err
}

// --- OAuth State ---

func (s *SQLiteStore) OAuthStateCreate(_ context.Context, state OAuthState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT INTO oauth_state (state, created_at, expires_at) VALUES (?, ?, ?)",
		state.State, state.CreatedAt, state.ExpiresAt,
	)
	return err
}

func (s *SQLiteStore) OAuthStateConsume(_ context.Context, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(
		"DELETE FROM oauth_state WHERE state = ? AND expires_at > ?",
		state, time.Now().UTC(),
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("oauth state not found or expired")
	}
	return nil
}

// --- Invites ---

func (s *SQLiteStore) InviteCreate(_ context.Context, invite Invite) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT INTO invites (token, created_by, uses_remaining, expires_at, created_at) VALUES (?, ?, ?, ?, ?)",
		invite.Token, invite.CreatedBy, invite.UsesRemaining, invite.ExpiresAt, invite.CreatedAt,
	)
	return err
}

func (s *SQLiteStore) InviteGet(_ context.Context, token string) (*Invite, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var inv Invite
	err := s.db.QueryRow(
		"SELECT token, created_by, uses_remaining, expires_at, created_at FROM invites WHERE token = ? AND expires_at > ?",
		token, time.Now().UTC(),
	).Scan(&inv.Token, &inv.CreatedBy, &inv.UsesRemaining, &inv.ExpiresAt, &inv.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

func (s *SQLiteStore) InviteConsume(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()

	// Decrement uses_remaining for a valid, non-expired invite with remaining uses.
	res, err := s.db.Exec(
		"UPDATE invites SET uses_remaining = uses_remaining - 1 WHERE token = ? AND uses_remaining > 0 AND expires_at > ?",
		token, now,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("invite not found, expired, or no uses remaining")
	}

	// If uses_remaining reached 0, delete the row.
	s.db.Exec("DELETE FROM invites WHERE token = ? AND uses_remaining <= 0", token)
	return nil
}

func (s *SQLiteStore) InviteList(_ context.Context) ([]Invite, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		"SELECT token, created_by, uses_remaining, expires_at, created_at FROM invites WHERE expires_at > ? ORDER BY created_at",
		time.Now().UTC(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var invites []Invite
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.Token, &inv.CreatedBy, &inv.UsesRemaining, &inv.ExpiresAt, &inv.CreatedAt); err != nil {
			return nil, err
		}
		invites = append(invites, inv)
	}
	return invites, rows.Err()
}

func (s *SQLiteStore) InviteDelete(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM invites WHERE token = ?", token)
	return err
}

// --- Revoked Keys ---

func (s *SQLiteStore) RevokedKeyAdd(_ context.Context, key RevokedKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT INTO revoked_keys (public_key, revoked_at, reason) VALUES (?, ?, ?)
		 ON CONFLICT (public_key) DO UPDATE SET revoked_at = excluded.revoked_at, reason = excluded.reason`,
		key.PublicKey, key.RevokedAt, key.Reason,
	)
	return err
}

func (s *SQLiteStore) RevokedKeyCheck(_ context.Context, publicKey string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM revoked_keys WHERE public_key = ?", publicKey).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// --- OIDC Users ---

func (s *SQLiteStore) OIDCUserUpsert(_ context.Context, user OIDCUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT INTO oidc_users (sub, username, avatar_url, created_at, last_login_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (sub) DO UPDATE SET
		   username = excluded.username,
		   avatar_url = excluded.avatar_url,
		   last_login_at = excluded.last_login_at`,
		user.Sub, user.Username, user.AvatarURL, user.CreatedAt, user.LastLoginAt,
	)
	return err
}

func (s *SQLiteStore) OIDCUserGetBySub(_ context.Context, sub string) (*OIDCUser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var u OIDCUser
	err := s.db.QueryRow(
		"SELECT sub, username, avatar_url, created_at, last_login_at FROM oidc_users WHERE sub = ?",
		sub,
	).Scan(&u.Sub, &u.Username, &u.AvatarURL, &u.CreatedAt, &u.LastLoginAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// --- OIDC Sessions ---

func (s *SQLiteStore) OIDCSessionCreate(_ context.Context, sess OIDCSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT INTO oidc_sessions (token, sub, created_at, expires_at) VALUES (?, ?, ?, ?)",
		sess.Token, sess.Sub, sess.CreatedAt, sess.ExpiresAt,
	)
	return err
}

func (s *SQLiteStore) OIDCSessionGet(_ context.Context, token string) (*OIDCSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var sess OIDCSession
	err := s.db.QueryRow(
		"SELECT token, sub, created_at, expires_at FROM oidc_sessions WHERE token = ? AND expires_at > ?",
		token, time.Now().UTC(),
	).Scan(&sess.Token, &sess.Sub, &sess.CreatedAt, &sess.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *SQLiteStore) OIDCSessionDelete(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM oidc_sessions WHERE token = ?", token)
	return err
}

// --- OIDC Device Flows ---

func (s *SQLiteStore) OIDCDeviceFlowCreate(_ context.Context, flow OIDCDeviceFlow) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT INTO oidc_device_flows (poll_token, device_code, node_name, node_token, expires_at) VALUES (?, ?, ?, ?, ?)",
		flow.PollToken, flow.DeviceCode, flow.NodeName, flow.NodeToken, flow.ExpiresAt,
	)
	return err
}

func (s *SQLiteStore) OIDCDeviceFlowGet(_ context.Context, pollToken string) (*OIDCDeviceFlow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var flow OIDCDeviceFlow
	err := s.db.QueryRow(
		"SELECT poll_token, device_code, node_name, node_token, expires_at FROM oidc_device_flows WHERE poll_token = ? AND expires_at > ?",
		pollToken, time.Now().UTC(),
	).Scan(&flow.PollToken, &flow.DeviceCode, &flow.NodeName, &flow.NodeToken, &flow.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &flow, nil
}

func (s *SQLiteStore) OIDCDeviceFlowComplete(_ context.Context, pollToken, nodeToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		"UPDATE oidc_device_flows SET node_token = ? WHERE poll_token = ?",
		nodeToken, pollToken,
	)
	return err
}

// Close shuts down the cleanup goroutine and closes the database.
func (s *SQLiteStore) Close() error {
	close(s.closeCh)
	return s.db.Close()
}
