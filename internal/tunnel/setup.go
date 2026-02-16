package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/codespacesh/codewire/internal/config"
)

// RunSetup performs the device authorization flow: generate a WireGuard key,
// request a device code from the relay, wait for user to authorize in browser,
// then write config and optionally start the node.
func RunSetup(ctx context.Context, relayURL, dataDir string) error {
	fmt.Fprintln(os.Stderr, "→ Generating WireGuard key pair...")

	key, err := LoadOrGenerateKey(dataDir)
	if err != nil {
		return fmt.Errorf("generating key: %w", err)
	}

	pubKey, err := key.PublicKey()
	if err != nil {
		return fmt.Errorf("deriving public key: %w", err)
	}

	// Determine node name from current config or hostname.
	cfg, _ := config.LoadConfig(dataDir)
	nodeName := "codewire"
	if cfg != nil && cfg.Node.Name != "" {
		nodeName = cfg.Node.Name
	}

	// Request device code from relay.
	reqBody, _ := json.Marshal(map[string]string{
		"public_key": pubKey.String(),
		"node_name":  nodeName,
	})

	resp, err := http.Post(relayURL+"/api/v1/device/authorize", "application/json", bytes.NewReader(reqBody))
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
				fmt.Fprintln(os.Stderr, " ✓")

				tunnelURL := poll.TunnelURL
				if tunnelURL == "" {
					tunnelURL, _ = PublicKeyToTunnelURL(pubKey, relayURL)
				}

				fmt.Fprintf(os.Stderr, "→ Tunnel URL: %s\n", tunnelURL)

				// Write config.
				if err := writeRelayConfig(dataDir, relayURL); err != nil {
					return fmt.Errorf("writing config: %w", err)
				}

				fmt.Fprintln(os.Stderr, "→ Configuration saved.")
				return nil
			}
		}
	}
}

func writeRelayConfig(dataDir, relayURL string) error {
	configPath := filepath.Join(dataDir, "config.toml")

	// Load existing config or create new.
	cfg := &config.Config{}
	if _, err := os.Stat(configPath); err == nil {
		toml.DecodeFile(configPath, cfg)
	}

	cfg.RelayURL = &relayURL

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
