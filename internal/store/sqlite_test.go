package store

import (
	"context"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSQLiteStore(dir)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestKVSetGetDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Get non-existent key returns nil.
	val, err := s.KVGet(ctx, "ns", "key1")
	if err != nil {
		t.Fatal(err)
	}
	if val != nil {
		t.Fatalf("expected nil, got %v", val)
	}

	// Set and get.
	if err := s.KVSet(ctx, "ns", "key1", []byte("value1"), nil); err != nil {
		t.Fatal(err)
	}
	val, err = s.KVGet(ctx, "ns", "key1")
	if err != nil {
		t.Fatal(err)
	}
	if string(val) != "value1" {
		t.Fatalf("expected value1, got %s", val)
	}

	// Overwrite.
	if err := s.KVSet(ctx, "ns", "key1", []byte("value2"), nil); err != nil {
		t.Fatal(err)
	}
	val, err = s.KVGet(ctx, "ns", "key1")
	if err != nil {
		t.Fatal(err)
	}
	if string(val) != "value2" {
		t.Fatalf("expected value2, got %s", val)
	}

	// Delete.
	if err := s.KVDelete(ctx, "ns", "key1"); err != nil {
		t.Fatal(err)
	}
	val, err = s.KVGet(ctx, "ns", "key1")
	if err != nil {
		t.Fatal(err)
	}
	if val != nil {
		t.Fatalf("expected nil after delete, got %v", val)
	}
}

func TestKVTTL(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Set with a TTL long enough to survive the immediate read.
	ttl := 2 * time.Second
	if err := s.KVSet(ctx, "ns", "expiring", []byte("gone"), &ttl); err != nil {
		t.Fatal(err)
	}

	// Should exist immediately (well within 2s TTL).
	val, err := s.KVGet(ctx, "ns", "expiring")
	if err != nil {
		t.Fatal(err)
	}
	if val == nil {
		t.Fatal("expected value before expiry")
	}

	// Now set a very short TTL and wait for it to expire.
	ttl = time.Millisecond
	if err := s.KVSet(ctx, "ns", "expiring", []byte("gone"), &ttl); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	val, err = s.KVGet(ctx, "ns", "expiring")
	if err != nil {
		t.Fatal(err)
	}
	if val != nil {
		t.Fatalf("expected nil after TTL expiry, got %s", val)
	}
}

func TestKVNamespaceIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.KVSet(ctx, "ns1", "key", []byte("v1"), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.KVSet(ctx, "ns2", "key", []byte("v2"), nil); err != nil {
		t.Fatal(err)
	}

	val1, _ := s.KVGet(ctx, "ns1", "key")
	val2, _ := s.KVGet(ctx, "ns2", "key")
	if string(val1) != "v1" || string(val2) != "v2" {
		t.Fatalf("namespace isolation failed: ns1=%s ns2=%s", val1, val2)
	}
}

func TestKVList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.KVSet(ctx, "ns", "task:1", []byte("a"), nil)
	s.KVSet(ctx, "ns", "task:2", []byte("b"), nil)
	s.KVSet(ctx, "ns", "other", []byte("c"), nil)

	entries, err := s.KVList(ctx, "ns", "task:")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// All entries.
	all, err := s.KVList(ctx, "ns", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(all))
	}
}

func TestNodeCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	node := NodeRecord{
		Name:         "dev-1",
		Token:        "abc123token",
		AuthorizedAt: now,
		LastSeenAt:   now,
	}

	// Register.
	if err := s.NodeRegister(ctx, node); err != nil {
		t.Fatal(err)
	}

	// Get.
	got, err := s.NodeGet(ctx, "dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "dev-1" || got.Token != "abc123token" {
		t.Fatalf("unexpected node: %+v", got)
	}

	// List.
	nodes, err := s.NodeList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}

	// Update last seen.
	time.Sleep(time.Millisecond)
	if err := s.NodeUpdateLastSeen(ctx, "dev-1"); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.NodeGet(ctx, "dev-1")
	if !got2.LastSeenAt.After(got.LastSeenAt) {
		t.Fatal("last_seen_at not updated")
	}

	// Re-register (upsert) with updated token.
	node.Token = "updatedtoken"
	if err := s.NodeRegister(ctx, node); err != nil {
		t.Fatal(err)
	}
	got3, _ := s.NodeGet(ctx, "dev-1")
	if got3.Token != "updatedtoken" {
		t.Fatalf("upsert failed: %s", got3.Token)
	}

	// Delete.
	if err := s.NodeDelete(ctx, "dev-1"); err != nil {
		t.Fatal(err)
	}
	got4, err := s.NodeGet(ctx, "dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if got4 != nil {
		t.Fatal("expected nil after delete")
	}

	// Get non-existent.
	got5, err := s.NodeGet(ctx, "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if got5 != nil {
		t.Fatal("expected nil for nonexistent node")
	}
}

func TestNodeToken(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.NodeRegister(ctx, NodeRecord{
		Name:         "mynode",
		Token:        "secrettoken",
		AuthorizedAt: time.Now(),
		LastSeenAt:   time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.NodeGetByToken(ctx, "secrettoken")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "mynode" {
		t.Fatalf("expected mynode, got %+v", got)
	}

	got2, err := s.NodeGetByToken(ctx, "wrongtoken")
	if err != nil {
		t.Fatal(err)
	}
	if got2 != nil {
		t.Fatalf("expected nil for wrong token, got %+v", got2)
	}
}

func TestDeviceCodeFlow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	dc := DeviceCode{
		Code:      "CW-ABCD-1234",
		PublicKey: "pubkey123",
		NodeName:  "dev-1",
		Status:    "pending",
		CreatedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
	}

	// Create.
	if err := s.DeviceCodeCreate(ctx, dc); err != nil {
		t.Fatal(err)
	}

	// Get.
	got, err := s.DeviceCodeGet(ctx, "CW-ABCD-1234")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != "pending" {
		t.Fatalf("unexpected: %+v", got)
	}

	// Confirm.
	if err := s.DeviceCodeConfirm(ctx, "CW-ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.DeviceCodeGet(ctx, "CW-ABCD-1234")
	if got2.Status != "authorized" {
		t.Fatalf("expected authorized, got %s", got2.Status)
	}

	// Double confirm fails.
	if err := s.DeviceCodeConfirm(ctx, "CW-ABCD-1234"); err == nil {
		t.Fatal("expected error on double confirm")
	}

	// Get non-existent.
	got3, err := s.DeviceCodeGet(ctx, "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if got3 != nil {
		t.Fatal("expected nil for nonexistent code")
	}
}

func TestDeviceCodeExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	dc := DeviceCode{
		Code:      "CW-EXPIRE-TEST",
		PublicKey: "pubkey",
		NodeName:  "node",
		Status:    "pending",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Millisecond),
	}

	if err := s.DeviceCodeCreate(ctx, dc); err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)

	got, err := s.DeviceCodeGet(ctx, "CW-EXPIRE-TEST")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("expected nil for expired code")
	}
}

func TestOIDCUserUpsertAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	user := OIDCUser{
		Sub:         "CgVub2VsEgZnaXRlYQ",
		Username:    "noel",
		AvatarURL:   "https://example.com/avatar.png",
		CreatedAt:   time.Now().UTC().Truncate(time.Second),
		LastLoginAt: time.Now().UTC().Truncate(time.Second),
	}

	if err := s.OIDCUserUpsert(ctx, user); err != nil {
		t.Fatalf("OIDCUserUpsert: %v", err)
	}

	got, err := s.OIDCUserGetBySub(ctx, user.Sub)
	if err != nil {
		t.Fatalf("OIDCUserGetBySub: %v", err)
	}
	if got == nil {
		t.Fatal("expected user, got nil")
	}
	if got.Username != "noel" {
		t.Errorf("username = %q, want %q", got.Username, "noel")
	}

	// Test the update path: upsert with the same sub but different username and last_login_at.
	updatedLoginAt := user.LastLoginAt.Add(time.Hour)
	updated := OIDCUser{
		Sub:         user.Sub,
		Username:    "noel-updated",
		AvatarURL:   user.AvatarURL,
		CreatedAt:   user.CreatedAt,
		LastLoginAt: updatedLoginAt,
	}
	if err := s.OIDCUserUpsert(ctx, updated); err != nil {
		t.Fatalf("OIDCUserUpsert (update): %v", err)
	}
	got2, err := s.OIDCUserGetBySub(ctx, user.Sub)
	if err != nil {
		t.Fatalf("OIDCUserGetBySub after update: %v", err)
	}
	if got2 == nil {
		t.Fatal("expected user after update, got nil")
	}
	if got2.Username != "noel-updated" {
		t.Errorf("updated username = %q, want %q", got2.Username, "noel-updated")
	}
	if !got2.LastLoginAt.Equal(updatedLoginAt) {
		t.Errorf("updated last_login_at = %v, want %v", got2.LastLoginAt, updatedLoginAt)
	}
}

func TestOIDCSessionCreateGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Must upsert user first (FK constraint).
	if err := s.OIDCUserUpsert(ctx, OIDCUser{
		Sub: "sub123", Username: "alice",
		CreatedAt: time.Now().UTC(), LastLoginAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("OIDCUserUpsert (setup): %v", err)
	}

	sess := OIDCSession{
		Token:     "sess_abc123",
		Sub:       "sub123",
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Second),
	}
	if err := s.OIDCSessionCreate(ctx, sess); err != nil {
		t.Fatalf("OIDCSessionCreate: %v", err)
	}

	got, err := s.OIDCSessionGet(ctx, "sess_abc123")
	if err != nil {
		t.Fatalf("OIDCSessionGet: %v", err)
	}
	if got == nil {
		t.Fatal("expected session, got nil")
	}
	if got.Sub != "sub123" {
		t.Errorf("sub = %q, want %q", got.Sub, "sub123")
	}

	if err := s.OIDCSessionDelete(ctx, "sess_abc123"); err != nil {
		t.Fatalf("OIDCSessionDelete: %v", err)
	}
	got2, _ := s.OIDCSessionGet(ctx, "sess_abc123")
	if got2 != nil {
		t.Error("expected nil after delete")
	}
}

func TestOIDCDeviceFlow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	flow := OIDCDeviceFlow{
		PollToken:  "poll_xyz",
		DeviceCode: "dex_device_code_abc",
		NodeName:   "my-node",
		ExpiresAt:  time.Now().UTC().Add(5 * time.Minute),
	}
	if err := s.OIDCDeviceFlowCreate(ctx, flow); err != nil {
		t.Fatalf("OIDCDeviceFlowCreate: %v", err)
	}

	got, err := s.OIDCDeviceFlowGet(ctx, "poll_xyz")
	if err != nil {
		t.Fatalf("OIDCDeviceFlowGet: %v", err)
	}
	if got == nil {
		t.Fatal("expected flow, got nil")
	}
	if got.NodeToken != "" {
		t.Errorf("node_token should be empty before completion, got %q", got.NodeToken)
	}

	if err := s.OIDCDeviceFlowComplete(ctx, "poll_xyz", "node_tok_123"); err != nil {
		t.Fatalf("OIDCDeviceFlowComplete: %v", err)
	}

	got2, _ := s.OIDCDeviceFlowGet(ctx, "poll_xyz")
	if got2 == nil {
		t.Fatal("expected flow after complete, got nil")
	}
	if got2.NodeToken != "node_tok_123" {
		t.Errorf("node_token = %q, want %q", got2.NodeToken, "node_tok_123")
	}
}

func TestOIDCSessionExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Must upsert user first (FK constraint).
	if err := s.OIDCUserUpsert(ctx, OIDCUser{
		Sub: "sub_expire", Username: "expireuser",
		CreatedAt: time.Now().UTC(), LastLoginAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("OIDCUserUpsert (setup): %v", err)
	}

	sess := OIDCSession{
		Token:     "sess_expired",
		Sub:       "sub_expire",
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(-time.Second), // already expired
	}
	if err := s.OIDCSessionCreate(ctx, sess); err != nil {
		t.Fatalf("OIDCSessionCreate: %v", err)
	}

	got, err := s.OIDCSessionGet(ctx, "sess_expired")
	if err != nil {
		t.Fatalf("OIDCSessionGet: %v", err)
	}
	if got != nil {
		t.Error("expected nil for expired session, got a result")
	}
}

func TestOIDCDeviceFlowExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	flow := OIDCDeviceFlow{
		PollToken:  "poll_expired",
		DeviceCode: "dex_expired_code",
		NodeName:   "some-node",
		ExpiresAt:  time.Now().UTC().Add(-time.Second), // already expired
	}
	if err := s.OIDCDeviceFlowCreate(ctx, flow); err != nil {
		t.Fatalf("OIDCDeviceFlowCreate: %v", err)
	}

	got, err := s.OIDCDeviceFlowGet(ctx, "poll_expired")
	if err != nil {
		t.Fatalf("OIDCDeviceFlowGet: %v", err)
	}
	if got != nil {
		t.Error("expected nil for expired device flow, got a result")
	}
}
