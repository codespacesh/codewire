package tests

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/codewiresh/codewire/internal/protocol"
)

// uint64Ptr returns a pointer to a uint64 value.
func uint64Ptr(v uint64) *uint64 { return &v }

// ---------------------------------------------------------------------------
// TestLaunchWithName — launch with Name set, verify it appears in session list.
// ---------------------------------------------------------------------------

func TestLaunchWithName(t *testing.T) {
	dir := tempDir(t, "launch-name")
	sock := startTestNode(t, dir)

	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 30"},
		WorkingDir: "/tmp",
		Name:       "alice",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	time.Sleep(300 * time.Millisecond)

	// List sessions and verify the name appears.
	resp = requestResponse(t, sock, &protocol.Request{Type: "ListSessions"})
	if resp.Type != "SessionList" {
		t.Fatalf("expected SessionList, got %s: %s", resp.Type, resp.Message)
	}

	var found *protocol.SessionInfo
	for i := range *resp.Sessions {
		if (*resp.Sessions)[i].ID == id {
			found = &(*resp.Sessions)[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("session %d not found in list", id)
	}
	if found.Name != "alice" {
		t.Fatalf("expected name 'alice', got %q", found.Name)
	}

	// Clean up.
	requestResponse(t, sock, &protocol.Request{Type: "Kill", ID: uint32Ptr(id)})
}

// ---------------------------------------------------------------------------
// TestNameConflict — launching two sessions with the same name should error.
// ---------------------------------------------------------------------------

func TestNameConflict(t *testing.T) {
	dir := tempDir(t, "name-conflict")
	sock := startTestNode(t, dir)

	// First session with name "worker".
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 30"},
		WorkingDir: "/tmp",
		Name:       "worker",
	})
	if resp.Type != "Launched" {
		t.Fatalf("first launch: expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	firstID := *resp.ID

	time.Sleep(300 * time.Millisecond)

	// Second session with the same name — should fail.
	resp = requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 30"},
		WorkingDir: "/tmp",
		Name:       "worker",
	})
	if resp.Type != "Error" {
		t.Fatalf("second launch: expected Error, got %s: %s", resp.Type, resp.Message)
	}
	if resp.Message == "" {
		t.Fatal("error response should have a message")
	}

	// Clean up.
	requestResponse(t, sock, &protocol.Request{Type: "Kill", ID: uint32Ptr(firstID)})
}

// ---------------------------------------------------------------------------
// TestNameBasedMessaging — send a message using to_name and read it back.
// ---------------------------------------------------------------------------

func TestNameBasedMessaging(t *testing.T) {
	dir := tempDir(t, "msg-name")
	sock := startTestNode(t, dir)

	// Launch sender session.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 30"},
		WorkingDir: "/tmp",
		Name:       "sender",
	})
	if resp.Type != "Launched" {
		t.Fatalf("launch sender: expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	senderID := *resp.ID

	// Launch receiver session.
	resp = requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 30"},
		WorkingDir: "/tmp",
		Name:       "receiver",
	})
	if resp.Type != "Launched" {
		t.Fatalf("launch receiver: expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	receiverID := *resp.ID

	time.Sleep(300 * time.Millisecond)

	// Send message using to_name.
	resp = requestResponse(t, sock, &protocol.Request{
		Type:   "MsgSend",
		ID:     uint32Ptr(senderID),
		ToName: "receiver",
		Body:   "hello by name",
	})
	if resp.Type != "MsgSent" {
		t.Fatalf("MsgSend: expected MsgSent, got %s: %s", resp.Type, resp.Message)
	}
	if resp.MessageID == "" {
		t.Fatal("MsgSent should include a message_id")
	}

	time.Sleep(300 * time.Millisecond)

	// Read messages for receiver by name.
	resp = requestResponse(t, sock, &protocol.Request{
		Type:   "MsgRead",
		ID:     uint32Ptr(receiverID),
		ToName: "receiver",
	})
	if resp.Type != "MsgReadResult" {
		t.Fatalf("MsgRead: expected MsgReadResult, got %s: %s", resp.Type, resp.Message)
	}
	if resp.Messages == nil || len(*resp.Messages) == 0 {
		t.Fatal("expected at least one message in inbox")
	}

	var found bool
	for _, msg := range *resp.Messages {
		if msg.Body == "hello by name" {
			found = true
			if msg.From != senderID {
				t.Fatalf("expected from=%d, got %d", senderID, msg.From)
			}
			break
		}
	}
	if !found {
		t.Fatalf("message 'hello by name' not found in inbox, got %+v", *resp.Messages)
	}

	// Clean up.
	requestResponse(t, sock, &protocol.Request{Type: "KillAll"})
}

// ---------------------------------------------------------------------------
// TestDirectMessage — send a message using to_id and read it back.
// ---------------------------------------------------------------------------

func TestDirectMessage(t *testing.T) {
	dir := tempDir(t, "msg-direct")
	sock := startTestNode(t, dir)

	// Launch two sessions.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 30"},
		WorkingDir: "/tmp",
		Name:       "alpha",
	})
	if resp.Type != "Launched" {
		t.Fatalf("launch alpha: expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	alphaID := *resp.ID

	resp = requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 30"},
		WorkingDir: "/tmp",
		Name:       "beta",
	})
	if resp.Type != "Launched" {
		t.Fatalf("launch beta: expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	betaID := *resp.ID

	time.Sleep(300 * time.Millisecond)

	// Send message using to_id.
	resp = requestResponse(t, sock, &protocol.Request{
		Type: "MsgSend",
		ID:   uint32Ptr(alphaID),
		ToID: uint32Ptr(betaID),
		Body: "hello by id",
	})
	if resp.Type != "MsgSent" {
		t.Fatalf("MsgSend: expected MsgSent, got %s: %s", resp.Type, resp.Message)
	}
	sentMsgID := resp.MessageID
	if sentMsgID == "" {
		t.Fatal("MsgSent should include a message_id")
	}

	time.Sleep(300 * time.Millisecond)

	// Read messages for beta.
	resp = requestResponse(t, sock, &protocol.Request{
		Type: "MsgRead",
		ID:   uint32Ptr(betaID),
	})
	if resp.Type != "MsgReadResult" {
		t.Fatalf("MsgRead: expected MsgReadResult, got %s: %s", resp.Type, resp.Message)
	}
	if resp.Messages == nil || len(*resp.Messages) == 0 {
		t.Fatal("expected at least one message in beta's inbox")
	}

	var found bool
	for _, msg := range *resp.Messages {
		if msg.Body == "hello by id" && msg.From == alphaID {
			found = true
			if msg.MessageID != sentMsgID {
				t.Fatalf("message_id mismatch: expected %s, got %s", sentMsgID, msg.MessageID)
			}
			break
		}
	}
	if !found {
		t.Fatalf("message 'hello by id' from alpha not found in beta's inbox, got %+v", *resp.Messages)
	}

	// Clean up.
	requestResponse(t, sock, &protocol.Request{Type: "KillAll"})
}

// ---------------------------------------------------------------------------
// TestRequestReplyE2E — MsgRequest blocks until MsgReply is sent.
// ---------------------------------------------------------------------------

func TestRequestReplyE2E(t *testing.T) {
	dir := tempDir(t, "msg-reqreply")
	sock := startTestNode(t, dir)

	// Launch requester and responder sessions.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 30"},
		WorkingDir: "/tmp",
		Name:       "requester",
	})
	if resp.Type != "Launched" {
		t.Fatalf("launch requester: expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	requesterID := *resp.ID

	resp = requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 30"},
		WorkingDir: "/tmp",
		Name:       "responder",
	})
	if resp.Type != "Launched" {
		t.Fatalf("launch responder: expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	responderID := *resp.ID

	time.Sleep(300 * time.Millisecond)

	// Use a raw connection for the requester since MsgRequest blocks.
	reqConn, reqReader, reqWriter := connectRaw(t, sock)
	defer reqConn.Close()

	type requestResult struct {
		resp *protocol.Response
		err  error
	}
	resultCh := make(chan requestResult, 1)

	// Send MsgRequest in a goroutine (it blocks until reply).
	go func() {
		timeout := uint64(10)
		if err := reqWriter.SendRequest(&protocol.Request{
			Type:           "MsgRequest",
			ID:             uint32Ptr(requesterID),
			ToID:           uint32Ptr(responderID),
			Body:           "what is the status?",
			TimeoutSeconds: &timeout,
		}); err != nil {
			resultCh <- requestResult{nil, err}
			return
		}

		f, err := reqReader.ReadFrame()
		if err != nil {
			resultCh <- requestResult{nil, err}
			return
		}
		if f == nil {
			resultCh <- requestResult{nil, nil}
			return
		}
		var r protocol.Response
		if err := json.Unmarshal(f.Payload, &r); err != nil {
			resultCh <- requestResult{nil, err}
			return
		}
		resultCh <- requestResult{&r, nil}
	}()

	// Give the request time to register.
	time.Sleep(500 * time.Millisecond)

	// Read messages for the responder to find the request_id.
	resp = requestResponse(t, sock, &protocol.Request{
		Type: "MsgRead",
		ID:   uint32Ptr(responderID),
	})
	if resp.Type != "MsgReadResult" {
		t.Fatalf("MsgRead for responder: expected MsgReadResult, got %s: %s", resp.Type, resp.Message)
	}
	if resp.Messages == nil || len(*resp.Messages) == 0 {
		t.Fatal("responder should have received the request message")
	}

	// Find the request message and extract request_id.
	var requestID string
	for _, msg := range *resp.Messages {
		if msg.Body == "what is the status?" {
			requestID = msg.RequestID
			break
		}
	}
	if requestID == "" {
		t.Fatalf("could not find request_id in responder's inbox, got %+v", *resp.Messages)
	}

	// Send the reply.
	resp = requestResponse(t, sock, &protocol.Request{
		Type:      "MsgReply",
		ID:        uint32Ptr(responderID),
		RequestID: requestID,
		Body:      "all systems go",
	})
	if resp.Type != "MsgReplySent" {
		t.Fatalf("MsgReply: expected MsgReplySent, got %s: %s", resp.Type, resp.Message)
	}
	if resp.RequestID != requestID {
		t.Fatalf("reply request_id mismatch: expected %s, got %s", requestID, resp.RequestID)
	}

	// Wait for the goroutine to receive MsgRequestResult.
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("requester error: %v", result.err)
		}
		if result.resp == nil {
			t.Fatal("requester received nil response")
		}
		if result.resp.Type != "MsgRequestResult" {
			t.Fatalf("expected MsgRequestResult, got %s: %s", result.resp.Type, result.resp.Message)
		}
		if result.resp.RequestID != requestID {
			t.Fatalf("result request_id mismatch: expected %s, got %s", requestID, result.resp.RequestID)
		}
		if result.resp.ReplyBody != "all systems go" {
			t.Fatalf("expected reply body 'all systems go', got %q", result.resp.ReplyBody)
		}
		if result.resp.FromID == nil || *result.resp.FromID != responderID {
			t.Fatalf("expected from_id=%d, got %v", responderID, result.resp.FromID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for MsgRequestResult")
	}

	// Clean up.
	requestResponse(t, sock, &protocol.Request{Type: "KillAll"})
}

// ---------------------------------------------------------------------------
// TestMsgListen — connect a listener, send a message, verify the listener
// receives an Event.
// ---------------------------------------------------------------------------

func TestMsgListen(t *testing.T) {
	dir := tempDir(t, "msg-listen")
	sock := startTestNode(t, dir)

	// Launch two sessions.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 30"},
		WorkingDir: "/tmp",
		Name:       "producer",
	})
	if resp.Type != "Launched" {
		t.Fatalf("launch producer: expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	producerID := *resp.ID

	resp = requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 30"},
		WorkingDir: "/tmp",
		Name:       "consumer",
	})
	if resp.Type != "Launched" {
		t.Fatalf("launch consumer: expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	consumerID := *resp.ID

	time.Sleep(300 * time.Millisecond)

	// Connect a listener on a raw connection.
	listenConn, listenReader, listenWriter := connectRaw(t, sock)
	defer listenConn.Close()

	// Send MsgListen for the consumer session.
	if err := listenWriter.SendRequest(&protocol.Request{
		Type: "MsgListen",
		ID:   uint32Ptr(consumerID),
	}); err != nil {
		t.Fatalf("send MsgListen: %v", err)
	}

	// Read MsgListenAck.
	f, err := listenReader.ReadFrame()
	if err != nil {
		t.Fatalf("read MsgListenAck: %v", err)
	}
	if f == nil {
		t.Fatal("unexpected EOF reading MsgListenAck")
	}
	var ackResp protocol.Response
	if err := json.Unmarshal(f.Payload, &ackResp); err != nil {
		t.Fatalf("parse MsgListenAck: %v", err)
	}
	if ackResp.Type != "MsgListenAck" {
		t.Fatalf("expected MsgListenAck, got %s: %s", ackResp.Type, ackResp.Message)
	}

	// Start reading events from the listener in a goroutine.
	frameCh := make(chan frameResult, 64)
	go func() {
		for {
			f, err := listenReader.ReadFrame()
			frameCh <- frameResult{f, err}
			if err != nil || f == nil {
				return
			}
		}
	}()

	// Send a message from producer to consumer (via a separate connection).
	resp = requestResponse(t, sock, &protocol.Request{
		Type: "MsgSend",
		ID:   uint32Ptr(producerID),
		ToID: uint32Ptr(consumerID),
		Body: "event test message",
	})
	if resp.Type != "MsgSent" {
		t.Fatalf("MsgSend: expected MsgSent, got %s: %s", resp.Type, resp.Message)
	}

	// Wait for the listener to receive an Event.
	var receivedEvent *protocol.Response
	timeout := time.After(5 * time.Second)

loop:
	for {
		select {
		case fr := <-frameCh:
			if fr.err != nil {
				t.Fatalf("listener read error: %v", fr.err)
			}
			if fr.frame == nil {
				t.Fatal("listener connection closed unexpectedly")
			}
			if fr.frame.Type == protocol.FrameControl {
				var r protocol.Response
				if err := json.Unmarshal(fr.frame.Payload, &r); err != nil {
					t.Fatalf("parse listener event: %v", err)
				}
				if r.Type == "Event" {
					receivedEvent = &r
					break loop
				}
			}
		case <-timeout:
			break loop
		}
	}

	if receivedEvent == nil {
		t.Fatal("listener should have received an Event")
	}
	if receivedEvent.Event == nil {
		t.Fatal("Event response should have event field")
	}
	if receivedEvent.Event.EventType != "direct.message" {
		t.Fatalf("expected event type 'direct.message', got %q", receivedEvent.Event.EventType)
	}
	if receivedEvent.SessionID == nil || *receivedEvent.SessionID != consumerID {
		t.Fatalf("expected session_id=%d, got %v", consumerID, receivedEvent.SessionID)
	}

	// Verify the event data contains the message body.
	var eventData map[string]interface{}
	if err := json.Unmarshal(receivedEvent.Event.Data, &eventData); err != nil {
		t.Fatalf("parse event data: %v", err)
	}
	if body, ok := eventData["body"].(string); !ok || body != "event test message" {
		t.Fatalf("expected body 'event test message' in event data, got %v", eventData)
	}

	// Clean up.
	requestResponse(t, sock, &protocol.Request{Type: "KillAll"})
}
