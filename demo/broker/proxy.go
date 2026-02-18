package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // CORS handled at middleware level
	},
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// HandleWS proxies a WebSocket connection to a specific demo pod's ttyd.
// Route: /ws/{pod-name}?token={token}
// Input from the browser is silently dropped — this is a read-only stream.
func (p *Pool) HandleWS(w http.ResponseWriter, r *http.Request) {
	// Extract pod name from path: /ws/{pod-name}
	path := strings.TrimPrefix(r.URL.Path, "/ws/")
	podName := strings.TrimRight(path, "/")
	token := r.URL.Query().Get("token")

	if podName == "" || token == "" {
		http.Error(w, `{"error":"missing pod or token"}`, http.StatusBadRequest)
		return
	}

	pod, ok := p.Lookup(token)
	if !ok || pod.Name != podName {
		http.Error(w, `{"error":"invalid session"}`, http.StatusForbidden)
		return
	}

	if pod.IP == "" {
		http.Error(w, `{"error":"pod not ready"}`, http.StatusServiceUnavailable)
		return
	}

	// Connect to ttyd in the demo pod
	ttydURL := fmt.Sprintf("ws://%s:7681/ws", pod.IP)
	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
		},
	}

	backendConn, _, err := dialer.Dial(ttydURL, nil)
	if err != nil {
		log.Printf("dial ttyd on %s (%s): %v", podName, pod.IP, err)
		http.Error(w, `{"error":"demo not available"}`, http.StatusBadGateway)
		return
	}
	defer backendConn.Close()

	// Upgrade browser connection
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade client: %v", err)
		return
	}
	defer clientConn.Close()

	log.Printf("viewer connected to pod %s from %s", podName, r.RemoteAddr)

	done := make(chan struct{}, 2)

	// Backend (ttyd) -> client (browser): forward all output
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msgType, reader, err := backendConn.NextReader()
			if err != nil {
				return
			}
			writer, err := clientConn.NextWriter(msgType)
			if err != nil {
				return
			}
			if _, err := io.Copy(writer, reader); err != nil {
				return
			}
			if err := writer.Close(); err != nil {
				return
			}
		}
	}()

	// Client (browser) -> drain: silently consume any input (read-only mode)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			if _, _, err := clientConn.NextReader(); err != nil {
				return
			}
			// Input silently dropped — ttyd is in read-only mode (-R)
		}
	}()

	<-done
	log.Printf("viewer disconnected from pod %s (%s)", podName, r.RemoteAddr)
}
