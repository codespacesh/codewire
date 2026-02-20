package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	cdrslog "cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/wgtunnel/tunneld"
	"github.com/coder/wgtunnel/tunnelsdk"

	"github.com/codespacesh/codewire/internal/oauth"
	"github.com/codespacesh/codewire/internal/store"
)

// rateLimiter tracks request counts per IP with a sliding window.
type rateLimiter struct {
	mu      sync.Mutex
	entries map[string][]time.Time
	limit   int
	window  time.Duration
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		entries: make(map[string][]time.Time),
		limit:   limit,
		window:  window,
	}
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Remove old entries.
	times := rl.entries[ip]
	valid := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}

	if len(valid) >= rl.limit {
		rl.entries[ip] = valid
		return false
	}

	rl.entries[ip] = append(valid, now)
	return true
}

func remoteIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.SplitN(xff, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// RelayConfig configures the relay server.
type RelayConfig struct {
	// BaseURL is the public-facing HTTPS URL of the relay (e.g. https://relay.codespace.sh).
	BaseURL string
	// WireguardEndpoint is the UDP host:port advertised to clients for WireGuard.
	WireguardEndpoint string
	// WireguardPort is the UDP port to listen on for WireGuard (default 41820).
	WireguardPort uint16
	// ListenAddr is the HTTP listen address (default ":8080").
	ListenAddr string
	// DataDir is where relay.db lives.
	DataDir string
	// AuthMode controls registration: "github", "token", "none".
	AuthMode string
	// AuthToken is the shared secret when AuthMode is "token" or as fallback for headless/CI.
	AuthToken string
	// AllowedUsers is a list of GitHub usernames allowed to authenticate (GitHub mode).
	AllowedUsers []string
	// GitHubClientID is a manual override for GitHub OAuth App client ID (private networks).
	GitHubClientID string
	// GitHubClientSecret is a manual override for GitHub OAuth App client secret.
	GitHubClientSecret string
}

// RunRelay starts the relay server. It blocks until ctx is cancelled.
func RunRelay(ctx context.Context, cfg RelayConfig) error {
	if cfg.WireguardPort == 0 {
		cfg.WireguardPort = 41820
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}

	baseURL, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return fmt.Errorf("parsing base URL: %w", err)
	}

	// Open storage.
	st, err := store.NewSQLiteStore(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer st.Close()

	// Generate or load relay's own WireGuard key.
	relayKey, err := LoadOrGenerateKey(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("loading relay key: %w", err)
	}

	logger := cdrslog.Make(sloghuman.Sink(os.Stderr))

	// Determine WireGuard endpoint to advertise.
	wgEndpoint := cfg.WireguardEndpoint
	if wgEndpoint == "" {
		wgEndpoint = fmt.Sprintf("%s:%d", baseURL.Hostname(), cfg.WireguardPort)
	}

	// Start the wgtunnel relay (WireGuard device + HTTP reverse proxy).
	opts := &tunneld.Options{
		Log:               logger,
		BaseURL:           baseURL,
		WireguardEndpoint: wgEndpoint,
		WireguardPort:     cfg.WireguardPort,
		WireguardKey:      relayKey,
		WireguardMTU:      1280,
		WireguardServerIP: netip.MustParseAddr("fcca::1"),
		WireguardNetworkPrefix: netip.MustParsePrefix("fcca::/16"),
	}

	api, err := tunneld.New(opts)
	if err != nil {
		return fmt.Errorf("starting wgtunnel: %w", err)
	}
	defer api.Close()

	// Rate limiters.
	deviceRL := newRateLimiter(5, time.Minute)
	joinRL := newRateLimiter(10, time.Minute)

	// Auth middleware for protected API routes.
	authMiddleware := oauth.RequireAuth(st, cfg.AuthToken)

	// Build HTTP mux: our API routes + wgtunnel's reverse proxy router.
	mux := http.NewServeMux()

	// --- GitHub App Manifest flow (setup) ---
	if cfg.AuthMode == "github" {
		mux.HandleFunc("GET /auth/github/manifest/callback", oauth.ManifestCallbackHandler(st, cfg.BaseURL))
		mux.HandleFunc("GET /auth/github", oauth.LoginHandler(st, cfg.BaseURL, cfg.AllowedUsers))
		mux.HandleFunc("GET /auth/github/callback", oauth.CallbackHandler(st, cfg.BaseURL, cfg.AllowedUsers))
		mux.HandleFunc("GET /auth/session", oauth.SessionInfoHandler(st))

		// If manual GitHub credentials were provided, store them on first run.
		if cfg.GitHubClientID != "" && cfg.GitHubClientSecret != "" {
			existing, _ := st.GitHubAppGet(context.Background())
			if existing == nil {
				st.GitHubAppSet(context.Background(), store.GitHubApp{
					ClientID:     cfg.GitHubClientID,
					ClientSecret: cfg.GitHubClientSecret,
					Owner:        "manual",
					CreatedAt:    time.Now().UTC(),
				})
			}
		}
	}

	// --- Device authorization endpoints (legacy flow) ---
	mux.HandleFunc("POST /api/v1/device/authorize", rateLimitMiddleware(deviceRL, deviceAuthorizeHandler(st, opts, cfg)))
	mux.HandleFunc("GET /api/v1/device/poll/{code}", devicePollHandler(st))
	mux.HandleFunc("GET /auth/device", devicePageHandler())
	mux.HandleFunc("POST /auth/device/confirm", deviceConfirmHandler(st))

	// --- Device registration (new auth-aware endpoint) ---
	mux.Handle("POST /api/v1/register", authMiddleware(http.HandlerFunc(registerHandler(st, opts, cfg))))

	// --- Invite endpoints ---
	mux.Handle("POST /api/v1/invites", authMiddleware(http.HandlerFunc(inviteCreateHandler(st))))
	mux.Handle("GET /api/v1/invites", authMiddleware(http.HandlerFunc(inviteListHandler(st))))
	mux.Handle("DELETE /api/v1/invites/{token}", authMiddleware(http.HandlerFunc(inviteDeleteHandler(st))))

	// --- Invite redemption (public, rate-limited) ---
	mux.HandleFunc("POST /api/v1/join", rateLimitMiddleware(joinRL, joinHandler(st, opts, cfg)))
	mux.HandleFunc("GET /join", joinPageHandler(cfg.BaseURL))

	// --- Node management ---
	mux.Handle("DELETE /api/v1/nodes/{name}", authMiddleware(http.HandlerFunc(nodeRevokeHandler(st))))

	// --- Node discovery ---
	mux.HandleFunc("GET /api/v1/nodes", nodesListHandler(st))

	// --- KV API ---
	mux.HandleFunc("PUT /api/v1/kv/{namespace}/{key}", kvSetHandler(st, cfg))
	mux.HandleFunc("GET /api/v1/kv/{namespace}/{key}", kvGetHandler(st, cfg))
	mux.HandleFunc("DELETE /api/v1/kv/{namespace}/{key}", kvDeleteHandler(st, cfg))
	mux.HandleFunc("GET /api/v1/kv/{namespace}", kvListHandler(st, cfg))

	// --- Health check ---
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// --- Root page: setup (github mode) or tunnel passthrough ---
	tunnelRouter := api.Router()
	if cfg.AuthMode == "github" {
		setupPage := oauth.SetupPageHandler(st, cfg.BaseURL)
		mux.HandleFunc("GET /{$}", setupPage)
		// Non-root paths fall through to wgtunnel router.
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			tunnelRouter.ServeHTTP(w, r)
		})
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			tunnelRouter.ServeHTTP(w, r)
		})
	}

	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	// Start WireGuard UDP listener separately — tunneld already does this
	// via the device in New(). We just need to start the HTTP server.

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "[relay] listening on %s (base_url=%s, wg_port=%d)\n", cfg.ListenAddr, cfg.BaseURL, cfg.WireguardPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// --- Device Authorization ---

type deviceAuthorizeRequest struct {
	PublicKey string `json:"public_key"`
	NodeName  string `json:"node_name"`
}

type deviceAuthorizeResponse struct {
	Code       string `json:"code"`
	PollURL    string `json:"poll_url"`
	BrowserURL string `json:"browser_url"`
}

func generateDeviceCode() string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("CW-%02X%02X-%02X%02X", b[0], b[1], b[2], b[3])
}

func deviceAuthorizeHandler(st store.Store, opts *tunneld.Options, cfg RelayConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check auth for token mode.
		if cfg.AuthMode == "token" {
			if !checkAuthToken(r, cfg.AuthToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		var req deviceAuthorizeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		code := generateDeviceCode()
		now := time.Now().UTC()

		dc := store.DeviceCode{
			Code:      code,
			PublicKey: req.PublicKey,
			NodeName:  req.NodeName,
			Status:    "pending",
			CreatedAt: now,
			ExpiresAt: now.Add(15 * time.Minute),
		}

		if err := st.DeviceCodeCreate(r.Context(), dc); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		baseURL := cfg.BaseURL
		resp := deviceAuthorizeResponse{
			Code:       code,
			PollURL:    baseURL + "/api/v1/device/poll/" + code,
			BrowserURL: baseURL + "/auth/device?code=" + code,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func devicePollHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.PathValue("code")
		dc, err := st.DeviceCodeGet(r.Context(), code)
		if err != nil || dc == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		resp := map[string]interface{}{
			"status": dc.Status,
		}

		if dc.Status == "authorized" {
			// Compute tunnel URL from the node's public key.
			pubKey, err := tunnelsdk.ParsePublicKey(dc.PublicKey)
			if err == nil {
				tunnelURL, _ := PublicKeyToTunnelURL(pubKey, r.Host)
				resp["tunnel_url"] = tunnelURL
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func devicePageHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>CodeWire Device Authorization</title>
<style>body{font-family:system-ui;max-width:400px;margin:80px auto;text-align:center}
button{padding:12px 24px;font-size:16px;cursor:pointer;background:#2563eb;color:white;border:none;border-radius:6px}
button:hover{background:#1d4ed8}.code{font-size:24px;font-weight:bold;letter-spacing:2px;margin:20px 0}</style>
</head><body>
<h2>Authorize Device</h2>
<p>A CodeWire node is requesting access.</p>
<p class="code">%s</p>
<form method="POST" action="/auth/device/confirm">
<input type="hidden" name="code" value="%s">
<button type="submit">Authorize</button>
</form>
</body></html>`, code, code)
	}
}

func deviceConfirmHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var code string
		if r.Header.Get("Content-Type") == "application/json" {
			var body struct{ Code string `json:"code"` }
			json.NewDecoder(r.Body).Decode(&body)
			code = body.Code
		} else {
			r.ParseForm()
			code = r.FormValue("code")
		}

		if err := st.DeviceCodeConfirm(r.Context(), code); err != nil {
			http.Error(w, "failed: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Register the node.
		dc, _ := st.DeviceCodeGet(r.Context(), code)
		if dc != nil {
			pubKey, err := tunnelsdk.ParsePublicKey(dc.PublicKey)
			if err == nil {
				tunnelURL, _ := PublicKeyToTunnelURL(pubKey, "https://"+r.Host)
				_ = tunnelURL
				st.NodeRegister(r.Context(), store.NodeRecord{
					Name:         dc.NodeName,
					Token:        dc.PublicKey,
					AuthorizedAt: time.Now().UTC(),
					LastSeenAt:   time.Now().UTC(),
				})
			}
		}

		if r.Header.Get("Content-Type") == "application/json" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "authorized"})
		} else {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Authorized</title>
<style>body{font-family:system-ui;max-width:400px;margin:80px auto;text-align:center}</style>
</head><body><h2>Device Authorized</h2><p>You can close this window.</p></body></html>`)
		}
	}
}

// --- Device Registration (auth-aware) ---

type registerRequest struct {
	PublicKey string `json:"public_key"`
	NodeName  string `json:"node_name"`
}

func registerHandler(st store.Store, opts *tunneld.Options, cfg RelayConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if req.PublicKey == "" || req.NodeName == "" {
			http.Error(w, "public_key and node_name required", http.StatusBadRequest)
			return
		}

		// Check if key is revoked.
		revoked, _ := st.RevokedKeyCheck(r.Context(), req.PublicKey)
		if revoked {
			http.Error(w, "this device key has been revoked", http.StatusForbidden)
			return
		}

		auth := oauth.GetAuth(r.Context())
		var githubID *int64
		if auth != nil && auth.UserID != 0 {
			githubID = &auth.UserID
		}

		pubKey, err := tunnelsdk.ParsePublicKey(req.PublicKey)
		if err != nil {
			http.Error(w, "invalid public key", http.StatusBadRequest)
			return
		}

		tunnelURL, _ := PublicKeyToTunnelURL(pubKey, cfg.BaseURL)

		node := store.NodeRecord{
			Name:         req.NodeName,
			Token:        req.PublicKey,
			GitHubID:     githubID,
			AuthorizedAt: time.Now().UTC(),
			LastSeenAt:   time.Now().UTC(),
		}

		if err := st.NodeRegister(r.Context(), node); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":     "registered",
			"tunnel_url": tunnelURL,
		})
	}
}

// --- Invite Handlers ---

type inviteCreateRequest struct {
	Uses int    `json:"uses"`
	TTL  string `json:"ttl"`
}

func inviteCreateHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req inviteCreateRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.Uses <= 0 {
			req.Uses = 1
		}

		ttl := time.Hour
		if req.TTL != "" {
			parsed, err := time.ParseDuration(req.TTL)
			if err != nil {
				http.Error(w, "invalid ttl", http.StatusBadRequest)
				return
			}
			ttl = parsed
		}

		auth := oauth.GetAuth(r.Context())
		var createdBy *int64
		if auth != nil && auth.UserID != 0 {
			createdBy = &auth.UserID
		}

		now := time.Now().UTC()
		invite := store.Invite{
			Token:         oauth.GenerateInviteToken(),
			CreatedBy:     createdBy,
			UsesRemaining: req.Uses,
			ExpiresAt:     now.Add(ttl),
			CreatedAt:     now,
		}

		if err := st.InviteCreate(r.Context(), invite); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(invite)
	}
}

func inviteListHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		invites, err := st.InviteList(r.Context())
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(invites)
	}
}

func inviteDeleteHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		if err := st.InviteDelete(r.Context(), token); err != nil {
			http.Error(w, "invite not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- Invite Redemption (public) ---

type joinRequest struct {
	PublicKey   string `json:"public_key"`
	NodeName    string `json:"node_name"`
	InviteToken string `json:"invite_token"`
}

func joinHandler(st store.Store, opts *tunneld.Options, cfg RelayConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req joinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if req.PublicKey == "" || req.NodeName == "" || req.InviteToken == "" {
			http.Error(w, "public_key, node_name, and invite_token required", http.StatusBadRequest)
			return
		}

		// Consume invite (validates and decrements).
		if err := st.InviteConsume(r.Context(), req.InviteToken); err != nil {
			http.Error(w, "invalid or expired invite", http.StatusForbidden)
			return
		}

		// Check if key is revoked.
		revoked, _ := st.RevokedKeyCheck(r.Context(), req.PublicKey)
		if revoked {
			http.Error(w, "this device key has been revoked", http.StatusForbidden)
			return
		}

		// Look up who created the invite to associate the node with.
		invite, _ := st.InviteGet(r.Context(), req.InviteToken)
		var githubID *int64
		if invite != nil && invite.CreatedBy != nil {
			githubID = invite.CreatedBy
		}

		pubKey, err := tunnelsdk.ParsePublicKey(req.PublicKey)
		if err != nil {
			http.Error(w, "invalid public key", http.StatusBadRequest)
			return
		}

		tunnelURL, _ := PublicKeyToTunnelURL(pubKey, cfg.BaseURL)

		node := store.NodeRecord{
			Name:         req.NodeName,
			Token:        req.PublicKey,
			GitHubID:     githubID,
			AuthorizedAt: time.Now().UTC(),
			LastSeenAt:   time.Now().UTC(),
		}

		if err := st.NodeRegister(r.Context(), node); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":     "registered",
			"tunnel_url": tunnelURL,
		})
	}
}

func joinPageHandler(baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		invite := r.URL.Query().Get("invite")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>Join CodeWire Relay</title>
<style>body{font-family:system-ui;max-width:480px;margin:80px auto;text-align:center;color:#1a1a1a}
h2{font-weight:600}
.code{font-family:monospace;background:#f5f5f5;padding:8px 16px;border-radius:6px;display:inline-block;margin:12px 0;word-break:break-all}
p{color:#525252;line-height:1.6}
</style></head><body>
<h2>Join CodeWire Relay</h2>
<p>Use this invite code to register your device:</p>
<div class="code">%s</div>
<p>Run on your device:</p>
<div class="code">cw setup %s --invite %s</div>
</body></html>`, invite, baseURL, invite)
	}
}

// --- Node Revocation ---

func nodeRevokeHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")

		node, err := st.NodeGet(r.Context(), name)
		if err != nil || node == nil {
			http.Error(w, "node not found", http.StatusNotFound)
			return
		}

		// Add key to revoked list.
		if err := st.RevokedKeyAdd(r.Context(), store.RevokedKey{
			PublicKey: node.Token,
			RevokedAt: time.Now().UTC(),
			Reason:    "revoked via API",
		}); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Delete the node record.
		if err := st.NodeDelete(r.Context(), name); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "revoked",
			"node":   name,
		})
	}
}

// --- Node Discovery ---

type nodeResponse struct {
	Name      string `json:"name"`
	TunnelURL string `json:"tunnel_url"`
	Connected bool   `json:"connected"`
}

func nodesListHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nodes, err := st.NodeList(r.Context())
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		resp := make([]nodeResponse, 0, len(nodes))
		for _, n := range nodes {
			connected := time.Since(n.LastSeenAt) < 2*time.Minute
			resp = append(resp, nodeResponse{
				Name:      n.Name,
				TunnelURL: "",
				Connected: connected,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// --- KV API ---

func kvSetHandler(st store.Store, cfg RelayConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("namespace")
		key := r.PathValue("key")

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		var ttl *time.Duration
		if ttlStr := r.Header.Get("X-TTL"); ttlStr != "" {
			d, err := time.ParseDuration(ttlStr)
			if err != nil {
				http.Error(w, "invalid X-TTL header", http.StatusBadRequest)
				return
			}
			ttl = &d
		}

		if err := st.KVSet(r.Context(), ns, key, body, ttl); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func kvGetHandler(st store.Store, cfg RelayConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("namespace")
		key := r.PathValue("key")

		val, err := st.KVGet(r.Context(), ns, key)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if val == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(val)
	}
}

func kvDeleteHandler(st store.Store, cfg RelayConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("namespace")
		key := r.PathValue("key")

		if err := st.KVDelete(r.Context(), ns, key); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func kvListHandler(st store.Store, cfg RelayConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("namespace")
		prefix := r.URL.Query().Get("prefix")

		entries, err := st.KVList(r.Context(), ns, prefix)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
	}
}

// --- Helpers ---

func rateLimitMiddleware(rl *rateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := remoteIP(r)
		if !rl.allow(ip) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

func checkAuthToken(r *http.Request, expected string) bool {
	// Check Authorization header first.
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ") == expected
	}
	// Fall back to query parameter.
	return r.URL.Query().Get("token") == expected
}

// ListenUDP starts a UDP listener. Used for WireGuard.
func ListenUDP(port uint16) (*net.UDPConn, error) {
	return net.ListenUDP("udp", &net.UDPAddr{Port: int(port)})
}
