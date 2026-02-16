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
	// AuthMode controls registration: "token", "none".
	AuthMode string
	// AuthToken is the shared secret when AuthMode is "token".
	AuthToken string
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

	// Rate limiter: 5 device registrations per IP per minute.
	deviceRL := newRateLimiter(5, time.Minute)

	// Build HTTP mux: our API routes + wgtunnel's reverse proxy router.
	mux := http.NewServeMux()

	// Device authorization endpoints.
	mux.HandleFunc("POST /api/v1/device/authorize", rateLimitMiddleware(deviceRL, deviceAuthorizeHandler(st, opts, cfg)))
	mux.HandleFunc("GET /api/v1/device/poll/{code}", devicePollHandler(st))
	mux.HandleFunc("GET /auth/device", devicePageHandler())
	mux.HandleFunc("POST /auth/device/confirm", deviceConfirmHandler(st))

	// Node discovery.
	mux.HandleFunc("GET /api/v1/nodes", nodesListHandler(st))

	// KV API.
	mux.HandleFunc("PUT /api/v1/kv/{namespace}/{key}", kvSetHandler(st, cfg))
	mux.HandleFunc("GET /api/v1/kv/{namespace}/{key}", kvGetHandler(st, cfg))
	mux.HandleFunc("DELETE /api/v1/kv/{namespace}/{key}", kvDeleteHandler(st, cfg))
	mux.HandleFunc("GET /api/v1/kv/{namespace}", kvListHandler(st, cfg))

	// Health check.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Fall through to wgtunnel router for tunnel reverse-proxy.
	tunnelRouter := api.Router()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		tunnelRouter.ServeHTTP(w, r)
	})

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
				st.NodeRegister(r.Context(), store.NodeRecord{
					Name:         dc.NodeName,
					PublicKey:    dc.PublicKey,
					TunnelURL:    tunnelURL,
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
				TunnelURL: n.TunnelURL,
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
