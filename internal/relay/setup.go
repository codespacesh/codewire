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

	"github.com/BurntSushi/toml"
	"github.com/codespacesh/codewire/internal/config"
)

// SetupOptions configures the relay setup flow.
type SetupOptions struct {
	RelayURL    string
	DataDir     string
	InviteToken string
	AuthToken   string
}

// RunSetup registers this node with the relay and writes relay_url + relay_token
// to the node's config.toml. Supports two modes: admin token or invite token.
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
	case opts.InviteToken != "":
		nodeToken, err = registerWithInvite(ctx, opts.RelayURL, nodeName, opts.InviteToken)
	default:
		return fmt.Errorf("provide --token or --invite to register with relay")
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
