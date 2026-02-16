package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/codespacesh/codewire/internal/protocol"
)

// ---------------------------------------------------------------------------
// Broadcaster — replaces tokio::sync::broadcast
// ---------------------------------------------------------------------------

// Broadcaster fans out byte slices to multiple subscribers. Slow consumers
// are dropped (non-blocking send) to avoid back-pressure on the PTY reader.
type Broadcaster struct {
	mu        sync.RWMutex
	listeners map[uint64]chan []byte
	nextID    uint64
}

// NewBroadcaster creates a ready-to-use Broadcaster.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		listeners: make(map[uint64]chan []byte),
	}
}

// Subscribe registers a new listener. Returns (id, channel). bufSize controls
// the channel buffer depth.
func (b *Broadcaster) Subscribe(bufSize int) (uint64, <-chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan []byte, bufSize)
	b.listeners[id] = ch
	return id, ch
}

// Unsubscribe removes and closes a listener by ID.
func (b *Broadcaster) Unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.listeners[id]; ok {
		close(ch)
		delete(b.listeners, id)
	}
}

// Send broadcasts data to every listener. Non-blocking: if a listener's
// channel is full the message is silently dropped for that consumer.
func (b *Broadcaster) Send(data []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.listeners {
		select {
		case ch <- data:
		default: // drop for slow consumers
		}
	}
}

// ---------------------------------------------------------------------------
// StatusWatcher — replaces tokio::sync::watch
// ---------------------------------------------------------------------------

// StatusWatcher holds a SessionStatus and notifies waiters on change.
type StatusWatcher struct {
	mu     sync.Mutex
	status SessionStatus
	waitCh chan struct{} // closed on change, then replaced
}

// NewStatusWatcher creates a watcher with the given initial status.
func NewStatusWatcher(initial SessionStatus) *StatusWatcher {
	return &StatusWatcher{
		status: initial,
		waitCh: make(chan struct{}),
	}
}

// Set updates the status and wakes all current waiters.
func (w *StatusWatcher) Set(s SessionStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = s
	close(w.waitCh)
	w.waitCh = make(chan struct{})
}

// Get returns the current status.
func (w *StatusWatcher) Get() SessionStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

// Changed returns a channel that is closed when the status next changes.
// After the channel fires, call Changed again for subsequent notifications.
func (w *StatusWatcher) Changed() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.waitCh
}

// ---------------------------------------------------------------------------
// SessionStatus
// ---------------------------------------------------------------------------

// SessionStatus represents the lifecycle state of a session.
type SessionStatus struct {
	State    string // "running", "completed", "killed"
	ExitCode int    // only meaningful when State == "completed"
}

// String returns a human-readable representation matching the Rust Display impl.
func (s SessionStatus) String() string {
	switch s.State {
	case "completed":
		return fmt.Sprintf("completed (%d)", s.ExitCode)
	case "killed":
		return "killed"
	default:
		return "running"
	}
}

// StatusRunning returns the running status.
func StatusRunning() SessionStatus { return SessionStatus{State: "running"} }

// StatusCompleted returns a completed status with the given exit code.
func StatusCompleted(code int) SessionStatus {
	return SessionStatus{State: "completed", ExitCode: code}
}

// StatusKilled returns the killed status.
func StatusKilled() SessionStatus { return SessionStatus{State: "killed"} }

// ---------------------------------------------------------------------------
// SessionMeta — persisted to sessions.json
// ---------------------------------------------------------------------------

// SessionMeta holds the serialisable metadata for a session. It is written to
// dataDir/sessions.json so that session IDs survive restarts.
type SessionMeta struct {
	ID         uint32    `json:"id"`
	Prompt     string    `json:"prompt"`
	WorkingDir string    `json:"working_dir"`
	CreatedAt  time.Time `json:"created_at"`
	Status     string    `json:"status"`
	PID        *uint32   `json:"pid,omitempty"`
}

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

// Session represents a live PTY session with its communication channels.
type Session struct {
	Meta          SessionMeta
	master        *os.File // PTY master fd (from creack/pty)
	attachedCount atomic.Int32
	broadcaster   *Broadcaster
	inputCh       chan []byte // buffered channel for PTY input writes
	statusWatcher *StatusWatcher
	logPath       string
	mu            sync.Mutex // protects Meta.Status updates
}

// ---------------------------------------------------------------------------
// AttachChannels
// ---------------------------------------------------------------------------

// AttachChannels groups the channels returned by SessionManager.Attach.
type AttachChannels struct {
	OutputCh <-chan []byte
	OutputID uint64 // for Broadcaster.Unsubscribe
	InputCh  chan<- []byte
	Status   *StatusWatcher
}

// ---------------------------------------------------------------------------
// SessionManager
// ---------------------------------------------------------------------------

// SessionManager owns all live sessions and persists their metadata to disk.
type SessionManager struct {
	mu        sync.RWMutex
	sessions  map[uint32]*Session
	nextID    atomic.Uint32
	dataDir   string
	PersistCh chan struct{} // exported: the node package drains this to trigger writes
}

// NewSessionManager creates a SessionManager rooted at dataDir. It reads
// sessions.json (if present) to restore the next session ID counter. If the
// file is corrupt it is backed up and an empty session list is used.
func NewSessionManager(dataDir string) (*SessionManager, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}

	var startID uint32 = 1

	metaPath := filepath.Join(dataDir, "sessions.json")
	data, err := os.ReadFile(metaPath)
	if err == nil {
		var metas []SessionMeta
		if jsonErr := json.Unmarshal(data, &metas); jsonErr != nil {
			// Backup corrupt file
			ts := time.Now().UTC().Format("20060102_150405")
			backupPath := metaPath + ".corrupt." + ts
			if cpErr := copyFile(metaPath, backupPath); cpErr != nil {
				slog.Error("failed to backup corrupt sessions.json", "err", cpErr)
			} else {
				slog.Info("backed up corrupt sessions.json", "path", backupPath)
			}
			slog.Error("corrupt sessions.json — starting with empty session list", "err", jsonErr)
		} else {
			var maxID uint32
			for _, m := range metas {
				if m.ID > maxID {
					maxID = m.ID
				}
			}
			startID = maxID + 1
		}
	}
	// If the file does not exist we silently start from ID 1.

	sm := &SessionManager{
		sessions:  make(map[uint32]*Session),
		dataDir:   dataDir,
		PersistCh: make(chan struct{}, 1),
	}
	sm.nextID.Store(startID)
	return sm, nil
}

// triggerPersist sends a non-blocking signal on PersistCh.
func (m *SessionManager) triggerPersist() {
	select {
	case m.PersistCh <- struct{}{}:
	default:
	}
}

// Launch starts a new PTY session executing command in workingDir.
func (m *SessionManager) Launch(command []string, workingDir string) (uint32, error) {
	if len(command) == 0 {
		return 0, fmt.Errorf("command must not be empty")
	}

	// Validate command binary.
	cmdName := command[0]
	if filepath.IsAbs(cmdName) {
		if _, err := os.Stat(cmdName); err != nil {
			return 0, fmt.Errorf("command %q does not exist", cmdName)
		}
	} else {
		if _, err := exec.LookPath(cmdName); err != nil {
			return 0, fmt.Errorf("command %q not found in PATH", cmdName)
		}
	}

	// Validate working directory.
	info, err := os.Stat(workingDir)
	if err != nil {
		return 0, fmt.Errorf("working directory %q does not exist", workingDir)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("working directory %q is not a directory", workingDir)
	}

	// Allocate ID (starts at 1).
	id := m.nextID.Add(1) - 1

	// Ensure log directory.
	logDir := filepath.Join(m.dataDir, "sessions", fmt.Sprintf("%d", id))
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return 0, fmt.Errorf("creating log dir: %w", err)
	}
	logPath := filepath.Join(logDir, "output.log")

	// Build exec.Cmd.
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = workingDir

	// Start with a PTY.
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return 0, fmt.Errorf("opening PTY: %w", err)
	}

	// Process ID.
	var pid *uint32
	if cmd.Process != nil {
		p := uint32(cmd.Process.Pid)
		pid = &p
	}

	displayCommand := strings.Join(command, " ")

	broadcaster := NewBroadcaster()
	inputCh := make(chan []byte, 256)
	statusWatcher := NewStatusWatcher(StatusRunning())

	sess := &Session{
		Meta: SessionMeta{
			ID:         id,
			Prompt:     displayCommand,
			WorkingDir: workingDir,
			CreatedAt:  time.Now().UTC(),
			Status:     StatusRunning().String(),
			PID:        pid,
		},
		master:        ptmx,
		broadcaster:   broadcaster,
		inputCh:       inputCh,
		statusWatcher: statusWatcher,
		logPath:       logPath,
	}

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()

	// Open log file.
	logFile, logErr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if logErr != nil {
		slog.Error("failed to open session log file", "id", id, "path", logPath, "err", logErr)
	}

	// Goroutine 1: PTY reader → log file + broadcast.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := ptmx.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				if logFile != nil {
					if _, wErr := logFile.Write(data); wErr != nil {
						slog.Error("log write error", "id", id, "err", wErr)
					}
				}
				broadcaster.Send(data)
			}
			if readErr != nil {
				if readErr == io.EOF || isEIO(readErr) {
					break
				}
				slog.Error("PTY read error", "id", id, "err", readErr)
				break
			}
		}
		if logFile != nil {
			logFile.Close()
		}
		slog.Info("output reader exited", "id", id)
	}()

	// Goroutine 2: input channel → PTY writer.
	go func() {
		for data := range inputCh {
			if _, wErr := ptmx.Write(data); wErr != nil {
				slog.Error("PTY write error", "id", id, "err", wErr)
				break
			}
		}
		slog.Info("input writer exited", "id", id)
	}()

	// Goroutine 3: wait for process exit → update status.
	go func() {
		waitErr := cmd.Wait()
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				code := exitErr.ExitCode()
				slog.Info("session process exited", "id", id, "code", code)
				statusWatcher.Set(StatusCompleted(code))
			} else {
				slog.Error("waiting for child", "id", id, "err", waitErr)
				statusWatcher.Set(StatusCompleted(-1))
			}
		} else {
			slog.Info("session process exited", "id", id, "code", 0)
			statusWatcher.Set(StatusCompleted(0))
		}
	}()

	slog.Info("session launched", "id", id)
	m.triggerPersist()
	return id, nil
}

// List returns a SessionInfo slice for every known session, sorted by ID.
func (m *SessionManager) List() []protocol.SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]protocol.SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		status := s.statusWatcher.Get()
		attached := s.attachedCount.Load() > 0
		infos = append(infos, protocol.SessionInfo{
			ID:         s.Meta.ID,
			Prompt:     s.Meta.Prompt,
			WorkingDir: s.Meta.WorkingDir,
			CreatedAt:  s.Meta.CreatedAt.Format(time.RFC3339),
			Status:     status.String(),
			Attached:   attached,
			PID:        s.Meta.PID,
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos
}

// Attach returns the channels needed to interact with a running session.
func (m *SessionManager) Attach(id uint32) (*AttachChannels, error) {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %d not found", id)
	}

	if sess.statusWatcher.Get().State != "running" {
		return nil, fmt.Errorf("session %d is not running", id)
	}

	sess.attachedCount.Add(1)
	subID, ch := sess.broadcaster.Subscribe(4096)

	return &AttachChannels{
		OutputCh: ch,
		OutputID: subID,
		InputCh:  sess.inputCh,
		Status:   sess.statusWatcher,
	}, nil
}

// Detach decrements the attached client count for a session.
func (m *SessionManager) Detach(id uint32) error {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %d not found", id)
	}
	sess.attachedCount.Add(-1)
	return nil
}

// Resize changes the PTY window size for a session.
func (m *SessionManager) Resize(id uint32, cols, rows uint16) error {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %d not found", id)
	}
	return pty.Setsize(sess.master, &pty.Winsize{Rows: rows, Cols: cols})
}

// Kill sends SIGTERM to the session's process and marks it killed.
func (m *SessionManager) Kill(id uint32) error {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %d not found", id)
	}

	sess.statusWatcher.Set(StatusKilled())

	if sess.Meta.PID != nil {
		_ = syscall.Kill(int(*sess.Meta.PID), syscall.SIGTERM)
	}

	sess.mu.Lock()
	sess.Meta.Status = StatusKilled().String()
	sess.mu.Unlock()

	m.triggerPersist()
	return nil
}

// KillAll kills every running session and returns the count killed.
func (m *SessionManager) KillAll() int {
	m.mu.RLock()
	ids := make([]uint32, 0)
	for id, s := range m.sessions {
		if s.statusWatcher.Get().State == "running" {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range ids {
		_ = m.Kill(id)
	}
	return len(ids)
}

// LogPath returns the path to a session's output log file.
func (m *SessionManager) LogPath(id uint32) (string, error) {
	m.mu.RLock()
	_, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("session %d not found", id)
	}
	return filepath.Join(m.dataDir, "sessions", fmt.Sprintf("%d", id), "output.log"), nil
}

// SendInput writes data to a session's PTY. It is non-blocking: if the input
// channel is full the send fails with an error.
func (m *SessionManager) SendInput(id uint32, data []byte) (int, error) {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("session %d not found", id)
	}

	select {
	case sess.inputCh <- data:
		return len(data), nil
	default:
		return 0, fmt.Errorf("input channel full for session %d", id)
	}
}

// GetStatus returns detailed status information for a session, including log
// file size and the last few lines of output.
func (m *SessionManager) GetStatus(id uint32) (protocol.SessionInfo, uint64, error) {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return protocol.SessionInfo{}, 0, fmt.Errorf("session %d not found", id)
	}

	status := sess.statusWatcher.Get()
	attached := sess.attachedCount.Load() > 0

	// Log file size.
	var outputSize uint64
	if fi, err := os.Stat(sess.logPath); err == nil {
		outputSize = uint64(fi.Size())
	}

	// Last few lines of output.
	var snippet *string
	if content, err := os.ReadFile(sess.logPath); err == nil {
		lines := strings.Split(string(content), "\n")
		// Take up to the last 5 non-empty lines.
		start := len(lines) - 5
		if start < 0 {
			start = 0
		}
		tail := lines[start:]
		joined := strings.Join(tail, "\n")
		if joined != "" {
			snippet = &joined
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("failed to read log file for snippet", "id", id, "err", err)
	}

	info := protocol.SessionInfo{
		ID:                sess.Meta.ID,
		Prompt:            sess.Meta.Prompt,
		WorkingDir:        sess.Meta.WorkingDir,
		CreatedAt:         sess.Meta.CreatedAt.Format(time.RFC3339),
		Status:            status.String(),
		Attached:          attached,
		PID:               sess.Meta.PID,
		OutputSizeBytes:   &outputSize,
		LastOutputSnippet: snippet,
	}

	return info, outputSize, nil
}

// SubscribeOutput returns a broadcast subscription for a session's PTY output.
func (m *SessionManager) SubscribeOutput(id uint32) (uint64, <-chan []byte, error) {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return 0, nil, fmt.Errorf("session %d not found", id)
	}
	subID, ch := sess.broadcaster.Subscribe(4096)
	return subID, ch, nil
}

// UnsubscribeOutput removes a broadcast subscription for a session.
func (m *SessionManager) UnsubscribeOutput(id uint32, subID uint64) {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return
	}
	sess.broadcaster.Unsubscribe(subID)
}

// SubscribeStatus returns the StatusWatcher for a session.
func (m *SessionManager) SubscribeStatus(id uint32) (*StatusWatcher, error) {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %d not found", id)
	}
	return sess.statusWatcher, nil
}

// RefreshStatuses synchronises each session's Meta.Status with its
// StatusWatcher and triggers persistence if anything changed.
func (m *SessionManager) RefreshStatuses() {
	changed := false

	m.mu.RLock()
	for _, sess := range m.sessions {
		current := sess.statusWatcher.Get().String()
		sess.mu.Lock()
		if sess.Meta.Status != current {
			sess.Meta.Status = current
			changed = true
		}
		sess.mu.Unlock()
	}
	m.mu.RUnlock()

	if changed {
		m.triggerPersist()
	}
}

// PersistMeta writes all session metadata to dataDir/sessions.json.
func (m *SessionManager) PersistMeta() {
	m.mu.RLock()
	metas := make([]SessionMeta, 0, len(m.sessions))
	for _, sess := range m.sessions {
		sess.mu.Lock()
		metas = append(metas, sess.Meta)
		sess.mu.Unlock()
	}
	m.mu.RUnlock()

	path := filepath.Join(m.dataDir, "sessions.json")
	data, err := json.MarshalIndent(metas, "", "  ")
	if err != nil {
		slog.Error("failed to serialise session metadata", "err", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Error("failed to persist session metadata", "path", path, "err", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// isEIO returns true if err is an EIO (errno 5) wrapped in an *os.PathError.
func isEIO(err error) bool {
	var pe *os.PathError
	if errors.As(err, &pe) {
		if errno, ok := pe.Err.(syscall.Errno); ok {
			return errno == syscall.EIO
		}
	}
	return false
}

// copyFile copies src to dst using simple read + write.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
