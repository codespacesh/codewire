package protocol

import "encoding/json"

// SessionInfo describes a terminal session, matching the Rust SessionInfo struct.
type SessionInfo struct {
	ID                uint32  `json:"id"`
	Prompt            string  `json:"prompt"`
	WorkingDir        string  `json:"working_dir"`
	CreatedAt         string  `json:"created_at"`
	Status            string  `json:"status"`
	Attached          bool    `json:"attached"`
	PID               *uint32 `json:"pid,omitempty"`
	OutputSizeBytes   *uint64 `json:"output_size_bytes,omitempty"`
	LastOutputSnippet *string `json:"last_output_snippet,omitempty"`
}

// Request is the union of all client-to-server control messages.
// The Type field is the serde tag discriminator.
// Optional fields use omitempty so only relevant fields appear in JSON.
type Request struct {
	Type           string   `json:"type"`
	Command        []string `json:"command,omitempty"`
	WorkingDir     string   `json:"working_dir,omitempty"`
	ID             *uint32  `json:"id,omitempty"`
	IncludeHistory *bool    `json:"include_history,omitempty"`
	HistoryLines   *uint    `json:"history_lines,omitempty"`
	Cols           *uint16  `json:"cols,omitempty"`
	Rows           *uint16  `json:"rows,omitempty"`
	Follow         *bool    `json:"follow,omitempty"`
	Tail           *uint    `json:"tail,omitempty"`
	Data           []byte   `json:"data,omitempty"`
}

// UnmarshalJSON implements custom JSON unmarshalling for Request.
// When the type is "Attach" or "WatchSession" and include_history is absent,
// it defaults to true (matching Rust's #[serde(default = "default_true")]).
func (r *Request) UnmarshalJSON(b []byte) error {
	// Use an alias to avoid infinite recursion.
	type Alias Request
	aux := &Alias{}
	if err := json.Unmarshal(b, aux); err != nil {
		return err
	}
	*r = Request(*aux)

	// Check if include_history was explicitly present in the JSON.
	if r.Type == "Attach" && r.IncludeHistory == nil {
		t := true
		r.IncludeHistory = &t
	}

	return nil
}

// Response is the union of all server-to-client control messages.
// The Type field is the serde tag discriminator.
type Response struct {
	Type       string        `json:"type"`
	Sessions   *[]SessionInfo `json:"sessions,omitempty"`
	ID         *uint32       `json:"id,omitempty"`
	Count      *uint         `json:"count,omitempty"`
	Data       string        `json:"data,omitempty"`
	Done       *bool         `json:"done,omitempty"`
	Bytes      *uint         `json:"bytes,omitempty"`
	Info       *SessionInfo  `json:"info,omitempty"`
	OutputSize *uint64       `json:"output_size,omitempty"`
	Status     string        `json:"status,omitempty"`
	Output     *string       `json:"output,omitempty"`
	Message    string        `json:"message,omitempty"`
}
