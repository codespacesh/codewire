package tunnel

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/coder/wgtunnel/tunnelsdk"
)

// NodeTunnel manages a WireGuard tunnel connection from a node to a relay.
type NodeTunnel struct {
	relayURL  string
	key       tunnelsdk.Key
	tunnel    *tunnelsdk.Tunnel
	cancelReg context.CancelFunc
}

// Connect establishes a WireGuard tunnel to the relay and returns the tunnel
// URL and a net.Listener that accepts connections proxied through the relay.
func Connect(ctx context.Context, relayURL, dataDir string) (*NodeTunnel, error) {
	key, err := LoadOrGenerateKey(dataDir)
	if err != nil {
		return nil, fmt.Errorf("loading key: %w", err)
	}

	relayParsed, err := url.Parse(relayURL)
	if err != nil {
		return nil, fmt.Errorf("parsing relay URL: %w", err)
	}

	client := tunnelsdk.New(relayParsed)

	tun, err := client.LaunchTunnel(ctx, tunnelsdk.TunnelConfig{
		PrivateKey: key,
	})
	if err != nil {
		return nil, fmt.Errorf("launching tunnel: %w", err)
	}

	// Start periodic re-registration in the background.
	regCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-regCtx.Done():
				return
			case <-ticker.C:
				// Re-register to keep the tunnel alive.
				client.LaunchTunnel(regCtx, tunnelsdk.TunnelConfig{
					PrivateKey: key,
				})
			}
		}
	}()

	return &NodeTunnel{
		relayURL:  relayURL,
		key:       key,
		tunnel:    tun,
		cancelReg: cancel,
	}, nil
}

// URL returns the public tunnel URL for this node.
func (t *NodeTunnel) URL() string {
	if t.tunnel != nil && t.tunnel.URL != nil {
		return t.tunnel.URL.String()
	}
	return ""
}

// Listener returns the net.Listener that accepts connections through the tunnel.
func (t *NodeTunnel) Listener() net.Listener {
	if t.tunnel != nil {
		return t.tunnel.Listener
	}
	return nil
}

// Close tears down the tunnel.
func (t *NodeTunnel) Close() error {
	if t.cancelReg != nil {
		t.cancelReg()
	}
	if t.tunnel != nil {
		return t.tunnel.Close()
	}
	return nil
}
