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
	"github.com/codespacesh/codewire/internal/mcp"
	"github.com/codespacesh/codewire/internal/node"
	"github.com/codespacesh/codewire/internal/tunnel"
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
		nodesCmd(),
		subscribeCmd(),
		waitSessionCmd(),
		kvCmd(),
		serverCmd(),
		relayCmd(),
		setupCmd(),
		msgCmd(),
		inboxCmd(),
		requestCmd(),
		replyCmd(),
		listenCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// nodeCmd (aliases: start)
// ---------------------------------------------------------------------------

func nodeCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "node",
		Aliases: []string{"start"},
		Short:   "Start the codewire node",
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
		Short: "Stop the running node",
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
	var (
		workDir     string
		tags        []string
		name        string
		envVars     []string
		autoApprove bool
		promptFile  string
	)

	cmd := &cobra.Command{
		Use:     "run [name] -- command...",
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

			dash := cmd.ArgsLenAtDash()
			if dash == -1 {
				return fmt.Errorf("command required after --")
			}

			var command []string
			if dash == 1 {
				// cw launch planner -- claude -p "..."
				if name == "" {
					name = args[0]
				}
				command = args[1:]
			} else if dash == 0 {
				// cw launch -- claude -p "..."
				command = args
			} else {
				return fmt.Errorf("expected at most one name before --")
			}

			if len(command) == 0 {
				return fmt.Errorf("command required after --")
			}

			// If --auto-approve, inject --dangerously-skip-permissions after the binary.
			if autoApprove && len(command) > 0 {
				command = append([]string{command[0], "--dangerously-skip-permissions"}, command[1:]...)
			}

			// Default to current working directory if --dir not specified.
			if workDir == "" {
				workDir, _ = os.Getwd()
			}

			var stdinData []byte
			if promptFile != "" {
				var readErr error
				stdinData, readErr = os.ReadFile(promptFile)
				if readErr != nil {
					return fmt.Errorf("reading prompt file: %w", readErr)
				}
			}

			return client.Run(target, command, workDir, name, envVars, stdinData, tags...)
		},
	}

	cmd.Flags().StringVarP(&workDir, "dir", "d", "", "Working directory for the session")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "Tags for the session (can be repeated)")
	cmd.Flags().StringVar(&name, "name", "", "Unique name for the session (alphanumeric + hyphens, 1-32 chars)")
	cmd.Flags().StringArrayVar(&envVars, "env", nil, "Environment variable overrides (KEY=VALUE, can be repeated)")
	cmd.Flags().BoolVar(&autoApprove, "auto-approve", false, "Inject --dangerously-skip-permissions after the command binary")
	cmd.Flags().StringVar(&promptFile, "prompt-file", "", "File whose contents are injected as stdin after launch")

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
		Use:   "attach [session]",
		Short: "Attach to a session's PTY (by ID or name)",
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
				resolved, err := client.ResolveSessionArg(target, args[0])
				if err != nil {
					return err
				}
				id = &resolved
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
	var (
		all  bool
		tags []string
	)

	cmd := &cobra.Command{
		Use:   "kill [session]",
		Short: "Kill a session (by ID or name), all sessions, or sessions by tag",
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

			if len(tags) > 0 {
				return client.KillByTags(target, tags)
			}

			if len(args) == 0 {
				return fmt.Errorf("session id or name required (or use --all / --tag)")
			}

			resolved, err := client.ResolveSessionArg(target, args[0])
			if err != nil {
				return err
			}

			return client.Kill(target, resolved)
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "Kill all sessions")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "Kill sessions matching tag (can be repeated)")

	return cmd
}

// ---------------------------------------------------------------------------
// logsCmd
// ---------------------------------------------------------------------------

func logsCmd() *cobra.Command {
	var (
		follow bool
		tail   int
		raw    bool
	)

	cmd := &cobra.Command{
		Use:   "logs <session>",
		Short: "View session output logs (by ID or name)",
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

			resolved, err := client.ResolveSessionArg(target, args[0])
			if err != nil {
				return err
			}

			var tailPtr *int
			if cmd.Flags().Changed("tail") {
				tailPtr = &tail
			}

			return client.Logs(target, resolved, follow, tailPtr, raw)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().IntVarP(&tail, "tail", "t", 0, "Number of lines to show from end")
	cmd.Flags().BoolVar(&raw, "raw", false, "Output raw log data without stripping ANSI escape codes")

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
		Use:   "send <session> [input]",
		Short: "Send input to a session (by ID or name)",
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

			resolved, err := client.ResolveSessionArg(target, args[0])
			if err != nil {
				return err
			}

			var input *string
			if len(args) > 1 {
				input = &args[1]
			}

			var filePtr *string
			if cmd.Flags().Changed("file") {
				filePtr = &file
			}

			return client.SendInput(target, resolved, input, useStdin, filePtr, noNewline)
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
		tags      []string
	)

	cmd := &cobra.Command{
		Use:   "watch [session]",
		Short: "Watch session output in real-time (by ID, name, or --tag for multi-session)",
		Args:  cobra.MaximumNArgs(1),
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

			// Multi-session watch by tag.
			if len(tags) > 0 {
				var timeoutPtr *uint64
				if cmd.Flags().Changed("timeout") {
					timeoutPtr = &timeout
				}
				return client.WatchMultiByTag(target, tags[0], os.Stdout, timeoutPtr)
			}

			if len(args) == 0 {
				return fmt.Errorf("session id or name required (or use --tag)")
			}

			resolved, err := client.ResolveSessionArg(target, args[0])
			if err != nil {
				return err
			}

			var tailPtr *int
			if cmd.Flags().Changed("tail") {
				tailPtr = &tail
			}

			var timeoutPtr *uint64
			if cmd.Flags().Changed("timeout") {
				timeoutPtr = &timeout
			}

			return client.WatchSession(target, resolved, tailPtr, noHistory, timeoutPtr)
		},
	}

	cmd.Flags().IntVarP(&tail, "tail", "t", 0, "Number of lines to show from end")
	cmd.Flags().BoolVar(&noHistory, "no-history", false, "Do not replay session history")
	cmd.Flags().Uint64Var(&timeout, "timeout", 0, "Timeout in seconds")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "Watch all sessions matching tag (multiplexed)")

	return cmd
}

// ---------------------------------------------------------------------------
// statusCmd
// ---------------------------------------------------------------------------

func statusCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "status <session>",
		Short: "Get detailed status for a session (by ID or name)",
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

			resolved, err := client.ResolveSessionArg(target, args[0])
			if err != nil {
				return err
			}

			return client.GetStatus(target, resolved, jsonOutput)
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
// nodesCmd — list nodes from relay
// ---------------------------------------------------------------------------

func nodesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "nodes",
		Short: "List registered nodes from the relay",
		RunE: func(cmd *cobra.Command, args []string) error {
			relayURL, err := resolveRelayURL()
			if err != nil {
				return err
			}
			return client.Nodes(relayURL)
		},
	}
}

// ---------------------------------------------------------------------------
// subscribeCmd — subscribe to session events
// ---------------------------------------------------------------------------

func subscribeCmd() *cobra.Command {
	var (
		tags       []string
		eventTypes []string
		sessionID  uint64
	)

	cmd := &cobra.Command{
		Use:   "subscribe",
		Short: "Subscribe to session events",
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

			var sid *uint32
			if cmd.Flags().Changed("session") {
				v := uint32(sessionID)
				sid = &v
			}

			return client.SubscribeEvents(target, sid, tags, eventTypes)
		},
	}

	cmd.Flags().StringSliceVar(&tags, "tag", nil, "Filter by tag (can be repeated)")
	cmd.Flags().StringSliceVar(&eventTypes, "event", nil, "Filter by event type (can be repeated)")
	cmd.Flags().Uint64Var(&sessionID, "session", 0, "Filter by session ID")

	return cmd
}

// ---------------------------------------------------------------------------
// waitSessionCmd — wait for session(s) to complete
// ---------------------------------------------------------------------------

func waitSessionCmd() *cobra.Command {
	var (
		tags      []string
		condition string
		timeout   uint64
	)

	cmd := &cobra.Command{
		Use:   "wait [session]",
		Short: "Wait for session(s) to complete (by ID or name)",
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

			var sid *uint32
			if len(args) > 0 {
				resolved, err := client.ResolveSessionArg(target, args[0])
				if err != nil {
					return err
				}
				sid = &resolved
			}

			var timeoutPtr *uint64
			if cmd.Flags().Changed("timeout") {
				timeoutPtr = &timeout
			}

			return client.WaitForSession(target, sid, tags, condition, timeoutPtr)
		},
	}

	cmd.Flags().StringSliceVar(&tags, "tag", nil, "Wait for sessions matching tag (can be repeated)")
	cmd.Flags().StringVar(&condition, "condition", "all", "Wait condition: all or any")
	cmd.Flags().Uint64Var(&timeout, "timeout", 0, "Timeout in seconds")

	return cmd
}

// ---------------------------------------------------------------------------
// kvCmd — key-value store subcommand group
// ---------------------------------------------------------------------------

func kvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kv",
		Short: "Shared key-value store (requires relay)",
	}

	cmd.AddCommand(
		kvSetCmd(),
		kvGetCmd(),
		kvListCmd(),
		kvDeleteCmd(),
	)

	return cmd
}

func kvSetCmd() *cobra.Command {
	var (
		namespace string
		ttl       string
	)

	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a key-value pair",
		Args:  cobra.ExactArgs(2),
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

			return client.KVSet(target, namespace, args[0], args[1], ttl)
		},
	}

	cmd.Flags().StringVar(&namespace, "ns", "default", "Namespace")
	cmd.Flags().StringVar(&ttl, "ttl", "", "Time-to-live (e.g. 60s, 5m)")

	return cmd
}

func kvGetCmd() *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "get <key>",
		Short: "Get a value by key",
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

			return client.KVGet(target, namespace, args[0])
		},
	}

	cmd.Flags().StringVar(&namespace, "ns", "default", "Namespace")

	return cmd
}

func kvListCmd() *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "list [prefix]",
		Short: "List keys",
		Args:  cobra.MaximumNArgs(1),
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

			prefix := ""
			if len(args) > 0 {
				prefix = args[0]
			}

			return client.KVList(target, namespace, prefix)
		},
	}

	cmd.Flags().StringVar(&namespace, "ns", "default", "Namespace")

	return cmd
}

func kvDeleteCmd() *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "delete <key>",
		Short: "Delete a key",
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

			return client.KVDelete(target, namespace, args[0])
		},
	}

	cmd.Flags().StringVar(&namespace, "ns", "default", "Namespace")

	return cmd
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

	cmd.Flags().StringVar(&token, "token", "", "Auth token for the server (optional for relay URLs)")

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
// setupCmd
// ---------------------------------------------------------------------------

func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup [relay-url]",
		Short: "Connect this node to a relay via device authorization",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			relayURL := "https://relay.codespace.sh"
			if len(args) > 0 {
				relayURL = args[0]
			}

			dir := dataDir()
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("creating data dir: %w", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
			go func() {
				<-sigCh
				cancel()
			}()

			return tunnel.RunSetup(ctx, relayURL, dir)
		},
	}
}

// ---------------------------------------------------------------------------
// relayCmd
// ---------------------------------------------------------------------------

func relayCmd() *cobra.Command {
	var (
		baseURL     string
		wgPort      uint16
		wgEndpoint  string
		listen      string
		relayDir    string
		authMode    string
		authToken   string
	)

	cmd := &cobra.Command{
		Use:   "relay",
		Short: "Run a CodeWire relay server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if baseURL == "" {
				return fmt.Errorf("--base-url is required")
			}

			if relayDir == "" {
				relayDir = filepath.Join(dataDir(), "relay")
			}

			if err := os.MkdirAll(relayDir, 0o755); err != nil {
				return fmt.Errorf("creating relay data dir: %w", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
			go func() {
				<-sigCh
				fmt.Fprintln(os.Stderr, "[cw] relay shutting down...")
				cancel()
			}()

			return tunnel.RunRelay(ctx, tunnel.RelayConfig{
				BaseURL:           baseURL,
				WireguardEndpoint: wgEndpoint,
				WireguardPort:     wgPort,
				ListenAddr:        listen,
				DataDir:           relayDir,
				AuthMode:          authMode,
				AuthToken:         authToken,
			})
		},
	}

	cmd.Flags().StringVar(&baseURL, "base-url", "", "Public base URL of the relay (e.g. https://relay.codespace.sh)")
	cmd.Flags().Uint16Var(&wgPort, "wg-port", 41820, "WireGuard UDP port")
	cmd.Flags().StringVar(&wgEndpoint, "wg-endpoint", "", "WireGuard endpoint to advertise (defaults to base-url hostname:wg-port)")
	cmd.Flags().StringVar(&listen, "listen", ":8080", "HTTP listen address")
	cmd.Flags().StringVar(&relayDir, "data-dir", "", "Data directory for relay (default: ~/.codewire/relay)")
	cmd.Flags().StringVar(&authMode, "auth-mode", "none", "Auth mode: token, none")
	cmd.Flags().StringVar(&authToken, "auth-token", "", "Shared auth token (required when --auth-mode=token)")

	return cmd
}

// ---------------------------------------------------------------------------
// msgCmd — send a direct message to a session
// ---------------------------------------------------------------------------

func msgCmd() *cobra.Command {
	var from string

	cmd := &cobra.Command{
		Use:   "msg <target> <body>",
		Short: "Send a message to a session (by ID or name)",
		Args:  cobra.ExactArgs(2),
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

			toID, err := client.ResolveSessionArg(target, args[0])
			if err != nil {
				return err
			}

			var fromID *uint32
			if from != "" {
				resolved, err := client.ResolveSessionArg(target, from)
				if err != nil {
					return err
				}
				fromID = &resolved
			}

			return client.Msg(target, fromID, toID, args[1])
		},
	}

	cmd.Flags().StringVarP(&from, "from", "f", "", "Sender session (ID or name)")

	return cmd
}

// ---------------------------------------------------------------------------
// inboxCmd — read messages for a session
// ---------------------------------------------------------------------------

func inboxCmd() *cobra.Command {
	var tail int

	cmd := &cobra.Command{
		Use:   "inbox <session>",
		Short: "Read messages for a session (by ID or name)",
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

			sessionID, err := client.ResolveSessionArg(target, args[0])
			if err != nil {
				return err
			}

			return client.Inbox(target, sessionID, tail)
		},
	}

	cmd.Flags().IntVarP(&tail, "tail", "t", 50, "Number of messages to show")

	return cmd
}

// ---------------------------------------------------------------------------
// listenCmd — stream message traffic
// ---------------------------------------------------------------------------

func listenCmd() *cobra.Command {
	var sessionArg string

	cmd := &cobra.Command{
		Use:   "listen",
		Short: "Stream all message traffic in real-time",
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

			var sessionID *uint32
			if sessionArg != "" {
				resolved, err := client.ResolveSessionArg(target, sessionArg)
				if err != nil {
					return err
				}
				sessionID = &resolved
			}

			return client.Listen(target, sessionID)
		},
	}

	cmd.Flags().StringVar(&sessionArg, "session", "", "Filter by session (ID or name)")

	return cmd
}

// ---------------------------------------------------------------------------
// requestCmd — send a request and block for reply
// ---------------------------------------------------------------------------

func requestCmd() *cobra.Command {
	var (
		from    string
		timeout uint64
	)

	cmd := &cobra.Command{
		Use:   "request <target> <body>",
		Short: "Send a request to a session and wait for a reply",
		Args:  cobra.ExactArgs(2),
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

			toID, err := client.ResolveSessionArg(target, args[0])
			if err != nil {
				return err
			}

			var fromID *uint32
			if from != "" {
				resolved, err := client.ResolveSessionArg(target, from)
				if err != nil {
					return err
				}
				fromID = &resolved
			}

			return client.Request(target, fromID, toID, args[1], timeout)
		},
	}

	cmd.Flags().StringVarP(&from, "from", "f", "", "Sender session (ID or name)")
	cmd.Flags().Uint64Var(&timeout, "timeout", 60, "Timeout in seconds")

	return cmd
}

// ---------------------------------------------------------------------------
// replyCmd — reply to a pending request
// ---------------------------------------------------------------------------

func replyCmd() *cobra.Command {
	var from string

	cmd := &cobra.Command{
		Use:   "reply <request-id> <body>",
		Short: "Reply to a pending request",
		Args:  cobra.ExactArgs(2),
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

			var fromID *uint32
			if from != "" {
				resolved, err := client.ResolveSessionArg(target, from)
				if err != nil {
					return err
				}
				fromID = &resolved
			}

			return client.Reply(target, fromID, args[0], args[1])
		},
	}

	cmd.Flags().StringVarP(&from, "from", "f", "", "Sender session (ID or name)")

	return cmd
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
	url := serverFlag
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		// Relay URL — token is optional (relay handles auth).
		return &client.Target{URL: url, Token: tokenFlag}, nil
	}

	if tokenFlag == "" {
		return nil, fmt.Errorf("--token required for ad-hoc WebSocket server")
	}

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

func resolveRelayURL() (string, error) {
	cfg, err := config.LoadConfig(dataDir())
	if err != nil {
		return "", fmt.Errorf("loading config: %w", err)
	}
	if cfg.RelayURL == nil || *cfg.RelayURL == "" {
		return "", fmt.Errorf("relay not configured (run 'cw setup <relay-url>' or set CODEWIRE_RELAY_URL)")
	}
	return *cfg.RelayURL, nil
}
