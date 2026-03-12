package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	cwconfig "github.com/codewiresh/codewire/internal/config"
	"github.com/codewiresh/codewire/internal/platform"
)

// loadCodewireYAML is a convenience wrapper around config.LoadCodewireConfig.
func loadCodewireYAML(path string) (*cwconfig.CodewireConfig, error) {
	return cwconfig.LoadCodewireConfig(path)
}

func envParentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "env",
		Short:   "Manage environments",
		Aliases: []string{"environment"},
	}
	cmd.AddCommand(envCreateCmd())
	cmd.AddCommand(envListCmd())
	cmd.AddCommand(envInfoCmd())
	cmd.AddCommand(envStopCmd())
	cmd.AddCommand(envStartCmd())
	cmd.AddCommand(envRmCmd())
	cmd.AddCommand(envExecCmd())
	cmd.AddCommand(envSSHCmd())
	cmd.AddCommand(envCpCmd())
	cmd.AddCommand(envPruneCmd())
	cmd.AddCommand(envNukeCmd())
	cmd.AddCommand(envLogsCmd())
	return cmd
}

// parseRepoSpec splits "url@branch" into (url, branch).
// If no "@" is present, branch is empty.
func parseRepoSpec(spec string) (string, string) {
	// Don't split on @ inside the user@host part of SSH/HTTPS URLs.
	// The branch delimiter is the last @ that appears after a "/" or at the end.
	idx := strings.LastIndex(spec, "@")
	if idx == -1 {
		return spec, ""
	}
	// Ensure the @ is after a path separator (not in user@host).
	url := spec[:idx]
	branch := spec[idx+1:]
	if !strings.Contains(url, "/") {
		// No slash before @, so this is user@host, not url@branch.
		return spec, ""
	}
	return url, branch
}

func getDefaultOrg() (string, *platform.Client, error) {
	client, err := platform.NewClient()
	if err != nil {
		return "", nil, err
	}
	cfg, err := platform.LoadConfig()
	if err != nil {
		return "", nil, err
	}
	if cfg.DefaultOrg == "" {
		return "", nil, fmt.Errorf("no default org set. Run 'cw login' and set a default org")
	}
	return cfg.DefaultOrg, client, nil
}

func resolveEnvID(client *platform.Client, orgID, ref string) (string, error) {
	// Fast path: if it looks like a UUID, try direct lookup.
	if len(ref) >= 36 && strings.Contains(ref, "-") {
		if _, err := client.GetEnvironment(orgID, ref); err == nil {
			return ref, nil
		}
	}

	// Name lookup: list all and filter by name.
	envs, err := client.ListEnvironments(orgID, "", "", false)
	if err != nil {
		return "", fmt.Errorf("list environments: %w", err)
	}

	var matches []platform.Environment
	for _, e := range envs {
		if e.Name != nil && *e.Name == ref {
			matches = append(matches, e)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", fmt.Errorf("environment %q not found", ref)
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return "", fmt.Errorf("multiple environments named %q, use ID: %s", ref, strings.Join(ids, ", "))
	}
}

func envCompletionFunc(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	orgID, client, err := getDefaultOrg()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	envs, err := client.ListEnvironments(orgID, "", "", false)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	seen := make(map[string]bool)
	var completions []string
	// Names first (preferred), then IDs.
	for _, e := range envs {
		if e.Name != nil && *e.Name != "" && !seen[*e.Name] {
			seen[*e.Name] = true
			completions = append(completions, *e.Name)
		}
	}
	for _, e := range envs {
		completions = append(completions, e.ID)
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

func timeAgo(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func envCreateCmd() *cobra.Command {
	var (
		templateSlug   string
		templateID     string
		name           string
		ttl            string
		cpu            int
		memory         int
		disk           int
		repoFlags      []string
		branch         string
		image          string
		install        string
		startup        string
		agent          string
		envVars        []string
		secretProject  string
		noOrgSecrets   bool
		noUserSecrets  bool
		follow         bool
	)

	cmd := &cobra.Command{
		Use:   "create [repo-url ...]",
		Short: "Create a new environment",
		Long: `Create a new environment. Smart create detects configuration from a repo URL.

Multiple repos can be specified as positional args or with --repo flags.
Use url@branch syntax to specify a branch per repo.

Examples:
  cw env create https://github.com/foo/bar
  cw env create https://github.com/foo/frontend https://github.com/foo/api
  cw env create --repo github.com/foo/frontend@main --repo git.noel.sh/foo/backend
  cw env create --template go --name my-env
  cw env create --image go --name my-env
  cw env create --secrets my-project https://github.com/foo/bar`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Collect repos from positional args + --repo flags.
			var repoURL string
			var repos []platform.RepoEntry

			allRepoSpecs := append(args, repoFlags...)
			for _, spec := range allRepoSpecs {
				u, b := parseRepoSpec(spec)
				repos = append(repos, platform.RepoEntry{URL: u, Branch: b})
			}

			// Single repo: also set repoURL for backward compat.
			if len(repos) == 1 {
				repoURL = repos[0].URL
				if repos[0].Branch != "" && branch == "" {
					branch = repos[0].Branch
				}
			} else if len(repos) > 1 {
				repoURL = repos[0].URL
			}

			// 0-arg support: look for ./codewire.yaml
			if templateSlug == "" && templateID == "" && image == "" && repoURL == "" {
				cfg, err := loadCodewireYAML("codewire.yaml")
				if err == nil {
					fmt.Println("Using ./codewire.yaml")
					if cfg.Template != "" && templateSlug == "" {
						templateSlug = cfg.Template
					}
					if cfg.Install != "" && install == "" {
						install = cfg.Install
					}
					if cfg.Startup != "" && startup == "" {
						startup = cfg.Startup
					}
					if cfg.Secrets != "" && secretProject == "" {
						secretProject = cfg.Secrets
					}
					if cfg.Agent != "" && agent == "" {
						agent = cfg.Agent
					}
					if cfg.CPU > 0 && cpu == 0 {
						cpu = cfg.CPU
					}
					if cfg.Memory > 0 && memory == 0 {
						memory = cfg.Memory
					}
					if cfg.Disk > 0 && disk == 0 {
						disk = cfg.Disk
					}
					if cfg.IncludeOrgSecrets != nil && !*cfg.IncludeOrgSecrets {
						noOrgSecrets = true
					}
					if cfg.IncludeUserSecrets != nil && !*cfg.IncludeUserSecrets {
						noUserSecrets = true
					}
					for k, v := range cfg.Env {
						envVars = append(envVars, k+"="+v)
					}
				} else {
					// No codewire.yaml — try to infer repo URL from git remote.
				if url, b, err := detectLocalRepo("."); err == nil && url != "" {
					repoURL = url
					if branch == "" {
						branch = b
					}
					fmt.Printf("Using repo: %s\n", repoURL)
				} else {
					return fmt.Errorf("provide a repo URL, --image, or --template")
				}
				}
			}

			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			// Parse env vars from KEY=val format.
			parsedEnvVars := make(map[string]string)
			for _, ev := range envVars {
				parts := strings.SplitN(ev, "=", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid --env format %q, expected KEY=val", ev)
				}
				parsedEnvVars[parts[0]] = parts[1]
			}

			// Image expansion with Docker-like semantics.
			if image != "" {
				image = expandImageRef(image)
			}

			req := &platform.CreateEnvironmentRequest{
				TemplateID:     templateID,
				TemplateSlug:   templateSlug,
				Name:           name,
				RepoURL:        repoURL,
				Branch:         branch,
				Image:          image,
				InstallCommand: install,
				StartupScript:  startup,
				Agent:          agent,
				SecretProject:  secretProject,
			}
			if len(repos) > 0 {
				req.Repos = repos
			}
			if len(parsedEnvVars) > 0 {
				req.EnvVars = parsedEnvVars
			}
			if cpu > 0 {
				req.CPUMillicores = &cpu
			}
			if memory > 0 {
				req.MemoryMB = &memory
			}
			if disk > 0 {
				req.DiskGB = &disk
			}
			if ttl != "" {
				d, err := time.ParseDuration(ttl)
				if err != nil {
					return fmt.Errorf("invalid --ttl duration: %w", err)
				}
				secs := int(d.Seconds())
				req.TTLSeconds = &secs
			}

			if noOrgSecrets {
				f := false
				req.IncludeOrgSecrets = &f
			}
			if noUserSecrets {
				f := false
				req.IncludeUserSecrets = &f
			}

			// Resolve Claude OAuth token if agent is claude-code.
			if agent == "claude-code" {
				token := resolveClaudeOAuthToken()
				if token != "" {
					req.AgentEnv = map[string]string{
						"CLAUDE_CODE_OAUTH_TOKEN": token,
					}
				}
			}

			var env *platform.Environment
			err = withReauth(client, func() error {
				var createErr error
				env, createErr = client.CreateEnvironment(orgID, req)
				return createErr
			})
			if err != nil {
				return fmt.Errorf("create environment: %w", err)
			}

			envName := env.ID
			if env.Name != nil {
				envName = *env.Name
			}
			fmt.Printf("Environment created: %s\n", envName)
			fmt.Printf("  ID:    %s\n", env.ID)
			fmt.Printf("  State: %s\n", env.State)
			fmt.Printf("  Type:  %s\n", env.Type)
			fmt.Printf("  CPU:   %dm\n", env.CPUMillicores)
			fmt.Printf("  Mem:   %dMB\n", env.MemoryMB)
			fmt.Printf("  Disk:  %dGB\n", env.DiskGB)

			if follow {
				fmt.Println()
				if err := followEnvironmentLogs(client, orgID, env.ID); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: could not follow logs: %v\n", err)
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&templateSlug, "template", "", "Template slug (e.g. go, node, python)")
	cmd.Flags().StringVar(&templateID, "template-id", "", "Template ID (exact)")
	cmd.Flags().StringVar(&name, "name", "", "Environment name")
	cmd.Flags().StringVar(&ttl, "ttl", "", "Time to live (e.g. 1h, 30m)")
	cmd.Flags().IntVar(&cpu, "cpu", 0, "CPU in millicores")
	cmd.Flags().IntVar(&memory, "memory", 0, "Memory in MB")
	cmd.Flags().IntVar(&disk, "disk", 0, "Disk in GB")
	cmd.Flags().StringArrayVar(&repoFlags, "repo", nil, "Git repo URL (repeatable, url@branch syntax)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to checkout")
	cmd.Flags().StringVar(&image, "image", "", "Container image (shorthand: go → workspace-go)")
	cmd.Flags().StringVar(&install, "install", "", "Install command")
	cmd.Flags().StringVar(&startup, "startup", "", "Startup script")
	cmd.Flags().StringVar(&agent, "agent", "", "AI agent (claude-code)")
	cmd.Flags().StringSliceVar(&envVars, "env", nil, "Env vars (KEY=val)")
	cmd.Flags().StringVar(&secretProject, "secrets", "", "Secret project to bind")
	cmd.Flags().BoolVar(&noOrgSecrets, "no-org-secrets", false, "Don't inject org-level secrets")
	cmd.Flags().BoolVar(&noUserSecrets, "no-user-secrets", false, "Don't inject user-level secrets")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow startup logs after creation")
	return cmd
}

func envListCmd() *cobra.Command {
	var (
		envType string
		state   string
		all     bool
	)

	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List environments",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			envs, err := client.ListEnvironments(orgID, envType, state, all)
			if err != nil {
				return fmt.Errorf("list environments: %w", err)
			}

			if len(envs) == 0 {
				fmt.Println("No environments found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tSTATE\tTYPE\tCPU/MEM\tTTL\tCREATED")
			for _, e := range envs {
				envName := "--"
				if e.Name != nil {
					envName = *e.Name
				}

				cpuMem := fmt.Sprintf("%dm/%dMB", e.CPUMillicores, e.MemoryMB)

				ttlStr := "--"
				if e.ShutdownAt != nil {
					shutdownTime, err := time.Parse(time.RFC3339, *e.ShutdownAt)
					if err == nil {
						remaining := time.Until(shutdownTime)
						if remaining > 0 {
							ttlStr = fmt.Sprintf("%dm", int(remaining.Minutes()))
						} else {
							ttlStr = "expired"
						}
					}
				}

				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					e.ID, envName, e.State, e.Type, cpuMem, ttlStr, timeAgo(e.CreatedAt))
			}
			return w.Flush()
		},
	}

	cmd.Flags().StringVar(&envType, "type", "", "Filter by type (coder, sandbox)")
	cmd.Flags().StringVar(&state, "state", "", "Filter by state")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "Include destroyed environments")
	return cmd
}

func envInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "info <id-or-name>",
		Short:             "Show environment details",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: envCompletionFunc,
		RunE: func(cmd *cobra.Command, args []string) error {
			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			envID, err := resolveEnvID(client, orgID, args[0])
			if err != nil {
				return err
			}

			env, err := client.GetEnvironment(orgID, envID)
			if err != nil {
				return fmt.Errorf("get environment: %w", err)
			}

			envName := "--"
			if env.Name != nil {
				envName = *env.Name
			}

			fmt.Printf("ID:            %s\n", env.ID)
			fmt.Printf("Name:          %s\n", envName)
			fmt.Printf("Type:          %s\n", env.Type)
			fmt.Printf("State:         %s\n", env.State)
			fmt.Printf("DesiredState:  %s\n", env.DesiredState)
			fmt.Printf("TemplateID:    %s\n", env.TemplateID)
			fmt.Printf("CPU:           %dm\n", env.CPUMillicores)
			fmt.Printf("Memory:        %dMB\n", env.MemoryMB)
			fmt.Printf("Disk:          %dGB\n", env.DiskGB)
			fmt.Printf("CreatedBy:     %s\n", env.CreatedBy)
			fmt.Printf("CreatedAt:     %s\n", env.CreatedAt)
			if env.StartedAt != nil {
				fmt.Printf("StartedAt:     %s\n", *env.StartedAt)
			}
			if env.StoppedAt != nil {
				fmt.Printf("StoppedAt:     %s\n", *env.StoppedAt)
			}
			if env.ShutdownAt != nil {
				fmt.Printf("ShutdownAt:    %s\n", *env.ShutdownAt)
			}
			if env.TTLSeconds != nil {
				fmt.Printf("TTL:           %ds\n", *env.TTLSeconds)
			}
			if env.ErrorReason != nil {
				fmt.Printf("ErrorReason:   %s\n", *env.ErrorReason)
			}
			fmt.Printf("Recoverable:   %v\n", env.Recoverable)
			fmt.Printf("TotalRunning:  %ds\n", env.TotalRunningSeconds)
			return nil
		},
	}
}

func envStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "stop <id-or-name>",
		Short:             "Stop an environment",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: envCompletionFunc,
		RunE: func(cmd *cobra.Command, args []string) error {
			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			envID, err := resolveEnvID(client, orgID, args[0])
			if err != nil {
				return err
			}

			resp, err := client.StopEnvironment(orgID, envID)
			if err != nil {
				return fmt.Errorf("stop environment: %w", err)
			}
			fmt.Printf("Environment %s: %s\n", args[0], resp.Status)
			return nil
		},
	}
}

func envStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "start <id-or-name>",
		Short:             "Start an environment",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: envCompletionFunc,
		RunE: func(cmd *cobra.Command, args []string) error {
			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			envID, err := resolveEnvID(client, orgID, args[0])
			if err != nil {
				return err
			}

			resp, err := client.StartEnvironment(orgID, envID)
			if err != nil {
				return fmt.Errorf("start environment: %w", err)
			}
			fmt.Printf("Environment %s: %s\n", args[0], resp.Status)
			return nil
		},
	}
}

func envRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "rm <id-or-name>",
		Short:             "Delete an environment",
		Aliases:           []string{"delete"},
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: envCompletionFunc,
		RunE: func(cmd *cobra.Command, args []string) error {
			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			envID, err := resolveEnvID(client, orgID, args[0])
			if err != nil {
				return err
			}

			if err := client.DeleteEnvironment(orgID, envID); err != nil {
				return fmt.Errorf("delete environment: %w", err)
			}
			fmt.Printf("Environment %s deleted.\n", args[0])
			return nil
		},
	}
}

func envPruneCmd() *cobra.Command {
	var (
		yes    bool
		state  string
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Bulk delete stale environments",
		Long:  "Delete environments stuck in error, creating, or pending states.",
		RunE: func(cmd *cobra.Command, args []string) error {
			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			pruneStates := make(map[string]bool)
			for _, s := range strings.Split(state, ",") {
				s = strings.TrimSpace(s)
				if s != "" {
					pruneStates[s] = true
				}
			}

			envs, err := client.ListEnvironments(orgID, "", "", false)
			if err != nil {
				return fmt.Errorf("list environments: %w", err)
			}

			var targets []platform.Environment
			for _, e := range envs {
				if pruneStates[e.State] {
					targets = append(targets, e)
				}
			}

			if len(targets) == 0 {
				fmt.Println("No environments to prune.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "ID\tNAME\tSTATE\tAGE\n")
			for _, e := range targets {
				name := "--"
				if e.Name != nil {
					name = *e.Name
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.ID, name, e.State, timeAgo(e.CreatedAt))
			}
			w.Flush()
			fmt.Println()

			if dryRun {
				fmt.Printf("Would prune %d environments (dry run).\n", len(targets))
				return nil
			}

			if !yes {
				answer, err := prompt(fmt.Sprintf("Delete %d environments? [y/N] ", len(targets)))
				if err != nil {
					return err
				}
				if strings.ToLower(answer) != "y" {
					fmt.Println("Aborted.")
					return nil
				}
			}

			deleted := 0
			for _, e := range targets {
				if err := client.DeleteEnvironment(orgID, e.ID); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to delete %s: %v\n", e.ID, err)
					continue
				}
				label := e.ID
				if e.Name != nil {
					label = fmt.Sprintf("%s (%s)", e.ID, *e.Name)
				}
				fmt.Printf("Deleted %s\n", label)
				deleted++
			}

			fmt.Printf("Pruned %d environments.\n", deleted)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
	cmd.Flags().StringVar(&state, "state", "error,creating,pending", "Comma-separated states to prune")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be pruned without deleting")
	return cmd
}

func envNukeCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "nuke",
		Short: "Delete ALL environments",
		Long:  "Delete every environment in the current org. Requires typed confirmation unless --yes is passed.",
		RunE: func(cmd *cobra.Command, args []string) error {
			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			envs, err := client.ListEnvironments(orgID, "", "", false)
			if err != nil {
				return fmt.Errorf("list environments: %w", err)
			}

			if len(envs) == 0 {
				fmt.Println("No environments to nuke.")
				return nil
			}

			cfg, _ := platform.LoadConfig()
			orgLabel := orgID
			if cfg != nil && cfg.DefaultOrg != "" {
				orgLabel = cfg.DefaultOrg
			}

			fmt.Printf("This will DELETE ALL %d environments in org %q.\n\n", len(envs), orgLabel)

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tSTATE")
			for _, e := range envs {
				name := "--"
				if e.Name != nil {
					name = *e.Name
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", e.ID, name, e.State)
			}
			w.Flush()
			fmt.Println()

			if !yes {
				expected := fmt.Sprintf("nuke %d environments", len(envs))
				answer, err := prompt(fmt.Sprintf("Type %q to confirm: ", expected))
				if err != nil {
					return err
				}
				if answer != expected {
					fmt.Println("Aborted.")
					return nil
				}
			}

			deleted := 0
			for _, e := range envs {
				if err := client.DeleteEnvironment(orgID, e.ID); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to delete %s: %v\n", e.ID, err)
					continue
				}
				label := e.ID
				if e.Name != nil {
					label = fmt.Sprintf("%s (%s)", e.ID, *e.Name)
				}
				fmt.Printf("Deleted %s\n", label)
				deleted++
			}

			fmt.Printf("Nuked %d environments.\n", deleted)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip typed confirmation")
	return cmd
}

func envLogsCmd() *cobra.Command {
	var follow bool

	cmd := &cobra.Command{
		Use:               "logs <id>",
		Short:             "Show environment startup logs",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: envCompletionFunc,
		RunE: func(cmd *cobra.Command, args []string) error {
			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			envID, err := resolveEnvID(client, orgID, args[0])
			if err != nil {
				return err
			}

			if follow {
				return followEnvironmentLogs(client, orgID, envID)
			}

			// Non-follow: fetch and print historical logs.
			logs, err := client.GetEnvironmentLogs(orgID, envID)
			if err != nil {
				return fmt.Errorf("get environment logs: %w", err)
			}
			if len(logs) == 0 {
				fmt.Println("No logs yet.")
				return nil
			}

			phases := make(map[string]time.Time)
			for _, ev := range logs {
				// Use the event's own created_at for elapsed calculation.
				if ev.Status == "started" {
					t, err := time.Parse(time.RFC3339, ev.CreatedAt)
					if err == nil {
						phases[ev.Phase] = t
					}
				}
				renderEnvLogEventHistorical(ev, phases)
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", true, "Follow live logs (default: true)")
	return cmd
}

// renderEnvLogEventHistorical prints a historical log event with server-side timestamps.
func renderEnvLogEventHistorical(ev platform.EnvironmentLog, phases map[string]time.Time) {
	switch ev.Status {
	case "started":
		fmt.Fprintf(os.Stderr, "  \033[33m◌\033[0m %s...\n", ev.Message)
	case "completed":
		elapsed := ""
		if start, ok := phases[ev.Phase]; ok {
			end, err := time.Parse(time.RFC3339, ev.CreatedAt)
			if err == nil {
				elapsed = fmt.Sprintf("  %s", end.Sub(start).Truncate(time.Second))
			}
		}
		fmt.Fprintf(os.Stderr, "  \033[32m✓\033[0m %s%s\n", ev.Message, elapsed)
	case "warning":
		fmt.Fprintf(os.Stderr, "  \033[33m!\033[0m %s\n", ev.Message)
	case "failed":
		fmt.Fprintf(os.Stderr, "  \033[31m✗\033[0m %s\n", ev.Message)
	default:
		fmt.Fprintf(os.Stderr, "  · %s\n", ev.Message)
	}
}

// expandImageRef applies Docker-like image name expansion:
//   - Has registry domain (first segment contains "." or ":") → as-is
//   - Starts with "workspace-" → ghcr.io/codewiresh/workspace-*
//   - No "/" → docker.io/library/<image> (official image)
//   - One "/" → docker.io/<image> (user image)
func expandImageRef(image string) string {
	// Split name from tag.
	name, tag, hasTag := strings.Cut(image, ":")

	// Check if first path segment looks like a registry domain.
	if slash := strings.Index(name, "/"); slash > 0 {
		firstSeg := name[:slash]
		if strings.Contains(firstSeg, ".") || strings.Contains(firstSeg, ":") {
			return image // fully qualified
		}
	}

	if strings.HasPrefix(name, "workspace-") {
		// Codewire workspace shorthand.
		ref := "ghcr.io/codewiresh/" + name
		if hasTag {
			return ref + ":" + tag
		}
		return ref + ":latest"
	}

	if !strings.Contains(name, "/") {
		// Bare name → Docker Hub official image.
		ref := "docker.io/library/" + name
		if hasTag {
			return ref + ":" + tag
		}
		return ref + ":latest"
	}

	// user/repo → Docker Hub user image.
	ref := "docker.io/" + name
	if hasTag {
		return ref + ":" + tag
	}
	return ref + ":latest"
}

