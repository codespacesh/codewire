package update

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input            string
		maj, min, patch  int
		ok               bool
	}{
		{"v0.2.48", 0, 2, 48, true},
		{"0.2.48", 0, 2, 48, true},
		{"v1.0.0", 1, 0, 0, true},
		{"v10.20.30", 10, 20, 30, true},
		{"dev", 0, 0, 0, false},
		{"", 0, 0, 0, false},
		{"v1.2", 0, 0, 0, false},
		{"v1.2.three", 0, 0, 0, false},
		{"v0.2.52-2-gf2fe21a", 0, 2, 52, true},
		{"0.2.52-2-gf2fe21a", 0, 2, 52, true},
		{"v1.0.0-dirty", 1, 0, 0, true},
	}
	for _, tt := range tests {
		maj, min, patch, ok := parseSemver(tt.input)
		if ok != tt.ok || maj != tt.maj || min != tt.min || patch != tt.patch {
			t.Errorf("parseSemver(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				tt.input, maj, min, patch, ok, tt.maj, tt.min, tt.patch, tt.ok)
		}
	}
}

func TestIsNewer(t *testing.T) {
	tests := []struct {
		current, latest string
		want            bool
	}{
		{"v0.2.48", "v0.2.49", true},
		{"v0.2.48", "v0.3.0", true},
		{"v0.2.48", "v1.0.0", true},
		{"v0.2.48", "v0.2.48", false},
		{"v0.2.49", "v0.2.48", false},
		{"v1.0.0", "v0.9.99", false},
		{"dev", "v0.2.49", false},
		{"v0.2.48", "dev", false},
		{"bad", "worse", false},
		{"v0.2.52-2-gf2fe21a", "v0.2.57", true},
		{"v0.2.57", "v0.2.57", false},
	}
	for _, tt := range tests {
		got := IsNewer(tt.current, tt.latest)
		if got != tt.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
		}
	}
}

func TestFetchLatestVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("expected Accept header, got %q", r.Header.Get("Accept"))
		}
		json.NewEncoder(w).Encode(githubRelease{TagName: "v0.2.49"})
	}))
	defer srv.Close()

	// Use srv's own client to avoid timeout issues under load.
	saved := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = saved }()

	got, err := fetchLatestVersionFrom(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "v0.2.49" {
		t.Errorf("got %q, want %q", got, "v0.2.49")
	}
}

func TestFetchLatestVersionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	saved := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = saved }()

	_, err := fetchLatestVersionFrom(srv.URL)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestAssetSuffix(t *testing.T) {
	suffix := assetSuffix()
	if suffix == "" {
		t.Skip("unsupported platform for this test")
	}
	// Should contain a known target triple component
	known := []string{"apple-darwin", "unknown-linux-musl", "unknown-linux-gnu"}
	found := false
	for _, k := range known {
		if contains(suffix, k) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("assetSuffix() = %q, doesn't match any known suffix", suffix)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestAssetName(t *testing.T) {
	name := AssetName("v0.2.49")
	if name == "" {
		t.Skip("unsupported platform")
	}
	if name[:3] != "cw-" {
		t.Errorf("AssetName should start with 'cw-', got %q", name)
	}
	if !containsStr(name, "v0.2.49") {
		t.Errorf("AssetName should keep 'v' prefix, got %q", name)
	}
}

func TestDetectInstallMethod(t *testing.T) {
	// On a typical dev machine, should return DirectBinary
	method := DetectInstallMethod()
	if method.String() == "" {
		t.Error("String() returned empty")
	}
}
