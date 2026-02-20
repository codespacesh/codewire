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
