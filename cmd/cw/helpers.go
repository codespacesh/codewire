package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
)

func openBrowser(url string) error {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "linux":
		cmd = "xdg-open"
	case "windows":
		cmd = "start"
	default:
		return fmt.Errorf("unsupported platform")
	}
	return exec.Command(cmd, url).Start()
}

// isLocalPath returns true if the argument looks like a local directory (not a URL).
func isLocalPath(arg string) bool {
	if arg == "." || arg == ".." {
		return true
	}
	if strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") {
		return true
	}
	if !strings.Contains(arg, "://") && !strings.Contains(arg, "@") && !strings.Contains(arg, ".") {
		return true
	}
	return false
}

// detectLocalRepo reads the git origin remote from a local directory and returns the HTTPS URL + current branch.
func detectLocalRepo(dir string) (string, string, error) {
	remoteCmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	remoteOut, err := remoteCmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("not a git repository or no 'origin' remote: %w", err)
	}
	remoteURL := strings.TrimSpace(string(remoteOut))
	repoURL := normalizeGitURL(remoteURL)

	branchCmd := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	branchOut, err := branchCmd.Output()
	if err != nil {
		return repoURL, "", nil
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch == "HEAD" {
		branch = ""
	}

	return repoURL, branch, nil
}

// resolveClaudeOAuthToken returns the Claude Code OAuth token from env or credentials file.
func resolveClaudeOAuthToken() string {
	if token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); token != "" {
		return token
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return ""
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}
	return creds.ClaudeAiOauth.AccessToken
}

// normalizeGitURL converts SSH git URLs to HTTPS.
func normalizeGitURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	rawURL = strings.TrimSuffix(rawURL, ".git")

	if strings.HasPrefix(rawURL, "git@") {
		rawURL = strings.TrimPrefix(rawURL, "git@")
		rawURL = strings.Replace(rawURL, ":", "/", 1)
		return "https://" + rawURL
	}

	if strings.HasPrefix(rawURL, "ssh://") {
		rawURL = strings.TrimPrefix(rawURL, "ssh://")
		rawURL = strings.TrimPrefix(rawURL, "git@")
		return "https://" + rawURL
	}

	return rawURL
}
