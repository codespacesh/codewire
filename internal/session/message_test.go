package session

import (
	"encoding/json"
	"testing"
	"time"
)

// launchSleepSession is a helper that launches a "sleep 30" session and returns
// its ID. It calls t.Fatal on failure and registers a cleanup to kill the
// session when the test finishes.
func launchSleepSession(t *testing.T, sm *SessionManager) uint32 {
	t.Helper()
	id, err := sm.Launch([]string{"sleep", "30"}, "/tmp")
	if err != nil {
		t.Fatalf("failed to launch session: %v", err)
	}
	t.Cleanup(func() { _ = sm.Kill(id) })
	return id
}

func TestSendMessage(t *testing.T) {
	sm, err := NewSessionManager(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create session manager: %v", err)
	}

	sender := launchSleepSession(t, sm)
	recipient := launchSleepSession(t, sm)

	msgID, err := sm.SendMessage(sender, recipient, "hello from sender")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	if msgID == "" {
		t.Fatal("expected non-empty message ID")
	}

	// Verify the message appears in the recipient's message log.
	events, err := sm.ReadMessages(recipient, 0)
	if err != nil {
		t.Fatalf("ReadMessages failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 message in recipient log, got %d", len(events))
	}
	if events[0].Type != EventDirectMessage {
		t.Fatalf("expected event type %s, got %s", EventDirectMessage, events[0].Type)
	}

	var dm DirectMessageData
	if err := json.Unmarshal(events[0].Data, &dm); err != nil {
		t.Fatalf("failed to unmarshal message data: %v", err)
	}
	if dm.Body != "hello from sender" {
		t.Fatalf("expected body %q, got %q", "hello from sender", dm.Body)
	}
	if dm.From != sender {
		t.Fatalf("expected From=%d, got %d", sender, dm.From)
	}
	if dm.To != recipient {
		t.Fatalf("expected To=%d, got %d", recipient, dm.To)
	}
}

func TestSendMessageBothLogs(t *testing.T) {
	sm, err := NewSessionManager(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create session manager: %v", err)
	}

	sender := launchSleepSession(t, sm)
	recipient := launchSleepSession(t, sm)

	_, err = sm.SendMessage(sender, recipient, "bidirectional check")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	// Verify the message appears in the sender's log.
	senderEvents, err := sm.ReadMessages(sender, 0)
	if err != nil {
		t.Fatalf("ReadMessages (sender) failed: %v", err)
	}
	if len(senderEvents) != 1 {
		t.Fatalf("expected 1 message in sender log, got %d", len(senderEvents))
	}
	if senderEvents[0].Type != EventDirectMessage {
		t.Fatalf("expected event type %s in sender log, got %s", EventDirectMessage, senderEvents[0].Type)
	}

	// Verify the message appears in the recipient's log.
	recipientEvents, err := sm.ReadMessages(recipient, 0)
	if err != nil {
		t.Fatalf("ReadMessages (recipient) failed: %v", err)
	}
	if len(recipientEvents) != 1 {
		t.Fatalf("expected 1 message in recipient log, got %d", len(recipientEvents))
	}
	if recipientEvents[0].Type != EventDirectMessage {
		t.Fatalf("expected event type %s in recipient log, got %s", EventDirectMessage, recipientEvents[0].Type)
	}

	// Both logs should contain the same message body.
	var senderDM, recipientDM DirectMessageData
	if err := json.Unmarshal(senderEvents[0].Data, &senderDM); err != nil {
		t.Fatalf("failed to unmarshal sender message data: %v", err)
	}
	if err := json.Unmarshal(recipientEvents[0].Data, &recipientDM); err != nil {
		t.Fatalf("failed to unmarshal recipient message data: %v", err)
	}
	if senderDM.Body != "bidirectional check" {
		t.Fatalf("sender log body: expected %q, got %q", "bidirectional check", senderDM.Body)
	}
	if recipientDM.Body != "bidirectional check" {
		t.Fatalf("recipient log body: expected %q, got %q", "bidirectional check", recipientDM.Body)
	}
	if senderDM.MessageID != recipientDM.MessageID {
		t.Fatalf("message IDs differ: sender=%q, recipient=%q", senderDM.MessageID, recipientDM.MessageID)
	}
}

func TestReadMessagesTail(t *testing.T) {
	sm, err := NewSessionManager(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create session manager: %v", err)
	}

	sender := launchSleepSession(t, sm)
	recipient := launchSleepSession(t, sm)

	// Send 5 messages.
	for i := 0; i < 5; i++ {
		_, err := sm.SendMessage(sender, recipient, "msg-"+string(rune('A'+i)))
		if err != nil {
			t.Fatalf("SendMessage %d failed: %v", i, err)
		}
	}

	// Read all messages to confirm count.
	all, err := sm.ReadMessages(recipient, 0)
	if err != nil {
		t.Fatalf("ReadMessages (all) failed: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(all))
	}

	// Read with tail=2 — should return only the last 2.
	tail, err := sm.ReadMessages(recipient, 2)
	if err != nil {
		t.Fatalf("ReadMessages (tail=2) failed: %v", err)
	}
	if len(tail) != 2 {
		t.Fatalf("expected 2 messages with tail=2, got %d", len(tail))
	}

	// Verify the tail messages are the last two sent.
	var dm3, dm4 DirectMessageData
	if err := json.Unmarshal(tail[0].Data, &dm3); err != nil {
		t.Fatalf("failed to unmarshal tail[0]: %v", err)
	}
	if err := json.Unmarshal(tail[1].Data, &dm4); err != nil {
		t.Fatalf("failed to unmarshal tail[1]: %v", err)
	}
	if dm3.Body != "msg-D" {
		t.Fatalf("expected tail[0] body %q, got %q", "msg-D", dm3.Body)
	}
	if dm4.Body != "msg-E" {
		t.Fatalf("expected tail[1] body %q, got %q", "msg-E", dm4.Body)
	}
}

func TestRequestReply(t *testing.T) {
	sm, err := NewSessionManager(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create session manager: %v", err)
	}

	sender := launchSleepSession(t, sm)
	if err := sm.SetName(sender, "requester"); err != nil {
		t.Fatalf("SetName failed: %v", err)
	}

	recipient := launchSleepSession(t, sm)
	if err := sm.SetName(recipient, "responder"); err != nil {
		t.Fatalf("SetName failed: %v", err)
	}

	requestID, replyCh, err := sm.SendRequest(sender, recipient, "what is 2+2?")
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}
	if requestID == "" {
		t.Fatal("expected non-empty request ID")
	}

	// Reply from recipient.
	if err := sm.SendReply(recipient, requestID, "4"); err != nil {
		t.Fatalf("SendReply failed: %v", err)
	}

	// Verify the reply is received on the channel.
	select {
	case reply := <-replyCh:
		if reply.RequestID != requestID {
			t.Fatalf("reply RequestID: expected %q, got %q", requestID, reply.RequestID)
		}
		if reply.From != recipient {
			t.Fatalf("reply From: expected %d, got %d", recipient, reply.From)
		}
		if reply.FromName != "responder" {
			t.Fatalf("reply FromName: expected %q, got %q", "responder", reply.FromName)
		}
		if reply.Body != "4" {
			t.Fatalf("reply Body: expected %q, got %q", "4", reply.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for reply on channel")
	}
}

func TestRequestTimeout(t *testing.T) {
	sm, err := NewSessionManager(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create session manager: %v", err)
	}

	sender := launchSleepSession(t, sm)
	recipient := launchSleepSession(t, sm)

	requestID, replyCh, err := sm.SendRequest(sender, recipient, "this will timeout")
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}

	// Do not reply. Instead, clean up the pending request (simulating timeout).
	sm.CleanupRequest(requestID)

	// The reply channel should not receive anything.
	select {
	case reply, ok := <-replyCh:
		if ok {
			t.Fatalf("expected no reply after cleanup, got: %+v", reply)
		}
	default:
		// No reply received — correct behavior.
	}

	// Attempting to reply after cleanup should fail.
	err = sm.SendReply(recipient, requestID, "too late")
	if err == nil {
		t.Fatal("expected error when replying to cleaned-up request")
	}
}

func TestRequestReplyAfterCleanup(t *testing.T) {
	sm, err := NewSessionManager(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create session manager: %v", err)
	}

	sender := launchSleepSession(t, sm)
	recipient := launchSleepSession(t, sm)

	requestID, _, err := sm.SendRequest(sender, recipient, "will be cleaned up")
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}

	// Cleanup the pending request first.
	sm.CleanupRequest(requestID)

	// Now try to reply — should error because no pending request exists.
	err = sm.SendReply(recipient, requestID, "late reply")
	if err == nil {
		t.Fatal("expected error when replying after cleanup, got nil")
	}

	// Verify the error message mentions the request ID.
	expectedSubstr := requestID
	if got := err.Error(); !containsSubstring(got, expectedSubstr) {
		t.Fatalf("expected error to contain %q, got %q", expectedSubstr, got)
	}
}

// containsSubstring checks if s contains substr.
func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
