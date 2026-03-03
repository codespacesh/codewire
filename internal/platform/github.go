package platform

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ── Types ─────────────────────────────────────────────────────────────

type GitHubAppConfig struct {
	ClientID string `json:"client_id"`
	AppSlug  string `json:"app_slug"`
}

type GitHubStatus struct {
	Connected      bool    `json:"connected"`
	Username       string  `json:"username,omitempty"`
	InstallationID *int64  `json:"installation_id,omitempty"`
	ConnectedAt    *string `json:"connected_at,omitempty"`
}

type GitHubDeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type GitHubTokenResponse struct {
	AccessToken           string `json:"access_token"`
	TokenType             string `json:"token_type"`
	Scope                 string `json:"scope"`
	RefreshToken          string `json:"refresh_token,omitempty"`
	ExpiresIn             int    `json:"expires_in,omitempty"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in,omitempty"`
	Error                 string `json:"error,omitempty"`
	ErrorDescription      string `json:"error_description,omitempty"`
}

type SaveGitHubTokenRequest struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token,omitempty"`
	TokenType             string `json:"token_type,omitempty"`
	ExpiresAt             string `json:"expires_at,omitempty"`
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at,omitempty"`
	InstallationID        *int64 `json:"installation_id,omitempty"`
	GitHubUsername        string `json:"github_username,omitempty"`
}

// ── Client methods (server API) ───────────────────────────────────────

// GetGitHubConfig fetches the GitHub App config from the server.
func (c *Client) GetGitHubConfig() (*GitHubAppConfig, error) {
	var cfg GitHubAppConfig
	if err := c.do("GET", "/api/v1/github/config", nil, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// GetGitHubStatus returns the user's GitHub connection status.
func (c *Client) GetGitHubStatus() (*GitHubStatus, error) {
	var status GitHubStatus
	if err := c.do("GET", "/api/v1/github/status", nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// SaveGitHubToken stores the GitHub token on the server.
func (c *Client) SaveGitHubToken(req *SaveGitHubTokenRequest) error {
	return c.do("POST", "/api/v1/github/token", req, nil)
}

// DisconnectGitHub removes the stored GitHub token.
func (c *Client) DisconnectGitHub() error {
	return c.do("DELETE", "/api/v1/github/token", nil, nil)
}

// ── Standalone functions (direct GitHub API, no server) ───────────────

// RequestDeviceCode starts the GitHub device flow.
func RequestDeviceCode(clientID string) (*GitHubDeviceCodeResponse, error) {
	data := url.Values{
		"client_id": {clientID},
		"scope":     {""},
	}

	req, err := http.NewRequest("POST",
		"https://github.com/login/device/code",
		strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result GitHubDeviceCodeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse device code response: %w", err)
	}
	if result.DeviceCode == "" {
		return nil, fmt.Errorf("no device_code in response: %s", string(body))
	}
	return &result, nil
}

// PollForToken polls GitHub until the user authorizes the device.
func PollForToken(clientID, deviceCode string, interval int) (*GitHubTokenResponse, error) {
	if interval < 5 {
		interval = 5
	}

	for {
		time.Sleep(time.Duration(interval) * time.Second)

		data := url.Values{
			"client_id":   {clientID},
			"device_code": {deviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}

		req, err := http.NewRequest("POST",
			"https://github.com/login/oauth/access_token",
			strings.NewReader(data.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")

		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		var tokenResp GitHubTokenResponse
		if err := json.Unmarshal(body, &tokenResp); err != nil {
			return nil, fmt.Errorf("parse token response: %w", err)
		}

		switch tokenResp.Error {
		case "":
			if tokenResp.AccessToken != "" {
				return &tokenResp, nil
			}
			return nil, fmt.Errorf("unexpected empty response: %s", string(body))
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5
			continue
		case "expired_token":
			return nil, fmt.Errorf("device code expired — please try again")
		case "access_denied":
			return nil, fmt.Errorf("authorization denied by user")
		default:
			return nil, fmt.Errorf("github error: %s — %s", tokenResp.Error, tokenResp.ErrorDescription)
		}
	}
}

// FetchGitHubUsername gets the authenticated user's login.
func FetchGitHubUsername(accessToken string) (string, error) {
	req, err := http.NewRequest("GET", "https://api.github.com/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var user struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return "", err
	}
	return user.Login, nil
}

// FetchInstallationID checks if the user has installed the GitHub App.
func FetchInstallationID(accessToken, appSlug string) (int64, error) {
	req, err := http.NewRequest("GET", "https://api.github.com/user/installations", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Installations []struct {
			ID      int64 `json:"id"`
			AppSlug string `json:"app_slug"`
		} `json:"installations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}

	for _, inst := range result.Installations {
		if inst.AppSlug == appSlug {
			return inst.ID, nil
		}
	}
	return 0, nil // not installed
}
