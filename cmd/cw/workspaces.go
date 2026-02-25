package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/codewiresh/codewire/internal/platform"
)

func launchCmd() *cobra.Command {
	var (
		branch       string
		templateName string
		templateID   string
		resourceID   string
		noWait       bool
	)

	cmd := &cobra.Command{
		Use:   "launch <repo-url>",
		Short: "Create a workspace from a repo URL",
		Long:  "Create a new workspace on the default Coder resource, cloning the given repository.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repoURL := args[0]

			client, err := platform.NewClient()
			if err != nil {
				return err
			}

			// Resolve resource ID
			resID := resourceID
			if resID == "" {
				cfg, err := platform.LoadConfig()
				if err != nil || cfg.DefaultResource == "" {
					return fmt.Errorf("no default resource set (run 'cw setup' or pass --resource)")
				}
				resID = cfg.DefaultResource
			}

			// Derive workspace name from repo URL
			wsName := deriveWorkspaceName(repoURL)

			// Resolve template
			tmplID := templateID
			if tmplID == "" && templateName == "" {
				// Auto-pick first template
				templates, err := client.ListTemplates(resID)
				if err != nil {
					return fmt.Errorf("list templates: %w", err)
				}
				if len(templates) == 0 {
					return fmt.Errorf("no templates available on this resource")
				}
				tmplID = templates[0].ID
				if templateName == "" {
					templateName = templates[0].Name
				}
			}

			// Build rich parameters
			var params []platform.RichParameterValue
			params = append(params, platform.RichParameterValue{Name: "repo", Value: repoURL})
			if branch != "" {
				params = append(params, platform.RichParameterValue{Name: "branch", Value: branch})
			}

			fmt.Printf("Creating workspace %q...\n", wsName)
			ws, err := client.CreateWorkspace(resID, &platform.CreateWorkspaceRequest{
				Name:         wsName,
				TemplateID:   tmplID,
				TemplateName: templateName,
				RichParams:   params,
			})
			if err != nil {
				return fmt.Errorf("create workspace: %w", err)
			}

			if noWait {
				fmt.Printf("Workspace %s created (status: %s)\n", ws.Name, ws.Status)
				return nil
			}

			// Wait for workspace to be running
			fmt.Print("Waiting for workspace to start...")
			ws, err = client.WaitForWorkspace(resID, ws.ID, 5*time.Minute)
			if err != nil {
				fmt.Println(" timeout")
				return err
			}
			fmt.Printf(" %s\n", ws.Status)

			if ws.Status != "running" {
				return fmt.Errorf("workspace ended with status: %s", ws.Status)
			}

			fmt.Printf("\nWorkspace %s is running.\n", ws.Name)
			fmt.Printf("  cw open %s          # Open in browser\n", ws.Name)
			fmt.Printf("  coder ssh %s        # SSH into workspace\n", ws.Name)
			return nil
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "", "Branch to checkout (default: repo default)")
	cmd.Flags().StringVar(&templateName, "template", "", "Template name (default: first available)")
	cmd.Flags().StringVar(&templateID, "template-id", "", "Template ID")
	cmd.Flags().StringVar(&resourceID, "resource", "", "Resource ID (default: from config)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Don't wait for workspace to start")
	return cmd
}

func openCmd() *cobra.Command {
	var resourceID string

	cmd := &cobra.Command{
		Use:   "open <workspace>",
		Short: "Open workspace in browser",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := platform.NewClient()
			if err != nil {
				return err
			}

			resID := resourceID
			if resID == "" {
				cfg, _ := platform.LoadConfig()
				if cfg != nil {
					resID = cfg.DefaultResource
				}
			}
			if resID == "" {
				return fmt.Errorf("no resource specified (pass --resource or run 'cw setup')")
			}

			res, err := client.GetResource(resID)
			if err != nil {
				return fmt.Errorf("get resource: %w", err)
			}

			var domain string
			if m, ok := (*res.Metadata)["domain"].(string); ok {
				domain = m
			}
			if domain == "" {
				return fmt.Errorf("resource has no domain")
			}

			wsURL := fmt.Sprintf("https://%s/@admin/%s", domain, args[0])
			fmt.Printf("Opening %s\n", wsURL)
			return openBrowser(wsURL)
		},
	}

	cmd.Flags().StringVar(&resourceID, "resource", "", "Resource ID (default: from config)")
	return cmd
}

func workspaceStartCmd() *cobra.Command {
	var resourceID string

	cmd := &cobra.Command{
		Use:   "start <workspace-id>",
		Short: "Start a stopped workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, resID, err := resolveResourceClient(resourceID)
			if err != nil {
				return err
			}
			if err := client.StartWorkspace(resID, args[0]); err != nil {
				return fmt.Errorf("start workspace: %w", err)
			}
			fmt.Println("Workspace starting.")
			return nil
		},
	}

	cmd.Flags().StringVar(&resourceID, "resource", "", "Resource ID (default: from config)")
	return cmd
}

func workspaceStopCmd() *cobra.Command {
	var resourceID string

	cmd := &cobra.Command{
		Use:   "stop <workspace-id>",
		Short: "Stop a running workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, resID, err := resolveResourceClient(resourceID)
			if err != nil {
				return err
			}
			if err := client.StopWorkspace(resID, args[0]); err != nil {
				return fmt.Errorf("stop workspace: %w", err)
			}
			fmt.Println("Workspace stopping.")
			return nil
		},
	}

	cmd.Flags().StringVar(&resourceID, "resource", "", "Resource ID (default: from config)")
	return cmd
}

func workspacesListCmd() *cobra.Command {
	var (
		resourceID string
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "workspaces",
		Short: "List workspaces on a resource",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, resID, err := resolveResourceClient(resourceID)
			if err != nil {
				return err
			}

			resp, err := client.ListWorkspaces(resID)
			if err != nil {
				return fmt.Errorf("list workspaces: %w", err)
			}

			if jsonOutput {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(resp.Workspaces)
			}

			if len(resp.Workspaces) == 0 {
				fmt.Println("No workspaces.")
				return nil
			}

			for _, ws := range resp.Workspaces {
				fmt.Printf("  %-20s %-10s %s\n", ws.Name, ws.Status, ws.TemplateDisplayName)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&resourceID, "resource", "", "Resource ID (default: from config)")
	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output as JSON")
	return cmd
}

// ── Helpers ──────────────────────────────────────────────────────────

func resolveResourceClient(resourceID string) (*platform.Client, string, error) {
	client, err := platform.NewClient()
	if err != nil {
		return nil, "", err
	}
	resID := resourceID
	if resID == "" {
		cfg, _ := platform.LoadConfig()
		if cfg != nil {
			resID = cfg.DefaultResource
		}
	}
	if resID == "" {
		return nil, "", fmt.Errorf("no resource specified (pass --resource or run 'cw setup')")
	}
	return client, resID, nil
}

func deriveWorkspaceName(repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil {
		// Fall back to simple parsing
		parts := strings.Split(repoURL, "/")
		name := parts[len(parts)-1]
		return strings.TrimSuffix(name, ".git")
	}
	name := path.Base(u.Path)
	return strings.TrimSuffix(name, ".git")
}

func openBrowser(url string) error {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "linux":
		cmd = "xdg-open"
	case "windows":
		cmd = "start"
	default:
		return fmt.Errorf("unsupported platform")
	}
	return exec.Command(cmd, url).Start()
}
