package platform

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDetectRepo(t *testing.T) {
	expected := DetectionResult{
		TemplateImage:  "ghcr.io/codewiresh/workspace-node:latest",
		InstallCommand: "npm install",
		StartupScript:  "npm run build",
		Language:       "typescript",
		Framework:      "nextjs",
		SuggestedName:  "my-app",
		NeedsDocker:    true,
		HasCompose:     false,
		AppPorts:       []AppPort{{Label: "web", Port: 3000}},
		CPU:            "4",
		Memory:         "8",
		SetupNotes:     "Next.js app",
	}

	var gotMethod, gotPath, gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer srv.Close()

	client := &Client{
		ServerURL:    srv.URL,
		SessionToken: "test-token",
		HTTP:         &http.Client{Timeout: 5 * time.Second},
	}

	result, err := client.DetectRepo("https://github.com/vercel/next.js", "canary")
	if err != nil {
		t.Fatalf("DetectRepo: %v", err)
	}

	// Verify request
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/launch/detect" {
		t.Errorf("path = %q, want /api/v1/launch/detect", gotPath)
	}

	var reqBody map[string]string
	if err := json.Unmarshal([]byte(gotBody), &reqBody); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if reqBody["repo_url"] != "https://github.com/vercel/next.js" {
		t.Errorf("repo_url = %q, want https://github.com/vercel/next.js", reqBody["repo_url"])
	}
	if reqBody["branch"] != "canary" {
		t.Errorf("branch = %q, want canary", reqBody["branch"])
	}

	// Verify response parsing
	if result.Language != "typescript" {
		t.Errorf("Language = %q, want typescript", result.Language)
	}
	if result.Framework != "nextjs" {
		t.Errorf("Framework = %q, want nextjs", result.Framework)
	}
	if result.InstallCommand != "npm install" {
		t.Errorf("InstallCommand = %q, want npm install", result.InstallCommand)
	}
	if result.StartupScript != "npm run build" {
		t.Errorf("StartupScript = %q, want npm run build", result.StartupScript)
	}
	if result.SuggestedName != "my-app" {
		t.Errorf("SuggestedName = %q, want my-app", result.SuggestedName)
	}
	if !result.NeedsDocker {
		t.Error("NeedsDocker = false, want true")
	}
	if result.HasCompose {
		t.Error("HasCompose = true, want false")
	}
	if len(result.AppPorts) != 1 {
		t.Fatalf("AppPorts len = %d, want 1", len(result.AppPorts))
	}
	if result.AppPorts[0].Label != "web" || result.AppPorts[0].Port != 3000 {
		t.Errorf("AppPorts[0] = %+v, want {web 3000}", result.AppPorts[0])
	}
	if result.CPU != "4" {
		t.Errorf("CPU = %q, want 4", result.CPU)
	}
	if result.Memory != "8" {
		t.Errorf("Memory = %q, want 8", result.Memory)
	}
}

func TestDetectRepo_NoBranch(t *testing.T) {
	var gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DetectionResult{Language: "go"})
	}))
	defer srv.Close()

	client := &Client{
		ServerURL:    srv.URL,
		SessionToken: "test-token",
		HTTP:         &http.Client{Timeout: 5 * time.Second},
	}

	result, err := client.DetectRepo("https://github.com/golang/go", "")
	if err != nil {
		t.Fatalf("DetectRepo: %v", err)
	}

	// Branch should not be in the body
	var reqBody map[string]string
	if err := json.Unmarshal([]byte(gotBody), &reqBody); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if _, ok := reqBody["branch"]; ok {
		t.Error("branch should not be present when empty")
	}

	if result.Language != "go" {
		t.Errorf("Language = %q, want go", result.Language)
	}
}

func TestDetectRepo_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"title":  "Internal Server Error",
			"detail": "Anthropic API unavailable",
		})
	}))
	defer srv.Close()

	client := &Client{
		ServerURL:    srv.URL,
		SessionToken: "test-token",
		HTTP:         &http.Client{Timeout: 5 * time.Second},
	}

	_, err := client.DetectRepo("https://github.com/test/repo", "")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestDetectRepo_AuthHeader(t *testing.T) {
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DetectionResult{})
	}))
	defer srv.Close()

	client := &Client{
		ServerURL:    srv.URL,
		SessionToken: "my-session-token",
		HTTP:         &http.Client{Timeout: 5 * time.Second},
	}

	_, err := client.DetectRepo("https://github.com/test/repo", "")
	if err != nil {
		t.Fatalf("DetectRepo: %v", err)
	}

	if gotAuth != "Bearer my-session-token" {
		t.Errorf("Authorization = %q, want Bearer my-session-token", gotAuth)
	}
}
