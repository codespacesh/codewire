package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
)

const tokenLength = 32

const alphanumeric = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// GenerateToken creates a random 32-character alphanumeric token
// and writes it to dataDir/token with permissions 0600.
func GenerateToken(dataDir string) (string, error) {
	token, err := randomAlphanumeric(tokenLength)
	if err != nil {
		return "", fmt.Errorf("generating random token: %w", err)
	}

	path := tokenPath(dataDir)
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		return "", fmt.Errorf("writing token to %s: %w", path, err)
	}

	return token, nil
}

// LoadOrGenerateToken returns the auth token using this priority:
//  1. CODEWIRE_TOKEN environment variable (also written to disk so ValidateToken works)
//  2. Existing token file on disk
//  3. Newly generated token
func LoadOrGenerateToken(dataDir string) (string, error) {
	// Allow pre-setting token via env var (useful for containers).
	if envToken := strings.TrimSpace(os.Getenv("CODEWIRE_TOKEN")); envToken != "" {
		path := tokenPath(dataDir)
		if err := os.WriteFile(path, []byte(envToken), 0600); err != nil {
			return "", fmt.Errorf("writing token to %s: %w", path, err)
		}
		return envToken, nil
	}

	// Try reading existing token from disk.
	path := tokenPath(dataDir)
	if data, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token, nil
		}
	}

	// Fall back to generating a new token.
	return GenerateToken(dataDir)
}

// ValidateToken compares a candidate token against the stored token on disk.
// Returns false if the token file cannot be read or the tokens do not match.
// Uses constant-time comparison to prevent timing attacks.
func ValidateToken(dataDir string, candidate string) bool {
	data, err := os.ReadFile(tokenPath(dataDir))
	if err != nil {
		return false
	}
	stored := strings.TrimSpace(string(data))
	candidate = strings.TrimSpace(candidate)
	return subtle.ConstantTimeCompare([]byte(stored), []byte(candidate)) == 1
}

func tokenPath(dataDir string) string {
	return filepath.Join(dataDir, "token")
}

func randomAlphanumeric(n int) (string, error) {
	max := big.NewInt(int64(len(alphanumeric)))
	b := make([]byte, n)
	for i := range b {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = alphanumeric[idx.Int64()]
	}
	return string(b), nil
}
