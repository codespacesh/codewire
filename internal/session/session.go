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
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/codespacesh/codewire/internal/protocol"
)

// namePattern validates session names: alphanumeric + hyphens, 1-32 chars.
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,31}$`)

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
	ID          uint32     `json:"id"`
	Name        string     `json:"name,omitempty"`
	Prompt      string     `json:"prompt"`
	WorkingDir  string     `json:"working_dir"`
	CreatedAt   time.Time  `json:"created_at"`
	Status      string     `json:"status"`
	PID         *uint32    `json:"pid,omitempty"`
	Tags        []string   `json:"tags,omitempty"`
	ExitCode    *int       `json:"exit_code,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
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

	// Enriched tracking (new).
	outputBytes  atomic.Uint64
	outputLines  atomic.Uint64
	lastOutputAt atomic.Int64 // unix nano
	eventLog     *EventLog
	messageLog   *EventLog // JSONL at sessions/{id}/messages.jsonl
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
	mu            sync.RWMutex
	sessions      map[uint32]*Session
	nameIndex     map[string]uint32 // name → session ID (guarded by mu)
	nextID        atomic.Uint32
	dataDir       string
	PersistCh     chan struct{} // exported: the node package drains this to trigger writes
	Subscriptions *SubscriptionManager

	pendingRequestsMu sync.Mutex
	pendingRequests   map[string]chan ReplyData // requestID → reply channel
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
		sessions:        make(map[uint32]*Session),
		nameIndex:       make(map[string]uint32),
		dataDir:         dataDir,
		PersistCh:       make(chan struct{}, 1),
		Subscriptions:   NewSubscriptionManager(),
		pendingRequests: make(map[string]chan ReplyData),
	}
	sm.nextID.Store(startID)
	return sm, nil
}

// SetName assigns a unique name to a session. Returns an error if the name is
// invalid or already taken by another session.
func (m *SessionManager) SetName(id uint32, name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid name %q: must be 1-32 alphanumeric characters or hyphens, starting with alphanumeric", name)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	sess, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %d not found", id)
	}

	if existing, taken := m.nameIndex[name]; taken && existing != id {
		return fmt.Errorf("name %q already in use by session %d", name, existing)
	}

	// Remove old name from index if renaming.
	sess.mu.Lock()
	oldName := sess.Meta.Name
	sess.Meta.Name = name
	sess.mu.Unlock()

	if oldName != "" && oldName != name {
		delete(m.nameIndex, oldName)
	}
	m.nameIndex[name] = id

	return nil
}

// ResolveByName looks up a session ID by name. Returns an error if no session
// has the given name.
func (m *SessionManager) ResolveByName(name string) (uint32, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	id, ok := m.nameIndex[name]
	if !ok {
		return 0, fmt.Errorf("no session named %q", name)
	}
	return id, nil
}

// GetName returns the name for a session, or empty string if unnamed.
func (m *SessionManager) GetName(id uint32) string {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return ""
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.Meta.Name
}

// SendMessage sends a direct message from one session to another, recording it
// in both sessions' message logs and publishing it via the SubscriptionManager.
func (m *SessionManager) SendMessage(fromID, toID uint32, body string) (string, error) {
	m.mu.RLock()
	fromSess, fromOK := m.sessions[fromID]
	toSess, toOK := m.sessions[toID]
	m.mu.RUnlock()

	if !fromOK {
		return "", fmt.Errorf("sender session %d not found", fromID)
	}
	if !toOK {
		return "", fmt.Errorf("recipient session %d not found", toID)
	}

	msgID := fmt.Sprintf("msg_%d_%d_%d", fromID, toID, time.Now().UnixNano())

	fromSess.mu.Lock()
	fromName := fromSess.Meta.Name
	fromSess.mu.Unlock()

	toSess.mu.Lock()
	toName := toSess.Meta.Name
	toSess.mu.Unlock()

	msgData := DirectMessageData{
		MessageID: msgID,
		From:      fromID,
		FromName:  fromName,
		To:        toID,
		ToName:    toName,
		Body:      body,
	}
	event := NewDirectMessageEvent(msgData)

	// Write to both sessions' message logs.
	if fromSess.messageLog != nil {
		fromSess.messageLog.Append(event)
	}
	if toSess.messageLog != nil {
		toSess.messageLog.Append(event)
	}

	// Publish to subscriptions (on the recipient's session ID).
	m.Subscriptions.Publish(toID, toSess.Meta.Tags, event)
	// Also publish on sender so listen can see sent messages.
	if fromID != toID {
		m.Subscriptions.Publish(fromID, fromSess.Meta.Tags, event)
	}

	return msgID, nil
}

// ReadMessages reads messages from a session's message log, returning the last
// `tail` events. If tail <= 0, all messages are returned.
func (m *SessionManager) ReadMessages(sessionID uint32, tail int) ([]Event, error) {
	m.mu.RLock()
	sess, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %d not found", sessionID)
	}
	if sess.messageLog == nil {
		return nil, nil
	}
	return sess.messageLog.ReadTail(tail)
}

// SendRequest sends a request from one session to another and returns a channel
// that will receive the reply. The caller should block on the channel with a timeout.
func (m *SessionManager) SendRequest(fromID, toID uint32, body string) (string, <-chan ReplyData, error) {
	m.mu.RLock()
	fromSess, fromOK := m.sessions[fromID]
	toSess, toOK := m.sessions[toID]
	m.mu.RUnlock()

	if !fromOK {
		return "", nil, fmt.Errorf("sender session %d not found", fromID)
	}
	if !toOK {
		return "", nil, fmt.Errorf("recipient session %d not found", toID)
	}

	requestID := fmt.Sprintf("req_%d_%d_%d", fromID, toID, time.Now().UnixNano())

	fromSess.mu.Lock()
	fromName := fromSess.Meta.Name
	fromSess.mu.Unlock()

	toSess.mu.Lock()
	toName := toSess.Meta.Name
	toSess.mu.Unlock()

	reqData := RequestData{
		RequestID: requestID,
		From:      fromID,
		FromName:  fromName,
		To:        toID,
		ToName:    toName,
		Body:      body,
	}
	event := NewRequestEvent(reqData)

	// Write to recipient's message log and publish.
	if toSess.messageLog != nil {
		toSess.messageLog.Append(event)
	}
	m.Subscriptions.Publish(toID, toSess.Meta.Tags, event)
	// Also publish on sender.
	if fromID != toID {
		if fromSess.messageLog != nil {
			fromSess.messageLog.Append(event)
		}
		m.Subscriptions.Publish(fromID, fromSess.Meta.Tags, event)
	}

	// Register reply channel.
	replyCh := make(chan ReplyData, 1)
	m.pendingRequestsMu.Lock()
	m.pendingRequests[requestID] = replyCh
	m.pendingRequestsMu.Unlock()

	return requestID, replyCh, nil
}

// SendReply sends a reply to a pending request. It looks up the reply channel,
// sends the reply, and records the reply event in both sessions' message logs.
func (m *SessionManager) SendReply(fromID uint32, requestID string, body string) error {
	m.pendingRequestsMu.Lock()
	replyCh, ok := m.pendingRequests[requestID]
	if ok {
		delete(m.pendingRequests, requestID)
	}
	m.pendingRequestsMu.Unlock()

	if !ok {
		return fmt.Errorf("no pending request with ID %q", requestID)
	}

	m.mu.RLock()
	fromSess, fromOK := m.sessions[fromID]
	m.mu.RUnlock()

	var fromName string
	if fromOK {
		fromSess.mu.Lock()
		fromName = fromSess.Meta.Name
		fromSess.mu.Unlock()
	}

	replyData := ReplyData{
		RequestID: requestID,
		From:      fromID,
		FromName:  fromName,
		Body:      body,
	}
	event := NewReplyEvent(replyData)

	// Write to sender's message log.
	if fromOK && fromSess.messageLog != nil {
		fromSess.messageLog.Append(event)
	}
	m.Subscriptions.Publish(fromID, nil, event)

	// Send to the reply channel (non-blocking in case caller timed out).
	select {
	case replyCh <- replyData:
	default:
	}

	return nil
}

// CleanupRequest removes a pending request entry (called on timeout).
func (m *SessionManager) CleanupRequest(requestID string) {
	m.pendingRequestsMu.Lock()
	delete(m.pendingRequests, requestID)
	m.pendingRequestsMu.Unlock()
}

// triggerPersist sends a non-blocking signal on PersistCh.
func (m *SessionManager) triggerPersist() {
	select {
	case m.PersistCh <- struct{}{}:
	default:
	}
}

// Launch starts a new PTY session executing command in workingDir.
// tags are optional labels for filtering/grouping.
func (m *SessionManager) Launch(command []string, workingDir string, tags ...string) (uint32, error) {
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
	cmd.Env = buildEnv(nil)

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

	// Open event log.
	eventsPath := filepath.Join(logDir, "events.jsonl")
	eventLog, evErr := NewEventLog(eventsPath)
	if evErr != nil {
		slog.Error("failed to open event log", "id", id, "err", evErr)
	}

	// Open message log.
	messagesPath := filepath.Join(logDir, "messages.jsonl")
	messageLog, msgErr := NewEventLog(messagesPath)
	if msgErr != nil {
		slog.Error("failed to open message log", "id", id, "err", msgErr)
	}

	if tags == nil {
		tags = []string{}
	}

	sess := &Session{
		Meta: SessionMeta{
			ID:         id,
			Prompt:     displayCommand,
			WorkingDir: workingDir,
			CreatedAt:  time.Now().UTC(),
			Status:     StatusRunning().String(),
			PID:        pid,
			Tags:       tags,
		},
		master:        ptmx,
		broadcaster:   broadcaster,
		inputCh:       inputCh,
		statusWatcher: statusWatcher,
		logPath:       logPath,
		eventLog:      eventLog,
		messageLog:    messageLog,
	}

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()

	// Emit session.created event.
	createdEvent := NewSessionCreatedEvent(command, workingDir, tags)
	if eventLog != nil {
		eventLog.Append(createdEvent)
	}
	m.Subscriptions.Publish(id, tags, createdEvent)

	// Open log file.
	logFile, logErr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if logErr != nil {
		slog.Error("failed to open session log file", "id", id, "path", logPath, "err", logErr)
	}

	// Goroutine 1: PTY reader → log file + broadcast + output tracking.
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

				// Track output stats.
				sess.outputBytes.Add(uint64(n))
				for _, b := range data {
					if b == '\n' {
						sess.outputLines.Add(1)
					}
				}
				sess.lastOutputAt.Store(time.Now().UTC().UnixNano())
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
		if eventLog != nil {
			eventLog.Close()
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

	// Goroutine 3: wait for process exit → update status + emit events.
	go func() {
		var exitCode int
		waitErr := cmd.Wait()
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}
		slog.Info("session process exited", "id", id, "code", exitCode)

		now := time.Now().UTC()
		durationMs := now.Sub(sess.Meta.CreatedAt).Milliseconds()

		sess.mu.Lock()
		sess.Meta.ExitCode = &exitCode
		sess.Meta.CompletedAt = &now
		sess.mu.Unlock()

		statusWatcher.Set(StatusCompleted(exitCode))

		// Emit session.status event.
		statusEvent := NewSessionStatusEvent("running", "completed", &exitCode, &durationMs)
		if sess.eventLog != nil {
			sess.eventLog.Append(statusEvent)
		}
		m.Subscriptions.Publish(id, tags, statusEvent)
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
		infos = append(infos, m.buildSessionInfo(s))
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

	info := m.buildSessionInfo(sess)

	// Add snippet for GetStatus specifically.
	if content, err := os.ReadFile(sess.logPath); err == nil {
		lines := strings.Split(string(content), "\n")
		start := len(lines) - 5
		if start < 0 {
			start = 0
		}
		tail := lines[start:]
		joined := strings.Join(tail, "\n")
		if joined != "" {
			info.LastOutputSnippet = &joined
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("failed to read log file for snippet", "id", id, "err", err)
	}

	var outputSize uint64
	if info.OutputBytes != nil {
		outputSize = *info.OutputBytes
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

// buildSessionInfo constructs a fully enriched SessionInfo from a live Session.
func (m *SessionManager) buildSessionInfo(s *Session) protocol.SessionInfo {
	status := s.statusWatcher.Get()
	attached := s.attachedCount.Load() > 0
	attachedCount := s.attachedCount.Load()

	outputBytes := s.outputBytes.Load()
	outputLines := s.outputLines.Load()

	info := protocol.SessionInfo{
		ID:            s.Meta.ID,
		Name:          s.Meta.Name,
		Prompt:        s.Meta.Prompt,
		WorkingDir:    s.Meta.WorkingDir,
		CreatedAt:     s.Meta.CreatedAt.Format(time.RFC3339),
		Status:        status.String(),
		Attached:      attached,
		PID:           s.Meta.PID,
		Tags:          s.Meta.Tags,
		OutputBytes:   &outputBytes,
		OutputLines:   &outputLines,
		AttachedCount: attachedCount,
	}

	// File-based output size.
	if fi, err := os.Stat(s.logPath); err == nil {
		sz := uint64(fi.Size())
		info.OutputSizeBytes = &sz
	}

	// Exit code and completion info.
	s.mu.Lock()
	if s.Meta.ExitCode != nil {
		info.ExitCode = s.Meta.ExitCode
	}
	if s.Meta.CompletedAt != nil {
		completedStr := s.Meta.CompletedAt.Format(time.RFC3339)
		info.CompletedAt = &completedStr
		durationMs := s.Meta.CompletedAt.Sub(s.Meta.CreatedAt).Milliseconds()
		info.DurationMs = &durationMs
	}
	s.mu.Unlock()

	// Last output timestamp.
	if lastNano := s.lastOutputAt.Load(); lastNano > 0 {
		lastStr := time.Unix(0, lastNano).UTC().Format(time.RFC3339)
		info.LastOutputAt = &lastStr
	}

	return info
}

// GetSessionTags returns the tags for a session (used by handler for event filtering).
func (m *SessionManager) GetSessionTags(id uint32) []string {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	return sess.Meta.Tags
}

// ListByTags returns sessions matching any of the given tags.
func (m *SessionManager) ListByTags(tags []string) []protocol.SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var infos []protocol.SessionInfo
	for _, s := range m.sessions {
		if matchesTags(s.Meta.Tags, tags) {
			infos = append(infos, m.buildSessionInfo(s))
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos
}

func matchesTags(sessionTags, filterTags []string) bool {
	for _, ft := range filterTags {
		for _, st := range sessionTags {
			if ft == st {
				return true
			}
		}
	}
	return false
}

// KillByTags kills all running sessions matching any of the given tags.
func (m *SessionManager) KillByTags(tags []string) int {
	m.mu.RLock()
	var ids []uint32
	for id, s := range m.sessions {
		if s.statusWatcher.Get().State == "running" && matchesTags(s.Meta.Tags, tags) {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range ids {
		m.Kill(id)
	}
	return len(ids)
}

// buildEnv constructs child env from os.Environ() with CLAUDECODE stripped
// and optional KEY=VALUE overrides applied.
func buildEnv(overrides []string) []string {
	base := os.Environ()
	filtered := make([]string, 0, len(base))
	for _, e := range base {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			filtered = append(filtered, e)
		}
	}
	if len(overrides) == 0 {
		return filtered
	}
	keyIdx := make(map[string]int, len(filtered))
	for i, e := range filtered {
		if eq := strings.IndexByte(e, '='); eq >= 0 {
			keyIdx[e[:eq]] = i
		}
	}
	result := make([]string, len(filtered))
	copy(result, filtered)
	for _, ov := range overrides {
		eq := strings.IndexByte(ov, '=')
		if eq < 0 {
			continue
		}
		key := ov[:eq]
		if idx, exists := keyIdx[key]; exists {
			result[idx] = ov
		} else {
			result = append(result, ov)
			keyIdx[key] = len(result) - 1
		}
	}
	return result
}

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
