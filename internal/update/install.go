package update

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// InstallMethod describes how cw was installed.
type InstallMethod int

const (
	DirectBinary InstallMethod = iota
	Homebrew
	APT
	AUR
)

func (m InstallMethod) String() string {
	switch m {
	case Homebrew:
		return "homebrew"
	case APT:
		return "apt"
	case AUR:
		return "aur"
	default:
		return "binary"
	}
}

// DetectInstallMethod determines how cw was installed by inspecting the
// binary path and system package databases.
func DetectInstallMethod() InstallMethod {
	exe, err := os.Executable()
	if err != nil {
		return DirectBinary
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}

	if strings.Contains(resolved, "/Cellar/codewire/") || strings.Contains(resolved, "/homebrew/") {
		return Homebrew
	}
	if _, err := os.Stat("/var/lib/dpkg/info/codewire.list"); err == nil {
		return APT
	}
	matches, _ := filepath.Glob("/var/lib/pacman/local/codewire-bin-*")
	if len(matches) > 0 {
		return AUR
	}
	return DirectBinary
}

// UpgradeCommand returns the shell command users should run for package-manager installs.
func UpgradeCommand(method InstallMethod) string {
	switch method {
	case Homebrew:
		return "brew upgrade codewire"
	case APT:
		return "sudo apt update && sudo apt upgrade codewire"
	case AUR:
		return "yay -Syu codewire-bin"
	default:
		return ""
	}
}

func assetSuffix() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "aarch64-apple-darwin"
	case "darwin/amd64":
		return "x86_64-apple-darwin"
	case "linux/amd64":
		return "x86_64-unknown-linux-musl"
	case "linux/arm64":
		return "aarch64-unknown-linux-gnu"
	default:
		return ""
	}
}

// AssetName returns the expected binary asset name for the current platform.
func AssetName(version string) string {
	suffix := assetSuffix()
	if suffix == "" {
		return ""
	}
	return fmt.Sprintf("cw-%s-%s", version, suffix)
}

func releaseURL(version, filename string) string {
	return fmt.Sprintf("https://github.com/codewiresh/codewire/releases/download/%s/%s", version, filename)
}

// SelfUpdate downloads the latest binary and replaces the running executable.
func SelfUpdate(currentVersion, latestVersion string) error {
	asset := AssetName(latestVersion)
	if asset == "" {
		return fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	// Fetch checksums
	sumsURL := releaseURL(latestVersion, "SHA256SUMS")
	expectedHash, err := fetchChecksum(sumsURL, asset)
	if err != nil {
		return fmt.Errorf("fetching checksums: %w", err)
	}

	// Resolve current executable path
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolving symlinks: %w", err)
	}

	// Download binary to temp file in same directory (ensures same filesystem for rename)
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".cw-update-*")
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("permission denied writing to %s — try: sudo cw update", dir)
		}
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()

	assetURL := releaseURL(latestVersion, asset)
	resp, err := http.Get(assetURL)
	if err != nil {
		return fmt.Errorf("downloading binary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		return fmt.Errorf("writing binary: %w", err)
	}
	tmp.Close()

	// Verify checksum
	gotHash := hex.EncodeToString(hasher.Sum(nil))
	if gotHash != expectedHash {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, gotHash)
	}

	// Atomic replace
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("permission denied replacing %s — try: sudo cw update", exe)
		}
		return fmt.Errorf("replacing binary: %w", err)
	}

	return nil
}

func fetchChecksum(sumsURL, assetName string) (string, error) {
	resp, err := http.Get(sumsURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("SHA256SUMS returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(string(body), "\n") {
		// Format: "<hash>  <filename>" or "<hash> <filename>"
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == assetName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum found for %s", assetName)
}
