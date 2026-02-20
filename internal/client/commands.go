package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/codespacesh/codewire/internal/connection"
	"github.com/codespacesh/codewire/internal/protocol"
	"github.com/codespacesh/codewire/internal/statusbar"
	"github.com/codespacesh/codewire/internal/terminal"
)

// ResolveSessionArg resolves a session argument that can be either a numeric ID
// or a session name (optionally prefixed with @). It queries the node to
// resolve names to IDs.
func ResolveSessionArg(target *Target, arg string) (uint32, error) {
	// Strip leading @ if present.
	name := strings.TrimPrefix(arg, "@")

	// Try numeric ID first.
	if parsed, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uint32(parsed), nil
	}

	// Resolve by name — list sessions and find by name.
	resp, err := requestResponse(target, &protocol.Request{Type: "ListSessions"})
	if err != nil {
		return 0, err
	}
	if resp.Type == "Error" {
		return 0, fmt.Errorf("%s", formatError(resp.Message))
	}
	if resp.Sessions == nil {
		return 0, fmt.Errorf("no sessions found")
	}
	for _, s := range *resp.Sessions {
		if s.Name == name {
			return s.ID, nil
		}
	}
	return 0, fmt.Errorf("no session named %q", name)
}

// ResolveSessionOrTag tries to resolve arg as a session ID/name, then as a tag.
// Returns (sessionID, tags, err). Exactly one of sessionID or tags will be non-nil/non-empty.
func ResolveSessionOrTag(target *Target, arg string) (*uint32, []string, error) {
	// Try as session ID or name first.
	id, err := ResolveSessionArg(target, arg)
	if err == nil {
		return &id, nil, nil
	}

	// Only fall back to tag for "not found" errors, not connection errors.
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "not found") && !strings.Contains(lower, "no session named") {
		return nil, nil, err
	}

	// Check if any sessions have this tag.
	resp, listErr := requestResponse(target, &protocol.Request{Type: "ListSessions"})
	if listErr != nil {
		return nil, nil, err // return original error
	}
	if resp.Sessions != nil {
		for _, s := range *resp.Sessions {
			for _, t := range s.Tags {
				if t == arg {
					return nil, []string{arg}, nil
				}
			}
		}
	}

	return nil, nil, fmt.Errorf("no session or tag named %q\n\nUse 'cw list' to see active sessions", arg)
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// List retrieves sessions, optionally filtered by status.
func List(target *Target, jsonOutput bool, statusFilter string) error {
	sessions, err := ListFiltered(target, statusFilter)
	if err != nil {
		return err
	}
	if jsonOutput {
		data, err := json.MarshalIndent(sessions, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	if len(sessions) == 0 {
		fmt.Println("No sessions")
		return nil
	}
	printSessionTable(sessions)
	return nil
}

// ListFiltered returns sessions filtered by status: "all", "running", "completed", "killed".
func ListFiltered(target *Target, statusFilter string) ([]protocol.SessionInfo, error) {
	resp, err := requestResponse(target, &protocol.Request{Type: "ListSessions"})
	if err != nil {
		return nil, err
	}
	if resp.Type == "Error" {
		return nil, fmt.Errorf("%s", formatError(resp.Message))
	}
	if resp.Sessions == nil {
		return nil, fmt.Errorf("unexpected response type: %s", resp.Type)
	}
	sessions := *resp.Sessions
	if statusFilter == "" || statusFilter == "all" {
		return sessions, nil
	}
	var filtered []protocol.SessionInfo
	for _, s := range sessions {
		if statusFilter == "completed" {
			if strings.HasPrefix(s.Status, "completed") {
				filtered = append(filtered, s)
			}
		} else if strings.HasPrefix(s.Status, statusFilter) {
			filtered = append(filtered, s)
		}
	}
	return filtered, nil
}

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

// Run launches a new session on the node with the given command, working
// directory, and optional tags. If name is non-empty, the session is assigned
// that name for addressing.
func Run(target *Target, command []string, workingDir string, name string, env []string, stdinData []byte, tags ...string) error {
	resp, err := requestResponse(target, &protocol.Request{
		Type:       "Launch",
		Command:    command,
		WorkingDir: workingDir,
		Name:       name,
		Env:        env,
		StdinData:  stdinData,
		Tags:       tags,
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
// KillByTags
// ---------------------------------------------------------------------------

// KillByTags terminates all sessions matching the given tags.
func KillByTags(target *Target, tags []string) error {
	resp, err := requestResponse(target, &protocol.Request{
		Type: "KillByTags",
		Tags: tags,
	})
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
	fmt.Fprintf(os.Stderr, "Killed %d session(s) matching tags %v\n", count, tags)
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
func Logs(target *Target, id uint32, follow bool, tail *int, raw bool) error {
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
	if raw {
		f := false
		req.StripANSI = &f
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
// WatchMultiByTag — multiplexed watch
// ---------------------------------------------------------------------------

type watchLine struct {
	label string
	data  string
	done  bool
	err   error
}

var watchColors = []string{
	"\x1b[32m", "\x1b[33m", "\x1b[34m",
	"\x1b[35m", "\x1b[36m", "\x1b[31m",
}

const colorReset = "\x1b[0m"

// WatchMultiByTag watches all sessions matching a tag, merging their output
// with colored prefixes. It writes to w (os.Stdout for CLI, or a buffer for
// tests). If timeout is non-nil, it stops after that many seconds.
func WatchMultiByTag(target *Target, tag string, w io.Writer, timeout *uint64) error {
	// 1. List sessions, filter by tag.
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

	var matched []protocol.SessionInfo
	for _, s := range *resp.Sessions {
		for _, t := range s.Tags {
			if t == tag {
				matched = append(matched, s)
				break
			}
		}
	}

	if len(matched) == 0 {
		return fmt.Errorf("no sessions found with tag %q", tag)
	}

	// 2. For each matched session, spawn a goroutine to watch it.
	merged := make(chan watchLine, len(matched)*64)
	var wg sync.WaitGroup

	for idx, s := range matched {
		wg.Add(1)
		label := s.Name
		if label == "" {
			label = fmt.Sprintf("%d", s.ID)
		}
		color := watchColors[idx%len(watchColors)]
		sessionID := s.ID

		go func() {
			defer wg.Done()
			watchSingleToChannel(target, sessionID, label, color, merged)
		}()
	}

	// Close merged channel when all watchers are done.
	go func() {
		wg.Wait()
		close(merged)
	}()

	// 3. Set up timeout.
	var timeoutDuration time.Duration
	if timeout != nil {
		timeoutDuration = time.Duration(*timeout) * time.Second
	} else {
		timeoutDuration = time.Duration(math.MaxInt64)
	}
	timer := time.NewTimer(timeoutDuration)
	defer timer.Stop()

	// 4. Drain merged channel, write prefixed output to w.
	for {
		select {
		case line, ok := <-merged:
			if !ok {
				return nil // all watchers done
			}
			if line.err != nil {
				fmt.Fprintf(w, "%s[%s]%s error: %v\n", line.label, line.label, colorReset, line.err)
				continue
			}
			if line.data != "" {
				fmt.Fprintf(w, "%s[%s]%s %s", line.label, line.label, colorReset, line.data)
			}
		case <-timer.C:
			fmt.Fprintf(os.Stderr, "\n[cw] watch timeout reached\n")
			return nil
		}
	}
}

// watchSingleToChannel connects to a single session's WatchSession stream
// and sends output lines to the merged channel.
func watchSingleToChannel(target *Target, sessionID uint32, label, color string, merged chan<- watchLine) {
	reader, writer, err := target.Connect()
	if err != nil {
		merged <- watchLine{label: color, err: err}
		return
	}
	defer reader.Close()
	defer writer.Close()

	includeHistory := true
	req := &protocol.Request{
		Type:           "WatchSession",
		ID:             &sessionID,
		IncludeHistory: &includeHistory,
	}
	if err := writer.SendRequest(req); err != nil {
		merged <- watchLine{label: color, err: err}
		return
	}

	frameCh := make(chan frameEvent, 1)
	go readFrames(reader, frameCh)

	for fe := range frameCh {
		if fe.err != nil {
			return
		}
		if fe.frame == nil {
			return
		}
		if fe.frame.Type != protocol.FrameControl {
			continue
		}
		var resp protocol.Response
		if json.Unmarshal(fe.frame.Payload, &resp) != nil {
			continue
		}
		if resp.Type == "WatchUpdate" {
			if resp.Output != nil && *resp.Output != "" {
				merged <- watchLine{label: color, data: *resp.Output}
			}
			if resp.Done != nil && *resp.Done {
				return
			}
		}
		if resp.Type == "Error" {
			merged <- watchLine{label: color, err: fmt.Errorf("%s", resp.Message)}
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
	fmt.Printf("%-4s %-14s %-32s %-10s %-8s\n", "ID", "NAME", "COMMAND", "STATUS", "AGE")

	for _, s := range sessions {
		name := s.Name
		if name == "" {
			name = "-"
		}
		if len(name) > 14 {
			name = name[:11] + "..."
		}
		prompt := s.Prompt
		if len(prompt) > 32 {
			prompt = prompt[:29] + "..."
		}
		age := formatRelativeTime(s.CreatedAt)
		fmt.Printf("%-4d %-14s %-32s %-10s %-8s\n", s.ID, name, prompt, s.Status, age)
	}
}

// ---------------------------------------------------------------------------
// Nodes (relay discovery)
// ---------------------------------------------------------------------------

// Nodes fetches the list of registered nodes from a relay URL and prints them.
func Nodes(relayURL string) error {
	resp, err := fetchJSON(relayURL + "/api/v1/nodes")
	if err != nil {
		return err
	}

	var nodes []struct {
		Name      string `json:"name"`
		TunnelURL string `json:"tunnel_url"`
		Connected bool   `json:"connected"`
	}
	if err := json.Unmarshal(resp, &nodes); err != nil {
		return fmt.Errorf("parsing nodes: %w", err)
	}

	if len(nodes) == 0 {
		fmt.Println("No registered nodes")
		return nil
	}

	fmt.Printf("%-20s %-40s %-10s\n", "NAME", "TUNNEL URL", "STATUS")
	for _, n := range nodes {
		status := "offline"
		if n.Connected {
			status = "online"
		}
		fmt.Printf("%-20s %-40s %-10s\n", n.Name, n.TunnelURL, status)
	}
	return nil
}

func fetchJSON(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	return io.ReadAll(resp.Body)
}

// ---------------------------------------------------------------------------
// SubscribeEvents
// ---------------------------------------------------------------------------

// SubscribeEvents subscribes to session events and prints them as they arrive.
func SubscribeEvents(target *Target, sessionID *uint32, tags []string, eventTypes []string) error {
	reader, writer, err := target.Connect()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()

	req := &protocol.Request{
		Type:       "Subscribe",
		ID:         sessionID,
		Tags:       tags,
		EventTypes: eventTypes,
	}
	if err := writer.SendRequest(req); err != nil {
		return err
	}

	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			return err
		}
		if frame == nil {
			return nil
		}
		if frame.Type != protocol.FrameControl {
			continue
		}

		var resp protocol.Response
		if err := json.Unmarshal(frame.Payload, &resp); err != nil {
			continue
		}

		switch resp.Type {
		case "SubscribeAck":
			fmt.Fprintf(os.Stderr, "[cw] subscribed (id=%d)\n", *resp.SubscriptionID)
		case "Event":
			if resp.Event != nil && resp.SessionID != nil {
				data, _ := json.Marshal(resp.Event)
				fmt.Printf("[session %d] %s\n", *resp.SessionID, string(data))
			}
		case "Error":
			return fmt.Errorf("%s", resp.Message)
		case "Unsubscribed":
			return nil
		}
	}
}

// ---------------------------------------------------------------------------
// WaitForSession
// ---------------------------------------------------------------------------

// WaitForSession blocks until the target session(s) complete.
func WaitForSession(target *Target, sessionID *uint32, tags []string, condition string, timeout *uint64) error {
	reader, writer, err := target.Connect()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()

	req := &protocol.Request{
		Type:           "Wait",
		ID:             sessionID,
		Tags:           tags,
		Condition:      condition,
		TimeoutSeconds: timeout,
	}
	if err := writer.SendRequest(req); err != nil {
		return err
	}

	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			return err
		}
		if frame == nil {
			return nil
		}
		if frame.Type != protocol.FrameControl {
			continue
		}

		var resp protocol.Response
		if err := json.Unmarshal(frame.Payload, &resp); err != nil {
			continue
		}

		switch resp.Type {
		case "WaitResult":
			if resp.Sessions != nil {
				for _, s := range *resp.Sessions {
					exitStr := "n/a"
					if s.ExitCode != nil {
						exitStr = fmt.Sprintf("%d", *s.ExitCode)
					}
					fmt.Printf("Session %d: %s (exit_code=%s)\n", s.ID, s.Status, exitStr)
				}
			}
			return nil
		case "Error":
			return fmt.Errorf("%s", resp.Message)
		}
	}
}

// ---------------------------------------------------------------------------
// KV commands
// ---------------------------------------------------------------------------

// KVSet sets a key-value pair via the node (which proxies to the relay).
func KVSet(target *Target, namespace, key, value, ttl string) error {
	resp, err := requestResponse(target, &protocol.Request{
		Type:      "KVSet",
		Namespace: namespace,
		Key:       key,
		Value:     []byte(value),
		TTL:       ttl,
	})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", resp.Message)
	}
	fmt.Fprintf(os.Stderr, "Set %s/%s\n", namespace, key)
	return nil
}

// KVGet retrieves a value by key via the node.
func KVGet(target *Target, namespace, key string) error {
	resp, err := requestResponse(target, &protocol.Request{
		Type:      "KVGet",
		Namespace: namespace,
		Key:       key,
	})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", resp.Message)
	}
	if resp.Value == nil {
		fmt.Println("(not found)")
		return nil
	}
	fmt.Println(string(resp.Value))
	return nil
}

// KVList lists keys by prefix via the node.
func KVList(target *Target, namespace, prefix string) error {
	resp, err := requestResponse(target, &protocol.Request{
		Type:      "KVList",
		Namespace: namespace,
		Key:       prefix,
	})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", resp.Message)
	}
	if resp.Entries == nil || len(*resp.Entries) == 0 {
		fmt.Println("No keys found")
		return nil
	}
	for _, e := range *resp.Entries {
		fmt.Printf("%-30s %s\n", e.Key, string(e.Value))
	}
	return nil
}

// KVDelete deletes a key via the node.
func KVDelete(target *Target, namespace, key string) error {
	resp, err := requestResponse(target, &protocol.Request{
		Type:      "KVDelete",
		Namespace: namespace,
		Key:       key,
	})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", resp.Message)
	}
	fmt.Fprintf(os.Stderr, "Deleted %s/%s\n", namespace, key)
	return nil
}

// ---------------------------------------------------------------------------
// Msg — send a direct message
// ---------------------------------------------------------------------------

// Msg sends a direct message to a session.
func Msg(target *Target, fromID *uint32, toID uint32, body string) error {
	resp, err := requestResponse(target, &protocol.Request{
		Type:  "MsgSend",
		ID:    fromID,
		ToID:  &toID,
		Body:  body,
	})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", formatError(resp.Message))
	}
	fmt.Fprintf(os.Stderr, "Message sent: %s\n", resp.MessageID)
	return nil
}

// ---------------------------------------------------------------------------
// Inbox — read messages for a session
// ---------------------------------------------------------------------------

// Inbox reads and displays messages for a session.
func Inbox(target *Target, sessionID uint32, tail int) error {
	t := uint(tail)
	resp, err := requestResponse(target, &protocol.Request{
		Type: "MsgRead",
		ID:   &sessionID,
		Tail: &t,
	})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", formatError(resp.Message))
	}
	if resp.Messages == nil {
		fmt.Println("No messages")
		return nil
	}

	messages := *resp.Messages
	if len(messages) == 0 {
		fmt.Println("No messages")
		return nil
	}

	for _, m := range messages {
		fromLabel := fmt.Sprintf("%d", m.From)
		if m.FromName != "" {
			fromLabel = m.FromName
		}
		toLabel := fmt.Sprintf("%d", m.To)
		if m.ToName != "" {
			toLabel = m.ToName
		}

		switch m.EventType {
		case "message.request":
			fmt.Printf("[%s] REQUEST %s → %s (req=%s): %s\n", m.Timestamp, fromLabel, toLabel, m.RequestID, m.Body)
		case "message.reply":
			fmt.Printf("[%s] REPLY %s (req=%s): %s\n", m.Timestamp, fromLabel, m.RequestID, m.Body)
		default:
			fmt.Printf("[%s] %s → %s: %s\n", m.Timestamp, fromLabel, toLabel, m.Body)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Request — send a request and wait for reply
// ---------------------------------------------------------------------------

// Request sends a request to a session and blocks until a reply arrives.
// When rawOutput is true, only the reply body is printed (no "[reply from X]" prefix).
func Request(target *Target, fromID *uint32, toID uint32, body string, timeout uint64, rawOutput bool) error {
	reader, writer, err := target.Connect()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()

	req := &protocol.Request{
		Type:           "MsgRequest",
		ID:             fromID,
		ToID:           &toID,
		Body:           body,
		TimeoutSeconds: &timeout,
	}
	if err := writer.SendRequest(req); err != nil {
		return fmt.Errorf("sending request: %w", err)
	}

	// Read response — blocks until reply or timeout.
	frame, err := reader.ReadFrame()
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if frame == nil {
		return fmt.Errorf("connection closed before response")
	}
	if frame.Type != protocol.FrameControl {
		return fmt.Errorf("expected control frame")
	}

	var resp protocol.Response
	if err := json.Unmarshal(frame.Payload, &resp); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}

	switch resp.Type {
	case "MsgRequestResult":
		if rawOutput {
			fmt.Println(resp.ReplyBody)
		} else {
			fromLabel := "unknown"
			if resp.FromName != "" {
				fromLabel = resp.FromName
			} else if resp.FromID != nil {
				fromLabel = fmt.Sprintf("%d", *resp.FromID)
			}
			fmt.Printf("[reply from %s] %s\n", fromLabel, resp.ReplyBody)
		}
	case "Error":
		return fmt.Errorf("%s", resp.Message)
	default:
		return fmt.Errorf("unexpected response: %s", resp.Type)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Reply — reply to a pending request
// ---------------------------------------------------------------------------

// Reply sends a reply to a pending request.
func Reply(target *Target, fromID *uint32, requestID string, body string) error {
	resp, err := requestResponse(target, &protocol.Request{
		Type:      "MsgReply",
		ID:        fromID,
		RequestID: requestID,
		Body:      body,
	})
	if err != nil {
		return err
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", formatError(resp.Message))
	}
	fmt.Fprintf(os.Stderr, "Reply sent for request %s\n", requestID)
	return nil
}

// ---------------------------------------------------------------------------
// Listen — stream message traffic
// ---------------------------------------------------------------------------

// Listen streams all message traffic on the node in real-time.
func Listen(target *Target, sessionID *uint32) error {
	reader, writer, err := target.Connect()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()

	req := &protocol.Request{
		Type: "MsgListen",
		ID:   sessionID,
	}
	if err := writer.SendRequest(req); err != nil {
		return err
	}

	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			return err
		}
		if frame == nil {
			return nil
		}
		if frame.Type != protocol.FrameControl {
			continue
		}

		var resp protocol.Response
		if err := json.Unmarshal(frame.Payload, &resp); err != nil {
			continue
		}

		switch resp.Type {
		case "MsgListenAck":
			fmt.Fprintf(os.Stderr, "[cw] listening for messages...\n")
		case "Event":
			if resp.Event != nil {
				printMessageEvent(resp.SessionID, resp.Event)
			}
		case "Error":
			return fmt.Errorf("%s", resp.Message)
		}
	}
}

// printMessageEvent formats a message event for the listen stream.
func printMessageEvent(sessionID *uint32, event *protocol.SessionEvent) {
	switch event.EventType {
	case "direct.message":
		var d struct {
			From     uint32 `json:"from"`
			FromName string `json:"from_name"`
			To       uint32 `json:"to"`
			ToName   string `json:"to_name"`
			Body     string `json:"body"`
		}
		if json.Unmarshal(event.Data, &d) != nil {
			return
		}
		fromLabel := fmt.Sprintf("%d", d.From)
		if d.FromName != "" {
			fromLabel = d.FromName
		}
		toLabel := fmt.Sprintf("%d", d.To)
		if d.ToName != "" {
			toLabel = d.ToName
		}
		fmt.Printf("[%s → %s] %s\n", fromLabel, toLabel, d.Body)

	case "message.request":
		var d struct {
			RequestID string `json:"request_id"`
			From      uint32 `json:"from"`
			FromName  string `json:"from_name"`
			To        uint32 `json:"to"`
			ToName    string `json:"to_name"`
			Body      string `json:"body"`
		}
		if json.Unmarshal(event.Data, &d) != nil {
			return
		}
		fromLabel := fmt.Sprintf("%d", d.From)
		if d.FromName != "" {
			fromLabel = d.FromName
		}
		toLabel := fmt.Sprintf("%d", d.To)
		if d.ToName != "" {
			toLabel = d.ToName
		}
		fmt.Printf("[%s → %s] REQUEST (%s): %s\n", fromLabel, toLabel, d.RequestID, d.Body)

	case "message.reply":
		var d struct {
			RequestID string `json:"request_id"`
			From      uint32 `json:"from"`
			FromName  string `json:"from_name"`
			Body      string `json:"body"`
		}
		if json.Unmarshal(event.Data, &d) != nil {
			return
		}
		fromLabel := fmt.Sprintf("%d", d.From)
		if d.FromName != "" {
			fromLabel = d.FromName
		}
		fmt.Printf("[%s] REPLY (%s): %s\n", fromLabel, d.RequestID, d.Body)
	}
}

// ---------------------------------------------------------------------------
// Invite — create an invite code via relay API
// ---------------------------------------------------------------------------

// Invite creates an invite code on the relay and optionally prints a QR code.
func Invite(dataDir string, uses int, ttl string, showQR bool) error {
	relayURL, authToken, err := loadRelayAuth(dataDir)
	if err != nil {
		return err
	}

	reqBody, _ := json.Marshal(map[string]interface{}{
		"uses": uses,
		"ttl":  ttl,
	})

	req, err := http.NewRequest(http.MethodPost, relayURL+"/api/v1/invites", strings.NewReader(string(reqBody)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+authToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contacting relay: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("failed to create invite: %s", string(body))
	}

	var invite struct {
		Token         string    `json:"token"`
		UsesRemaining int       `json:"uses_remaining"`
		ExpiresAt     time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&invite); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}

	joinURL := relayURL + "/join?invite=" + invite.Token

	fmt.Fprintf(os.Stderr, "Invite created!\n\n")
	fmt.Fprintf(os.Stderr, "  Token:   %s\n", invite.Token)
	fmt.Fprintf(os.Stderr, "  Uses:    %d\n", invite.UsesRemaining)
	fmt.Fprintf(os.Stderr, "  Expires: %s\n", invite.ExpiresAt.Format(time.RFC3339))
	fmt.Fprintf(os.Stderr, "  URL:     %s\n\n", joinURL)
	fmt.Fprintf(os.Stderr, "To setup another device:\n")
	fmt.Fprintf(os.Stderr, "  cw setup %s --invite %s\n", relayURL, invite.Token)

	if showQR {
		printQR(joinURL)
	}

	return nil
}

// printQR renders a QR code to the terminal using Unicode half-blocks.
func printQR(content string) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n(QR generation failed: %v)\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "\n%s\n", q.ToSmallString(false))
}

// ---------------------------------------------------------------------------
// Revoke — revoke a node's access via relay API
// ---------------------------------------------------------------------------

// Revoke removes a node from the relay and adds its key to the revoked list.
func Revoke(dataDir string, nodeName string) error {
	relayURL, authToken, err := loadRelayAuth(dataDir)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodDelete, relayURL+"/api/v1/nodes/"+nodeName, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+authToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contacting relay: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("failed to revoke node: %s", string(body))
	}

	fmt.Fprintf(os.Stderr, "Node %q revoked\n", nodeName)
	return nil
}

// loadRelayAuth loads the relay URL and auth token from config.
func loadRelayAuth(dataDir string) (relayURL, authToken string, err error) {
	cfg, err := loadConfigFromDir(dataDir)
	if err != nil {
		return "", "", err
	}
	if cfg.relayURL == "" {
		return "", "", fmt.Errorf("relay not configured (run 'cw setup <relay-url>')")
	}
	return cfg.relayURL, cfg.authToken, nil
}

type relayAuthConfig struct {
	relayURL  string
	authToken string
}

func loadConfigFromDir(dataDir string) (*relayAuthConfig, error) {
	// Read config.toml for relay_url and relay_session.
	configPath := dataDir + "/config.toml"
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	// Simple extraction — parse the TOML manually for the fields we need.
	var cfg struct {
		RelayURL     *string `toml:"relay_url"`
		RelaySession *string `toml:"relay_session"`
	}

	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	result := &relayAuthConfig{}
	if cfg.RelayURL != nil {
		result.relayURL = *cfg.RelayURL
	}
	if cfg.RelaySession != nil {
		result.authToken = *cfg.RelaySession
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Gateway — run an approval gateway for worker sessions
// ---------------------------------------------------------------------------

// Gateway launches a stub session and subscribes to message.request events,
// evaluating each request via execCmd and replying automatically.
func Gateway(target *Target, name, execCmd, notifyMethod string) error {
	// 1. Launch stub session
	resp, err := requestResponse(target, &protocol.Request{
		Type:    "Launch",
		Command: []string{"sleep", "infinity"},
		Tags:    []string{"_gateway"},
		Name:    name,
	})
	if err != nil {
		return fmt.Errorf("launching gateway session: %w", err)
	}
	if resp.Type == "Error" {
		return fmt.Errorf("%s", resp.Message)
	}
	stubID := *resp.ID
	fmt.Fprintf(os.Stderr, "[cw gateway] listening as %q (session %d)\n", name, stubID)

	// 2. Setup cleanup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	defer func() {
		signal.Stop(sigCh)
		_ = Kill(target, stubID)
		fmt.Fprintf(os.Stderr, "[cw gateway] stopped\n")
	}()

	// 3. Subscribe to message.request on stub session
	reader, writer, err := target.Connect()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()

	if err := writer.SendRequest(&protocol.Request{
		Type:       "Subscribe",
		ID:         &stubID,
		EventTypes: []string{"message.request"},
	}); err != nil {
		return fmt.Errorf("subscribing: %w", err)
	}

	// 4. Event loop
	frameCh := make(chan *protocol.Frame, 16)
	readErr := make(chan error, 1)
	go func() {
		for {
			frame, err := reader.ReadFrame()
			if err != nil {
				readErr <- err
				return
			}
			if frame == nil {
				readErr <- nil
				return
			}
			frameCh <- frame
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case frame := <-frameCh:
			if frame.Type != protocol.FrameControl {
				continue
			}
			var resp protocol.Response
			if err := json.Unmarshal(frame.Payload, &resp); err != nil {
				continue
			}
			if resp.Type != "Event" || resp.Event == nil {
				continue
			}
			if resp.Event.EventType != "message.request" {
				continue
			}
			var reqData struct {
				RequestID string `json:"request_id"`
				From      uint32 `json:"from"`
				FromName  string `json:"from_name"`
				Body      string `json:"body"`
			}
			if err := json.Unmarshal(resp.Event.Data, &reqData); err != nil {
				continue
			}
			go gatewayHandleRequest(target, execCmd, notifyMethod, reqData.RequestID, reqData.Body, reqData.FromName)
		}
	}
}

func gatewayHandleRequest(target *Target, execCmd, notifyMethod, requestID, body, fromName string) {
	reply := gatewayEvaluate(execCmd, body, fromName)
	upperReply := strings.ToUpper(reply)

	if strings.HasPrefix(upperReply, "ESCALATE") && notifyMethod != "" {
		gatewayNotify(notifyMethod, body, fromName)
	}

	if _, err := requestResponse(target, &protocol.Request{
		Type:      "MsgReply",
		RequestID: requestID,
		Body:      reply,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[cw gateway] reply error: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "[cw gateway] %s -> %s\n", fromName, reply)
	}
}

func gatewayEvaluate(execCmd, body, fromName string) string {
	if execCmd == "" {
		return "APPROVED"
	}
	cmd := exec.Command("sh", "-c", execCmd)
	cmd.Stdin = strings.NewReader(body)
	cmd.Env = append(os.Environ(),
		"CW_REQUEST_BODY="+body,
		"CW_REQUEST_FROM="+fromName,
	)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Sprintf("DENIED: exec error: %v", err)
	}
	reply := strings.TrimSpace(string(out))
	if reply == "" {
		return "APPROVED"
	}
	return reply
}

func gatewayNotify(method, body, fromName string) {
	switch {
	case method == "macos":
		msg := fmt.Sprintf("Approval needed from %s: %s", fromName, body)
		_ = exec.Command("osascript", "-e",
			fmt.Sprintf(`display notification %q with title "cw gateway"`, msg)).Run()
	case strings.HasPrefix(method, "ntfy:"):
		url := strings.TrimPrefix(method, "ntfy:")
		_ = exec.Command("curl", "-s", "-d", body, url).Run()
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
