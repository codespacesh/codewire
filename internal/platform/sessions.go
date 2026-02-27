package platform

import "time"

type HeartbeatRequest struct {
	WorkspaceID string            `json:"workspace_id"`
	ResourceID  string            `json:"resource_id"`
	Sessions    []SessionSnapshot `json:"sessions"`
}

type SessionSnapshot struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Command   string `json:"command"`
	Status    string `json:"status"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	StartedAt string `json:"started_at"`
}

type SessionEntry struct {
	WorkspaceID string            `json:"workspace_id"`
	ResourceID  string            `json:"resource_id"`
	Sessions    []SessionSnapshot `json:"sessions"`
	ReportedAt  time.Time         `json:"reported_at"`
	Stale       bool              `json:"stale"`
}

// PostHeartbeat sends a workspace session heartbeat to the server.
func (c *Client) PostHeartbeat(req *HeartbeatRequest) error {
	return c.do("POST", "/api/v1/heartbeat", req, nil)
}

// ListSessions returns session data from heartbeat reports.
func (c *Client) ListSessions(resourceID, workspaceID string) ([]SessionEntry, error) {
	path := "/api/v1/sessions?"
	if resourceID != "" {
		path += "resource_id=" + resourceID + "&"
	}
	if workspaceID != "" {
		path += "workspace_id=" + workspaceID
	}
	var entries []SessionEntry
	if err := c.do("GET", path, nil, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}
