package node

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/codespacesh/codewire/internal/connection"
	"github.com/codespacesh/codewire/internal/protocol"
	"github.com/codespacesh/codewire/internal/session"
)

// handleClient reads the first control frame from a client, dispatches the
// request by type, and returns. Each Unix/WebSocket connection is handled
// by exactly one goroutine calling this function.
func handleClient(reader connection.FrameReader, writer connection.FrameWriter, manager *session.SessionManager) {
	defer reader.Close()
	defer writer.Close()

	f, err := reader.ReadFrame()
	if err != nil {
		slog.Error("failed to read initial frame", "err", err)
		return
	}
	if f == nil {
		return // clean disconnect
	}
	if f.Type != protocol.FrameControl {
		slog.Error("expected control frame, got data frame")
		return
	}

	var req protocol.Request
	if err := json.Unmarshal(f.Payload, &req); err != nil {
		slog.Error("failed to parse request", "err", err)
		return
	}

	switch req.Type {
	case "ListSessions":
		sessions := manager.List()
		_ = writer.SendResponse(&protocol.Response{
			Type:     "SessionList",
			Sessions: &sessions,
		})

	case "Launch":
		id, launchErr := manager.Launch(req.Command, req.WorkingDir)
		if launchErr != nil {
			msg := launchErr.Error()
			_ = writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: msg,
			})
			return
		}
		_ = writer.SendResponse(&protocol.Response{
			Type: "Launched",
			ID:   &id,
		})

	case "Attach":
		if req.ID == nil {
			_ = writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: "missing session id",
			})
			return
		}
		sessionID := *req.ID

		channels, attachErr := manager.Attach(sessionID)
		if attachErr != nil {
			_ = writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: attachErr.Error(),
			})
			return
		}
		defer manager.Detach(sessionID)

		// Unsubscribe the output broadcast when we are done.
		defer manager.UnsubscribeOutput(sessionID, channels.OutputID)

		// Send Attached confirmation.
		_ = writer.SendResponse(&protocol.Response{
			Type: "Attached",
			ID:   &sessionID,
		})

		// Replay history if requested.
		includeHistory := req.IncludeHistory == nil || *req.IncludeHistory
		if includeHistory {
			logPath, logErr := manager.LogPath(sessionID)
			if logErr == nil {
				if histErr := replayHistory(writer, logPath, req.HistoryLines); histErr != nil {
					slog.Warn("failed to replay history", "id", sessionID, "err", histErr)
				}
			}
		}

		// Bridge PTY and client until detach or disconnect.
		if bridgeErr := handleAttachSession(reader, writer, channels, sessionID, manager); bridgeErr != nil {
			slog.Debug("attach session ended", "id", sessionID, "err", bridgeErr)
		}

	case "Kill":
		if req.ID == nil {
			_ = writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: "missing session id",
			})
			return
		}
		if killErr := manager.Kill(*req.ID); killErr != nil {
			_ = writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: killErr.Error(),
			})
			return
		}
		_ = writer.SendResponse(&protocol.Response{
			Type: "Killed",
			ID:   req.ID,
		})

	case "KillAll":
		count := manager.KillAll()
		c := uint(count)
		_ = writer.SendResponse(&protocol.Response{
			Type:  "KilledAll",
			Count: &c,
		})

	case "Resize":
		_ = writer.SendResponse(&protocol.Response{
			Type: "Resized",
		})

	case "Detach":
		_ = writer.SendResponse(&protocol.Response{
			Type: "Detached",
		})

	case "Logs":
		if req.ID == nil {
			_ = writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: "missing session id",
			})
			return
		}
		logPath, logErr := manager.LogPath(*req.ID)
		if logErr != nil {
			_ = writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: logErr.Error(),
			})
			return
		}
		follow := req.Follow != nil && *req.Follow
		if logsErr := handleLogs(writer, logPath, follow, req.Tail); logsErr != nil {
			slog.Debug("logs handler ended", "id", *req.ID, "err", logsErr)
		}

	case "SendInput":
		if req.ID == nil {
			_ = writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: "missing session id",
			})
			return
		}
		n, inputErr := manager.SendInput(*req.ID, req.Data)
		if inputErr != nil {
			_ = writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: inputErr.Error(),
			})
			return
		}
		bytes := uint(n)
		_ = writer.SendResponse(&protocol.Response{
			Type:  "InputSent",
			Bytes: &bytes,
		})

	case "GetStatus":
		if req.ID == nil {
			_ = writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: "missing session id",
			})
			return
		}
		info, outputSize, statusErr := manager.GetStatus(*req.ID)
		if statusErr != nil {
			_ = writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: statusErr.Error(),
			})
			return
		}
		_ = writer.SendResponse(&protocol.Response{
			Type:       "SessionStatus",
			Info:       &info,
			OutputSize: &outputSize,
		})

	case "WatchSession":
		if req.ID == nil {
			_ = writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: "missing session id",
			})
			return
		}
		includeHistory := req.IncludeHistory == nil || *req.IncludeHistory
		if watchErr := handleWatchSession(reader, writer, manager, *req.ID, includeHistory, req.HistoryLines); watchErr != nil {
			slog.Debug("watch session ended", "id", *req.ID, "err", watchErr)
		}

	default:
		_ = writer.SendResponse(&protocol.Response{
			Type:    "Error",
			Message: fmt.Sprintf("unknown request type: %s", req.Type),
		})
	}
}

// frameOrError bundles a frame read result for channel-based communication.
type frameOrError struct {
	frame *protocol.Frame
	err   error
}

// handleAttachSession bridges PTY output and client input until the session
// ends, the client disconnects, or the client sends a Detach command.
func handleAttachSession(
	reader connection.FrameReader,
	writer connection.FrameWriter,
	channels *session.AttachChannels,
	sessionID uint32,
	manager *session.SessionManager,
) error {
	// Spawn a goroutine to read frames from the client, since ReadFrame blocks.
	frameCh := make(chan frameOrError, 1)
	go func() {
		for {
			f, err := reader.ReadFrame()
			frameCh <- frameOrError{frame: f, err: err}
			if err != nil || f == nil {
				return
			}
		}
	}()

	for {
		select {
		case data := <-channels.OutputCh:
			// PTY output to client.
			if err := writer.SendData(data); err != nil {
				return fmt.Errorf("sending output data: %w", err)
			}

		case fe := <-frameCh:
			if fe.err != nil {
				return fmt.Errorf("reading client frame: %w", fe.err)
			}
			if fe.frame == nil {
				// Client disconnected.
				return nil
			}

			if fe.frame.Type == protocol.FrameData {
				// Client sending PTY input.
				select {
				case channels.InputCh <- fe.frame.Payload:
				default:
					slog.Warn("input channel full, dropping data", "id", sessionID)
				}
				continue
			}

			// Control frame — parse the request.
			var req protocol.Request
			if err := json.Unmarshal(fe.frame.Payload, &req); err != nil {
				slog.Error("failed to parse attach control frame", "err", err)
				continue
			}

			switch req.Type {
			case "Detach":
				_ = writer.SendResponse(&protocol.Response{
					Type: "Detached",
					ID:   &sessionID,
				})
				return nil

			case "Resize":
				if req.Cols != nil && req.Rows != nil {
					if err := manager.Resize(sessionID, *req.Cols, *req.Rows); err != nil {
						slog.Error("resize failed", "id", sessionID, "err", err)
					}
				}

			default:
				slog.Warn("unexpected control frame during attach", "type", req.Type)
			}

		case <-channels.Status.Changed():
			status := channels.Status.Get()
			if status.State != "running" {
				msg := fmt.Sprintf("session %s", status.String())
				_ = writer.SendResponse(&protocol.Response{
					Type:    "Error",
					Message: msg,
				})
				return nil
			}
		}
	}
}

// replayHistory reads the session log file and sends its contents as a data
// frame. If historyLines is non-nil, only the last N lines are sent.
func replayHistory(writer connection.FrameWriter, logPath string, historyLines *uint) error {
	content, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no history yet
		}
		return fmt.Errorf("reading log file: %w", err)
	}

	if len(content) == 0 {
		return nil
	}

	if historyLines != nil {
		lines := strings.Split(string(content), "\n")
		n := int(*historyLines)
		if n < len(lines) {
			lines = lines[len(lines)-n:]
		}
		content = []byte(strings.Join(lines, "\n"))
	}

	if len(content) > 0 {
		return writer.SendData(content)
	}
	return nil
}

// handleWatchSession subscribes to a session's output and status, streaming
// updates to the client until the session ends or the client disconnects.
func handleWatchSession(
	reader connection.FrameReader,
	writer connection.FrameWriter,
	manager *session.SessionManager,
	id uint32,
	includeHistory bool,
	historyLines *uint,
) error {
	subID, outputCh, err := manager.SubscribeOutput(id)
	if err != nil {
		return writer.SendResponse(&protocol.Response{
			Type:    "Error",
			Message: err.Error(),
		})
	}
	defer manager.UnsubscribeOutput(id, subID)

	statusWatcher, err := manager.SubscribeStatus(id)
	if err != nil {
		return writer.SendResponse(&protocol.Response{
			Type:    "Error",
			Message: err.Error(),
		})
	}

	// Send history if requested.
	if includeHistory {
		logPath, logErr := manager.LogPath(id)
		if logErr == nil {
			content, readErr := os.ReadFile(logPath)
			if readErr == nil && len(content) > 0 {
				data := content
				if historyLines != nil {
					lines := strings.Split(string(data), "\n")
					n := int(*historyLines)
					if n < len(lines) {
						lines = lines[len(lines)-n:]
					}
					data = []byte(strings.Join(lines, "\n"))
				}
				if len(data) > 0 {
					output := string(data)
					f := false
					_ = writer.SendResponse(&protocol.Response{
						Type:   "WatchUpdate",
						Status: "running",
						Output: &output,
						Done:   &f,
					})
				}
			}
		}
	}

	// Spawn a goroutine to detect client disconnect.
	disconnectCh := make(chan struct{}, 1)
	go func() {
		for {
			f, err := reader.ReadFrame()
			if err != nil || f == nil {
				select {
				case disconnectCh <- struct{}{}:
				default:
				}
				return
			}
			// Ignore any frames from the client during watch.
		}
	}()

	for {
		select {
		case data := <-outputCh:
			output := string(data)
			f := false
			if sendErr := writer.SendResponse(&protocol.Response{
				Type:   "WatchUpdate",
				Status: "running",
				Output: &output,
				Done:   &f,
			}); sendErr != nil {
				return sendErr
			}

		case <-statusWatcher.Changed():
			s := statusWatcher.Get()
			done := s.State != "running"
			_ = writer.SendResponse(&protocol.Response{
				Type:   "WatchUpdate",
				Status: s.String(),
				Output: nil,
				Done:   &done,
			})
			if done {
				return nil
			}

		case <-disconnectCh:
			return nil
		}
	}
}

// handleLogs reads a session's log file and sends it to the client. If follow
// is true, it polls for new data every 500ms until the connection is closed.
func handleLogs(writer connection.FrameWriter, logPath string, follow bool, tail *uint) error {
	content, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			content = nil
		} else {
			return writer.SendResponse(&protocol.Response{
				Type:    "Error",
				Message: fmt.Sprintf("reading log file: %v", err),
			})
		}
	}

	data := string(content)

	// Apply tail.
	if tail != nil && len(content) > 0 {
		lines := strings.Split(data, "\n")
		n := int(*tail)
		if n < len(lines) {
			lines = lines[len(lines)-n:]
		}
		data = strings.Join(lines, "\n")
	}

	done := !follow
	if sendErr := writer.SendResponse(&protocol.Response{
		Type: "LogData",
		Data: data,
		Done: &done,
	}); sendErr != nil {
		return sendErr
	}

	if !follow {
		return nil
	}

	// Follow mode: poll for new data.
	offset := int64(len(content))
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		fi, statErr := os.Stat(logPath)
		if statErr != nil {
			continue
		}
		newSize := fi.Size()
		if newSize <= offset {
			continue
		}

		f, openErr := os.Open(logPath)
		if openErr != nil {
			continue
		}

		buf := make([]byte, newSize-offset)
		if _, seekErr := f.Seek(offset, 0); seekErr != nil {
			f.Close()
			continue
		}
		n, readErr := f.Read(buf)
		f.Close()

		if readErr != nil && n == 0 {
			continue
		}

		offset += int64(n)
		chunk := string(buf[:n])
		notDone := false
		if sendErr := writer.SendResponse(&protocol.Response{
			Type: "LogData",
			Data: chunk,
			Done: &notDone,
		}); sendErr != nil {
			return sendErr
		}
	}

	return nil
}
