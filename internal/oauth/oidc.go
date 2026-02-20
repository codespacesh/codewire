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

// OIDCProvider handles OIDC discovery and authentication flows.
// Call Discover() before using any handler methods.
type OIDCProvider struct {
	Issuer        string
	ClientID      string
	ClientSecret  string
	AllowedGroups []string

	// Populated by Discover().
	authEndpoint     string
	tokenEndpoint    string
	userinfoEndpoint string
	deviceEndpoint   string
}

type oidcDiscovery struct {
	Issuer                      string `json:"issuer"`
	AuthorizationEndpoint       string `json:"authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
	UserinfoEndpoint            string `json:"userinfo_endpoint"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
}

// Discover fetches the OIDC discovery document and populates the provider endpoints.
// Must be called before using any handler methods.
func (p *OIDCProvider) Discover(ctx context.Context) error {
	discURL := strings.TrimRight(p.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discURL, nil)
	if err != nil {
		return fmt.Errorf("creating discovery request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetching OIDC discovery document from %s: %w", discURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading discovery document: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("discovery endpoint returned %d: %s", resp.StatusCode, body)
	}
	var doc oidcDiscovery
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("parsing discovery document: %w", err)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return fmt.Errorf("incomplete discovery document (missing authorization or token endpoint)")
	}
	p.authEndpoint = doc.AuthorizationEndpoint
	p.tokenEndpoint = doc.TokenEndpoint
	p.userinfoEndpoint = doc.UserinfoEndpoint
	p.deviceEndpoint = doc.DeviceAuthorizationEndpoint
	return nil
}

// DeviceEndpoint returns the discovered device authorization endpoint URL.
func (p *OIDCProvider) DeviceEndpoint() string { return p.deviceEndpoint }

// TokenEndpoint returns the discovered token endpoint URL.
func (p *OIDCProvider) TokenEndpoint() string { return p.tokenEndpoint }

// CheckGroups returns nil if the user is allowed. If AllowedGroups is empty,
// all authenticated users are allowed. Otherwise, the user must be in at least
// one of the allowed groups.
func (p *OIDCProvider) CheckGroups(userGroups []string) error {
	if len(p.AllowedGroups) == 0 {
		return nil
	}
	for _, ag := range p.AllowedGroups {
		for _, ug := range userGroups {
			if ag == ug {
				return nil
			}
		}
	}
	return fmt.Errorf("not a member of any allowed group (%v)", p.AllowedGroups)
}

// UserinfoClaims calls the userinfo endpoint and returns sub, username, groups, avatarURL, and err.
func (p *OIDCProvider) UserinfoClaims(ctx context.Context, accessToken string) (sub, username string, groups []string, avatarURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userinfoEndpoint, nil)
	if err != nil {
		return "", "", nil, "", fmt.Errorf("creating userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", nil, "", fmt.Errorf("userinfo request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", nil, "", fmt.Errorf("userinfo returned %d: %s", resp.StatusCode, b)
	}
	var claims struct {
		Sub               string   `json:"sub"`
		PreferredUsername string   `json:"preferred_username"`
		Name              string   `json:"name"`
		Groups            []string `json:"groups"`
		Picture           string   `json:"picture"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return "", "", nil, "", fmt.Errorf("parsing userinfo response: %w", err)
	}
	name := claims.PreferredUsername
	if name == "" {
		name = claims.Name
	}
	return claims.Sub, name, claims.Groups, claims.Picture, nil
}

// ExchangeCode exchanges an authorization code for an access_token.
func (p *OIDCProvider) ExchangeCode(ctx context.Context, code, redirectURI string) (string, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, body)
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("empty access_token in response: %s", body)
	}
	return result.AccessToken, nil
}

// AuthCodeURL builds the redirect URL to the IdP for the authorization code flow.
func (p *OIDCProvider) AuthCodeURL(state, redirectURI string) string {
	return fmt.Sprintf("%s?client_id=%s&redirect_uri=%s&response_type=code&scope=%s&state=%s",
		p.authEndpoint,
		url.QueryEscape(p.ClientID),
		url.QueryEscape(redirectURI),
		url.QueryEscape("openid profile email groups"),
		url.QueryEscape(state),
	)
}

// LoginHandler initiates the OIDC authorization code flow.
// Registers: GET /auth/oidc
func (p *OIDCProvider) LoginHandler(st store.Store, baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state := GenerateState()
		if err := st.OAuthStateCreate(r.Context(), store.OAuthState{
			State:     state,
			CreatedAt: time.Now().UTC(),
			ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
		}); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		redirectURI := baseURL + "/auth/oidc/callback"
		http.Redirect(w, r, p.AuthCodeURL(state, redirectURI), http.StatusFound)
	}
}

// CallbackHandler handles the OIDC authorization code callback.
// Registers: GET /auth/oidc/callback
func (p *OIDCProvider) CallbackHandler(st store.Store, baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		redirectURI := baseURL + "/auth/oidc/callback"
		accessToken, err := p.ExchangeCode(r.Context(), code, redirectURI)
		if err != nil {
			http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		sub, username, groups, avatarURL, err := p.UserinfoClaims(r.Context(), accessToken)
		if err != nil {
			http.Error(w, "userinfo failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		if err := p.CheckGroups(groups); err != nil {
			http.Error(w, "access denied: "+err.Error(), http.StatusForbidden)
			return
		}
		now := time.Now().UTC()
		if err := st.OIDCUserUpsert(r.Context(), store.OIDCUser{
			Sub: sub, Username: username, AvatarURL: avatarURL, CreatedAt: now, LastLoginAt: now,
		}); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		sessToken := GenerateSessionToken()
		if err := st.OIDCSessionCreate(r.Context(), store.OIDCSession{
			Token: sessToken, Sub: sub, CreatedAt: now, ExpiresAt: now.Add(30 * 24 * time.Hour),
		}); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		parsed, _ := url.Parse(baseURL)
		http.SetCookie(w, &http.Cookie{
			Name:     "cw_session",
			Value:    sessToken,
			Path:     "/",
			HttpOnly: true,
			Secure:   parsed != nil && parsed.Scheme == "https",
			SameSite: http.SameSiteLaxMode,
			MaxAge:   30 * 24 * 60 * 60,
		})
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

// OIDCSessionInfoHandler returns the current OIDC user's session info as JSON.
// Registers: GET /auth/session (when authMode == "oidc")
func (p *OIDCProvider) OIDCSessionInfoHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var token string
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}
		if token == "" {
			if cookie, err := r.Cookie("cw_session"); err == nil {
				token = cookie.Value
			}
		}
		if token == "" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		sess, err := st.OIDCSessionGet(r.Context(), token)
		if err != nil || sess == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if time.Now().After(sess.ExpiresAt) {
			http.Error(w, `{"error":"session expired"}`, http.StatusUnauthorized)
			return
		}
		user, err := st.OIDCUserGetBySub(r.Context(), sess.Sub)
		if err != nil || user == nil {
			http.Error(w, `{"error":"user not found"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"sub":        user.Sub,
			"username":   user.Username,
			"avatar_url": user.AvatarURL,
			"expires_at": sess.ExpiresAt,
		})
	}
}

// OIDCIndexHandler serves a simple status page when OIDC is configured.
// Registers: GET / (when authMode == "oidc")
func (p *OIDCProvider) OIDCIndexHandler(baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>CodeWire Relay</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:480px;margin:80px auto;text-align:center;color:#1a1a1a}
h2{font-weight:600}
.badge{display:inline-block;padding:6px 16px;background:#dcfce7;color:#166534;border-radius:20px;font-size:14px;margin-top:8px}
a{color:#2563eb}
</style></head><body>
<h2>CodeWire Relay</h2>
<div class="badge">Relay is running</div>
<p><a href="/auth/oidc">Sign in</a></p>
</body></html>`)
	}
}
