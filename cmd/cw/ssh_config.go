package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	sshConfigMarkerStart = "# ---- START CODEWIRE ----"
	sshConfigMarkerEnd   = "# ---- END CODEWIRE ----"
)

func sshConfigBlock() string {
	return fmt.Sprintf(`%s
Host cw-*
    ProxyCommand cw ssh --stdio %%n
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
    User coder
%s`, sshConfigMarkerStart, sshConfigMarkerEnd)
}

// writeSSHConfig updates ~/.ssh/config with the Codewire SSH config block.
// If the block already exists (between markers), it replaces it.
// If not, it appends the block.
func writeSSHConfig() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return fmt.Errorf("create .ssh dir: %w", err)
	}

	configPath := filepath.Join(sshDir, "config")

	var existing string
	if data, err := os.ReadFile(configPath); err == nil {
		existing = string(data)
	}

	block := sshConfigBlock()

	// Check if markers exist
	startIdx := strings.Index(existing, sshConfigMarkerStart)
	endIdx := strings.Index(existing, sshConfigMarkerEnd)

	var newContent string
	if startIdx >= 0 && endIdx >= 0 {
		// Replace existing block
		endIdx += len(sshConfigMarkerEnd)
		// Skip trailing newline after end marker
		if endIdx < len(existing) && existing[endIdx] == '\n' {
			endIdx++
		}
		newContent = existing[:startIdx] + block + "\n" + existing[endIdx:]
	} else {
		// Append
		if existing != "" && !strings.HasSuffix(existing, "\n") {
			existing += "\n"
		}
		if existing != "" {
			existing += "\n"
		}
		newContent = existing + block + "\n"
	}

	return os.WriteFile(configPath, []byte(newContent), 0600)
}

// ensureSSHKey checks for ~/.ssh/id_ed25519, generates if missing.
// Returns the public key content.
func ensureSSHKey() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	privPath := filepath.Join(home, ".ssh", "id_ed25519")
	pubPath := privPath + ".pub"

	// Check if key exists
	if _, err := os.Stat(pubPath); err == nil {
		pubKey, err := os.ReadFile(pubPath)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(pubKey)), nil
	}

	// Generate new ed25519 key
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0700); err != nil {
		return "", err
	}

	pub, priv, err := generateED25519Key()
	if err != nil {
		return "", fmt.Errorf("generate SSH key: %w", err)
	}

	if err := os.WriteFile(privPath, priv, 0600); err != nil {
		return "", fmt.Errorf("write private key: %w", err)
	}
	if err := os.WriteFile(pubPath, []byte(pub+"\n"), 0644); err != nil {
		return "", fmt.Errorf("write public key: %w", err)
	}

	return pub, nil
}

// generateED25519Key generates an ed25519 keypair.
// Returns (public key line, private key PEM).
func generateED25519Key() (string, []byte, error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}

	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return "", nil, err
	}
	pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))

	block, err := ssh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return "", nil, err
	}
	privPEM := pem.EncodeToMemory(block)

	hostname, _ := os.Hostname()
	user := os.Getenv("USER")
	if user == "" {
		user = "cw"
	}
	comment := user + "@" + hostname

	return fmt.Sprintf("%s %s", pubLine, comment), privPEM, nil
}
