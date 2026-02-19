package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/codespacesh/codewire/internal/store"
)

// GitHubTokenResponse is the response from GitHub's OAuth token exchange.
type GitHubTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
}

// GitHubUser is the response from GitHub's /user endpoint.
type GitHubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

// LoginHandler initiates the GitHub OAuth flow by redirecting to GitHub's
// authorization page with the stored app's client_id and a random state.
func LoginHandler(st store.Store, baseURL string, allowedUsers []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, err := st.GitHubAppGet(r.Context())
		if err != nil || app == nil {
			http.Error(w, "GitHub App not configured. Visit / to set up.", http.StatusPreconditionFailed)
			return
		}

		state := GenerateState()
		oauthState := store.OAuthState{
			State:     state,
			CreatedAt: time.Now().UTC(),
			ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
		}
		if err := st.OAuthStateCreate(r.Context(), oauthState); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		redirectURI := baseURL + "/auth/github/callback"
		authURL := fmt.Sprintf(
			"https://github.com/login/oauth/authorize?client_id=%s&redirect_uri=%s&state=%s",
			url.QueryEscape(app.ClientID),
			url.QueryEscape(redirectURI),
			url.QueryEscape(state),
		)

		http.Redirect(w, r, authURL, http.StatusFound)
	}
}

// CallbackHandler handles the OAuth callback from GitHub. It exchanges the
// authorization code for an access token, fetches the user profile, and
// creates a session.
func CallbackHandler(st store.Store, baseURL string, allowedUsers []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Validate state parameter.
		state := r.URL.Query().Get("state")
		if state == "" {
			http.Error(w, "missing state parameter", http.StatusBadRequest)
			return
		}
		if err := st.OAuthStateConsume(r.Context(), state); err != nil {
			http.Error(w, "invalid or expired state parameter", http.StatusBadRequest)
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code parameter", http.StatusBadRequest)
			return
		}

		// Load GitHub App credentials.
		app, err := st.GitHubAppGet(r.Context())
		if err != nil || app == nil {
			http.Error(w, "GitHub App not configured", http.StatusInternalServerError)
			return
		}

		// Exchange code for access token.
		tokenResp, err := exchangeCode(r.Context(), app.ClientID, app.ClientSecret, code)
		if err != nil {
			http.Error(w, "failed to exchange code: "+err.Error(), http.StatusBadGateway)
			return
		}

		// Fetch user profile.
		ghUser, err := fetchGitHubUser(r.Context(), tokenResp.AccessToken)
		if err != nil {
			http.Error(w, "failed to fetch user: "+err.Error(), http.StatusBadGateway)
			return
		}

		// Check allowlist.
		if len(allowedUsers) > 0 {
			allowed := false
			for _, u := range allowedUsers {
				if strings.EqualFold(u, ghUser.Login) {
					allowed = true
					break
				}
			}
			if !allowed {
				http.Error(w, "access denied: user not in allowed list", http.StatusForbidden)
				return
			}
		}

		// Upsert user.
		now := time.Now().UTC()
		user := store.User{
			GitHubID:    ghUser.ID,
			Username:    ghUser.Login,
			AvatarURL:   ghUser.AvatarURL,
			CreatedAt:   now,
			LastLoginAt: now,
		}
		if err := st.UserUpsert(r.Context(), user); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Create session.
		sessionToken := GenerateSessionToken()
		sess := store.Session{
			Token:     sessionToken,
			GitHubID:  ghUser.ID,
			CreatedAt: now,
			ExpiresAt: now.Add(30 * 24 * time.Hour), // 30 days
		}
		if err := st.SessionCreate(r.Context(), sess); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Set session cookie.
		parsed, _ := url.Parse(baseURL)
		secure := parsed != nil && parsed.Scheme == "https"
		http.SetCookie(w, &http.Cookie{
			Name:     "cw_session",
			Value:    sessionToken,
			Path:     "/",
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   30 * 24 * 60 * 60, // 30 days in seconds
		})

		http.Redirect(w, r, "/", http.StatusFound)
	}
}

// SessionInfoHandler returns the current user's session information as JSON.
// It checks the cw_session cookie or Authorization: Bearer header.
func SessionInfoHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Try Authorization: Bearer header first.
		var token string
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}

		// Fall back to cookie.
		if token == "" {
			if cookie, err := r.Cookie("cw_session"); err == nil {
				token = cookie.Value
			}
		}

		if token == "" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		sess, err := st.SessionGet(r.Context(), token)
		if err != nil || sess == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		if time.Now().After(sess.ExpiresAt) {
			http.Error(w, `{"error":"session expired"}`, http.StatusUnauthorized)
			return
		}

		user, err := st.UserGetByID(r.Context(), sess.GitHubID)
		if err != nil || user == nil {
			http.Error(w, `{"error":"user not found"}`, http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"github_id":  user.GitHubID,
			"username":   user.Username,
			"avatar_url": user.AvatarURL,
			"expires_at": sess.ExpiresAt,
		})
	}
}

// exchangeCode exchanges an OAuth authorization code for an access token.
func exchangeCode(ctx context.Context, clientID, clientSecret, code string) (*GitHubTokenResponse, error) {
	data := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://github.com/login/oauth/access_token",
		strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contacting GitHub: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp GitHubTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("empty access token in response: %s", string(body))
	}

	return &tokenResp, nil
}

// fetchGitHubUser fetches the authenticated user's profile from GitHub.
func fetchGitHubUser(ctx context.Context, accessToken string) (*GitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contacting GitHub: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, string(body))
	}

	var user GitHubUser
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &user, nil
}
