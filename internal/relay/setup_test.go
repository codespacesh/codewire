package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetAuthConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/auth/config" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"auth_mode": "oidc"})
	}))
	defer srv.Close()

	authMode, err := getAuthConfig(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("getAuthConfig: %v", err)
	}
	if authMode != "oidc" {
		t.Errorf("auth_mode = %q, want %q", authMode, "oidc")
	}
}

func TestGetAuthConfig_TokenMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"auth_mode": "token"})
	}))
	defer srv.Close()

	authMode, err := getAuthConfig(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("getAuthConfig: %v", err)
	}
	if authMode != "token" {
		t.Errorf("auth_mode = %q, want %q", authMode, "token")
	}
}

func TestRegisterWithDeviceFlow(t *testing.T) {
	pollCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/device/authorize":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var req struct {
				NodeName string `json:"node_name"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.NodeName == "" {
				http.Error(w, "node_name required", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"poll_token":       "poll_test_token",
				"user_code":        "ABCD-1234",
				"verification_uri": "https://example.com/device",
				"expires_in":       300,
				"interval":         1,
			})
		case "/api/v1/device/poll":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var req struct {
				PollToken string `json:"poll_token"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.PollToken != "poll_test_token" {
				http.Error(w, "invalid poll token", http.StatusGone)
				return
			}
			pollCount++
			if pollCount == 1 {
				// First poll: still pending.
				w.WriteHeader(http.StatusAccepted)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
				return
			}
			// Second poll: authorized.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"status":     "authorized",
				"node_token": "node_tok_test_abc",
			})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	nodeToken, err := registerWithDeviceFlow(context.Background(), srv.URL, "test-node")
	if err != nil {
		t.Fatalf("registerWithDeviceFlow: %v", err)
	}
	if nodeToken != "node_tok_test_abc" {
		t.Errorf("node_token = %q, want %q", nodeToken, "node_tok_test_abc")
	}
	if pollCount < 2 {
		t.Errorf("expected at least 2 polls, got %d", pollCount)
	}
}

func TestRegisterWithDeviceFlow_Expired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/device/authorize":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"poll_token":       "poll_expired",
				"user_code":        "WXYZ-5678",
				"verification_uri": "https://example.com/device",
				"expires_in":       1,
				"interval":         1,
			})
		case "/api/v1/device/poll":
			http.Error(w, "gone", http.StatusGone)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	_, err := registerWithDeviceFlow(context.Background(), srv.URL, "test-node")
	if err == nil {
		t.Fatal("expected error for expired device code, got nil")
	}
}
