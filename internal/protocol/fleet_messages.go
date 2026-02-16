package protocol

// NodeInfo describes a fleet node's metadata, matching the Rust NodeInfo struct.
type NodeInfo struct {
	Name        string        `json:"name"`
	ExternalURL *string       `json:"external_url,omitempty"`
	Sessions    []SessionInfo `json:"sessions"`
	UptimeSecs  uint64        `json:"uptime_secs"`
}

// FleetRequest is the union of all fleet control messages sent over NATS.
// The Type field is the serde tag discriminator.
type FleetRequest struct {
	Type       string   `json:"type"`
	Command    []string `json:"command,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
	ID         *uint32  `json:"id,omitempty"`
	Data       []byte   `json:"data,omitempty"`
}

// FleetResponse is the union of all fleet responses from nodes over NATS.
//
// For the NodeInfo variant, Rust's serde with #[serde(tag = "type")] on a
// newtype variant flattens the inner struct fields into the top-level object:
//
//	{"type":"NodeInfo","name":"...","sessions":[...],"uptime_secs":123}
//
// We model this by embedding NodeInfo fields directly. When Type is "NodeInfo",
// the Name, ExternalURL, Sessions, and UptimeSecs fields carry the node info.
// For other variants, the Node field identifies the source node.
type FleetResponse struct {
	Type string `json:"type"`

	// Used by all variants except NodeInfo to identify the source node.
	Node string `json:"node,omitempty"`

	// NodeInfo variant fields (flattened from Rust's newtype variant).
	// These overlap with the NodeInfo struct fields.
	Name        string        `json:"name,omitempty"`
	ExternalURL *string       `json:"external_url,omitempty"`
	UptimeSecs  *uint64       `json:"uptime_secs,omitempty"`

	// Shared across multiple variants.
	Sessions   *[]SessionInfo `json:"sessions,omitempty"`
	ID         *uint32       `json:"id,omitempty"`
	Info       *SessionInfo  `json:"info,omitempty"`
	OutputSize *uint64       `json:"output_size,omitempty"`
	Bytes      *uint         `json:"bytes,omitempty"`
	Message    string        `json:"message,omitempty"`
}

// NewFleetResponseNodeInfo creates a FleetResponse for the NodeInfo variant.
func NewFleetResponseNodeInfo(info NodeInfo) FleetResponse {
	sessions := info.Sessions
	return FleetResponse{
		Type:        "NodeInfo",
		Name:        info.Name,
		ExternalURL: info.ExternalURL,
		Sessions:    &sessions,
		UptimeSecs:  &info.UptimeSecs,
	}
}
