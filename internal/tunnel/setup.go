package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/codespacesh/codewire/internal/config"
)

// SetupOptions configures the setup flow.
type SetupOptions struct {
	RelayURL    string
	DataDir     string
	InviteToken string // If set, use invite flow (skip OAuth/device auth)
	AuthToken   string // If set, use admin token flow (headless/CI)
}

// RunSetup performs device setup with the relay. It supports three modes:
//   - Invite: redeem an invite token (--invite)
//   - Token: use admin token (--token)
//   - OAuth: GitHub OAuth via browser (default in github auth mode)
//   - Device code: legacy browser confirmation (default in token/none mode)
func RunSetup(ctx context.Context, opts SetupOptions) error {
	fmt.Fprintln(os.Stderr, "→ Generating WireGuard key pair...")

	key, err := LoadOrGenerateKey(opts.DataDir)
	if err != nil {
		return fmt.Errorf("generating key: %w", err)
	}

	pubKey, err := key.PublicKey()
	if err != nil {
		return fmt.Errorf("deriving public key: %w", err)
	}

	// Determine node name from current config or hostname.
	cfg, _ := config.LoadConfig(opts.DataDir)
	nodeName := "codewire"
	if cfg != nil && cfg.Node.Name != "" {
		nodeName = cfg.Node.Name
	}

	pubKeyStr := pubKey.String()

	// --- Invite flow ---
	if opts.InviteToken != "" {
		return setupWithInvite(ctx, opts, pubKeyStr, nodeName)
	}

	// --- Admin token flow ---
	if opts.AuthToken != "" {
		return setupWithToken(ctx, opts, pubKeyStr, nodeName)
	}

	// --- OAuth flow: try to detect if relay supports GitHub auth ---
	sessionToken, err := tryOAuthSetup(ctx, opts.RelayURL)
	if err == nil && sessionToken != "" {
		return setupWithSession(ctx, opts, pubKeyStr, nodeName, sessionToken)
	}

	// --- Legacy device code flow ---
	return setupWithDeviceCode(ctx, opts, pubKeyStr, nodeName)
}

// setupWithInvite redeems an invite token to register the device.
func setupWithInvite(ctx context.Context, opts SetupOptions, pubKey, nodeName string) error {
	fmt.Fprintln(os.Stderr, "→ Redeeming invite code...")

	reqBody, _ := json.Marshal(map[string]string{
		"public_key":   pubKey,
		"node_name":    nodeName,
		"invite_token": opts.InviteToken,
	})

	resp, err := http.Post(opts.RelayURL+"/api/v1/join", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("contacting relay: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("invite rejected: %s", string(body))
	}

	var result struct {
		TunnelURL string `json:"tunnel_url"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	fmt.Fprintf(os.Stderr, "→ Registered! Tunnel URL: %s\n", result.TunnelURL)

	if err := writeRelayConfig(opts.DataDir, opts.RelayURL, nil); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	fmt.Fprintln(os.Stderr, "→ Configuration saved.")
	return nil
}

// setupWithToken registers the device using an admin auth token.
func setupWithToken(ctx context.Context, opts SetupOptions, pubKey, nodeName string) error {
	fmt.Fprintln(os.Stderr, "→ Registering with admin token...")

	reqBody, _ := json.Marshal(map[string]string{
		"public_key": pubKey,
		"node_name":  nodeName,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.RelayURL+"/api/v1/register", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+opts.AuthToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contacting relay: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("registration failed: %s", string(body))
	}

	var result struct {
		TunnelURL string `json:"tunnel_url"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	fmt.Fprintf(os.Stderr, "→ Registered! Tunnel URL: %s\n", result.TunnelURL)

	if err := writeRelayConfig(opts.DataDir, opts.RelayURL, nil); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	fmt.Fprintln(os.Stderr, "→ Configuration saved.")
	return nil
}

// setupWithSession registers using an OAuth session token.
func setupWithSession(ctx context.Context, opts SetupOptions, pubKey, nodeName, sessionToken string) error {
	fmt.Fprintln(os.Stderr, "→ Registering device...")

	reqBody, _ := json.Marshal(map[string]string{
		"public_key": pubKey,
		"node_name":  nodeName,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.RelayURL+"/api/v1/register", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contacting relay: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("registration failed: %s", string(body))
	}

	var result struct {
		TunnelURL string `json:"tunnel_url"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	fmt.Fprintf(os.Stderr, "→ Registered! Tunnel URL: %s\n", result.TunnelURL)

	if err := writeRelayConfig(opts.DataDir, opts.RelayURL, &sessionToken); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	fmt.Fprintln(os.Stderr, "→ Configuration saved.")
	return nil
}

// tryOAuthSetup attempts GitHub OAuth and returns a session token.
func tryOAuthSetup(ctx context.Context, relayURL string) (string, error) {
	// Check if the relay supports GitHub OAuth by probing /auth/session.
	resp, err := http.Get(relayURL + "/auth/session")
	if err != nil {
		return "", err
	}
	resp.Body.Close()

	// If the relay returns 404, it doesn't support OAuth.
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("relay does not support OAuth")
	}

	// Open browser for GitHub OAuth login.
	loginURL := relayURL + "/auth/github"
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "→ Opening browser for GitHub login...")
	fmt.Fprintf(os.Stderr, "  %s\n", loginURL)
	fmt.Fprintln(os.Stderr)

	openBrowser(loginURL)

	fmt.Fprintln(os.Stderr, "→ After logging in, paste your session token here.")
	fmt.Fprintln(os.Stderr, "  (The relay will show it after successful login)")
	fmt.Fprint(os.Stderr, "→ Session token: ")

	// TODO: In a future iteration, we could use a local HTTP server to
	// receive the callback, or poll /auth/session with a browser cookie.
	// For now, the session token is set via the cookie in the browser,
	// and the user uses the session token from the relay dashboard.
	// The primary flows are --token and --invite.

	return "", fmt.Errorf("OAuth interactive flow not fully automated yet; use --token or --invite")
}

// setupWithDeviceCode uses the legacy device authorization flow.
func setupWithDeviceCode(ctx context.Context, opts SetupOptions, pubKey, nodeName string) error {
	// Request device code from relay.
	reqBody, _ := json.Marshal(map[string]string{
		"public_key": pubKey,
		"node_name":  nodeName,
	})

	resp, err := http.Post(opts.RelayURL+"/api/v1/device/authorize", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("contacting relay: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("relay returned status %d", resp.StatusCode)
	}

	var authResp deviceAuthorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&authResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "→ Open this URL to authorize your node:")
	fmt.Fprintf(os.Stderr, "  %s\n", authResp.BrowserURL)
	fmt.Fprintln(os.Stderr)

	// Try to open browser automatically.
	openBrowser(authResp.BrowserURL)

	fmt.Fprint(os.Stderr, "→ Waiting for authorization...")

	// Poll until authorized or context cancelled.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	timeout := time.After(15 * time.Minute)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("authorization timed out")
		case <-ticker.C:
			pollResp, err := http.Get(authResp.PollURL)
			if err != nil {
				continue
			}

			var poll struct {
				Status    string `json:"status"`
				TunnelURL string `json:"tunnel_url"`
			}
			json.NewDecoder(pollResp.Body).Decode(&poll)
			pollResp.Body.Close()

			if poll.Status == "authorized" {
				fmt.Fprintln(os.Stderr, " done")

				tunnelURL := poll.TunnelURL
				if tunnelURL == "" {
					pk, _ := LoadOrGenerateKey(opts.DataDir)
					pub, _ := pk.PublicKey()
					tunnelURL, _ = PublicKeyToTunnelURL(pub, opts.RelayURL)
				}

				fmt.Fprintf(os.Stderr, "→ Tunnel URL: %s\n", tunnelURL)

				if err := writeRelayConfig(opts.DataDir, opts.RelayURL, nil); err != nil {
					return fmt.Errorf("writing config: %w", err)
				}

				fmt.Fprintln(os.Stderr, "→ Configuration saved.")
				return nil
			}
		}
	}
}

func writeRelayConfig(dataDir, relayURL string, sessionToken *string) error {
	configPath := filepath.Join(dataDir, "config.toml")

	// Load existing config or create new.
	cfg := &config.Config{}
	if _, err := os.Stat(configPath); err == nil {
		toml.DecodeFile(configPath, cfg)
	}

	cfg.RelayURL = &relayURL
	if sessionToken != nil {
		cfg.RelaySession = sessionToken
	}

	f, err := os.Create(configPath)
	if err != nil {
		return err
	}
	defer f.Close()

	return toml.NewEncoder(f).Encode(cfg)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	cmd.Start()
}
