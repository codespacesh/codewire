package fleet

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/codespacesh/codewire/internal/config"
	"github.com/codespacesh/codewire/internal/protocol"
	"github.com/codespacesh/codewire/internal/session"
)

// ConnectNATS connects to a NATS server using the provided configuration.
// It supports both token-based and credentials-file-based authentication.
func ConnectNATS(cfg *config.NatsConfig) (*nats.Conn, error) {
	opts := make([]nats.Option, 0, 2)

	if cfg.Token != nil && *cfg.Token != "" {
		opts = append(opts, nats.Token(*cfg.Token))
	}
	if cfg.CredsFile != nil && *cfg.CredsFile != "" {
		opts = append(opts, nats.UserCredentials(*cfg.CredsFile))
	}

	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %s: %w", cfg.URL, err)
	}
	return nc, nil
}

// RunFleet starts the fleet integration for a node. It subscribes to the
// fleet discovery subject and per-node direct subjects, starts a heartbeat
// goroutine, and blocks forever.
func RunFleet(nc *nats.Conn, nodeCfg *config.NodeConfig, manager *session.SessionManager) error {
	startTime := time.Now()

	// Subscribe to fleet-wide discovery requests.
	_, err := nc.Subscribe("cw.fleet.discover", func(msg *nats.Msg) {
		handleMessage(msg, nodeCfg, manager, startTime)
	})
	if err != nil {
		return fmt.Errorf("subscribing to cw.fleet.discover: %w", err)
	}
	slog.Info("subscribed to fleet discovery", "subject", "cw.fleet.discover")

	// Subscribe to direct requests addressed to this node.
	directSubject := fmt.Sprintf("cw.%s.>", nodeCfg.Name)
	_, err = nc.Subscribe(directSubject, func(msg *nats.Msg) {
		handleMessage(msg, nodeCfg, manager, startTime)
	})
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", directSubject, err)
	}
	slog.Info("subscribed to direct requests", "subject", directSubject)

	// Start the heartbeat goroutine.
	go heartbeatLoop(nc, nodeCfg, manager, startTime)

	// Publish an initial heartbeat immediately so peers see us right away.
	publishHeartbeat(nc, nodeCfg, manager, startTime)

	slog.Info("fleet integration running", "node", nodeCfg.Name)

	// Block forever.
	select {}
}

// handleMessage processes a single inbound NATS message (either discovery or
// direct), dispatches to the appropriate session manager method, and replies
// if the message has a reply subject.
func handleMessage(msg *nats.Msg, nodeCfg *config.NodeConfig, manager *session.SessionManager, startTime time.Time) {
	var req protocol.FleetRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("failed to parse fleet request", "err", err, "subject", msg.Subject)
		return
	}

	var resp protocol.FleetResponse

	switch req.Type {
	case "Discover", "ListSessions":
		sessions := manager.List()
		uptimeSecs := uint64(time.Since(startTime).Seconds())
		nodeInfo := protocol.NodeInfo{
			Name:        nodeCfg.Name,
			ExternalURL: nodeCfg.ExternalURL,
			Sessions:    sessions,
			UptimeSecs:  uptimeSecs,
		}
		resp = protocol.NewFleetResponseNodeInfo(nodeInfo)

	case "Launch":
		if len(req.Command) == 0 {
			resp = protocol.FleetResponse{
				Type:    "Error",
				Node:    nodeCfg.Name,
				Message: "command must not be empty",
			}
		} else {
			workingDir := req.WorkingDir
			if workingDir == "" {
				workingDir = "/"
			}
			id, err := manager.Launch(req.Command, workingDir)
			if err != nil {
				resp = protocol.FleetResponse{
					Type:    "Error",
					Node:    nodeCfg.Name,
					Message: err.Error(),
				}
			} else {
				resp = protocol.FleetResponse{
					Type: "Launched",
					Node: nodeCfg.Name,
					ID:   &id,
				}
			}
		}

	case "Kill":
		if req.ID == nil {
			resp = protocol.FleetResponse{
				Type:    "Error",
				Node:    nodeCfg.Name,
				Message: "session id is required",
			}
		} else {
			err := manager.Kill(*req.ID)
			if err != nil {
				resp = protocol.FleetResponse{
					Type:    "Error",
					Node:    nodeCfg.Name,
					Message: err.Error(),
				}
			} else {
				resp = protocol.FleetResponse{
					Type: "Killed",
					Node: nodeCfg.Name,
					ID:   req.ID,
				}
			}
		}

	case "GetStatus":
		if req.ID == nil {
			resp = protocol.FleetResponse{
				Type:    "Error",
				Node:    nodeCfg.Name,
				Message: "session id is required",
			}
		} else {
			info, outputSize, err := manager.GetStatus(*req.ID)
			if err != nil {
				resp = protocol.FleetResponse{
					Type:    "Error",
					Node:    nodeCfg.Name,
					Message: err.Error(),
				}
			} else {
				resp = protocol.FleetResponse{
					Type:       "SessionStatus",
					Node:       nodeCfg.Name,
					Info:       &info,
					OutputSize: &outputSize,
				}
			}
		}

	case "SendInput":
		if req.ID == nil {
			resp = protocol.FleetResponse{
				Type:    "Error",
				Node:    nodeCfg.Name,
				Message: "session id is required",
			}
		} else {
			n, err := manager.SendInput(*req.ID, req.Data)
			if err != nil {
				resp = protocol.FleetResponse{
					Type:    "Error",
					Node:    nodeCfg.Name,
					Message: err.Error(),
				}
			} else {
				bytes := uint(n)
				resp = protocol.FleetResponse{
					Type:  "InputSent",
					Node:  nodeCfg.Name,
					Bytes: &bytes,
				}
			}
		}

	default:
		resp = protocol.FleetResponse{
			Type:    "Error",
			Node:    nodeCfg.Name,
			Message: fmt.Sprintf("unknown request type: %s", req.Type),
		}
	}

	// Reply only if the message has a reply subject (request-reply pattern).
	if msg.Reply != "" {
		data, err := json.Marshal(&resp)
		if err != nil {
			slog.Error("failed to marshal fleet response", "err", err)
			return
		}
		if err := msg.Respond(data); err != nil {
			slog.Error("failed to send fleet response", "err", err, "reply", msg.Reply)
		}
	}
}

// heartbeatLoop publishes a NodeInfo heartbeat every 30 seconds.
func heartbeatLoop(nc *nats.Conn, nodeCfg *config.NodeConfig, manager *session.SessionManager, startTime time.Time) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		publishHeartbeat(nc, nodeCfg, manager, startTime)
	}
}

// publishHeartbeat publishes a single heartbeat message to cw.fleet.heartbeat.
func publishHeartbeat(nc *nats.Conn, nodeCfg *config.NodeConfig, manager *session.SessionManager, startTime time.Time) {
	sessions := manager.List()
	uptimeSecs := uint64(time.Since(startTime).Seconds())

	info := protocol.NodeInfo{
		Name:        nodeCfg.Name,
		ExternalURL: nodeCfg.ExternalURL,
		Sessions:    sessions,
		UptimeSecs:  uptimeSecs,
	}

	data, err := json.Marshal(&info)
	if err != nil {
		slog.Error("failed to marshal heartbeat", "err", err)
		return
	}

	if err := nc.Publish("cw.fleet.heartbeat", data); err != nil {
		slog.Error("failed to publish heartbeat", "err", err)
	}
}
