package fleet

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/codespacesh/codewire/internal/config"
	"github.com/codespacesh/codewire/internal/protocol"
)

// DiscoverFleet performs scatter-gather discovery: it publishes a Discover
// request to all fleet nodes and collects NodeInfo responses until the
// timeout expires. Returns all nodes that responded.
func DiscoverFleet(nc *nats.Conn, timeout time.Duration) ([]protocol.NodeInfo, error) {
	inbox := nc.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("subscribing to inbox: %w", err)
	}
	defer sub.Unsubscribe()

	req := protocol.FleetRequest{Type: "Discover"}
	payload, err := json.Marshal(&req)
	if err != nil {
		return nil, fmt.Errorf("marshalling discover request: %w", err)
	}

	if err := nc.PublishRequest("cw.fleet.discover", inbox, payload); err != nil {
		return nil, fmt.Errorf("publishing discover request: %w", err)
	}

	deadline := time.Now().Add(timeout)
	var nodes []protocol.NodeInfo

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		msg, err := sub.NextMsg(remaining)
		if err != nil {
			// Timeout or subscription closed — return what we have.
			break
		}

		var resp protocol.FleetResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			slog.Warn("failed to parse discover response", "err", err)
			continue
		}

		if resp.Type != "NodeInfo" {
			continue
		}

		// Reconstruct NodeInfo from the flattened FleetResponse fields.
		var sessions []protocol.SessionInfo
		if resp.Sessions != nil {
			sessions = *resp.Sessions
		}
		var uptimeSecs uint64
		if resp.UptimeSecs != nil {
			uptimeSecs = *resp.UptimeSecs
		}

		node := protocol.NodeInfo{
			Name:        resp.Name,
			ExternalURL: resp.ExternalURL,
			Sessions:    sessions,
			UptimeSecs:  uptimeSecs,
		}
		nodes = append(nodes, node)
	}

	return nodes, nil
}

// FleetRequest sends a request to a specific node via NATS request-reply and
// returns the parsed response. The subject is determined by the request type.
func FleetRequest(nc *nats.Conn, nodeName string, req *protocol.FleetRequest, timeout time.Duration) (*protocol.FleetResponse, error) {
	subject := fleetSubject(nodeName, req.Type)

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshalling fleet request: %w", err)
	}

	msg, err := nc.Request(subject, payload, timeout)
	if err != nil {
		return nil, fmt.Errorf("fleet request to %s (subject %s): %w", nodeName, subject, err)
	}

	var resp protocol.FleetResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return nil, fmt.Errorf("parsing fleet response: %w", err)
	}

	return &resp, nil
}

// fleetSubject returns the NATS subject for a given node and request type.
func fleetSubject(nodeName, reqType string) string {
	switch reqType {
	case "ListSessions":
		return fmt.Sprintf("cw.%s.list", nodeName)
	case "Launch":
		return fmt.Sprintf("cw.%s.launch", nodeName)
	case "Kill":
		return fmt.Sprintf("cw.%s.kill", nodeName)
	case "GetStatus":
		return fmt.Sprintf("cw.%s.status", nodeName)
	case "SendInput":
		return fmt.Sprintf("cw.%s.send", nodeName)
	case "Discover":
		return "cw.fleet.discover"
	default:
		return fmt.Sprintf("cw.%s.%s", nodeName, strings.ToLower(reqType))
	}
}

// ParseFleetTarget parses a "node:session_id" string into its components.
func ParseFleetTarget(target string) (nodeName string, sessionID uint32, err error) {
	parts := strings.SplitN(target, ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("invalid fleet target %q: expected format node:session_id", target)
	}

	nodeName = parts[0]
	if nodeName == "" {
		return "", 0, fmt.Errorf("invalid fleet target %q: node name must not be empty", target)
	}

	id, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("invalid fleet target %q: session id must be a number", target)
	}

	return nodeName, uint32(id), nil
}

// PrintFleetDetail prints fleet discovery results in a human-readable format.
func PrintFleetDetail(nodes []protocol.NodeInfo) {
	if len(nodes) == 0 {
		fmt.Println("No nodes discovered.")
		return
	}

	for _, node := range nodes {
		url := "(no external url)"
		if node.ExternalURL != nil {
			url = *node.ExternalURL
		}
		fmt.Printf("%s  uptime=%s  url=%s\n", node.Name, FormatUptime(node.UptimeSecs), url)

		if len(node.Sessions) == 0 {
			fmt.Println("  (no sessions)")
		}
		for _, s := range node.Sessions {
			fmt.Printf("  [%d] %s (%s) %s\n", s.ID, s.Prompt, s.Status, s.WorkingDir)
		}
	}
}

// FormatUptime formats a duration in seconds into a compact human-readable string.
func FormatUptime(secs uint64) string {
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm", secs/60)
	case secs < 86400:
		return fmt.Sprintf("%dh", secs/3600)
	default:
		return fmt.Sprintf("%dd", secs/86400)
	}
}

// HandleFleetList handles the `cw fleet list` command: connects to NATS,
// discovers all fleet nodes, and prints results.
func HandleFleetList(natsCfg *config.NatsConfig, timeoutSecs uint64, jsonOutput bool) error {
	nc, err := ConnectNATS(natsCfg)
	if err != nil {
		return err
	}
	defer nc.Close()

	timeout := time.Duration(timeoutSecs) * time.Second
	nodes, err := DiscoverFleet(nc, timeout)
	if err != nil {
		return fmt.Errorf("fleet discovery: %w", err)
	}

	if jsonOutput {
		data, err := json.MarshalIndent(nodes, "", "  ")
		if err != nil {
			return fmt.Errorf("marshalling nodes: %w", err)
		}
		fmt.Println(string(data))
	} else {
		PrintFleetDetail(nodes)
	}

	return nil
}

// HandleFleetLaunch handles the `cw fleet launch --on <node> -- <command>` command.
func HandleFleetLaunch(natsCfg *config.NatsConfig, nodeName string, command []string, workingDir string) error {
	nc, err := ConnectNATS(natsCfg)
	if err != nil {
		return err
	}
	defer nc.Close()

	req := &protocol.FleetRequest{
		Type:       "Launch",
		Command:    command,
		WorkingDir: workingDir,
	}

	resp, err := FleetRequest(nc, nodeName, req, 10*time.Second)
	if err != nil {
		return err
	}

	if resp.Type == "Error" {
		return fmt.Errorf("remote error from %s: %s", nodeName, resp.Message)
	}

	if resp.ID != nil {
		fmt.Printf("Launched session %d on %s\n", *resp.ID, nodeName)
	} else {
		fmt.Printf("Launched on %s\n", nodeName)
	}

	return nil
}

// HandleFleetKill handles the `cw fleet kill <node>:<id>` command.
func HandleFleetKill(natsCfg *config.NatsConfig, target string) error {
	nodeName, sessionID, err := ParseFleetTarget(target)
	if err != nil {
		return err
	}

	nc, err := ConnectNATS(natsCfg)
	if err != nil {
		return err
	}
	defer nc.Close()

	req := &protocol.FleetRequest{
		Type: "Kill",
		ID:   &sessionID,
	}

	resp, err := FleetRequest(nc, nodeName, req, 10*time.Second)
	if err != nil {
		return err
	}

	if resp.Type == "Error" {
		return fmt.Errorf("remote error from %s: %s", nodeName, resp.Message)
	}

	fmt.Printf("Killed session %d on %s\n", sessionID, nodeName)
	return nil
}

// HandleFleetSendInput handles the `cw fleet send <node>:<id> <text>` command.
func HandleFleetSendInput(natsCfg *config.NatsConfig, target string, data []byte) error {
	nodeName, sessionID, err := ParseFleetTarget(target)
	if err != nil {
		return err
	}

	nc, err := ConnectNATS(natsCfg)
	if err != nil {
		return err
	}
	defer nc.Close()

	req := &protocol.FleetRequest{
		Type: "SendInput",
		ID:   &sessionID,
		Data: data,
	}

	resp, err := FleetRequest(nc, nodeName, req, 10*time.Second)
	if err != nil {
		return err
	}

	if resp.Type == "Error" {
		return fmt.Errorf("remote error from %s: %s", nodeName, resp.Message)
	}

	if resp.Bytes != nil {
		fmt.Printf("Sent %d bytes to session %d on %s\n", *resp.Bytes, sessionID, nodeName)
	} else {
		fmt.Printf("Input sent to session %d on %s\n", sessionID, nodeName)
	}

	return nil
}

// FleetAttachInfo contains the information needed to attach to a remote
// session via WebSocket. Returned by HandleFleetAttach so the caller
// (typically main.go) can perform the actual attach without creating an
// import cycle with the client package.
type FleetAttachInfo struct {
	URL       string
	Token     string
	SessionID uint32
}

// HandleFleetAttach discovers fleet nodes, finds the target node's external
// URL, looks up the auth token from servers.toml, and returns the connection
// info needed to attach. The caller is responsible for performing the actual
// WebSocket attach using the client package.
func HandleFleetAttach(natsCfg *config.NatsConfig, dataDir string, target string) (*FleetAttachInfo, error) {
	nodeName, sessionID, err := ParseFleetTarget(target)
	if err != nil {
		return nil, err
	}

	nc, err := ConnectNATS(natsCfg)
	if err != nil {
		return nil, err
	}
	defer nc.Close()

	nodes, err := DiscoverFleet(nc, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("fleet discovery: %w", err)
	}

	// Find the target node.
	var targetNode *protocol.NodeInfo
	for i := range nodes {
		if nodes[i].Name == nodeName {
			targetNode = &nodes[i]
			break
		}
	}
	if targetNode == nil {
		return nil, fmt.Errorf("node %q not found in fleet (discovered %d nodes)", nodeName, len(nodes))
	}

	if targetNode.ExternalURL == nil {
		return nil, fmt.Errorf("node %q has no external URL configured", nodeName)
	}

	// Look up the auth token from servers.toml.
	serversCfg, err := config.LoadServersConfig(dataDir)
	if err != nil {
		return nil, fmt.Errorf("loading servers config: %w", err)
	}

	externalURL := *targetNode.ExternalURL
	token := ""

	// Try to find a matching server entry by URL.
	for _, entry := range serversCfg.Servers {
		if entry.URL == externalURL {
			token = entry.Token
			break
		}
	}

	// Also try matching by node name as the key.
	if token == "" {
		if entry, ok := serversCfg.Servers[nodeName]; ok {
			token = entry.Token
		}
	}

	if token == "" {
		slog.Warn("no auth token found for node", "node", nodeName, "url", externalURL)
	}

	return &FleetAttachInfo{
		URL:       externalURL,
		Token:     token,
		SessionID: sessionID,
	}, nil
}
