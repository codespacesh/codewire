package client

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/codespacesh/codewire/internal/connection"
	"github.com/codespacesh/codewire/internal/protocol"
	"github.com/codespacesh/codewire/internal/statusbar"
	"github.com/codespacesh/codewire/internal/terminal"
)

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// List retrieves all sessions from the node and prints them as a table or JSON.
func List(target *Target, jsonOutput bool) error {
	resp, err := requestResponse(target, &protocol.Request{Type: "ListSessions"})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", formatError(resp.Message))
	}
	if resp.Sessions == nil {
		return fmt.Errorf("unexpected response type: %s", resp.Type)
	}

	sessions := *resp.Sessions

	if jsonOutput {
		data, err := json.MarshalIndent(sessions, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	if len(sessions) == 0 {
		fmt.Println("No active sessions")
		return nil
	}

	printSessionTable(sessions)
	return nil
}

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

// Run launches a new session on the node with the given command and working
// directory.
func Run(target *Target, command []string, workingDir string) error {
	resp, err := requestResponse(target, &protocol.Request{
		Type:       "Launch",
		Command:    command,
		WorkingDir: workingDir,
	})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", formatError(resp.Message))
	}
	if resp.Type != "Launched" || resp.ID == nil {
		return fmt.Errorf("unexpected response type: %s", resp.Type)
	}

	display := strings.Join(command, " ")
	fmt.Fprintf(os.Stderr, "Session %d launched: %s\n", *resp.ID, display)
	return nil
}

// ---------------------------------------------------------------------------
// Attach
// ---------------------------------------------------------------------------

// stdinEvent carries the result of a single stdin read.
type stdinEvent struct {
	detach  bool
	forward []byte
	err     error
}

// frameEvent carries the result of a single frame read from the node.
type frameEvent struct {
	frame *protocol.Frame
	err   error
}

// Attach connects to a session's PTY. If id is nil, the oldest running
// unattached session is selected automatically. The terminal is put into raw
// mode and a status bar is drawn at the bottom of the screen.
func Attach(target *Target, id *uint32, noHistory bool) error {
	// ---------------------------------------------------------------
	// Step 1: auto-select session if no ID given
	// ---------------------------------------------------------------
	if id == nil {
		resp, err := requestResponse(target, &protocol.Request{Type: "ListSessions"})
		if err != nil {
			return err
		}
		if resp.Type == "Error" {
			return fmt.Errorf("%s", formatError(resp.Message))
		}
		if resp.Sessions == nil {
			return fmt.Errorf("unexpected response type: %s", resp.Type)
		}
		sessions := *resp.Sessions

		// Filter running and unattached.
		var candidates []protocol.SessionInfo
		for _, s := range sessions {
			if s.Status == "running" && !s.Attached {
				candidates = append(candidates, s)
			}
		}
		if len(candidates) == 0 {
			return fmt.Errorf("no running unattached sessions available\n\nUse 'cw list' to see active sessions")
		}
		// Sort by created_at ascending (oldest first).
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].CreatedAt < candidates[j].CreatedAt
		})
		id = &candidates[0].ID
	}

	// ---------------------------------------------------------------
	// Step 2: connect and send Attach request
	// ---------------------------------------------------------------
	reader, writer, err := target.Connect()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()

	includeHistory := !noHistory
	req := &protocol.Request{
		Type:           "Attach",
		ID:             id,
		IncludeHistory: &includeHistory,
	}
	if err := writer.SendRequest(req); err != nil {
		return fmt.Errorf("sending attach request: %w", err)
	}

	// Read the Attached response.
	frame, err := reader.ReadFrame()
	if err != nil {
		return fmt.Errorf("reading attach response: %w", err)
	}
	if frame == nil {
		return fmt.Errorf("connection closed before attach response")
	}
	if frame.Type != protocol.FrameControl {
		return fmt.Errorf("expected control frame, got type 0x%02x", frame.Type)
	}

	var resp protocol.Response
	if err := json.Unmarshal(frame.Payload, &resp); err != nil {
		return fmt.Errorf("parsing attach response: %w", err)
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", formatError(resp.Message))
	}
	if resp.Type != "Attached" {
		return fmt.Errorf("unexpected response: %s", resp.Type)
	}

	sessionID := *id
	fmt.Fprintf(os.Stderr, "[cw] attached to session %d\n", sessionID)

	// ---------------------------------------------------------------
	// Step 3: enter raw mode
	// ---------------------------------------------------------------
	guard, err := terminal.EnableRawMode()
	if err != nil {
		return fmt.Errorf("enabling raw mode: %w", err)
	}
	defer guard.Restore()

	// ---------------------------------------------------------------
	// Step 4: set up status bar
	// ---------------------------------------------------------------
	cols, rows, err := terminal.TerminalSize()
	if err != nil {
		guard.Restore()
		return fmt.Errorf("getting terminal size: %w", err)
	}

	bar := statusbar.New(uint32(sessionID), cols, rows)
	if setup := bar.Setup(); setup != nil {
		os.Stdout.Write(setup)
	}

	// Tell the node the PTY size (accounting for status bar).
	ptyCols, ptyRows := bar.PtySize()
	resizeReq := &protocol.Request{
		Type: "Resize",
		ID:   &sessionID,
		Cols: &ptyCols,
		Rows: &ptyRows,
	}
	if err := writer.SendRequest(resizeReq); err != nil {
		guard.Restore()
		return fmt.Errorf("sending initial resize: %w", err)
	}

	// ---------------------------------------------------------------
	// Step 5: set up SIGWINCH handler
	// ---------------------------------------------------------------
	winchCh, winchCleanup := terminal.ResizeSignal()
	defer winchCleanup()

	// ---------------------------------------------------------------
	// Step 6: set up 10s ticker for status bar redraw
	// ---------------------------------------------------------------
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// ---------------------------------------------------------------
	// Step 7: stdin reader goroutine
	// ---------------------------------------------------------------
	detector := terminal.NewDetachDetector()
	stdinCh := make(chan stdinEvent, 1)
	go func() {
		for {
			buf := make([]byte, 4096)
			n, readErr := os.Stdin.Read(buf)
			if n > 0 {
				detach, fwd := detector.FeedBuf(buf[:n])
				stdinCh <- stdinEvent{detach: detach, forward: fwd, err: nil}
				if detach {
					return
				}
			}
			if readErr != nil {
				stdinCh <- stdinEvent{err: readErr}
				return
			}
		}
	}()

	// ---------------------------------------------------------------
	// Step 8: frame reader goroutine
	// ---------------------------------------------------------------
	frameCh := make(chan frameEvent, 1)
	go func() {
		for {
			f, readErr := reader.ReadFrame()
			frameCh <- frameEvent{frame: f, err: readErr}
			if readErr != nil || f == nil {
				return
			}
		}
	}()

	// ---------------------------------------------------------------
	// Step 9: main select loop
	// ---------------------------------------------------------------
	for {
		select {
		case fe := <-frameCh:
			if fe.err != nil {
				teardown(bar, guard)
				fmt.Fprintf(os.Stderr, "\n[cw] connection error: %v\n", fe.err)
				os.Exit(1)
			}
			if fe.frame == nil {
				teardown(bar, guard)
				fmt.Fprintf(os.Stderr, "\n[cw] connection lost\n")
				os.Exit(1)
			}
			switch fe.frame.Type {
			case protocol.FrameData:
				os.Stdout.Write(fe.frame.Payload)
				if draw := bar.Draw(); draw != nil {
					os.Stdout.Write(draw)
				}
			case protocol.FrameControl:
				var ctrlResp protocol.Response
				if err := json.Unmarshal(fe.frame.Payload, &ctrlResp); err != nil {
					teardown(bar, guard)
					fmt.Fprintf(os.Stderr, "\n[cw] bad control frame: %v\n", err)
					os.Exit(1)
				}
				switch ctrlResp.Type {
				case "Detached":
					teardown(bar, guard)
					fmt.Fprintf(os.Stderr, "\n[cw] detached from session %d\n", sessionID)
					os.Exit(0)
				case "Error":
					teardown(bar, guard)
					fmt.Fprintf(os.Stderr, "\n[cw] %s\n", formatError(ctrlResp.Message))
					os.Exit(0)
				default:
					// Ignore other control messages.
				}
			}

		case se := <-stdinCh:
			if se.err != nil {
				// stdin closed or error, just continue until connection drops.
				continue
			}
			if se.detach {
				// Send detach request and wait for confirmation from the node.
				detachReq := &protocol.Request{
					Type: "Detach",
					ID:   &sessionID,
				}
				_ = writer.SendRequest(detachReq)
				continue
			}
			if len(se.forward) > 0 {
				if err := writer.SendData(se.forward); err != nil {
					teardown(bar, guard)
					fmt.Fprintf(os.Stderr, "\n[cw] write error: %v\n", err)
					os.Exit(1)
				}
			}

		case <-winchCh:
			newCols, newRows, err := terminal.TerminalSize()
			if err != nil {
				continue
			}
			if resize := bar.Resize(newCols, newRows); resize != nil {
				os.Stdout.Write(resize)
			}
			ptyCols, ptyRows := bar.PtySize()
			resizeReq := &protocol.Request{
				Type: "Resize",
				ID:   &sessionID,
				Cols: &ptyCols,
				Rows: &ptyRows,
			}
			_ = writer.SendRequest(resizeReq)

		case <-ticker.C:
			if draw := bar.Draw(); draw != nil {
				os.Stdout.Write(draw)
			}
		}
	}
}

// teardown restores the terminal and clears the status bar.
func teardown(bar *statusbar.StatusBar, guard *terminal.RawModeGuard) {
	if td := bar.Teardown(); td != nil {
		os.Stdout.Write(td)
	}
	guard.Restore()
}

// ---------------------------------------------------------------------------
// Kill
// ---------------------------------------------------------------------------

// Kill terminates a single session by ID.
func Kill(target *Target, id uint32) error {
	resp, err := requestResponse(target, &protocol.Request{
		Type: "Kill",
		ID:   &id,
	})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", formatError(resp.Message))
	}
	fmt.Fprintf(os.Stderr, "Session %d killed\n", id)
	return nil
}

// ---------------------------------------------------------------------------
// KillAll
// ---------------------------------------------------------------------------

// KillAll terminates all running sessions on the node.
func KillAll(target *Target) error {
	resp, err := requestResponse(target, &protocol.Request{Type: "KillAll"})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", formatError(resp.Message))
	}
	count := uint(0)
	if resp.Count != nil {
		count = *resp.Count
	}
	fmt.Fprintf(os.Stderr, "Killed %d session(s)\n", count)
	return nil
}

// ---------------------------------------------------------------------------
// Logs
// ---------------------------------------------------------------------------

// Logs retrieves the output log for a session. When follow is true, the client
// streams new output as it arrives until the session ends or the connection
// drops.
func Logs(target *Target, id uint32, follow bool, tail *int) error {
	reader, writer, err := target.Connect()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()

	req := &protocol.Request{
		Type:   "Logs",
		ID:     &id,
		Follow: &follow,
	}
	if tail != nil {
		t := uint(*tail)
		req.Tail = &t
	}

	if err := writer.SendRequest(req); err != nil {
		return fmt.Errorf("sending logs request: %w", err)
	}

	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			return fmt.Errorf("reading log frame: %w", err)
		}
		if frame == nil {
			return nil // clean EOF
		}

		if frame.Type != protocol.FrameControl {
			// Unexpected data frame; skip.
			continue
		}

		var resp protocol.Response
		if err := json.Unmarshal(frame.Payload, &resp); err != nil {
			return fmt.Errorf("parsing log response: %w", err)
		}

		switch resp.Type {
		case "LogData":
			if resp.Data != "" {
				os.Stdout.Write([]byte(resp.Data))
			}
			if resp.Done != nil && *resp.Done {
				return nil
			}
		case "Error":
			return fmt.Errorf("%s", formatError(resp.Message))
		default:
			// Ignore unknown response types.
		}
	}
}

// ---------------------------------------------------------------------------
// SendInput
// ---------------------------------------------------------------------------

// SendInput sends input to a session without attaching. The input can come
// from a direct argument, stdin, or a file. Unless noNewline is set, a
// trailing newline is appended.
func SendInput(target *Target, id uint32, input *string, useStdin bool, file *string, noNewline bool) error {
	var data []byte

	switch {
	case input != nil:
		data = []byte(*input)
	case useStdin:
		var err error
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
	case file != nil:
		var err error
		data, err = os.ReadFile(*file)
		if err != nil {
			return fmt.Errorf("reading file: %w", err)
		}
	default:
		return fmt.Errorf("no input source specified")
	}

	if !noNewline {
		data = append(data, '\n')
	}

	resp, err := requestResponse(target, &protocol.Request{
		Type: "SendInput",
		ID:   &id,
		Data: data,
	})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", formatError(resp.Message))
	}

	bytes := uint(0)
	if resp.Bytes != nil {
		bytes = *resp.Bytes
	}
	fmt.Fprintf(os.Stderr, "Sent %d bytes to session %d\n", bytes, id)
	return nil
}

// ---------------------------------------------------------------------------
// WatchSession
// ---------------------------------------------------------------------------

// WatchSession watches a session's output in real-time without attaching.
// An optional timeout (in seconds) limits how long to wait.
func WatchSession(target *Target, id uint32, tail *int, noHistory bool, timeout *uint64) error {
	reader, writer, err := target.Connect()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()

	includeHistory := !noHistory
	req := &protocol.Request{
		Type:           "WatchSession",
		ID:             &id,
		IncludeHistory: &includeHistory,
	}
	if tail != nil {
		t := uint(*tail)
		req.Tail = &t
	}

	if err := writer.SendRequest(req); err != nil {
		return fmt.Errorf("sending watch request: %w", err)
	}

	// Set up timeout timer.
	var timeoutDuration time.Duration
	if timeout != nil {
		timeoutDuration = time.Duration(*timeout) * time.Second
	} else {
		// Effectively infinite.
		timeoutDuration = time.Duration(math.MaxInt64)
	}
	timer := time.NewTimer(timeoutDuration)
	defer timer.Stop()

	// Frame reader goroutine.
	frameCh := make(chan frameEvent, 1)
	go readFrames(reader, frameCh)

	for {
		select {
		case fe := <-frameCh:
			if fe.err != nil {
				return fmt.Errorf("reading watch frame: %w", fe.err)
			}
			if fe.frame == nil {
				return nil // clean EOF
			}
			if fe.frame.Type != protocol.FrameControl {
				continue
			}
			var resp protocol.Response
			if err := json.Unmarshal(fe.frame.Payload, &resp); err != nil {
				return fmt.Errorf("parsing watch response: %w", err)
			}
			switch resp.Type {
			case "WatchUpdate":
				if resp.Output != nil {
					os.Stdout.Write([]byte(*resp.Output))
				}
				if resp.Done != nil && *resp.Done {
					return nil
				}
			case "Error":
				return fmt.Errorf("%s", formatError(resp.Message))
			}

		case <-timer.C:
			fmt.Fprintf(os.Stderr, "\n[cw] watch timeout reached\n")
			return nil
		}
	}
}

// readFrames reads frames in a loop and sends them to the channel.
func readFrames(reader connection.FrameReader, ch chan<- frameEvent) {
	for {
		f, err := reader.ReadFrame()
		ch <- frameEvent{frame: f, err: err}
		if err != nil || f == nil {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// GetStatus
// ---------------------------------------------------------------------------

// GetStatus retrieves detailed status information for a single session.
func GetStatus(target *Target, id uint32, jsonOutput bool) error {
	resp, err := requestResponse(target, &protocol.Request{
		Type: "GetStatus",
		ID:   &id,
	})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", formatError(resp.Message))
	}
	if resp.Info == nil {
		return fmt.Errorf("unexpected response type: %s", resp.Type)
	}

	info := resp.Info

	if jsonOutput {
		data, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	// Print a structured status view.
	fmt.Printf("Session %d\n", info.ID)
	fmt.Printf("  Command:     %s\n", info.Prompt)
	fmt.Printf("  Working Dir: %s\n", info.WorkingDir)
	fmt.Printf("  Status:      %s\n", info.Status)
	fmt.Printf("  Created:     %s\n", info.CreatedAt)
	fmt.Printf("  Attached:    %v\n", info.Attached)
	if info.PID != nil {
		fmt.Printf("  PID:         %d\n", *info.PID)
	}
	if info.OutputSizeBytes != nil {
		fmt.Printf("  Output Size: %d bytes\n", *info.OutputSizeBytes)
	}
	if resp.OutputSize != nil {
		fmt.Printf("  Log Size:    %d bytes\n", *resp.OutputSize)
	}
	if info.LastOutputSnippet != nil {
		fmt.Printf("  Last Output:\n%s\n", *info.LastOutputSnippet)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// printSessionTable prints a formatted table of sessions.
func printSessionTable(sessions []protocol.SessionInfo) {
	// Column headers.
	fmt.Printf("%-6s %-20s %-12s %-10s %-8s\n", "ID", "COMMAND", "STATUS", "CREATED", "ATTACHED")

	for _, s := range sessions {
		prompt := s.Prompt
		if len(prompt) > 20 {
			prompt = prompt[:17] + "..."
		}
		created := formatRelativeTime(s.CreatedAt)
		attached := "no"
		if s.Attached {
			attached = "yes"
		}
		fmt.Printf("%-6d %-20s %-12s %-10s %-8s\n", s.ID, prompt, s.Status, created, attached)
	}
}

// formatRelativeTime converts an RFC3339 timestamp to a human-readable
// relative time string such as "5m ago".
func formatRelativeTime(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso // fall back to the raw string
	}
	d := time.Since(t)

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
