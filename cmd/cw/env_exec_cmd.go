package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/codewiresh/codewire/internal/platform"
)

func envExecCmd() *cobra.Command {
	var (
		workDir string
		timeout int
	)

	cmd := &cobra.Command{
		Use:               "exec <id-or-name> -- <command> [args...]",
		Short:             "Execute a command in a running environment",
		Long:              "Run a one-shot command in a sandbox environment. Streams stdout/stderr and exits with the command's exit code.",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: envCompletionFunc,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Find the -- separator to split env ID from command
			cmdArgs := args[1:]
			if cmd.ArgsLenAtDash() > 0 {
				cmdArgs = args[cmd.ArgsLenAtDash():]
			}
			if len(cmdArgs) == 0 {
				return fmt.Errorf("no command specified. Usage: cw env exec <id-or-name> -- <command>")
			}

			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			envID, err := resolveEnvID(client, orgID, args[0])
			if err != nil {
				return err
			}

			result, err := client.ExecInEnvironment(orgID, envID, &platform.ExecRequest{
				Command:    cmdArgs,
				WorkingDir: workDir,
				Timeout:    timeout,
			})
			if err != nil {
				return fmt.Errorf("exec: %w", err)
			}

			if result.Stdout != "" {
				fmt.Print(result.Stdout)
			}
			if result.Stderr != "" {
				fmt.Fprint(os.Stderr, result.Stderr)
			}

			if result.ExitCode != 0 {
				os.Exit(result.ExitCode)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&workDir, "workdir", "w", "/workspace", "Working directory")
	cmd.Flags().IntVar(&timeout, "timeout", 30, "Timeout in seconds")
	return cmd
}

func envSSHCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "ssh <id-or-name>",
		Short:             "Open an interactive shell in a running environment",
		Long:              "Start an interactive bash shell in a sandbox environment.",
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

			// Use exec with an interactive shell
			// Note: for true SSH-like experience, we'd need WebSocket streaming.
			// For now, run bash in a pseudo-interactive mode.
			result, err := client.ExecInEnvironment(orgID, envID, &platform.ExecRequest{
				Command:    []string{"bash", "-l"},
				WorkingDir: "/workspace",
				Timeout:    0, // no timeout for interactive
			})
			if err != nil {
				return fmt.Errorf("ssh: %w", err)
			}

			if result.Stdout != "" {
				fmt.Print(result.Stdout)
			}
			if result.Stderr != "" {
				fmt.Fprint(os.Stderr, result.Stderr)
			}
			os.Exit(result.ExitCode)
			return nil
		},
	}
}

func envCpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cp <src> <dst>",
		Short: "Copy files to/from an environment",
		Long: `Copy files between local filesystem and a sandbox environment.

Use <env_id>:<path> syntax for remote paths:
  cw env cp local.txt <env_id>:/workspace/local.txt   # upload
  cw env cp <env_id>:/workspace/file.txt ./file.txt    # download`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, dst := args[0], args[1]

			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			srcEnv, srcPath := parseRemotePath(src)
			dstEnv, dstPath := parseRemotePath(dst)

			if srcEnv != "" && dstEnv != "" {
				return fmt.Errorf("cannot copy between two remote environments")
			}
			if srcEnv == "" && dstEnv == "" {
				return fmt.Errorf("at least one path must be remote (<env_id>:<path>)")
			}

			// Resolve env names to IDs.
			if srcEnv != "" {
				srcEnv, err = resolveEnvID(client, orgID, srcEnv)
				if err != nil {
					return err
				}
			}
			if dstEnv != "" {
				dstEnv, err = resolveEnvID(client, orgID, dstEnv)
				if err != nil {
					return err
				}
			}

			if dstEnv != "" {
				// Upload: local -> remote
				f, err := os.Open(srcPath)
				if err != nil {
					return fmt.Errorf("open local file: %w", err)
				}
				defer f.Close()

				if err := client.UploadFile(orgID, dstEnv, dstPath, f); err != nil {
					return fmt.Errorf("upload: %w", err)
				}
				fmt.Printf("Uploaded %s -> %s:%s\n", srcPath, dstEnv, dstPath)
				return nil
			}

			// Download: remote -> local
			body, err := client.DownloadFile(orgID, srcEnv, srcPath)
			if err != nil {
				return fmt.Errorf("download: %w", err)
			}
			defer body.Close()

			out, err := os.Create(dstPath)
			if err != nil {
				return fmt.Errorf("create local file: %w", err)
			}
			defer out.Close()

			n, err := out.ReadFrom(body)
			if err != nil {
				return fmt.Errorf("write local file: %w", err)
			}
			fmt.Printf("Downloaded %s:%s -> %s (%d bytes)\n", srcEnv, srcPath, dstPath, n)
			return nil
		},
	}
}

// parseRemotePath splits "<env_id>:<path>" into (envID, path).
// Returns ("", original) if no colon is found.
func parseRemotePath(s string) (string, string) {
	// Don't split on Windows-style paths like C:\
	idx := strings.Index(s, ":")
	if idx <= 0 {
		return "", s
	}
	// Single char before colon is likely a drive letter, not an env ID
	if idx == 1 {
		return "", s
	}
	return s[:idx], s[idx+1:]
}
