package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codespacesh/codewire/internal/connection"
	"github.com/codespacesh/codewire/internal/protocol"
)

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 types
// ---------------------------------------------------------------------------

type jsonRpcRequest struct {
	Jsonrpc string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type jsonRpcResponse struct {
	Jsonrpc string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  interface{}      `json:"result,omitempty"`
	Error   *jsonRpcError    `json:"error,omitempty"`
}

type jsonRpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

// ---------------------------------------------------------------------------
// MCP Server
// ---------------------------------------------------------------------------

// RunMCPServer reads JSON-RPC requests from stdin, dispatches them, and writes
// responses to stdout. It communicates with the codewire node over a Unix
// socket at dataDir/codewire.sock.
func RunMCPServer(dataDir string) error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1 MB buffer

	version := "0.1.0"

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var req jsonRpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			fmt.Fprintf(os.Stderr, "[mcp] invalid JSON-RPC: %v\n", err)
			continue
		}

		var resp jsonRpcResponse
		resp.Jsonrpc = "2.0"
		resp.ID = req.ID

		switch req.Method {
		case "initialize":
			resp.Result = map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{},
				},
				"serverInfo": map[string]interface{}{
					"name":    "codewire",
					"version": version,
				},
			}

		case "tools/list":
			resp.Result = map[string]interface{}{
				"tools": getTools(),
			}

		case "tools/call":
			result, err := handleToolCall(dataDir, req.Params)
			if err != nil {
				resp.Error = &jsonRpcError{Code: -32603, Message: err.Error()}
			} else {
				resp.Result = map[string]interface{}{
					"content": []map[string]interface{}{
						{"type": "text", "text": result},
					},
				}
			}

		default:
			resp.Error = &jsonRpcError{
				Code:    -32601,
				Message: fmt.Sprintf("method not found: %s", req.Method),
			}
		}

		out, _ := json.Marshal(resp)
		fmt.Fprintf(os.Stdout, "%s\n", out)
	}
	return scanner.Err()
}

// ---------------------------------------------------------------------------
// Tool definitions
// ---------------------------------------------------------------------------

// getTools returns the 7 MCP tools matching the Rust implementation.
func getTools() []tool {
	return []tool{
		{
			Name:        "codewire_list_sessions",
			Description: "List all CodeWire sessions with their status",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"status_filter": map[string]interface{}{
						"type":        "string",
						"description": "Filter by status: 'all', 'running', or 'completed'",
						"enum":        []string{"all", "running", "completed"},
					},
				},
			},
		},
		{
			Name:        "codewire_read_session_output",
			Description: "Read output from a session (snapshot, not live)",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"session_id": map[string]interface{}{
						"type":        "integer",
						"description": "The session ID to read from",
					},
					"tail": map[string]interface{}{
						"type":        "integer",
						"description": "Number of lines to show from end (optional)",
					},
					"max_chars": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum characters to return (default: 50000)",
					},
				},
				"required": []string{"session_id"},
			},
		},
		{
			Name:        "codewire_send_input",
			Description: "Send input to a session without attaching",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"session_id": map[string]interface{}{
						"type":        "integer",
						"description": "The session ID to send input to",
					},
					"input": map[string]interface{}{
						"type":        "string",
						"description": "The input text to send",
					},
					"auto_newline": map[string]interface{}{
						"type":        "boolean",
						"description": "Automatically add newline (default: true)",
					},
				},
				"required": []string{"session_id", "input"},
			},
		},
		{
			Name:        "codewire_watch_session",
			Description: "Monitor a session in real-time (time-bounded)",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"session_id": map[string]interface{}{
						"type":        "integer",
						"description": "The session ID to watch",
					},
					"include_history": map[string]interface{}{
						"type":        "boolean",
						"description": "Include recent history (default: true)",
					},
					"history_lines": map[string]interface{}{
						"type":        "integer",
						"description": "Number of history lines to include (default: 50)",
					},
					"max_duration_seconds": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum watch duration in seconds (default: 30)",
					},
				},
				"required": []string{"session_id"},
			},
		},
		{
			Name:        "codewire_get_session_status",
			Description: "Get detailed status information for a session",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"session_id": map[string]interface{}{
						"type":        "integer",
						"description": "The session ID to query",
					},
				},
				"required": []string{"session_id"},
			},
		},
		{
			Name:        "codewire_launch_session",
			Description: "Launch a new CodeWire session",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "Command and arguments to run",
					},
					"working_dir": map[string]interface{}{
						"type":        "string",
						"description": "Working directory (defaults to current dir)",
					},
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "codewire_kill_session",
			Description: "Terminate a running session",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"session_id": map[string]interface{}{
						"type":        "integer",
						"description": "The session ID to kill",
					},
				},
				"required": []string{"session_id"},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Tool dispatch
// ---------------------------------------------------------------------------

// handleToolCall dispatches to the appropriate tool handler.
func handleToolCall(dataDir string, params json.RawMessage) (string, error) {
	var p struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("invalid params: %w", err)
	}

	args := p.Arguments

	switch p.Name {
	case "codewire_list_sessions":
		return toolListSessions(dataDir, args)
	case "codewire_read_session_output":
		return toolReadSessionOutput(dataDir, args)
	case "codewire_send_input":
		return toolSendInput(dataDir, args)
	case "codewire_watch_session":
		return toolWatchSession(dataDir, args)
	case "codewire_get_session_status":
		return toolGetSessionStatus(dataDir, args)
	case "codewire_launch_session":
		return toolLaunchSession(dataDir, args)
	case "codewire_kill_session":
		return toolKillSession(dataDir, args)
	default:
		return "", fmt.Errorf("unknown tool: %s", p.Name)
	}
}

// ---------------------------------------------------------------------------
// Tool handlers
// ---------------------------------------------------------------------------

func toolListSessions(dataDir string, args map[string]interface{}) (string, error) {
	resp, err := nodeRequest(dataDir, &protocol.Request{Type: "ListSessions"})
	if err != nil {
		return "", err
	}
	if resp.Type == "Error" {
		return fmt.Sprintf("Error: %s", resp.Message), nil
	}
	if resp.Sessions == nil {
		return "Unexpected response", nil
	}

	sessions := *resp.Sessions

	filter, _ := args["status_filter"].(string)
	if filter == "" {
		filter = "all"
	}

	var filtered []protocol.SessionInfo
	for _, s := range sessions {
		switch filter {
		case "running":
			if strings.Contains(s.Status, "running") {
				filtered = append(filtered, s)
			}
		case "completed":
			if strings.Contains(s.Status, "completed") {
				filtered = append(filtered, s)
			}
		default:
			filtered = append(filtered, s)
		}
	}

	out, err := json.MarshalIndent(filtered, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func toolReadSessionOutput(dataDir string, args map[string]interface{}) (string, error) {
	sessionID, err := argUint32(args, "session_id")
	if err != nil {
		return "", err
	}

	var tail *uint
	if v, ok := args["tail"].(float64); ok {
		t := uint(v)
		tail = &t
	}

	maxChars := uint64(50000)
	if v, ok := args["max_chars"].(float64); ok {
		maxChars = uint64(v)
	}

	f := false
	resp, err := nodeRequest(dataDir, &protocol.Request{
		Type:   "Logs",
		ID:     &sessionID,
		Follow: &f,
		Tail:   tail,
	})
	if err != nil {
		return "", err
	}

	if resp.Type == "Error" {
		return fmt.Sprintf("Error: %s", resp.Message), nil
	}
	if resp.Type != "LogData" {
		return "Unexpected response", nil
	}

	data := resp.Data
	if uint64(len(data)) > maxChars {
		data = data[:maxChars] + "... [truncated]"
	}
	return data, nil
}

func toolSendInput(dataDir string, args map[string]interface{}) (string, error) {
	sessionID, err := argUint32(args, "session_id")
	if err != nil {
		return "", err
	}

	input, ok := args["input"].(string)
	if !ok {
		return "", fmt.Errorf("missing input")
	}

	autoNewline := true
	if v, ok := args["auto_newline"].(bool); ok {
		autoNewline = v
	}

	data := []byte(input)
	if autoNewline && !endsWithNewline(data) {
		data = append(data, '\n')
	}

	resp, err := nodeRequest(dataDir, &protocol.Request{
		Type: "SendInput",
		ID:   &sessionID,
		Data: data,
	})
	if err != nil {
		return "", err
	}

	if resp.Type == "Error" {
		return fmt.Sprintf("Error: %s", resp.Message), nil
	}
	if resp.Type == "InputSent" {
		bytes := uint(0)
		if resp.Bytes != nil {
			bytes = *resp.Bytes
		}
		return fmt.Sprintf("Sent %d bytes to session %d", bytes, sessionID), nil
	}
	return "Unexpected response", nil
}

func toolWatchSession(dataDir string, args map[string]interface{}) (string, error) {
	sessionID, err := argUint32(args, "session_id")
	if err != nil {
		return "", err
	}

	includeHistory := true
	if v, ok := args["include_history"].(bool); ok {
		includeHistory = v
	}

	var historyLines *uint
	if v, ok := args["history_lines"].(float64); ok {
		h := uint(v)
		historyLines = &h
	}

	maxDuration := uint64(30)
	if v, ok := args["max_duration_seconds"].(float64); ok {
		maxDuration = uint64(v)
	}

	return watchSessionTimed(dataDir, sessionID, includeHistory, historyLines, maxDuration)
}

func toolGetSessionStatus(dataDir string, args map[string]interface{}) (string, error) {
	sessionID, err := argUint32(args, "session_id")
	if err != nil {
		return "", err
	}

	resp, err := nodeRequest(dataDir, &protocol.Request{
		Type: "GetStatus",
		ID:   &sessionID,
	})
	if err != nil {
		return "", err
	}

	if resp.Type == "Error" {
		return fmt.Sprintf("Error: %s", resp.Message), nil
	}
	if resp.Type != "SessionStatus" || resp.Info == nil {
		return "Unexpected response", nil
	}

	// Marshal the session info and inject output_size.
	raw, err := json.Marshal(resp.Info)
	if err != nil {
		return "", err
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", err
	}
	if resp.OutputSize != nil {
		obj["output_size"] = *resp.OutputSize
	}

	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func toolLaunchSession(dataDir string, args map[string]interface{}) (string, error) {
	cmdRaw, ok := args["command"]
	if !ok {
		return "", fmt.Errorf("missing command")
	}
	cmdArr, ok := cmdRaw.([]interface{})
	if !ok {
		return "", fmt.Errorf("command must be an array")
	}
	var command []string
	for _, v := range cmdArr {
		s, ok := v.(string)
		if ok {
			command = append(command, s)
		}
	}

	workingDir, _ := args["working_dir"].(string)
	if workingDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			workingDir = "."
		} else {
			workingDir = wd
		}
	}

	resp, err := nodeRequest(dataDir, &protocol.Request{
		Type:       "Launch",
		Command:    command,
		WorkingDir: workingDir,
	})
	if err != nil {
		return "", err
	}

	if resp.Type == "Error" {
		return fmt.Sprintf("Error: %s", resp.Message), nil
	}
	if resp.Type == "Launched" && resp.ID != nil {
		return fmt.Sprintf("Launched session %d", *resp.ID), nil
	}
	return "Unexpected response", nil
}

func toolKillSession(dataDir string, args map[string]interface{}) (string, error) {
	sessionID, err := argUint32(args, "session_id")
	if err != nil {
		return "", err
	}

	resp, err := nodeRequest(dataDir, &protocol.Request{
		Type: "Kill",
		ID:   &sessionID,
	})
	if err != nil {
		return "", err
	}

	if resp.Type == "Error" {
		return fmt.Sprintf("Error: %s", resp.Message), nil
	}
	if resp.Type == "Killed" && resp.ID != nil {
		return fmt.Sprintf("Killed session %d", *resp.ID), nil
	}
	return "Unexpected response", nil
}

// ---------------------------------------------------------------------------
// Node communication
// ---------------------------------------------------------------------------

// nodeRequest connects to the Unix socket and sends a single request,
// returning the response.
func nodeRequest(dataDir string, req *protocol.Request) (*protocol.Response, error) {
	sockPath := filepath.Join(dataDir, "codewire.sock")
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("connecting to node: %w", err)
	}
	defer conn.Close()

	reader := connection.NewUnixReader(conn)
	writer := connection.NewUnixWriter(conn)

	if err := writer.SendRequest(req); err != nil {
		return nil, err
	}

	f, err := reader.ReadFrame()
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, fmt.Errorf("unexpected EOF")
	}
	if f.Type != protocol.FrameControl {
		return nil, fmt.Errorf("unexpected data frame")
	}

	var resp protocol.Response
	if err := json.Unmarshal(f.Payload, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// watchSessionTimed connects and watches a session with a maximum duration,
// collecting all output.
func watchSessionTimed(dataDir string, sessionID uint32, includeHistory bool, historyLines *uint, maxDurationSecs uint64) (string, error) {
	sockPath := filepath.Join(dataDir, "codewire.sock")
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	reader := connection.NewUnixReader(conn)
	writer := connection.NewUnixWriter(conn)

	req := &protocol.Request{
		Type:           "WatchSession",
		ID:             &sessionID,
		IncludeHistory: &includeHistory,
		HistoryLines:   historyLines,
	}
	if err := writer.SendRequest(req); err != nil {
		return "", err
	}

	var output string
	deadline := time.After(time.Duration(maxDurationSecs) * time.Second)

	type frameResult struct {
		frame *protocol.Frame
		err   error
	}
	frameCh := make(chan frameResult, 1)
	go func() {
		for {
			f, err := reader.ReadFrame()
			frameCh <- frameResult{f, err}
			if err != nil || f == nil {
				return
			}
		}
	}()

	for {
		select {
		case fr := <-frameCh:
			if fr.err != nil {
				return output, fr.err
			}
			if fr.frame == nil {
				return output, nil
			}
			if fr.frame.Type == protocol.FrameControl {
				var resp protocol.Response
				if err := json.Unmarshal(fr.frame.Payload, &resp); err != nil {
					continue
				}
				switch resp.Type {
				case "WatchUpdate":
					if resp.Output != nil {
						output += *resp.Output
					}
					if resp.Done != nil && *resp.Done {
						output += fmt.Sprintf("\n[Session %s]\n", resp.Status)
						return output, nil
					}
				case "Error":
					return "", fmt.Errorf("watch error: %s", resp.Message)
				}
			}

		case <-deadline:
			output += "\n[Watch timeout]\n"
			if len(output) > 100000 {
				output = output[:100000] + "\n... [output truncated to 100KB]"
			}
			return output, nil
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// argUint32 extracts a uint32 argument from the JSON-decoded arguments map.
// JSON numbers arrive as float64.
func argUint32(args map[string]interface{}, key string) (uint32, error) {
	v, ok := args[key].(float64)
	if !ok {
		return 0, fmt.Errorf("missing %s", key)
	}
	return uint32(v), nil
}

// endsWithNewline returns true if data ends with a newline byte.
func endsWithNewline(data []byte) bool {
	return len(data) > 0 && data[len(data)-1] == '\n'
}
