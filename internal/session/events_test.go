package session

import (
	"path/filepath"
	"testing"
)

func TestEventLogWriteRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	log, err := NewEventLog(path)
	if err != nil {
		t.Fatal(err)
	}

	// Write events.
	e1 := NewSessionCreatedEvent([]string{"echo", "hello"}, "/tmp", []string{"test"})
	if err := log.Append(e1); err != nil {
		t.Fatal(err)
	}

	exitCode := 0
	durationMs := int64(100)
	e2 := NewSessionStatusEvent("running", "completed", &exitCode, &durationMs)
	if err := log.Append(e2); err != nil {
		t.Fatal(err)
	}

	log.Close()

	// Read events.
	events, err := ReadEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != EventSessionCreated {
		t.Fatalf("expected session.created, got %s", events[0].Type)
	}
	if events[1].Type != EventSessionStatus {
		t.Fatalf("expected session.status, got %s", events[1].Type)
	}
}

func TestReadEventLog_NonExistent(t *testing.T) {
	events, err := ReadEventLog("/tmp/nonexistent_events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if events != nil {
		t.Fatalf("expected nil, got %v", events)
	}
}

func TestSubscriptionManager_BasicPubSub(t *testing.T) {
	sm := NewSubscriptionManager()

	sub := sm.Subscribe(nil, nil, nil)
	if sub.ID != 0 {
		t.Fatalf("expected ID 0, got %d", sub.ID)
	}

	// Publish an event.
	event := NewSessionCreatedEvent([]string{"test"}, "/tmp", []string{"worker"})
	sm.Publish(1, []string{"worker"}, event)

	// Should receive it.
	se := <-sub.Ch
	if se.SessionID != 1 {
		t.Fatalf("expected session 1, got %d", se.SessionID)
	}
	if se.Event.Type != EventSessionCreated {
		t.Fatalf("expected session.created, got %s", se.Event.Type)
	}

	sm.Unsubscribe(sub.ID)
}

func TestSubscriptionManager_SessionFilter(t *testing.T) {
	sm := NewSubscriptionManager()

	sessionID := uint32(5)
	sub := sm.Subscribe(&sessionID, nil, nil)

	event := NewSessionCreatedEvent([]string{"test"}, "/tmp", nil)

	// Wrong session — should not receive.
	sm.Publish(3, nil, event)
	select {
	case <-sub.Ch:
		t.Fatal("should not receive event for wrong session")
	default:
	}

	// Right session — should receive.
	sm.Publish(5, nil, event)
	se := <-sub.Ch
	if se.SessionID != 5 {
		t.Fatalf("expected session 5, got %d", se.SessionID)
	}

	sm.Unsubscribe(sub.ID)
}

func TestSubscriptionManager_TagFilter(t *testing.T) {
	sm := NewSubscriptionManager()

	sub := sm.Subscribe(nil, []string{"worker"}, nil)

	event := NewSessionCreatedEvent([]string{"test"}, "/tmp", nil)

	// No matching tags.
	sm.Publish(1, []string{"build"}, event)
	select {
	case <-sub.Ch:
		t.Fatal("should not receive event for non-matching tags")
	default:
	}

	// Matching tag.
	sm.Publish(2, []string{"worker", "build"}, event)
	se := <-sub.Ch
	if se.SessionID != 2 {
		t.Fatalf("expected session 2, got %d", se.SessionID)
	}

	sm.Unsubscribe(sub.ID)
}

func TestSubscriptionManager_EventTypeFilter(t *testing.T) {
	sm := NewSubscriptionManager()

	sub := sm.Subscribe(nil, nil, []EventType{EventSessionStatus})

	// Created event — should not match.
	sm.Publish(1, nil, NewSessionCreatedEvent([]string{"test"}, "/tmp", nil))
	select {
	case <-sub.Ch:
		t.Fatal("should not receive created event")
	default:
	}

	// Status event — should match.
	exitCode := 0
	sm.Publish(1, nil, NewSessionStatusEvent("running", "completed", &exitCode, nil))
	se := <-sub.Ch
	if se.Event.Type != EventSessionStatus {
		t.Fatalf("expected session.status, got %s", se.Event.Type)
	}

	sm.Unsubscribe(sub.ID)
}
