// Package tunnel provides WireGuard tunnel integration for CodeWire.
// It handles key management, tunnel URL derivation, and relay connectivity.
package tunnel

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/coder/wgtunnel/tunnelsdk"
)

const (
	// keyFileName is the WireGuard private key file stored in the data directory.
	keyFileName = "wg_private_key"
	// keyFilePerms ensures the private key is only readable by the owner.
	keyFilePerms = 0o600
)

// LoadOrGenerateKey reads the WireGuard private key from dataDir/wg_private_key.
// If the file doesn't exist, a new key pair is generated and persisted.
func LoadOrGenerateKey(dataDir string) (tunnelsdk.Key, error) {
	keyPath := filepath.Join(dataDir, keyFileName)

	data, err := os.ReadFile(keyPath)
	if err == nil {
		key, err := tunnelsdk.ParsePrivateKey(strings.TrimSpace(string(data)))
		if err != nil {
			return tunnelsdk.Key{}, fmt.Errorf("parsing stored key: %w", err)
		}
		return key, nil
	}

	if !os.IsNotExist(err) {
		return tunnelsdk.Key{}, fmt.Errorf("reading key file: %w", err)
	}

	// Generate new key.
	key, err := tunnelsdk.GeneratePrivateKey()
	if err != nil {
		return tunnelsdk.Key{}, fmt.Errorf("generating key: %w", err)
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return tunnelsdk.Key{}, fmt.Errorf("creating data dir: %w", err)
	}

	if err := os.WriteFile(keyPath, []byte(key.String()+"\n"), keyFilePerms); err != nil {
		return tunnelsdk.Key{}, fmt.Errorf("writing key file: %w", err)
	}

	return key, nil
}

// PublicKeyToTunnelURL computes the deterministic tunnel URL from a public key,
// matching the wgtunnel v2 hostname encoding: sha256(pubkey)[:8] → base32hex.
func PublicKeyToTunnelURL(pubKey tunnelsdk.Key, relayBaseURL string) (string, error) {
	base, err := url.Parse(relayBaseURL)
	if err != nil {
		return "", fmt.Errorf("parsing relay URL: %w", err)
	}

	hostname := PublicKeyToHostname(pubKey)
	// Insert hostname as subdomain of the relay's host.
	base.Host = hostname + "." + base.Host

	return base.String(), nil
}

// PublicKeyToHostname computes the wgtunnel v2 hostname from a public key:
// sha256(raw_pubkey_bytes)[:8] encoded as base32hex (lowercase, no padding).
func PublicKeyToHostname(pubKey tunnelsdk.Key) string {
	// Get the raw 32-byte Curve25519 public key.
	noiseKey := pubKey.NoisePublicKey()
	hash := sha256.Sum256(noiseKey[:])
	return strings.ToLower(base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(hash[:8]))
}
