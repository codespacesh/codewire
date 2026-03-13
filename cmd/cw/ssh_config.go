package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
    User codewire
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
