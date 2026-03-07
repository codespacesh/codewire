package platform

import "fmt"

func (c *Client) CreateEnvironment(orgID string, req *CreateEnvironmentRequest) (*Environment, error) {
	var env Environment
	if err := c.do("POST", fmt.Sprintf("/api/v1/organizations/%s/environments", orgID), req, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

func (c *Client) ListEnvironments(orgID string, envType, state string) ([]Environment, error) {
	path := fmt.Sprintf("/api/v1/organizations/%s/environments", orgID)
	sep := "?"
	if envType != "" {
		path += sep + "type=" + envType
		sep = "&"
	}
	if state != "" {
		path += sep + "state=" + state
	}
	var envs []Environment
	if err := c.do("GET", path, nil, &envs); err != nil {
		return nil, err
	}
	return envs, nil
}

func (c *Client) GetEnvironment(orgID, envID string) (*Environment, error) {
	var env Environment
	if err := c.do("GET", fmt.Sprintf("/api/v1/organizations/%s/environments/%s", orgID, envID), nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

func (c *Client) DeleteEnvironment(orgID, envID string) error {
	return c.do("DELETE", fmt.Sprintf("/api/v1/organizations/%s/environments/%s", orgID, envID), nil, nil)
}

func (c *Client) StopEnvironment(orgID, envID string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.do("POST", fmt.Sprintf("/api/v1/organizations/%s/environments/%s/stop", orgID, envID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) StartEnvironment(orgID, envID string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.do("POST", fmt.Sprintf("/api/v1/organizations/%s/environments/%s/start", orgID, envID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListEnvTemplates(orgID string, envType string) ([]EnvironmentTemplate, error) {
	path := fmt.Sprintf("/api/v1/organizations/%s/templates", orgID)
	if envType != "" {
		path += "?type=" + envType
	}
	var templates []EnvironmentTemplate
	if err := c.do("GET", path, nil, &templates); err != nil {
		return nil, err
	}
	return templates, nil
}

func (c *Client) CreateEnvTemplate(orgID string, req *CreateTemplateRequest) (*EnvironmentTemplate, error) {
	var tmpl EnvironmentTemplate
	if err := c.do("POST", fmt.Sprintf("/api/v1/organizations/%s/templates", orgID), req, &tmpl); err != nil {
		return nil, err
	}
	return &tmpl, nil
}

func (c *Client) DeleteEnvTemplate(orgID, templateID string) error {
	return c.do("DELETE", fmt.Sprintf("/api/v1/organizations/%s/templates/%s", orgID, templateID), nil, nil)
}
