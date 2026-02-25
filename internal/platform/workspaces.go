package platform

import (
	"fmt"
	"time"
)

type CreateWorkspaceRequest struct {
	Name         string               `json:"name"`
	TemplateID   string               `json:"template_id,omitempty"`
	TemplateName string               `json:"template_name,omitempty"`
	RichParams   []RichParameterValue `json:"rich_parameter_values,omitempty"`
}

type RichParameterValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type TemplateSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// CreateWorkspace creates a workspace on a Coder resource.
func (c *Client) CreateWorkspace(resourceID string, req *CreateWorkspaceRequest) (*WorkspaceSummary, error) {
	var ws WorkspaceSummary
	if err := c.do("POST", "/api/v1/resources/"+resourceID+"/workspaces", req, &ws); err != nil {
		return nil, err
	}
	return &ws, nil
}

// GetWorkspace returns details of a specific workspace.
func (c *Client) GetWorkspace(resourceID, workspaceID string) (*WorkspaceSummary, error) {
	var ws WorkspaceSummary
	if err := c.do("GET", "/api/v1/resources/"+resourceID+"/workspaces/"+workspaceID, nil, &ws); err != nil {
		return nil, err
	}
	return &ws, nil
}

// DeleteWorkspace deletes a workspace.
func (c *Client) DeleteWorkspace(resourceID, workspaceID string) error {
	return c.do("DELETE", "/api/v1/resources/"+resourceID+"/workspaces/"+workspaceID, nil, nil)
}

// StartWorkspace starts a stopped workspace.
func (c *Client) StartWorkspace(resourceID, workspaceID string) error {
	return c.do("POST", "/api/v1/resources/"+resourceID+"/workspaces/"+workspaceID+"/start", nil, nil)
}

// StopWorkspace stops a running workspace.
func (c *Client) StopWorkspace(resourceID, workspaceID string) error {
	return c.do("POST", "/api/v1/resources/"+resourceID+"/workspaces/"+workspaceID+"/stop", nil, nil)
}

// ListTemplates returns available workspace templates for a resource.
func (c *Client) ListTemplates(resourceID string) ([]TemplateSummary, error) {
	var templates []TemplateSummary
	if err := c.do("GET", "/api/v1/resources/"+resourceID+"/templates", nil, &templates); err != nil {
		return nil, err
	}
	return templates, nil
}

// WaitForWorkspace polls until a workspace reaches a terminal status.
func (c *Client) WaitForWorkspace(resourceID, workspaceID string, timeout time.Duration) (*WorkspaceSummary, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ws, err := c.GetWorkspace(resourceID, workspaceID)
		if err != nil {
			return nil, err
		}
		switch ws.Status {
		case "running", "stopped", "failed", "canceled", "deleted":
			return ws, nil
		}
		time.Sleep(3 * time.Second)
	}
	return nil, fmt.Errorf("timed out waiting for workspace")
}
