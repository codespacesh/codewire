package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// --- Event Types ---

// EventType is the discriminator for session events.
type EventType string

const (
	EventSessionCreated     EventType = "session.created"
	EventSessionStatus      EventType = "session.status"
	EventOutputSummary      EventType = "session.output_summary"
	EventInput              EventType = "session.input"
	EventAttached           EventType = "session.attached"
	EventDetached           EventType = "session.detached"
)

// Event is a typed, timestamped session event written to events.jsonl.
type Event struct {
	Timestamp time.Time       `json:"timestamp"`
	Type      EventType       `json:"type"`
	Data      json.RawMessage `json:"data"`
}

// --- Event Data Types ---

type SessionCreatedData struct {
	Command    []string `json:"command"`
	WorkingDir string   `json:"working_dir"`
	Tags       []string `json:"tags"`
}

type SessionStatusData struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	ExitCode   *int    `json:"exit_code,omitempty"`
	DurationMs *int64  `json:"duration_ms,omitempty"`
}

type OutputSummaryData struct {
	BytesDelta uint64 `json:"bytes_delta"`
	LinesDelta uint64 `json:"lines_delta"`
	TotalBytes uint64 `json:"total_bytes"`
	TotalLines uint64 `json:"total_lines"`
}

type InputData struct {
	Source     string `json:"source"`
	BytesCount int    `json:"bytes_count"`
}

type AttachDetachData struct {
	ClientID string `json:"client_id"`
}

// --- Event Constructors ---

func NewSessionCreatedEvent(command []string, workingDir string, tags []string) Event {
	data, _ := json.Marshal(SessionCreatedData{Command: command, WorkingDir: workingDir, Tags: tags})
	return Event{Timestamp: time.Now().UTC(), Type: EventSessionCreated, Data: data}
}

func NewSessionStatusEvent(from, to string, exitCode *int, durationMs *int64) Event {
	data, _ := json.Marshal(SessionStatusData{From: from, To: to, ExitCode: exitCode, DurationMs: durationMs})
	return Event{Timestamp: time.Now().UTC(), Type: EventSessionStatus, Data: data}
}

func NewOutputSummaryEvent(bytesDelta, linesDelta, totalBytes, totalLines uint64) Event {
	data, _ := json.Marshal(OutputSummaryData{BytesDelta: bytesDelta, LinesDelta: linesDelta, TotalBytes: totalBytes, TotalLines: totalLines})
	return Event{Timestamp: time.Now().UTC(), Type: EventOutputSummary, Data: data}
}

func NewInputEvent(source string, bytesCount int) Event {
	data, _ := json.Marshal(InputData{Source: source, BytesCount: bytesCount})
	return Event{Timestamp: time.Now().UTC(), Type: EventInput, Data: data}
}

func NewAttachedEvent(clientID string) Event {
	data, _ := json.Marshal(AttachDetachData{ClientID: clientID})
	return Event{Timestamp: time.Now().UTC(), Type: EventAttached, Data: data}
}

func NewDetachedEvent(clientID string) Event {
	data, _ := json.Marshal(AttachDetachData{ClientID: clientID})
	return Event{Timestamp: time.Now().UTC(), Type: EventDetached, Data: data}
}

// --- EventLog — append-only JSONL file ---

// EventLog provides append-only writes and sequential reads for a JSONL event file.
type EventLog struct {
	mu   sync.Mutex
	path string
	file *os.File
}

// NewEventLog opens or creates an event log at the given path.
func NewEventLog(path string) (*EventLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening event log: %w", err)
	}
	return &EventLog{path: path, file: f}, nil
}

// Append writes an event to the log.
func (l *EventLog) Append(e Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = l.file.Write(data)
	return err
}

// ReadAll reads all events from the log file.
func ReadEventLog(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var events []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1MB max line
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue // skip corrupt lines
		}
		events = append(events, e)
	}
	return events, scanner.Err()
}

// Close closes the underlying file.
func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// --- SubscriptionManager — fan-out events to subscribers ---

// Subscription filters and receives events.
type Subscription struct {
	ID         uint64
	SessionID  *uint32
	Tags       []string
	EventTypes []EventType
	Ch         chan SessionEvent
}

// SessionEvent pairs an event with its session ID for subscription dispatch.
type SessionEvent struct {
	SessionID uint32 `json:"session_id"`
	Event     Event  `json:"event"`
}

// SubscriptionManager tracks active subscriptions and dispatches events.
type SubscriptionManager struct {
	mu     sync.RWMutex
	subs   map[uint64]*Subscription
	nextID uint64
}

// NewSubscriptionManager creates a ready-to-use manager.
func NewSubscriptionManager() *SubscriptionManager {
	return &SubscriptionManager{
		subs: make(map[uint64]*Subscription),
	}
}

// Subscribe creates a new subscription with the given filters.
func (m *SubscriptionManager) Subscribe(sessionID *uint32, tags []string, eventTypes []EventType) *Subscription {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := m.nextID
	m.nextID++

	sub := &Subscription{
		ID:         id,
		SessionID:  sessionID,
		Tags:       tags,
		EventTypes: eventTypes,
		Ch:         make(chan SessionEvent, 256),
	}
	m.subs[id] = sub
	return sub
}

// Unsubscribe removes and closes a subscription.
func (m *SubscriptionManager) Unsubscribe(id uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sub, ok := m.subs[id]; ok {
		close(sub.Ch)
		delete(m.subs, id)
	}
}

// Publish dispatches an event to all matching subscriptions.
func (m *SubscriptionManager) Publish(sessionID uint32, tags []string, event Event) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	se := SessionEvent{SessionID: sessionID, Event: event}

	for _, sub := range m.subs {
		if !sub.matches(sessionID, tags, event.Type) {
			continue
		}
		select {
		case sub.Ch <- se:
		default: // drop for slow subscribers
		}
	}
}

// matches checks if a subscription's filters match the given event.
func (s *Subscription) matches(sessionID uint32, tags []string, eventType EventType) bool {
	// Session ID filter.
	if s.SessionID != nil && *s.SessionID != sessionID {
		return false
	}

	// Tag filter (any tag must match).
	if len(s.Tags) > 0 {
		matched := false
		for _, ft := range s.Tags {
			for _, st := range tags {
				if ft == st {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			return false
		}
	}

	// Event type filter.
	if len(s.EventTypes) > 0 {
		matched := false
		for _, et := range s.EventTypes {
			if et == eventType {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}
