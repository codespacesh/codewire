package platform

import (
	"fmt"
	"io"
	"net/http"
)

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

func (c *Client) ExecInEnvironment(orgID, envID string, req *ExecRequest) (*ExecResult, error) {
	var result ExecResult
	if err := c.do("POST", fmt.Sprintf("/api/v1/organizations/%s/environments/%s/exec", orgID, envID), req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) ListFiles(orgID, envID, path string) ([]FileEntry, error) {
	p := fmt.Sprintf("/api/v1/organizations/%s/environments/%s/files", orgID, envID)
	if path != "" {
		p += "?path=" + path
	}
	var entries []FileEntry
	if err := c.do("GET", p, nil, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (c *Client) UploadFile(orgID, envID, path string, data io.Reader) error {
	url := fmt.Sprintf("%s/api/v1/organizations/%s/environments/%s/files/upload?path=%s",
		c.ServerURL, orgID, envID, path)
	req, err := http.NewRequest(http.MethodPost, url, data)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.SessionToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.SessionToken)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed (%d): %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *Client) LookupRepoConfig(orgID, repoURL string) (*RepoConfig, error) {
	var rc RepoConfig
	if err := c.do("GET", fmt.Sprintf("/api/v1/organizations/%s/repo-configs/lookup?repo_url=%s", orgID, repoURL), nil, &rc); err != nil {
		return nil, err
	}
	return &rc, nil
}

func (c *Client) SaveRepoConfig(orgID string, repoURL, templateID string, setupConfig map[string]any) error {
	body := map[string]any{
		"repo_url":     repoURL,
		"template_id":  templateID,
		"setup_config": setupConfig,
	}
	return c.do("PUT", fmt.Sprintf("/api/v1/organizations/%s/repo-configs", orgID), body, nil)
}

func (c *Client) DownloadFile(orgID, envID, path string) (io.ReadCloser, error) {
	url := fmt.Sprintf("%s/api/v1/organizations/%s/environments/%s/files/download?path=%s",
		c.ServerURL, orgID, envID, path)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.SessionToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.SessionToken)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("download failed (%d): %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}
