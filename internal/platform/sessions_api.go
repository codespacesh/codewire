package platform

import "fmt"

type CreateSessionRequest struct {
	Name    string   `json:"name,omitempty"`
	Command []string `json:"command"`
}

type SessionInfoResponse struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Command string `json:"command"`
	Status  string `json:"status"`
	Tags    string `json:"tags,omitempty"`
}

type SessionListAPIResponse struct {
	Sessions []SessionInfoResponse `json:"sessions"`
}

func (c *Client) CreateWorkspaceSession(resourceID, workspaceID string, req *CreateSessionRequest) (*SessionInfoResponse, error) {
	var resp SessionInfoResponse
	err := c.do("POST", fmt.Sprintf("/api/v1/resources/%s/workspaces/%s/sessions", resourceID, workspaceID), req, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListWorkspaceSessions(resourceID, workspaceID string) ([]SessionInfoResponse, error) {
	var resp SessionListAPIResponse
	err := c.do("GET", fmt.Sprintf("/api/v1/resources/%s/workspaces/%s/sessions", resourceID, workspaceID), nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

func (c *Client) KillWorkspaceSession(resourceID, workspaceID, sessionName string) error {
	return c.do("DELETE", fmt.Sprintf("/api/v1/resources/%s/workspaces/%s/sessions/%s", resourceID, workspaceID, sessionName), nil, nil)
}
