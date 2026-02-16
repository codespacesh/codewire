package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/codespacesh/codewire/internal/client"
	"github.com/codespacesh/codewire/internal/config"
	"github.com/codespacesh/codewire/internal/fleet"
	"github.com/codespacesh/codewire/internal/mcp"
	"github.com/codespacesh/codewire/internal/node"
)

var (
	serverFlag string
	tokenFlag  string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "cw",
		Short: "Persistent process server for AI coding agents",
	}
	rootCmd.PersistentFlags().StringVarP(&serverFlag, "server", "s", "", "Connect to a remote server (name from servers.toml or ws://host:port)")
	rootCmd.PersistentFlags().StringVar(&tokenFlag, "token", "", "Auth token for remote server")

	rootCmd.AddCommand(
		nodeCmd(),
		stopCmd(),
		runCmd(),
		listCmd(),
		attachCmd(),
		killCmd(),
		logsCmd(),
		sendCmd(),
		watchCmd(),
		statusCmd(),
		mcpServerCmd(),
		fleetCmd(),
		serverCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// nodeCmd (aliases: daemon, start)
// ---------------------------------------------------------------------------

func nodeCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "node",
		Aliases: []string{"daemon", "start"},
		Short:   "Start the codewire node daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := dataDir()
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("creating data dir: %w", err)
			}

			n, err := node.NewNode(dir)
			if err != nil {
				return fmt.Errorf("initializing node: %w", err)
			}
			defer n.Cleanup()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
			go func() {
				<-sigCh
				fmt.Fprintln(os.Stderr, "[cw] shutting down...")
				cancel()
			}()

			return n.Run(ctx)
		},
	}
}

// ---------------------------------------------------------------------------
// stopCmd
// ---------------------------------------------------------------------------

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running node daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidPath := filepath.Join(dataDir(), "codewire.pid")
			data, err := os.ReadFile(pidPath)
			if err != nil {
				return fmt.Errorf("reading pid file: %w (is the node running?)", err)
			}

			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				return fmt.Errorf("invalid pid file: %w", err)
			}

			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				if err == syscall.ESRCH {
					// Process already gone — clean up stale files.
					_ = os.Remove(pidPath)
					fmt.Fprintln(os.Stderr, "[cw] node already stopped (stale pid file removed)")
					return nil
				}
				return fmt.Errorf("sending SIGTERM to pid %d: %w", pid, err)
			}

			fmt.Fprintf(os.Stderr, "[cw] sent SIGTERM to node (pid %d)\n", pid)
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// runCmd (alias: launch)
// ---------------------------------------------------------------------------

func runCmd() *cobra.Command {
	var workDir string

	cmd := &cobra.Command{
		Use:     "run [-- command...]",
		Aliases: []string{"launch"},
		Short:   "Launch a new session",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveTarget()
			if err != nil {
				return err
			}

			if target.IsLocal() {
				if err := ensureNode(); err != nil {
					return err
				}
			}

			if len(args) == 0 {
				return fmt.Errorf("command required after --")
			}

			return client.Run(target, args, workDir)
		},
	}

	cmd.Flags().StringVarP(&workDir, "dir", "d", "", "Working directory for the session")

	return cmd
}

// ---------------------------------------------------------------------------
// listCmd
// ---------------------------------------------------------------------------

func listCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveTarget()
			if err != nil {
				return err
			}

			if target.IsLocal() {
				if err := ensureNode(); err != nil {
					return err
				}
			}

			return client.List(target, jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")

	return cmd
}

// ---------------------------------------------------------------------------
// attachCmd
// ---------------------------------------------------------------------------

func attachCmd() *cobra.Command {
	var noHistory bool

	cmd := &cobra.Command{
		Use:   "attach [session-id]",
		Short: "Attach to a session's PTY",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveTarget()
			if err != nil {
				return err
			}

			if target.IsLocal() {
				if err := ensureNode(); err != nil {
					return err
				}
			}

			var id *uint32
			if len(args) > 0 {
				parsed, err := strconv.ParseUint(args[0], 10, 32)
				if err != nil {
					return fmt.Errorf("invalid session id: %w", err)
				}
				v := uint32(parsed)
				id = &v
			}

			return client.Attach(target, id, noHistory)
		},
	}

	cmd.Flags().BoolVar(&noHistory, "no-history", false, "Do not replay session history")

	return cmd
}

// ---------------------------------------------------------------------------
// killCmd
// ---------------------------------------------------------------------------

func killCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "kill [session-id]",
		Short: "Kill a session or all sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveTarget()
			if err != nil {
				return err
			}

			if target.IsLocal() {
				if err := ensureNode(); err != nil {
					return err
				}
			}

			if all {
				return client.KillAll(target)
			}

			if len(args) == 0 {
				return fmt.Errorf("session id required (or use --all)")
			}

			parsed, err := strconv.ParseUint(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid session id: %w", err)
			}

			return client.Kill(target, uint32(parsed))
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "Kill all sessions")

	return cmd
}

// ---------------------------------------------------------------------------
// logsCmd
// ---------------------------------------------------------------------------

func logsCmd() *cobra.Command {
	var (
		follow bool
		tail   int
	)

	cmd := &cobra.Command{
		Use:   "logs <session-id>",
		Short: "View session output logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveTarget()
			if err != nil {
				return err
			}

			if target.IsLocal() {
				if err := ensureNode(); err != nil {
					return err
				}
			}

			parsed, err := strconv.ParseUint(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid session id: %w", err)
			}

			var tailPtr *int
			if cmd.Flags().Changed("tail") {
				tailPtr = &tail
			}

			return client.Logs(target, uint32(parsed), follow, tailPtr)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().IntVarP(&tail, "tail", "t", 0, "Number of lines to show from end")

	return cmd
}

// ---------------------------------------------------------------------------
// sendCmd
// ---------------------------------------------------------------------------

func sendCmd() *cobra.Command {
	var (
		useStdin  bool
		file      string
		noNewline bool
	)

	cmd := &cobra.Command{
		Use:   "send <session-id> [input]",
		Short: "Send input to a session",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveTarget()
			if err != nil {
				return err
			}

			if target.IsLocal() {
				if err := ensureNode(); err != nil {
					return err
				}
			}

			parsed, err := strconv.ParseUint(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid session id: %w", err)
			}

			var input *string
			if len(args) > 1 {
				input = &args[1]
			}

			var filePtr *string
			if cmd.Flags().Changed("file") {
				filePtr = &file
			}

			return client.SendInput(target, uint32(parsed), input, useStdin, filePtr, noNewline)
		},
	}

	cmd.Flags().BoolVar(&useStdin, "stdin", false, "Read input from stdin")
	cmd.Flags().StringVarP(&file, "file", "f", "", "Read input from file")
	cmd.Flags().BoolVarP(&noNewline, "no-newline", "n", false, "Do not append newline")

	return cmd
}

// ---------------------------------------------------------------------------
// watchCmd
// ---------------------------------------------------------------------------

func watchCmd() *cobra.Command {
	var (
		tail      int
		noHistory bool
		timeout   uint64
	)

	cmd := &cobra.Command{
		Use:   "watch <session-id>",
		Short: "Watch session output in real-time",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveTarget()
			if err != nil {
				return err
			}

			if target.IsLocal() {
				if err := ensureNode(); err != nil {
					return err
				}
			}

			parsed, err := strconv.ParseUint(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid session id: %w", err)
			}

			var tailPtr *int
			if cmd.Flags().Changed("tail") {
				tailPtr = &tail
			}

			var timeoutPtr *uint64
			if cmd.Flags().Changed("timeout") {
				timeoutPtr = &timeout
			}

			return client.WatchSession(target, uint32(parsed), tailPtr, noHistory, timeoutPtr)
		},
	}

	cmd.Flags().IntVarP(&tail, "tail", "t", 0, "Number of lines to show from end")
	cmd.Flags().BoolVar(&noHistory, "no-history", false, "Do not replay session history")
	cmd.Flags().Uint64Var(&timeout, "timeout", 0, "Timeout in seconds")

	return cmd
}

// ---------------------------------------------------------------------------
// statusCmd
// ---------------------------------------------------------------------------

func statusCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "status <session-id>",
		Short: "Get detailed status for a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveTarget()
			if err != nil {
				return err
			}

			if target.IsLocal() {
				if err := ensureNode(); err != nil {
					return err
				}
			}

			parsed, err := strconv.ParseUint(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid session id: %w", err)
			}

			return client.GetStatus(target, uint32(parsed), jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")

	return cmd
}

// ---------------------------------------------------------------------------
// mcpServerCmd
// ---------------------------------------------------------------------------

func mcpServerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp-server",
		Short: "Run the MCP (Model Context Protocol) server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureNode(); err != nil {
				return err
			}
			return mcp.RunMCPServer(dataDir())
		},
	}
}

// ---------------------------------------------------------------------------
// fleetCmd — subcommand group
// ---------------------------------------------------------------------------

func fleetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Fleet management commands",
	}

	cmd.AddCommand(
		fleetListCmd(),
		fleetAttachCmd(),
		fleetLaunchCmd(),
		fleetKillCmd(),
		fleetSendCmd(),
	)

	return cmd
}

func fleetListCmd() *cobra.Command {
	var (
		timeout    uint64
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Discover and list all fleet nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			natsCfg, err := loadNatsConfig()
			if err != nil {
				return err
			}
			return fleet.HandleFleetList(natsCfg, timeout, jsonOutput)
		},
	}

	cmd.Flags().Uint64Var(&timeout, "timeout", 2, "Discovery timeout in seconds")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")

	return cmd
}

func fleetAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <node:session-id>",
		Short: "Attach to a session on a fleet node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			natsCfg, err := loadNatsConfig()
			if err != nil {
				return err
			}

			info, err := fleet.HandleFleetAttach(natsCfg, dataDir(), args[0])
			if err != nil {
				return err
			}

			target := &client.Target{
				URL:   info.URL,
				Token: info.Token,
			}
			return client.Attach(target, &info.SessionID, false)
		},
	}
}

func fleetLaunchCmd() *cobra.Command {
	var (
		onNode string
		dir    string
	)

	cmd := &cobra.Command{
		Use:   "launch [-- command...]",
		Short: "Launch a session on a fleet node",
		RunE: func(cmd *cobra.Command, args []string) error {
			if onNode == "" {
				return fmt.Errorf("--on <node> is required")
			}
			if len(args) == 0 {
				return fmt.Errorf("command required after --")
			}

			natsCfg, err := loadNatsConfig()
			if err != nil {
				return err
			}

			return fleet.HandleFleetLaunch(natsCfg, onNode, args, dir)
		},
	}

	cmd.Flags().StringVar(&onNode, "on", "", "Target node name")
	cmd.Flags().StringVar(&dir, "dir", "", "Working directory on the remote node")

	return cmd
}

func fleetKillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill <node:session-id>",
		Short: "Kill a session on a fleet node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			natsCfg, err := loadNatsConfig()
			if err != nil {
				return err
			}
			return fleet.HandleFleetKill(natsCfg, args[0])
		},
	}
}

func fleetSendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "send <node:session-id> <text>",
		Short: "Send input to a session on a fleet node",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			natsCfg, err := loadNatsConfig()
			if err != nil {
				return err
			}
			data := []byte(args[1] + "\n")
			return fleet.HandleFleetSendInput(natsCfg, args[0], data)
		},
	}
}

// ---------------------------------------------------------------------------
// serverCmd — subcommand group
// ---------------------------------------------------------------------------

func serverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Manage saved server connections",
	}

	cmd.AddCommand(
		serverAddCmd(),
		serverRemoveCmd(),
		serverListCmd(),
	)

	return cmd
}

func serverAddCmd() *cobra.Command {
	var token string

	cmd := &cobra.Command{
		Use:   "add <name> <url>",
		Short: "Add a server connection",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			url := args[1]

			if token == "" {
				return fmt.Errorf("--token is required")
			}

			dir := dataDir()
			servers, err := config.LoadServersConfig(dir)
			if err != nil {
				return err
			}

			servers.Servers[name] = config.ServerEntry{
				URL:   url,
				Token: token,
			}

			if err := servers.Save(dir); err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "Server %q added\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&token, "token", "", "Auth token for the server")

	return cmd
}

func serverRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a saved server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			dir := dataDir()

			servers, err := config.LoadServersConfig(dir)
			if err != nil {
				return err
			}

			if _, ok := servers.Servers[name]; !ok {
				return fmt.Errorf("server %q not found", name)
			}

			delete(servers.Servers, name)

			if err := servers.Save(dir); err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "Server %q removed\n", name)
			return nil
		},
	}
}

func serverListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved servers",
		RunE: func(cmd *cobra.Command, args []string) error {
			servers, err := config.LoadServersConfig(dataDir())
			if err != nil {
				return err
			}

			if len(servers.Servers) == 0 {
				fmt.Println("No saved servers")
				return nil
			}

			fmt.Printf("%-20s %s\n", "NAME", "URL")
			for name, entry := range servers.Servers {
				fmt.Printf("%-20s %s\n", name, entry.URL)
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func dataDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		fmt.Fprintln(os.Stderr, "[cw] ERROR: $HOME environment variable is not set")
		fmt.Fprintln(os.Stderr, "[cw] WARNING: Using insecure fallback directory /tmp/.codewire")
		return "/tmp/.codewire"
	}
	return filepath.Join(home, ".codewire")
}

func resolveTarget() (*client.Target, error) {
	dir := dataDir()

	if serverFlag == "" {
		return &client.Target{Local: dir}, nil
	}

	// Check servers.toml for a named entry.
	servers, err := config.LoadServersConfig(dir)
	if err == nil {
		if entry, ok := servers.Servers[serverFlag]; ok {
			token := tokenFlag
			if token == "" {
				token = entry.Token
			}
			return &client.Target{URL: entry.URL, Token: token}, nil
		}
	}

	// Treat serverFlag as a direct URL.
	if tokenFlag == "" {
		return nil, fmt.Errorf("--token required for ad-hoc server")
	}

	url := serverFlag
	if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
		url = "ws://" + url
	}

	return &client.Target{URL: url, Token: tokenFlag}, nil
}

func ensureNode() error {
	dir := dataDir()
	sock := filepath.Join(dir, "codewire.sock")

	// Check if node is already running.
	if conn, err := net.Dial("unix", sock); err == nil {
		conn.Close()
		return nil
	}

	// Clean stale socket.
	_ = os.Remove(sock)
	_ = os.MkdirAll(dir, 0o755)

	// Spawn `cw node` in background.
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "node")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawning node: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[cw] node started (pid %d)\n", cmd.Process.Pid)

	// Wait for socket to become available.
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if conn, err := net.Dial("unix", sock); err == nil {
			conn.Close()
			return nil
		}
	}

	return fmt.Errorf("node failed to start (socket not available after 5s)")
}

func loadNatsConfig() (*config.NatsConfig, error) {
	cfg, err := config.LoadConfig(dataDir())
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if cfg.Nats == nil {
		return nil, fmt.Errorf("NATS not configured (set CODEWIRE_NATS_URL or add [nats] to config.toml)")
	}
	return cfg.Nats, nil
}
