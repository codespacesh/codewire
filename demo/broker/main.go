package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Namespace     string
	DemoImage     string
	PoolSize      int // number of warm pods to keep ready
	PodMaxAge     int // max pod lifetime in seconds
	ListenAddr    string
	AllowedOrigin string
}

func configFromEnv() Config {
	return Config{
		Namespace:     envOr("DEMO_NAMESPACE", "codewire-demo"),
		DemoImage:     envOr("DEMO_IMAGE", "ghcr.io/codewiresh/codewire-demo:latest"),
		PoolSize:      envInt("DEMO_POOL_SIZE", 3),
		PodMaxAge:     envInt("DEMO_POD_MAX_AGE", 300),
		ListenAddr:    envOr("DEMO_LISTEN", ":8080"),
		AllowedOrigin: envOr("DEMO_ALLOWED_ORIGIN", "https://codewire.sh"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return def
}

func main() {
	cfg := configFromEnv()

	pool, err := NewPool(cfg)
	if err != nil {
		log.Fatalf("failed to create pool: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool.Start(ctx)

	mux := http.NewServeMux()

	// Health endpoint
	mux.HandleFunc("GET /api/health", corsMiddleware(cfg.AllowedOrigin, pool.HandleHealth))

	// Session assignment — returns WebSocket URL for a warm pod
	mux.HandleFunc("GET /api/session", corsMiddleware(cfg.AllowedOrigin, pool.HandleSession))

	// WebSocket proxy to demo pod: /ws/{pod-name}?token={token}
	mux.HandleFunc("GET /ws/", corsMiddleware(cfg.AllowedOrigin, pool.HandleWS))

	// CORS preflight
	mux.HandleFunc("OPTIONS /", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if corsAllowed(cfg.AllowedOrigin, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "3600")
		}
		w.WriteHeader(http.StatusNoContent)
	})

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-sigCh
		log.Println("shutting down...")
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		server.Shutdown(shutdownCtx)
	}()

	log.Printf("demo broker listening on %s (pool=%d, maxAge=%ds)", cfg.ListenAddr, cfg.PoolSize, cfg.PodMaxAge)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

func corsAllowed(allowedOrigin, origin string) bool {
	if origin == "" {
		return false
	}
	if allowedOrigin == "*" {
		return true
	}
	for _, o := range strings.Split(allowedOrigin, ",") {
		if strings.TrimSpace(o) == origin {
			return true
		}
	}
	return false
}

func corsMiddleware(allowedOrigin string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if corsAllowed(allowedOrigin, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		next(w, r)
	}
}
