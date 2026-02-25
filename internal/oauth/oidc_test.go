package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/codewiresh/codewire/internal/store"
)

// TestCheckGroups_EmptyAllowed verifies that an empty AllowedGroups list
// permits any authenticated user regardless of their group membership.
func TestCheckGroups_EmptyAllowed(t *testing.T) {
	p := &OIDCProvider{AllowedGroups: nil}
	if err := p.CheckGroups([]string{"any-group"}); err != nil {
		t.Errorf("empty AllowedGroups should allow all, got: %v", err)
	}
}

// TestCheckGroups_Match verifies that a user in an allowed group is permitted.
func TestCheckGroups_Match(t *testing.T) {
	p := &OIDCProvider{AllowedGroups: []string{"sonica", "admins"}}
	if err := p.CheckGroups([]string{"sonica"}); err != nil {
		t.Errorf("expected allow for matching group, got: %v", err)
	}
}

// TestCheckGroups_NoMatch verifies that a user not in any allowed group is denied.
func TestCheckGroups_NoMatch(t *testing.T) {
	p := &OIDCProvider{AllowedGroups: []string{"sonica"}}
	if err := p.CheckGroups([]string{"other-team"}); err == nil {
		t.Error("expected deny for non-matching group")
	}
}

// TestCheckGroups_EmptyUserGroups verifies that a user with no groups is denied
// when AllowedGroups is non-empty.
func TestCheckGroups_EmptyUserGroups(t *testing.T) {
	p := &OIDCProvider{AllowedGroups: []string{"sonica"}}
	if err := p.CheckGroups(nil); err == nil {
		t.Error("expected deny for user with no groups")
	}
}

// TestDiscover verifies that the Discover method correctly parses the
// OIDC discovery document and populates the provider's endpoint fields.
func TestDiscover(t *testing.T) {
	discovery := map[string]string{
		"issuer":                 "https://auth.example.com",
		"authorization_endpoint": "https://auth.example.com/auth",
		"token_endpoint":         "https://auth.example.com/token",
		"userinfo_endpoint":      "https://auth.example.com/userinfo",
		"device_authorization_endpoint": "https://auth.example.com/device/code",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(discovery)
	}))
	defer srv.Close()

	p := &OIDCProvider{
		Issuer:   srv.URL,
		ClientID: "test-client",
	}

	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover() error: %v", err)
	}

	if p.authEndpoint != discovery["authorization_endpoint"] {
		t.Errorf("authEndpoint = %q, want %q", p.authEndpoint, discovery["authorization_endpoint"])
	}
	if p.tokenEndpoint != discovery["token_endpoint"] {
		t.Errorf("tokenEndpoint = %q, want %q", p.tokenEndpoint, discovery["token_endpoint"])
	}
	if p.userinfoEndpoint != discovery["userinfo_endpoint"] {
		t.Errorf("userinfoEndpoint = %q, want %q", p.userinfoEndpoint, discovery["userinfo_endpoint"])
	}
	if p.TokenEndpoint() != discovery["token_endpoint"] {
		t.Errorf("TokenEndpoint() = %q, want %q", p.TokenEndpoint(), discovery["token_endpoint"])
	}
	if p.DeviceEndpoint() != discovery["device_authorization_endpoint"] {
		t.Errorf("DeviceEndpoint() = %q, want %q", p.DeviceEndpoint(), discovery["device_authorization_endpoint"])
	}
}

// TestDiscover_MissingEndpoint verifies that Discover returns an error when
// required endpoints are absent from the discovery document.
func TestDiscover_MissingEndpoint(t *testing.T) {
	// Serve a document that is missing the token_endpoint.
	discovery := map[string]string{
		"issuer":                 "https://auth.example.com",
		"authorization_endpoint": "https://auth.example.com/auth",
		// token_endpoint intentionally omitted
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(discovery)
	}))
	defer srv.Close()

	p := &OIDCProvider{Issuer: srv.URL}
	if err := p.Discover(context.Background()); err == nil {
		t.Error("expected error for incomplete discovery document, got nil")
	}
}

// mockStore is a minimal store.Store implementation sufficient for callback handler tests.
type mockStore struct {
	store.Store // embed to satisfy interface; panics on any unimplemented call
	states      map[string]bool
	oidcUsers   map[string]store.OIDCUser
	oidcSess    map[string]store.OIDCSession
}

func newMockStore() *mockStore {
	return &mockStore{
		states:    make(map[string]bool),
		oidcUsers: make(map[string]store.OIDCUser),
		oidcSess:  make(map[string]store.OIDCSession),
	}
}

func (m *mockStore) OAuthStateCreate(_ context.Context, s store.OAuthState) error {
	m.states[s.State] = true
	return nil
}

func (m *mockStore) OAuthStateConsume(_ context.Context, state string) error {
	if !m.states[state] {
		return &url.Error{Op: "consume", Err: http.ErrNoCookie}
	}
	delete(m.states, state)
	return nil
}

func (m *mockStore) OIDCUserUpsert(_ context.Context, u store.OIDCUser) error {
	m.oidcUsers[u.Sub] = u
	return nil
}

func (m *mockStore) OIDCUserGetBySub(_ context.Context, sub string) (*store.OIDCUser, error) {
	u, ok := m.oidcUsers[sub]
	if !ok {
		return nil, nil
	}
	return &u, nil
}

func (m *mockStore) OIDCSessionCreate(_ context.Context, sess store.OIDCSession) error {
	m.oidcSess[sess.Token] = sess
	return nil
}

func (m *mockStore) OIDCSessionGet(_ context.Context, token string) (*store.OIDCSession, error) {
	sess, ok := m.oidcSess[token]
	if !ok || time.Now().After(sess.ExpiresAt) {
		return nil, nil
	}
	return &sess, nil
}

func (m *mockStore) OIDCSessionDelete(_ context.Context, token string) error {
	delete(m.oidcSess, token)
	return nil
}

// TestCallbackHandler_GroupDenied verifies that the CallbackHandler returns
// HTTP 403 when the authenticated user is not in any allowed group.
func TestCallbackHandler_GroupDenied(t *testing.T) {
	// Set up a fake token endpoint that returns an access token.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": "test-access-token",
			"token_type":   "bearer",
		})
	}))
	defer tokenSrv.Close()

	// Set up a fake userinfo endpoint returning a user in "wrong-group".
	userInfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"sub":                "user-sub-123",
			"preferred_username": "alice",
			"groups":             []string{"wrong-group"},
		})
	}))
	defer userInfoSrv.Close()

	// Pre-seed state in the mock store.
	st := newMockStore()
	validState := "validstate123"
	st.states[validState] = true

	p := &OIDCProvider{
		Issuer:           "https://auth.example.com",
		ClientID:         "test-client",
		ClientSecret:     "test-secret",
		AllowedGroups:    []string{"sonica"},
		tokenEndpoint:    tokenSrv.URL,
		userinfoEndpoint: userInfoSrv.URL,
	}

	// Build a fake relay base URL (unused for redirect since we expect 403).
	baseURL := "https://relay.example.com"

	handler := p.CallbackHandler(st, baseURL)

	// Construct callback request with valid state and a code.
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state="+validState+"&code=authcode123", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "access denied") {
		t.Errorf("expected 'access denied' in body, got: %s", rec.Body.String())
	}
}

// TestCallbackHandler_Success verifies that the CallbackHandler creates a session
// and sets the cw_session cookie on success.
func TestCallbackHandler_Success(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": "test-access-token",
			"token_type":   "bearer",
		})
	}))
	defer tokenSrv.Close()

	userInfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"sub":                "user-sub-456",
			"preferred_username": "bob",
			"groups":             []string{"sonica"},
		})
	}))
	defer userInfoSrv.Close()

	st := newMockStore()
	validState := "goodstate456"
	st.states[validState] = true

	p := &OIDCProvider{
		Issuer:           "https://auth.example.com",
		ClientID:         "test-client",
		ClientSecret:     "test-secret",
		AllowedGroups:    []string{"sonica"},
		tokenEndpoint:    tokenSrv.URL,
		userinfoEndpoint: userInfoSrv.URL,
	}

	baseURL := "http://relay.example.com"
	handler := p.CallbackHandler(st, baseURL)

	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state="+validState+"&code=authcode456", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("expected 302 redirect, got %d; body: %s", rec.Code, rec.Body.String())
	}

	// Check that cw_session cookie was set.
	resp := rec.Result()
	var sessionCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "cw_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected cw_session cookie to be set")
	}
	if !strings.HasPrefix(sessionCookie.Value, "sess_") {
		t.Errorf("session token should have sess_ prefix, got: %s", sessionCookie.Value)
	}
	if sessionCookie.Path != "/" {
		t.Errorf("cookie Path = %q, want /", sessionCookie.Path)
	}
	if !sessionCookie.HttpOnly {
		t.Error("cookie should be HttpOnly")
	}
}
