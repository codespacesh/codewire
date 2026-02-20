package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codespacesh/codewire/internal/client"
	"github.com/codespacesh/codewire/internal/connection"
	"github.com/codespacesh/codewire/internal/node"
	"github.com/codespacesh/codewire/internal/protocol"
)

// tempDir creates a unique temporary directory for a test and registers cleanup.
func tempDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(os.TempDir(), "codewire-test", name, fmt.Sprintf("%d", os.Getpid()))
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// startTestNode creates a node, starts it in a background goroutine, and waits
// for the Unix socket to become connectable. Returns the socket path.
func startTestNode(t *testing.T, dataDir string) string {
	t.Helper()
	sockPath := filepath.Join(dataDir, "codewire.sock")

	n, err := node.NewNode(dataDir)
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		n.Cleanup()
	})

	go func() {
		_ = n.Run(ctx)
	}()

	// Wait for socket to appear and be connectable.
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if conn, err := net.Dial("unix", sockPath); err == nil {
			conn.Close()
			return sockPath
		}
	}
	t.Fatalf("node failed to start (socket not available at %s)", sockPath)
	return ""
}

// requestResponse connects to the Unix socket, sends a request, reads one
// control frame, and returns the parsed Response.
func requestResponse(t *testing.T, sockPath string, req *protocol.Request) *protocol.Response {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("connect to %s: %v", sockPath, err)
	}
	defer conn.Close()

	writer := connection.NewUnixWriter(conn)
	reader := connection.NewUnixReader(conn)

	if err := writer.SendRequest(req); err != nil {
		t.Fatalf("send request: %v", err)
	}

	f, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if f == nil {
		t.Fatal("unexpected EOF reading response")
	}
	if f.Type != protocol.FrameControl {
		t.Fatalf("expected control frame (0x%02x), got 0x%02x", protocol.FrameControl, f.Type)
	}

	var resp protocol.Response
	if err := json.Unmarshal(f.Payload, &resp); err != nil {
		t.Fatalf("parse response JSON: %v (raw: %s)", err, string(f.Payload))
	}
	return &resp
}

// boolPtr returns a pointer to a bool value.
func boolPtr(v bool) *bool { return &v }

// uintPtr returns a pointer to a uint value.
func uintPtr(v uint) *uint { return &v }

// uint32Ptr returns a pointer to a uint32 value.
func uint32Ptr(v uint32) *uint32 { return &v }

// uint16Ptr returns a pointer to a uint16 value.
func uint16Ptr(v uint16) *uint16 { return &v }

// frameResult bundles a frame read result for channel-based communication.
type frameResult struct {
	frame *protocol.Frame
	err   error
}

// connectRaw dials the Unix socket and returns the raw connection plus a
// FrameReader and FrameWriter. The caller owns conn and should close it.
func connectRaw(t *testing.T, sockPath string) (net.Conn, connection.FrameReader, connection.FrameWriter) {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("connectRaw: %v", err)
	}
	return conn, connection.NewUnixReader(conn), connection.NewUnixWriter(conn)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestLaunchAndList(t *testing.T) {
	dir := tempDir(t, "launch-list")
	sock := startTestNode(t, dir)

	// Launch a session.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "echo hello-from-codewire && sleep 5"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	sessionID := *resp.ID

	// Give the process a moment to start.
	time.Sleep(500 * time.Millisecond)

	// List sessions.
	resp = requestResponse(t, sock, &protocol.Request{Type: "ListSessions"})
	if resp.Type != "SessionList" {
		t.Fatalf("expected SessionList, got %s: %s", resp.Type, resp.Message)
	}
	sessions := *resp.Sessions
	if len(sessions) == 0 {
		t.Fatal("session list is empty")
	}

	var found *protocol.SessionInfo
	for i := range sessions {
		if sessions[i].ID == sessionID {
			found = &sessions[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("launched session %d not found in list", sessionID)
	}
	if found.Status != "running" {
		t.Errorf("expected status 'running', got %q", found.Status)
	}
	if !strings.Contains(found.Prompt, "hello-from-codewire") {
		t.Errorf("expected prompt to contain 'hello-from-codewire', got %q", found.Prompt)
	}
}

func TestKillSession(t *testing.T) {
	dir := tempDir(t, "kill")
	sock := startTestNode(t, dir)

	// Launch.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 60"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	time.Sleep(300 * time.Millisecond)

	// Kill.
	resp = requestResponse(t, sock, &protocol.Request{
		Type: "Kill",
		ID:   uint32Ptr(id),
	})
	if resp.Type != "Killed" {
		t.Fatalf("expected Killed, got %s: %s", resp.Type, resp.Message)
	}
	if *resp.ID != id {
		t.Fatalf("killed id mismatch: expected %d, got %d", id, *resp.ID)
	}

	// Wait for status to update.
	time.Sleep(1 * time.Second)

	// Verify it is no longer running.
	resp = requestResponse(t, sock, &protocol.Request{Type: "ListSessions"})
	if resp.Type != "SessionList" {
		t.Fatalf("expected SessionList, got %s", resp.Type)
	}
	for _, s := range *resp.Sessions {
		if s.ID == id {
			if s.Status == "running" {
				t.Fatalf("session should not still be running, got status %q", s.Status)
			}
			if !strings.Contains(s.Status, "killed") && !strings.Contains(s.Status, "completed") {
				t.Fatalf("status should be killed or completed, got %q", s.Status)
			}
			return
		}
	}
	t.Fatalf("session %d not found in list after kill", id)
}

func TestKillAll(t *testing.T) {
	dir := tempDir(t, "kill-all")
	sock := startTestNode(t, dir)

	// Launch two sessions.
	for i := 0; i < 2; i++ {
		resp := requestResponse(t, sock, &protocol.Request{
			Type:       "Launch",
			Command:    []string{"bash", "-c", "sleep 60"},
			WorkingDir: "/tmp",
		})
		if resp.Type != "Launched" {
			t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
		}
	}

	time.Sleep(300 * time.Millisecond)

	resp := requestResponse(t, sock, &protocol.Request{Type: "KillAll"})
	if resp.Type != "KilledAll" {
		t.Fatalf("expected KilledAll, got %s: %s", resp.Type, resp.Message)
	}
	if *resp.Count != 2 {
		t.Fatalf("expected count 2, got %d", *resp.Count)
	}
}

func TestSessionCompletesNaturally(t *testing.T) {
	dir := tempDir(t, "complete")
	sock := startTestNode(t, dir)

	// Launch a session that exits quickly.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "echo done"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	// Wait for it to complete.
	time.Sleep(2 * time.Second)

	resp = requestResponse(t, sock, &protocol.Request{Type: "ListSessions"})
	if resp.Type != "SessionList" {
		t.Fatalf("expected SessionList, got %s", resp.Type)
	}
	for _, s := range *resp.Sessions {
		if s.ID == id {
			if !strings.Contains(s.Status, "completed") {
				t.Fatalf("expected status 'completed', got %q", s.Status)
			}
			return
		}
	}
	t.Fatalf("session %d not found in list", id)
}

func TestLogs(t *testing.T) {
	dir := tempDir(t, "logs")
	sock := startTestNode(t, dir)

	// Launch a session that outputs something.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "echo LOG_TEST_OUTPUT_12345"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	// Wait for output to be captured.
	time.Sleep(2 * time.Second)

	// Read logs (non-follow mode).
	resp = requestResponse(t, sock, &protocol.Request{
		Type:   "Logs",
		ID:     uint32Ptr(id),
		Follow: boolPtr(false),
	})
	if resp.Type != "LogData" {
		t.Fatalf("expected LogData, got %s: %s", resp.Type, resp.Message)
	}
	if resp.Done == nil || !*resp.Done {
		t.Fatal("non-follow logs should have done=true")
	}
	if !strings.Contains(resp.Data, "LOG_TEST_OUTPUT_12345") {
		t.Fatalf("log should contain our output, got: %q", resp.Data)
	}
}

func TestAttachAndReceiveOutput(t *testing.T) {
	dir := tempDir(t, "attach")
	sock := startTestNode(t, dir)

	// Launch a session that outputs periodically.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "for i in 1 2 3; do echo ATTACH_TEST_$i; sleep 1; done"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	time.Sleep(500 * time.Millisecond)

	// Attach.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	reader := connection.NewUnixReader(conn)
	writer := connection.NewUnixWriter(conn)

	if err := writer.SendRequest(&protocol.Request{
		Type:           "Attach",
		ID:             uint32Ptr(id),
		IncludeHistory: boolPtr(true),
	}); err != nil {
		t.Fatalf("send attach: %v", err)
	}

	// Read attach confirmation.
	f, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("read attach confirmation: %v", err)
	}
	if f == nil || f.Type != protocol.FrameControl {
		t.Fatal("expected control frame for attach confirmation")
	}
	var attachResp protocol.Response
	if err := json.Unmarshal(f.Payload, &attachResp); err != nil {
		t.Fatalf("parse attach response: %v", err)
	}
	if attachResp.Type != "Attached" {
		t.Fatalf("expected Attached, got %s: %s", attachResp.Type, attachResp.Message)
	}
	if *attachResp.ID != id {
		t.Fatalf("attached id mismatch: expected %d, got %d", id, *attachResp.ID)
	}

	// Read data frames in a goroutine.
	frameCh := make(chan frameResult, 64)
	go func() {
		for {
			f, err := reader.ReadFrame()
			frameCh <- frameResult{f, err}
			if err != nil || f == nil {
				return
			}
		}
	}()

	var collected []byte
	timeout := time.After(5 * time.Second)

loop:
	for {
		select {
		case fr := <-frameCh:
			if fr.err != nil {
				break loop
			}
			if fr.frame == nil {
				break loop
			}
			if fr.frame.Type == protocol.FrameData {
				collected = append(collected, fr.frame.Payload...)
				if strings.Contains(string(collected), "ATTACH_TEST_3") {
					break loop
				}
			}
			// Control frames (e.g., session completed) also break.
			if fr.frame.Type == protocol.FrameControl {
				var r protocol.Response
				json.Unmarshal(fr.frame.Payload, &r)
				if r.Type == "Error" && strings.Contains(r.Message, "completed") {
					break loop
				}
			}
		case <-timeout:
			// Timeout: check partial output.
			if !strings.Contains(string(collected), "ATTACH_TEST_") {
				t.Fatalf("should have received some output, got: %q", string(collected))
			}
			break loop
		}
	}

	output := string(collected)
	if !strings.Contains(output, "ATTACH_TEST_") {
		t.Fatalf("attached client should receive PTY output, got: %q", output)
	}
}

func TestAttachSendInput(t *testing.T) {
	dir := tempDir(t, "input")
	sock := startTestNode(t, dir)

	// Launch an interactive session (cat echoes stdin to stdout).
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "cat"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	time.Sleep(500 * time.Millisecond)

	// Attach.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	reader := connection.NewUnixReader(conn)
	writer := connection.NewUnixWriter(conn)

	if err := writer.SendRequest(&protocol.Request{
		Type:           "Attach",
		ID:             uint32Ptr(id),
		IncludeHistory: boolPtr(true),
	}); err != nil {
		t.Fatalf("send attach: %v", err)
	}

	// Read attach confirmation.
	f, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("read attach confirmation: %v", err)
	}
	var attachResp protocol.Response
	json.Unmarshal(f.Payload, &attachResp)
	if attachResp.Type != "Attached" {
		t.Fatalf("expected Attached, got %s", attachResp.Type)
	}

	// Send input.
	if err := writer.SendData([]byte("INPUT_TEST_LINE\n")); err != nil {
		t.Fatalf("send data: %v", err)
	}

	// Read output -- cat should echo it back.
	frameCh := make(chan frameResult, 64)
	go func() {
		for {
			f, err := reader.ReadFrame()
			frameCh <- frameResult{f, err}
			if err != nil || f == nil {
				return
			}
		}
	}()

	var collected []byte
	timeout := time.After(3 * time.Second)

loop:
	for {
		select {
		case fr := <-frameCh:
			if fr.err != nil || fr.frame == nil {
				break loop
			}
			if fr.frame.Type == protocol.FrameData {
				collected = append(collected, fr.frame.Payload...)
				if strings.Contains(string(collected), "INPUT_TEST_LINE") {
					break loop
				}
			}
		case <-timeout:
			break loop
		}
	}

	output := string(collected)
	if !strings.Contains(output, "INPUT_TEST_LINE") {
		t.Fatalf("should receive echoed input, got: %q", output)
	}

	// Kill the session to clean up (cat doesn't exit on its own).
	resp = requestResponse(t, sock, &protocol.Request{
		Type: "Kill",
		ID:   uint32Ptr(id),
	})
	if resp.Type != "Killed" {
		t.Fatalf("expected Killed, got %s", resp.Type)
	}
}

func TestDetachFromAttach(t *testing.T) {
	dir := tempDir(t, "detach")
	sock := startTestNode(t, dir)

	// Launch a long-running session.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 30"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	time.Sleep(300 * time.Millisecond)

	// Attach.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	reader := connection.NewUnixReader(conn)
	writer := connection.NewUnixWriter(conn)

	if err := writer.SendRequest(&protocol.Request{
		Type:           "Attach",
		ID:             uint32Ptr(id),
		IncludeHistory: boolPtr(true),
	}); err != nil {
		t.Fatalf("send attach: %v", err)
	}

	// Read attach confirmation.
	f, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("read attach confirmation: %v", err)
	}
	var attachResp protocol.Response
	json.Unmarshal(f.Payload, &attachResp)
	if attachResp.Type != "Attached" {
		t.Fatalf("expected Attached, got %s", attachResp.Type)
	}

	// Send detach request.
	if err := writer.SendRequest(&protocol.Request{Type: "Detach"}); err != nil {
		t.Fatalf("send detach: %v", err)
	}

	// Should receive Detached response.
	f, err = reader.ReadFrame()
	if err != nil {
		t.Fatalf("read detach response: %v", err)
	}
	if f == nil || f.Type != protocol.FrameControl {
		t.Fatal("expected control frame for detach response")
	}
	var detachResp protocol.Response
	json.Unmarshal(f.Payload, &detachResp)
	if detachResp.Type != "Detached" {
		t.Fatalf("expected Detached, got %s: %s", detachResp.Type, detachResp.Message)
	}

	// Session should still be running.
	resp = requestResponse(t, sock, &protocol.Request{Type: "ListSessions"})
	if resp.Type != "SessionList" {
		t.Fatalf("expected SessionList, got %s", resp.Type)
	}
	for _, s := range *resp.Sessions {
		if s.ID == id {
			if s.Status != "running" {
				t.Fatalf("session should still be running after detach, got %q", s.Status)
			}
			if s.Attached {
				t.Fatal("session should not be attached after detach")
			}
			// Clean up.
			requestResponse(t, sock, &protocol.Request{Type: "Kill", ID: uint32Ptr(id)})
			return
		}
	}
	t.Fatalf("session %d not found after detach", id)
}

func TestAttachNonexistentSession(t *testing.T) {
	dir := tempDir(t, "attach-noexist")
	sock := startTestNode(t, dir)

	resp := requestResponse(t, sock, &protocol.Request{
		Type:           "Attach",
		ID:             uint32Ptr(9999),
		IncludeHistory: boolPtr(true),
	})
	if resp.Type != "Error" {
		t.Fatalf("expected Error, got %s", resp.Type)
	}
	if !strings.Contains(resp.Message, "not found") {
		t.Fatalf("error should mention 'not found': %s", resp.Message)
	}
}

func TestResizeDuringAttach(t *testing.T) {
	dir := tempDir(t, "resize")
	sock := startTestNode(t, dir)

	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "sleep 10"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	time.Sleep(300 * time.Millisecond)

	// Attach.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	reader := connection.NewUnixReader(conn)
	writer := connection.NewUnixWriter(conn)

	if err := writer.SendRequest(&protocol.Request{
		Type:           "Attach",
		ID:             uint32Ptr(id),
		IncludeHistory: boolPtr(true),
	}); err != nil {
		t.Fatalf("send attach: %v", err)
	}

	// Read attach confirmation.
	f, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("read attach confirmation: %v", err)
	}
	_ = f // Attached response

	// Send resize -- should not error.
	if err := writer.SendRequest(&protocol.Request{
		Type: "Resize",
		Cols: uint16Ptr(120),
		Rows: uint16Ptr(40),
	}); err != nil {
		t.Fatalf("send resize: %v", err)
	}

	// Small delay to process.
	time.Sleep(200 * time.Millisecond)

	// Detach cleanly.
	if err := writer.SendRequest(&protocol.Request{Type: "Detach"}); err != nil {
		t.Fatalf("send detach: %v", err)
	}
	f, err = reader.ReadFrame()
	if err != nil {
		t.Fatalf("read detach response: %v", err)
	}
	if f == nil || f.Type != protocol.FrameControl {
		t.Fatal("expected control frame for detach response")
	}
	var detachResp protocol.Response
	json.Unmarshal(f.Payload, &detachResp)
	if detachResp.Type != "Detached" {
		t.Fatalf("expected Detached, got %s", detachResp.Type)
	}

	requestResponse(t, sock, &protocol.Request{Type: "Kill", ID: uint32Ptr(id)})
}

func TestMultipleAttachments(t *testing.T) {
	dir := tempDir(t, "multi-attach")
	sock := startTestNode(t, dir)

	// Launch a session that outputs periodically.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "for i in 1 2 3 4 5; do echo MULTI_$i; sleep 1; done"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	time.Sleep(500 * time.Millisecond)

	// Helper to attach a client.
	attachClient := func(label string) (connection.FrameReader, connection.FrameWriter, net.Conn) {
		t.Helper()
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatalf("%s connect: %v", label, err)
		}
		r := connection.NewUnixReader(c)
		w := connection.NewUnixWriter(c)
		if err := w.SendRequest(&protocol.Request{
			Type:           "Attach",
			ID:             uint32Ptr(id),
			IncludeHistory: boolPtr(true),
		}); err != nil {
			t.Fatalf("%s send attach: %v", label, err)
		}
		f, err := r.ReadFrame()
		if err != nil || f == nil {
			t.Fatalf("%s read attach confirmation: %v", label, err)
		}
		var ar protocol.Response
		json.Unmarshal(f.Payload, &ar)
		if ar.Type != "Attached" {
			t.Fatalf("%s expected Attached, got %s", label, ar.Type)
		}
		return r, w, c
	}

	reader1, _, conn1 := attachClient("client1")
	defer conn1.Close()
	reader2, _, conn2 := attachClient("client2")
	defer conn2.Close()

	// Both clients should receive output.
	frameCh1 := make(chan frameResult, 64)
	frameCh2 := make(chan frameResult, 64)

	go func() {
		for {
			f, err := reader1.ReadFrame()
			frameCh1 <- frameResult{f, err}
			if err != nil || f == nil {
				return
			}
		}
	}()
	go func() {
		for {
			f, err := reader2.ReadFrame()
			frameCh2 <- frameResult{f, err}
			if err != nil || f == nil {
				return
			}
		}
	}()

	var output1, output2 []byte
	timeout := time.After(3 * time.Second)

loop:
	for {
		select {
		case fr := <-frameCh1:
			if fr.err == nil && fr.frame != nil && fr.frame.Type == protocol.FrameData {
				output1 = append(output1, fr.frame.Payload...)
			}
		case fr := <-frameCh2:
			if fr.err == nil && fr.frame != nil && fr.frame.Type == protocol.FrameData {
				output2 = append(output2, fr.frame.Payload...)
			}
		case <-timeout:
			break loop
		}
	}

	text1 := string(output1)
	text2 := string(output2)

	if !strings.Contains(text1, "MULTI_") {
		t.Fatalf("first client should receive output: %q", text1)
	}
	if !strings.Contains(text2, "MULTI_") {
		t.Fatalf("second client should receive output: %q", text2)
	}

	// Clean up.
	requestResponse(t, sock, &protocol.Request{Type: "Kill", ID: uint32Ptr(id)})
}

func TestSendInputCrossSession(t *testing.T) {
	dir := tempDir(t, "cross-input")
	sock := startTestNode(t, dir)

	// Launch an interactive session (cat echoes input).
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "cat"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	time.Sleep(500 * time.Millisecond)

	// Send input without attaching.
	testInput := []byte("CROSS_SESSION_TEST\n")
	resp = requestResponse(t, sock, &protocol.Request{
		Type: "SendInput",
		ID:   uint32Ptr(id),
		Data: testInput,
	})
	if resp.Type != "InputSent" {
		t.Fatalf("expected InputSent, got %s: %s", resp.Type, resp.Message)
	}
	if *resp.Bytes != uint(len(testInput)) {
		t.Fatalf("expected %d bytes, got %d", len(testInput), *resp.Bytes)
	}

	// Wait for processing.
	time.Sleep(1 * time.Second)

	// Verify output was captured in logs.
	resp = requestResponse(t, sock, &protocol.Request{
		Type:   "Logs",
		ID:     uint32Ptr(id),
		Follow: boolPtr(false),
	})
	if resp.Type != "LogData" {
		t.Fatalf("expected LogData, got %s: %s", resp.Type, resp.Message)
	}
	if !strings.Contains(resp.Data, "CROSS_SESSION_TEST") {
		t.Fatalf("output should contain our input: %q", resp.Data)
	}

	// Clean up.
	requestResponse(t, sock, &protocol.Request{Type: "Kill", ID: uint32Ptr(id)})
}

func TestGetSessionStatus(t *testing.T) {
	dir := tempDir(t, "status")
	sock := startTestNode(t, dir)

	// Launch a session.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "echo STATUS_TEST && sleep 2"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	time.Sleep(500 * time.Millisecond)

	// Get status.
	resp = requestResponse(t, sock, &protocol.Request{
		Type: "GetStatus",
		ID:   uint32Ptr(id),
	})
	if resp.Type != "SessionStatus" {
		t.Fatalf("expected SessionStatus, got %s: %s", resp.Type, resp.Message)
	}
	if resp.Info == nil {
		t.Fatal("info should not be nil")
	}
	if resp.Info.ID != id {
		t.Fatalf("info id mismatch: expected %d, got %d", id, resp.Info.ID)
	}
	if resp.Info.Status != "running" {
		t.Fatalf("expected status 'running', got %q", resp.Info.Status)
	}
	if resp.Info.PID == nil {
		t.Fatal("PID should be present")
	}
	if resp.OutputSize == nil || *resp.OutputSize == 0 {
		t.Fatal("should have captured some output (output_size > 0)")
	}
	if resp.Info.OutputSizeBytes == nil {
		t.Fatal("output_size_bytes should be present")
	}

	// Clean up.
	requestResponse(t, sock, &protocol.Request{Type: "Kill", ID: uint32Ptr(id)})
}

func TestWatchSession(t *testing.T) {
	dir := tempDir(t, "watch")
	sock := startTestNode(t, dir)

	// Launch a session that outputs periodically.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "for i in 1 2 3; do echo WATCH_$i; sleep 1; done"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	time.Sleep(500 * time.Millisecond)

	// Watch the session.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	reader := connection.NewUnixReader(conn)
	writer := connection.NewUnixWriter(conn)

	if err := writer.SendRequest(&protocol.Request{
		Type:           "WatchSession",
		ID:             uint32Ptr(id),
		IncludeHistory: boolPtr(true),
		HistoryLines:   uintPtr(10),
	}); err != nil {
		t.Fatalf("send watch: %v", err)
	}

	frameCh := make(chan frameResult, 64)
	go func() {
		for {
			f, err := reader.ReadFrame()
			frameCh <- frameResult{f, err}
			if err != nil || f == nil {
				return
			}
		}
	}()

	var collectedOutput string
	done := false
	timeout := time.After(5 * time.Second)

loop:
	for {
		select {
		case fr := <-frameCh:
			if fr.err != nil || fr.frame == nil {
				break loop
			}
			if fr.frame.Type == protocol.FrameControl {
				var r protocol.Response
				if err := json.Unmarshal(fr.frame.Payload, &r); err != nil {
					t.Fatalf("parse watch response: %v", err)
				}
				if r.Type == "WatchUpdate" {
					if r.Output != nil {
						collectedOutput += *r.Output
					}
					if r.Done != nil && *r.Done {
						done = true
						break loop
					}
				} else {
					t.Fatalf("unexpected response: %s: %s", r.Type, r.Message)
				}
			}
		case <-timeout:
			break loop
		}
	}

	if !strings.Contains(collectedOutput, "WATCH_") {
		t.Fatalf("should have received watch output: %q", collectedOutput)
	}
	if !done {
		t.Fatal("watch should complete when session ends")
	}
}

func TestLaunchWithEnv(t *testing.T) {
	dir := tempDir(t, "launch-env")
	sock := startTestNode(t, dir)

	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"bash", "-c", "echo MY_VAR=$MY_TEST_VAR"},
		WorkingDir: "/tmp",
		Env:        []string{"MY_TEST_VAR=hello-codewire"},
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID
	time.Sleep(2 * time.Second)

	resp = requestResponse(t, sock, &protocol.Request{
		Type:   "Logs",
		ID:     uint32Ptr(id),
		Follow: boolPtr(false),
	})
	if !strings.Contains(resp.Data, "MY_VAR=hello-codewire") {
		t.Fatalf("expected env var in output, got: %q", resp.Data)
	}
}

func TestLaunchWithStdinData(t *testing.T) {
	dir := tempDir(t, "stdin-data")
	sock := startTestNode(t, dir)

	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"cat"},
		WorkingDir: "/tmp",
		StdinData:  []byte("PROMPT_CONTENT_12345\n"),
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID
	time.Sleep(2 * time.Second)

	resp = requestResponse(t, sock, &protocol.Request{
		Type:   "Logs",
		ID:     uint32Ptr(id),
		Follow: boolPtr(false),
	})
	if !strings.Contains(resp.Data, "PROMPT_CONTENT_12345") {
		t.Fatalf("expected stdin content in output, got: %q", resp.Data)
	}
}

func TestMultiplexedWatch(t *testing.T) {
	dir := tempDir(t, "mux-watch")
	sock := startTestNode(t, dir)

	// Launch two tagged sessions.
	for i, msg := range []string{"OUTPUT_A", "OUTPUT_B"} {
		resp := requestResponse(t, sock, &protocol.Request{
			Type:       "Launch",
			Command:    []string{"bash", "-c", fmt.Sprintf("echo %s && sleep 1", msg)},
			WorkingDir: "/tmp",
			Tags:       []string{"mux-test"},
			Name:       fmt.Sprintf("mux-%d", i),
		})
		if resp.Type != "Launched" {
			t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
		}
	}

	time.Sleep(500 * time.Millisecond)

	// Use WatchMultiByTag with a writer to capture output.
	target := &client.Target{Local: dir}
	var buf strings.Builder
	timeout := uint64(5)
	err := client.WatchMultiByTag(target, "mux-test", &buf, &timeout)
	if err != nil {
		t.Fatalf("WatchMultiByTag: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "OUTPUT_A") {
		t.Fatalf("missing OUTPUT_A: %q", output)
	}
	if !strings.Contains(output, "OUTPUT_B") {
		t.Fatalf("missing OUTPUT_B: %q", output)
	}
}

func TestEventDrivenPersistence(t *testing.T) {
	dir := tempDir(t, "evt-persist")
	sock := startTestNode(t, dir)

	sessionsJSON := filepath.Join(dir, "sessions.json")

	// Launch a session.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"sleep", "5"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID

	// Wait for persistence (debounced to 500ms).
	time.Sleep(800 * time.Millisecond)

	// Get initial mtime.
	stat1, err := os.Stat(sessionsJSON)
	if err != nil {
		t.Fatalf("stat sessions.json: %v", err)
	}
	mtime1 := stat1.ModTime()

	// Wait 2 seconds -- no state changes, so no writes expected.
	time.Sleep(2 * time.Second)

	stat2, err := os.Stat(sessionsJSON)
	if err != nil {
		t.Fatalf("stat sessions.json: %v", err)
	}
	mtime2 := stat2.ModTime()

	if !mtime1.Equal(mtime2) {
		t.Fatalf("sessions.json was written without state changes (mtime1=%v, mtime2=%v)", mtime1, mtime2)
	}

	// Now make a state change (kill the session).
	requestResponse(t, sock, &protocol.Request{Type: "Kill", ID: uint32Ptr(id)})

	// Wait for persistence.
	time.Sleep(800 * time.Millisecond)

	stat3, err := os.Stat(sessionsJSON)
	if err != nil {
		t.Fatalf("stat sessions.json: %v", err)
	}
	mtime3 := stat3.ModTime()

	if !mtime3.After(mtime2) {
		t.Fatalf("sessions.json was not written on state change (mtime2=%v, mtime3=%v)", mtime2, mtime3)
	}
}

func TestCorruptSessionsJsonRecovery(t *testing.T) {
	dir := tempDir(t, "corrupt-sessions")
	sessionsJSON := filepath.Join(dir, "sessions.json")

	// Write corrupt JSON.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(sessionsJSON, []byte("invalid json{[["), 0o644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	// Start node -- should recover gracefully.
	sock := startTestNode(t, dir)

	// Should start with empty session list (corrupt file ignored).
	resp := requestResponse(t, sock, &protocol.Request{Type: "ListSessions"})
	if resp.Type != "SessionList" {
		t.Fatalf("expected SessionList, got %s: %s", resp.Type, resp.Message)
	}
	if len(*resp.Sessions) != 0 {
		t.Fatalf("should start with no sessions, got %d", len(*resp.Sessions))
	}

	// Backup file should exist.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	backupCount := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sessions.json.corrupt") {
			backupCount++
		}
	}
	if backupCount != 1 {
		t.Fatalf("expected 1 corrupt backup file, found %d", backupCount)
	}

	// Node should be functional -- launch a new session.
	resp = requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"echo", "test"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
}

func TestResolveSessionOrTag(t *testing.T) {
	dir := tempDir(t, "resolve-tag")
	sock := startTestNode(t, dir)
	target := &client.Target{Local: dir}

	// Launch two sessions with tag "batch-99"
	r1 := requestResponse(t, sock, &protocol.Request{
		Type: "Launch", Command: []string{"sleep", "5"}, WorkingDir: "/tmp",
		Tags: []string{"batch-99"},
	})
	if r1.Type != "Launched" {
		t.Fatalf("launch 1: %s", r1.Message)
	}
	r2 := requestResponse(t, sock, &protocol.Request{
		Type: "Launch", Command: []string{"sleep", "5"}, WorkingDir: "/tmp",
		Tags: []string{"batch-99"},
	})
	if r2.Type != "Launched" {
		t.Fatalf("launch 2: %s", r2.Message)
	}
	time.Sleep(200 * time.Millisecond)

	// "batch-99" is not a session name — should resolve to tag
	id, tags, err := client.ResolveSessionOrTag(target, "batch-99")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != nil {
		t.Fatalf("expected no session ID, got %d", *id)
	}
	if len(tags) != 1 || tags[0] != "batch-99" {
		t.Fatalf("expected tags=[batch-99], got %v", tags)
	}

	// A numeric ID resolves as session
	id2, tags2, err2 := client.ResolveSessionOrTag(target, fmt.Sprintf("%d", *r1.ID))
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if id2 == nil || *id2 != *r1.ID {
		t.Fatalf("expected session ID %d", *r1.ID)
	}
	if len(tags2) != 0 {
		t.Fatalf("expected no tags, got %v", tags2)
	}
}

func TestWaitByTagPositional(t *testing.T) {
	dir := tempDir(t, "wait-tag-positional")
	sock := startTestNode(t, dir)
	target := &client.Target{Local: dir}

	// Launch two short-lived sessions tagged "wt-42"
	for i := 0; i < 2; i++ {
		r := requestResponse(t, sock, &protocol.Request{
			Type: "Launch", Command: []string{"bash", "-c", "sleep 0.2"},
			WorkingDir: "/tmp", Tags: []string{"wt-42"},
		})
		if r.Type != "Launched" {
			t.Fatalf("launch %d: %s", i, r.Message)
		}
	}

	// WaitForSession with tag "wt-42" should wait for both
	done := make(chan error, 1)
	go func() {
		done <- client.WaitForSession(target, nil, []string{"wt-42"}, "all", nil)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForSession: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for tagged sessions")
	}
}

func TestListStatusFilter(t *testing.T) {
	dir := tempDir(t, "list-status")
	sock := startTestNode(t, dir)
	target := &client.Target{Local: dir}

	// Launch one long-running and one short session
	requestResponse(t, sock, &protocol.Request{
		Type: "Launch", Command: []string{"sleep", "30"}, WorkingDir: "/tmp",
	})
	requestResponse(t, sock, &protocol.Request{
		Type: "Launch", Command: []string{"bash", "-c", "exit 0"}, WorkingDir: "/tmp",
	})
	time.Sleep(300 * time.Millisecond)

	// Filter running — should see only running sessions
	sessions, err := client.ListFiltered(target, "running")
	if err != nil {
		t.Fatalf("ListFiltered: %v", err)
	}
	for _, s := range sessions {
		if s.Status != "running" {
			t.Fatalf("expected only running sessions, got %s", s.Status)
		}
	}
	if len(sessions) < 1 {
		t.Fatal("expected at least 1 running session")
	}
}

func TestCWSessionIDEnv(t *testing.T) {
	dir := tempDir(t, "cw-session-id")
	sock := startTestNode(t, dir)

	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"sh", "-c", "echo CW_SESSION_ID=$CW_SESSION_ID"},
		WorkingDir: "/tmp",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	id := *resp.ID
	time.Sleep(1 * time.Second)

	logsResp := requestResponse(t, sock, &protocol.Request{
		Type:   "Logs",
		ID:     uint32Ptr(id),
		Follow: boolPtr(false),
	})
	expected := fmt.Sprintf("CW_SESSION_ID=%d", id)
	if !strings.Contains(logsResp.Data, expected) {
		t.Fatalf("expected %q in output, got: %q", expected, logsResp.Data)
	}
}

func TestGatewayAutoReply(t *testing.T) {
	dir := tempDir(t, "gateway")
	sock := startTestNode(t, dir)

	// Launch "gateway" stub session
	gwResp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"sleep", "30"},
		WorkingDir: "/tmp",
		Name:       "gateway",
	})
	if gwResp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", gwResp.Type, gwResp.Message)
	}
	gatewayID := *gwResp.ID

	// Subscribe to message.request events on gateway session
	subConn, subReader, subWriter := connectRaw(t, sock)
	defer subConn.Close()
	if err := subWriter.SendRequest(&protocol.Request{
		Type:       "Subscribe",
		ID:         uint32Ptr(gatewayID),
		EventTypes: []string{"message.request"},
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Read SubscribeAck
	ackFrame, _ := subReader.ReadFrame()
	var ackResp protocol.Response
	json.Unmarshal(ackFrame.Payload, &ackResp)
	if ackResp.Type != "SubscribeAck" {
		t.Fatalf("expected SubscribeAck, got %s", ackResp.Type)
	}

	// Launch worker session
	wkResp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"sleep", "5"},
		WorkingDir: "/tmp",
	})
	if wkResp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s", wkResp.Type)
	}
	workerID := *wkResp.ID

	// Send MsgRequest from worker to gateway (non-blocking: just send the frame, read result later)
	reqConn, reqReader, reqWriter := connectRaw(t, sock)
	defer reqConn.Close()
	to := uint64(10)
	if err := reqWriter.SendRequest(&protocol.Request{
		Type:           "MsgRequest",
		ID:             uint32Ptr(workerID),
		ToName:         "gateway",
		Body:           "approve: git push",
		TimeoutSeconds: &to,
	}); err != nil {
		t.Fatalf("send MsgRequest: %v", err)
	}

	// Wait for message.request event from subscriber
	frameCh := make(chan frameResult, 8)
	go func() {
		for {
			f, err := subReader.ReadFrame()
			frameCh <- frameResult{f, err}
			if err != nil || f == nil {
				return
			}
		}
	}()

	var requestID string
	deadline := time.After(5 * time.Second)
loop:
	for {
		select {
		case fr := <-frameCh:
			if fr.err != nil {
				t.Errorf("reading event: %v", fr.err)
				break loop
			}
			if fr.frame == nil {
				break loop
			}
			if fr.frame.Type != protocol.FrameControl {
				continue
			}
			var evtResp protocol.Response
			json.Unmarshal(fr.frame.Payload, &evtResp)
			if evtResp.Type != "Event" || evtResp.Event == nil {
				continue
			}
			if evtResp.Event.EventType != "message.request" {
				continue
			}
			var rd struct {
				RequestID string `json:"request_id"`
				Body      string `json:"body"`
			}
			json.Unmarshal(evtResp.Event.Data, &rd)
			if rd.Body != "approve: git push" {
				t.Errorf("unexpected body: %s", rd.Body)
			}
			requestID = rd.RequestID
			break loop
		case <-deadline:
			t.Fatal("timeout waiting for message.request event")
		}
	}

	if requestID == "" {
		t.Fatal("did not receive request_id")
	}

	// Reply as gateway
	replyResp := requestResponse(t, sock, &protocol.Request{
		Type:      "MsgReply",
		ID:        uint32Ptr(gatewayID),
		RequestID: requestID,
		Body:      "APPROVED",
	})
	if replyResp.Type == "Error" {
		t.Fatalf("MsgReply error: %s", replyResp.Message)
	}

	// Verify worker got the reply
	resultCh := make(chan frameResult, 1)
	go func() {
		f, err := reqReader.ReadFrame()
		resultCh <- frameResult{f, err}
	}()

	select {
	case fr := <-resultCh:
		if fr.err != nil {
			t.Fatalf("reading MsgRequestResult: %v", fr.err)
		}
		var resultResp protocol.Response
		json.Unmarshal(fr.frame.Payload, &resultResp)
		if resultResp.Type != "MsgRequestResult" {
			t.Fatalf("expected MsgRequestResult, got %s: %s", resultResp.Type, resultResp.Message)
		}
		if resultResp.ReplyBody != "APPROVED" {
			t.Fatalf("expected APPROVED, got %q", resultResp.ReplyBody)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for MsgRequestResult")
	}
}

func TestAnonymousSendRequest(t *testing.T) {
	dir := tempDir(t, "anon-request")
	sock := startTestNode(t, dir)

	// Launch the target session.
	resp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"sleep", "30"},
		WorkingDir: "/tmp",
		Name:       "anon-target",
	})
	if resp.Type != "Launched" {
		t.Fatalf("expected Launched, got %s: %s", resp.Type, resp.Message)
	}
	targetID := *resp.ID

	time.Sleep(200 * time.Millisecond)

	// Subscribe to message.request events on the target session.
	subConn, subReader, subWriter := connectRaw(t, sock)
	defer subConn.Close()

	if err := subWriter.SendRequest(&protocol.Request{
		Type:       "Subscribe",
		ID:         uint32Ptr(targetID),
		EventTypes: []string{"message.request"},
	}); err != nil {
		t.Fatalf("send subscribe: %v", err)
	}

	// Read SubscribeAck.
	ackFrame, err := subReader.ReadFrame()
	if err != nil {
		t.Fatalf("read subscribe ack: %v", err)
	}
	var ackResp protocol.Response
	if err := json.Unmarshal(ackFrame.Payload, &ackResp); err != nil {
		t.Fatalf("parse ack: %v", err)
	}
	if ackResp.Type != "SubscribeAck" {
		t.Fatalf("expected SubscribeAck, got %s", ackResp.Type)
	}

	// Channel for the incoming message.request event.
	type eventResult struct {
		requestID string
		err       error
	}
	eventCh := make(chan eventResult, 1)

	go func() {
		deadline := time.After(5 * time.Second)
		for {
			frameCh := make(chan frameResult, 1)
			go func() {
				f, err := subReader.ReadFrame()
				frameCh <- frameResult{f, err}
			}()
			select {
			case fr := <-frameCh:
				if fr.err != nil {
					eventCh <- eventResult{err: fr.err}
					return
				}
				if fr.frame == nil {
					eventCh <- eventResult{err: fmt.Errorf("unexpected EOF")}
					return
				}
				if fr.frame.Type != protocol.FrameControl {
					continue
				}
				var r protocol.Response
				if err := json.Unmarshal(fr.frame.Payload, &r); err != nil {
					continue
				}
				if r.Type != "Event" || r.Event == nil {
					continue
				}
				if r.Event.EventType != "message.request" {
					continue
				}
				// Extract request_id from event data.
				var data struct {
					RequestID string `json:"request_id"`
				}
				if err := json.Unmarshal(r.Event.Data, &data); err != nil {
					eventCh <- eventResult{err: fmt.Errorf("unmarshal event data: %v", err)}
					return
				}
				eventCh <- eventResult{requestID: data.RequestID}
				return
			case <-deadline:
				eventCh <- eventResult{err: fmt.Errorf("timeout waiting for message.request event")}
				return
			}
		}
	}()

	// Send MsgRequest anonymously (no ID field = fromID 0).
	reqConn, reqReader, reqWriter := connectRaw(t, sock)
	defer reqConn.Close()

	if err := reqWriter.SendRequest(&protocol.Request{
		Type:  "MsgRequest",
		ToID:  uint32Ptr(targetID),
		Body:  "approve?",
	}); err != nil {
		t.Fatalf("send MsgRequest: %v", err)
	}

	// Wait for the event to arrive at the subscriber.
	evRes := <-eventCh
	if evRes.err != nil {
		t.Fatalf("waiting for message.request event: %v", evRes.err)
	}
	requestID := evRes.requestID
	if requestID == "" {
		t.Fatal("expected non-empty request_id in event")
	}

	// Reply to the request.
	replyConn, _, replyWriter := connectRaw(t, sock)
	defer replyConn.Close()

	if err := replyWriter.SendRequest(&protocol.Request{
		Type:      "MsgReply",
		RequestID: requestID,
		Body:      "ack",
	}); err != nil {
		t.Fatalf("send MsgReply: %v", err)
	}

	// Read the MsgRequestResult on the original requester connection.
	resultFrame, err := reqReader.ReadFrame()
	if err != nil {
		t.Fatalf("read MsgRequestResult frame: %v", err)
	}
	if resultFrame == nil {
		t.Fatal("unexpected EOF reading MsgRequestResult")
	}
	var resultResp protocol.Response
	if err := json.Unmarshal(resultFrame.Payload, &resultResp); err != nil {
		t.Fatalf("parse MsgRequestResult: %v", err)
	}
	if resultResp.Type != "MsgRequestResult" {
		t.Fatalf("expected MsgRequestResult, got %s: %s", resultResp.Type, resultResp.Message)
	}
	if resultResp.ReplyBody != "ack" {
		t.Fatalf("expected reply body 'ack', got %q", resultResp.ReplyBody)
	}
}

// TestHookNoGateway verifies that cw hook exits 0 (allow) when no gateway
// session is running.
func TestHookNoGateway(t *testing.T) {
	t.Parallel()
	dir := tempDir(t, "hook-no-gateway")
	sock := startTestNode(t, dir)

	target := &client.Target{Local: dir}
	_ = sock // node is running but no gateway session

	var out strings.Builder
	blocked, err := client.Hook(target, strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"rm -rf /"}}`), &out)
	if err != nil {
		t.Fatalf("Hook() error: %v", err)
	}
	if blocked {
		t.Fatalf("expected allow (no gateway), got block: %s", out.String())
	}
	if out.Len() != 0 {
		t.Fatalf("expected no output, got: %s", out.String())
	}
}

// TestHookReadOnlyBypass verifies that read-only tools are allowed without
// contacting the gateway.
func TestHookReadOnlyBypass(t *testing.T) {
	t.Parallel()
	dir := tempDir(t, "hook-readonly")
	_ = startTestNode(t, dir)
	target := &client.Target{Local: dir}

	for _, tool := range []string{"Read", "Glob", "Grep", "WebFetch", "WebSearch", "TodoRead", "TaskList", "TaskGet"} {
		tool := tool
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			input := fmt.Sprintf(`{"tool_name":%q,"tool_input":{}}`, tool)
			var out strings.Builder
			blocked, err := client.Hook(target, strings.NewReader(input), &out)
			if err != nil {
				t.Fatalf("Hook() error: %v", err)
			}
			if blocked {
				t.Fatalf("read-only tool %q should not be blocked", tool)
			}
		})
	}
}

// TestHookDenied verifies that cw hook exits 2 and writes a JSON block decision
// when the gateway returns DENIED.
func TestHookDenied(t *testing.T) {
	t.Parallel()
	dir := tempDir(t, "hook-denied")
	sock := startTestNode(t, dir)

	// Launch a gateway session.
	gatewayResp := requestResponse(t, sock, &protocol.Request{
		Type:       "Launch",
		Command:    []string{"sleep", "30"},
		WorkingDir: "/tmp",
		Name:       "gateway",
		Tags:       []string{"_gateway"},
	})
	if gatewayResp.Type != "Launched" || gatewayResp.ID == nil {
		t.Fatalf("launch gateway: unexpected response %q", gatewayResp.Type)
	}
	gatewayID := *gatewayResp.ID

	// Subscribe to message.request on the gateway session.
	subConn, subReader, subWriter := connectRaw(t, sock)
	defer subConn.Close()
	if err := subWriter.SendRequest(&protocol.Request{
		Type:       "Subscribe",
		ID:         &gatewayID,
		EventTypes: []string{"message.request"},
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Drain SubscribeAck.
	ackFrame, err := subReader.ReadFrame()
	if err != nil || ackFrame == nil {
		t.Fatalf("read SubscribeAck: %v", err)
	}

	// Run Hook() in a goroutine — it will block waiting for the gateway reply.
	target := &client.Target{Local: dir}
	var out strings.Builder
	hookDone := make(chan struct{ blocked bool; err error }, 1)
	go func() {
		blocked, err := client.Hook(target, strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"rm -rf /"}}`), &out)
		hookDone <- struct{ blocked bool; err error }{blocked, err}
	}()

	// Simulate gateway receiving the request and replying DENIED.
	select {
	case frame := <-func() <-chan *protocol.Frame {
		ch := make(chan *protocol.Frame, 1)
		go func() {
			f, _ := subReader.ReadFrame()
			ch <- f
		}()
		return ch
	}():
		if frame == nil {
			t.Fatal("no frame from gateway subscription")
		}
		var resp protocol.Response
		if err := json.Unmarshal(frame.Payload, &resp); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		if resp.Type != "Event" || resp.Event == nil {
			t.Fatalf("expected Event, got %q", resp.Type)
		}
		var reqData struct {
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(resp.Event.Data, &reqData); err != nil {
			t.Fatalf("unmarshal RequestData: %v", err)
		}
		// Reply with DENIED.
		replyConn, _, replyWriter := connectRaw(t, sock)
		defer replyConn.Close()
		if err := replyWriter.SendRequest(&protocol.Request{
			Type:      "MsgReply",
			RequestID: reqData.RequestID,
			Body:      "DENIED: too dangerous",
		}); err != nil {
			t.Fatalf("send MsgReply: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for gateway request event")
	}

	// Wait for Hook() to return.
	select {
	case result := <-hookDone:
		if result.err != nil {
			t.Fatalf("Hook() error: %v", result.err)
		}
		if !result.blocked {
			t.Fatal("expected block, got allow")
		}
		// Verify the output is valid JSON with decision=block.
		var decision struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &decision); err != nil {
			t.Fatalf("parse hook output %q: %v", out.String(), err)
		}
		if decision.Decision != "block" {
			t.Fatalf("expected decision=block, got %q", decision.Decision)
		}
		if !strings.Contains(decision.Reason, "too dangerous") {
			t.Fatalf("expected reason to contain 'too dangerous', got %q", decision.Reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Hook() to return")
	}
}
