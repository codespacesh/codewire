package oauth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/codespacesh/codewire/internal/store"
)

// ManifestPayload is the GitHub App manifest sent to GitHub.
type ManifestPayload struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	CallbackURLs       []string          `json:"callback_urls"`
	SetupURL           string            `json:"setup_url,omitempty"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
}

// ManifestResponse is what GitHub returns after the app is created.
type ManifestResponse struct {
	ID            int64  `json:"id"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// SetupPageHandler serves the relay setup page. If a GitHub App is already
// configured, it shows a simple "Relay is running" page. Otherwise it renders
// a form that initiates the GitHub App Manifest flow.
func SetupPageHandler(st store.Store, baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, err := st.GitHubAppGet(r.Context())
		if err == nil && app != nil {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>CodeWire Relay</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:480px;margin:80px auto;text-align:center;color:#1a1a1a}
h2{font-weight:600}
.badge{display:inline-block;padding:6px 16px;background:#dcfce7;color:#166534;border-radius:20px;font-size:14px;margin-top:8px}
</style>
</head><body>
<h2>CodeWire Relay</h2>
<p>Connected to GitHub as <strong>%s</strong></p>
<div class="badge">Relay is running</div>
</body></html>`, app.Owner)
			return
		}

		// Build the manifest JSON.
		parsed, _ := url.Parse(baseURL)
		hostname := parsed.Hostname()
		appName := "Codewire Relay (" + hostname + ")"
		if len(appName) > 34 {
			// GitHub enforces a 34 character limit on app names.
			// Truncate the hostname portion to fit.
			maxHost := 34 - len("Codewire Relay (") - len(")")
			if maxHost < 1 {
				appName = appName[:34]
			} else {
				if len(hostname) > maxHost {
					hostname = hostname[:maxHost]
				}
				appName = "Codewire Relay (" + hostname + ")"
			}
		}

		manifest := ManifestPayload{
			Name:         appName,
			URL:          "https://github.com/codespacesh/codewire",
			CallbackURLs: []string{baseURL + "/auth/github/callback"},
			Public:       false,
			DefaultPermissions: map[string]string{
				"metadata": "read",
			},
		}

		manifestJSON, _ := json.Marshal(manifest)

		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>CodeWire Relay Setup</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:480px;margin:80px auto;text-align:center;color:#1a1a1a}
h2{font-weight:600}
p{color:#525252;line-height:1.6}
button{padding:14px 32px;font-size:16px;cursor:pointer;background:#24292f;color:white;border:none;border-radius:8px;font-weight:500;transition:background 0.2s}
button:hover{background:#32383f}
.subtitle{font-size:14px;color:#737373;margin-top:24px}
</style>
</head><body>
<h2>CodeWire Relay Setup</h2>
<p>Create a GitHub App to enable authentication for your relay.</p>
<form method="POST" action="https://github.com/settings/apps/new">
<input type="hidden" name="manifest" value='%s'>
<button type="submit">Connect GitHub</button>
</form>
<p class="subtitle">This will create a GitHub App on your account.</p>
</body></html>`, jsonHTMLEscape(string(manifestJSON)))
	}
}

// ManifestCallbackHandler handles the callback from GitHub after the app
// manifest flow completes. GitHub redirects here with a temporary code that
// we exchange for the app credentials.
func ManifestCallbackHandler(st store.Store, baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code parameter", http.StatusBadRequest)
			return
		}

		// Exchange the temporary code for app credentials.
		convURL := fmt.Sprintf("https://api.github.com/app-manifests/%s/conversions", code)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, convURL, nil)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		req.Header.Set("Accept", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, "failed to contact GitHub: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			http.Error(w, "failed to read GitHub response", http.StatusBadGateway)
			return
		}

		if resp.StatusCode != http.StatusCreated {
			http.Error(w, fmt.Sprintf("GitHub returned %d: %s", resp.StatusCode, string(body)), http.StatusBadGateway)
			return
		}

		var manifest ManifestResponse
		if err := json.Unmarshal(body, &manifest); err != nil {
			http.Error(w, "failed to parse GitHub response", http.StatusBadGateway)
			return
		}

		app := store.GitHubApp{
			AppID:         manifest.ID,
			ClientID:      manifest.ClientID,
			ClientSecret:  manifest.ClientSecret,
			PEM:           manifest.PEM,
			WebhookSecret: manifest.WebhookSecret,
			Owner:         manifest.Owner.Login,
			CreatedAt:     time.Now().UTC(),
		}

		if err := st.GitHubAppSet(r.Context(), app); err != nil {
			http.Error(w, "failed to store app credentials: "+err.Error(), http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, "/?setup=success", http.StatusFound)
	}
}

// jsonHTMLEscape escapes a JSON string for safe inclusion in an HTML attribute.
func jsonHTMLEscape(s string) string {
	// Replace characters that would break HTML attribute context.
	var out []byte
	for _, c := range []byte(s) {
		switch c {
		case '&':
			out = append(out, []byte("&amp;")...)
		case '\'':
			out = append(out, []byte("&#39;")...)
		case '"':
			out = append(out, []byte("&quot;")...)
		case '<':
			out = append(out, []byte("&lt;")...)
		case '>':
			out = append(out, []byte("&gt;")...)
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
