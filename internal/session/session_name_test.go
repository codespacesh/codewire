package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// launchSleep launches a "sleep 5" session and returns its ID.
// It kills the session on test cleanup.
func launchSleep(t *testing.T, sm *SessionManager) uint32 {
	t.Helper()
	id, err := sm.Launch([]string{"sleep", "5"}, "/tmp", nil)
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	t.Cleanup(func() {
		_ = sm.Kill(id)
		// Give process time to exit so PTY goroutines can clean up.
		time.Sleep(100 * time.Millisecond)
	})
	return id
}

func TestSetNameSuccess(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	id := launchSleep(t, sm)

	// Set a valid name.
	if err := sm.SetName(id, "my-session"); err != nil {
		t.Fatalf("SetName failed: %v", err)
	}

	// Verify via GetName.
	got := sm.GetName(id)
	if got != "my-session" {
		t.Fatalf("GetName: expected %q, got %q", "my-session", got)
	}

	// Verify via ResolveByName.
	resolved, err := sm.ResolveByName("my-session")
	if err != nil {
		t.Fatalf("ResolveByName failed: %v", err)
	}
	if resolved != id {
		t.Fatalf("ResolveByName: expected %d, got %d", id, resolved)
	}
}

func TestSetNameUniqueness(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	id1 := launchSleep(t, sm)
	id2 := launchSleep(t, sm)

	// Set name on first session.
	if err := sm.SetName(id1, "worker"); err != nil {
		t.Fatalf("SetName(id1) failed: %v", err)
	}

	// Same name on second session should fail.
	err = sm.SetName(id2, "worker")
	if err == nil {
		t.Fatal("SetName(id2) should have failed for duplicate name")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("expected 'already in use' error, got: %v", err)
	}

	// Setting the same name on the same session should succeed (idempotent).
	if err := sm.SetName(id1, "worker"); err != nil {
		t.Fatalf("SetName(id1, same name) should succeed: %v", err)
	}
}

func TestSetNameValidation(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	id := launchSleep(t, sm)

	invalidNames := []struct {
		name   string
		reason string
	}{
		{"", "empty string"},
		{"-starts-with-hyphen", "starts with hyphen"},
		{"has spaces", "contains spaces"},
		{"has_underscore", "contains underscore"},
		{"has.dot", "contains dot"},
		{"special!char", "contains special character"},
		{"abcdefghijklmnopqrstuvwxyz1234567", "too long (33 chars)"},
	}

	for _, tc := range invalidNames {
		err := sm.SetName(id, tc.name)
		if err == nil {
			t.Errorf("SetName(%q) should have failed (%s)", tc.name, tc.reason)
		}
	}

	// Verify valid edge cases do work.
	validNames := []string{
		"a",                                // single char
		"A",                                // uppercase single char
		"a1",                               // alphanumeric
		"my-session",                       // with hyphens
		"abcdefghijklmnopqrstuvwxyz123456", // exactly 32 chars
	}
	for _, name := range validNames {
		if err := sm.SetName(id, name); err != nil {
			t.Errorf("SetName(%q) should succeed: %v", name, err)
		}
	}
}

func TestResolveByNameNotFound(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	_, err = sm.ResolveByName("nonexistent")
	if err == nil {
		t.Fatal("ResolveByName should fail for nonexistent name")
	}
	if !strings.Contains(err.Error(), "no session named") {
		t.Fatalf("expected 'no session named' error, got: %v", err)
	}
}

func TestNameCleanupOnRename(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	id := launchSleep(t, sm)

	// Set initial name.
	if err := sm.SetName(id, "old-name"); err != nil {
		t.Fatalf("SetName(old-name) failed: %v", err)
	}

	// Rename.
	if err := sm.SetName(id, "new-name"); err != nil {
		t.Fatalf("SetName(new-name) failed: %v", err)
	}

	// New name should resolve.
	resolved, err := sm.ResolveByName("new-name")
	if err != nil {
		t.Fatalf("ResolveByName(new-name) failed: %v", err)
	}
	if resolved != id {
		t.Fatalf("ResolveByName(new-name): expected %d, got %d", id, resolved)
	}

	// Old name should no longer resolve.
	_, err = sm.ResolveByName("old-name")
	if err == nil {
		t.Fatal("ResolveByName(old-name) should fail after rename")
	}
	if !strings.Contains(err.Error(), "no session named") {
		t.Fatalf("expected 'no session named' error, got: %v", err)
	}

	// GetName should return the new name.
	got := sm.GetName(id)
	if got != "new-name" {
		t.Fatalf("GetName: expected %q, got %q", "new-name", got)
	}
}

func TestNameReleasedOnKill(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	id1 := launchSleep(t, sm)
	if err := sm.SetName(id1, "reuse-me"); err != nil {
		t.Fatalf("SetName: %v", err)
	}

	if err := sm.Kill(id1); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	id2 := launchSleep(t, sm)
	if err := sm.SetName(id2, "reuse-me"); err != nil {
		t.Fatalf("SetName on id2 after kill should succeed: %v", err)
	}
}

func TestNameReleasedOnNaturalExit(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	id, err := sm.Launch([]string{"true"}, "/tmp", nil)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := sm.SetName(id, "short-lived"); err != nil {
		t.Fatalf("SetName: %v", err)
	}

	time.Sleep(1 * time.Second)

	id2 := launchSleep(t, sm)
	if err := sm.SetName(id2, "short-lived"); err != nil {
		t.Fatalf("SetName after exit should succeed: %v", err)
	}
}

func TestNamePersistence(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	id := launchSleep(t, sm)

	if err := sm.SetName(id, "persist-test"); err != nil {
		t.Fatalf("SetName failed: %v", err)
	}

	// Persist metadata to disk.
	sm.PersistMeta()

	// Read sessions.json and verify the name field is present.
	metaPath := filepath.Join(dir, "sessions.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile sessions.json: %v", err)
	}

	var metas []SessionMeta
	if err := json.Unmarshal(data, &metas); err != nil {
		t.Fatalf("Unmarshal sessions.json: %v", err)
	}

	if len(metas) == 0 {
		t.Fatal("sessions.json should contain at least one session")
	}

	var found bool
	for _, m := range metas {
		if m.ID == id {
			found = true
			if m.Name != "persist-test" {
				t.Fatalf("persisted name: expected %q, got %q", "persist-test", m.Name)
			}
			break
		}
	}
	if !found {
		t.Fatalf("session %d not found in sessions.json", id)
	}
}
