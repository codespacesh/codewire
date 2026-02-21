package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/codespacesh/codewire/internal/config"
)

// SetupOptions configures the relay setup flow.
type SetupOptions struct {
	RelayURL  string
	DataDir   string
	Token     string // invite token or positional token (empty = auto-detect)
	AuthToken string // admin/CI token (--token flag)
}

// RunSetup registers this node with the relay and writes relay_url + relay_token
// to the node's config.toml. Supports three modes: admin token, invite/positional
// token, or auto-detect (OIDC device flow).
func RunSetup(ctx context.Context, opts SetupOptions) error {
	cfg, _ := config.LoadConfig(opts.DataDir)
	nodeName := "codewire"
	if cfg != nil && cfg.Node.Name != "" {
		nodeName = cfg.Node.Name
	}

	var nodeToken string
	var err error

	switch {
	case opts.AuthToken != "":
		nodeToken, err = registerWithToken(ctx, opts.RelayURL, nodeName, opts.AuthToken)
	case opts.Token != "":
		nodeToken, err = registerWithInvite(ctx, opts.RelayURL, nodeName, opts.Token)
	default:
		nodeToken, err = registerAutoDetect(ctx, opts.RelayURL, nodeName)
	}

	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "→ Registered node %q with relay %s\n", nodeName, opts.RelayURL)

	if err := writeRelayConfig(opts.DataDir, opts.RelayURL, nodeToken); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	fmt.Fprintln(os.Stderr, "→ Configuration saved.")
	fmt.Fprintf(os.Stderr, "→ Start node agent: cw node -d\n")
	fmt.Fprintf(os.Stderr, "→ SSH access: ssh %s@<relay-host> -p 2222\n", nodeName)
	return nil
}

// getAuthConfig fetches the relay's auth mode via GET /api/v1/auth/config.
func getAuthConfig(ctx context.Context, relayURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, relayURL+"/api/v1/auth/config", nil)
	if err != nil {
		return "", fmt.Errorf("creating auth config request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("contacting relay: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("relay returned HTTP %d from /api/v1/auth/config (is this relay too old?): %s", resp.StatusCode, bytes.TrimSpace(b))
	}

	var cfg struct {
		AuthMode string `json:"auth_mode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return "", fmt.Errorf("parsing auth config response: %w", err)
	}
	return cfg.AuthMode, nil
}

// registerAutoDetect auto-detects the relay's auth mode and runs the appropriate flow.
func registerAutoDetect(ctx context.Context, relayURL, nodeName string) (string, error) {
	authMode, err := getAuthConfig(ctx, relayURL)
	if err != nil {
		return "", fmt.Errorf("fetching relay auth config: %w", err)
	}

	switch authMode {
	case "oidc":
		return registerWithDeviceFlow(ctx, relayURL, nodeName)
	default:
		return "", fmt.Errorf("relay auth mode is %q — provide a token: cw setup %s <token>", authMode, relayURL)
	}
}

// registerWithDeviceFlow performs RFC 8628 device authorization against the relay
// and returns the node token once the user approves in their browser.
func registerWithDeviceFlow(ctx context.Context, relayURL, nodeName string) (string, error) {
	// Step 1: initiate device auth.
	body, _ := json.Marshal(map[string]string{"node_name": nodeName})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, relayURL+"/api/v1/device/authorize", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("contacting relay: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("device authorize failed (%d): %s", resp.StatusCode, b)
	}

	var dauth struct {
		PollToken       string `json:"poll_token"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dauth); err != nil {
		return "", fmt.Errorf("parsing device authorize response: %w", err)
	}

	// Step 2: prompt user.
	fmt.Fprintf(os.Stderr, "→ Open %s\n", dauth.VerificationURI)
	fmt.Fprintf(os.Stderr, "→ Enter code: %s\n", dauth.UserCode)
	fmt.Fprintf(os.Stderr, "→ Waiting for authorization...\n")

	// Step 3: poll until approved or expired.
	interval := time.Duration(dauth.Interval) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	expiresIn := dauth.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}

		pollBody, _ := json.Marshal(map[string]string{"poll_token": dauth.PollToken})
		preq, _ := http.NewRequestWithContext(ctx, http.MethodPost, relayURL+"/api/v1/device/poll", bytes.NewReader(pollBody))
		preq.Header.Set("Content-Type", "application/json")
		presp, err := http.DefaultClient.Do(preq)
		if err != nil {
			continue // network hiccup, retry
		}

		switch presp.StatusCode {
		case http.StatusGone:
			presp.Body.Close()
			return "", fmt.Errorf("device code expired")
		case http.StatusForbidden:
			b, _ := io.ReadAll(io.LimitReader(presp.Body, 512))
			presp.Body.Close()
			return "", fmt.Errorf("authorization denied: %s", b)
		case http.StatusAccepted:
			// Still pending — check for slow_down status.
			var pollResp struct {
				Status string `json:"status"`
			}
			json.NewDecoder(presp.Body).Decode(&pollResp)
			presp.Body.Close()
			if pollResp.Status == "slow_down" {
				interval *= 2
			}
			continue
		case http.StatusOK:
			var result struct {
				NodeToken string `json:"node_token"`
			}
			if err := json.NewDecoder(presp.Body).Decode(&result); err != nil {
				presp.Body.Close()
				return "", fmt.Errorf("parsing poll response: %w", err)
			}
			presp.Body.Close()
			return result.NodeToken, nil
		default:
			presp.Body.Close()
			continue
		}
	}

	return "", fmt.Errorf("timed out waiting for authorization")
}

func registerWithToken(ctx context.Context, relayURL, nodeName, adminToken string) (string, error) {
	body, _ := json.Marshal(map[string]string{"node_name": nodeName})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, relayURL+"/api/v1/nodes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("contacting relay: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("registration failed (%d): %s", resp.StatusCode, b)
	}

	var result struct {
		NodeToken string `json:"node_token"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.NodeToken, nil
}

func registerWithInvite(ctx context.Context, relayURL, nodeName, inviteToken string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"node_name":    nodeName,
		"invite_token": inviteToken,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, relayURL+"/api/v1/join", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("contacting relay: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("invite rejected (%d): %s", resp.StatusCode, b)
	}

	var result struct {
		NodeToken string `json:"node_token"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.NodeToken, nil
}

func writeRelayConfig(dataDir, relayURL, nodeToken string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}

	configPath := filepath.Join(dataDir, "config.toml")
	cfg := &config.Config{}
	// Load existing config if present (preserves node.name etc.).
	toml.DecodeFile(configPath, cfg)

	cfg.RelayURL = &relayURL
	cfg.RelayToken = &nodeToken

	f, err := os.Create(configPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(cfg)
}
