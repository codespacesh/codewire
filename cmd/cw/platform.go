package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/codewiresh/codewire/internal/platform"
)

func loginCmd() *cobra.Command {
	var serverURL string
	var usePassword bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to Codewire",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Determine server URL
			url := serverURL
			if url == "" {
				cfg, err := platform.LoadConfig()
				if err != nil {
					return fmt.Errorf("no server configured (run 'cw setup' first, or pass --server)")
				}
				url = cfg.ServerURL
			}

			client := platform.NewClientWithURL(url)

			var displayName string

			if usePassword {
				name, err := loginWithPassword(client)
				if err != nil {
					return err
				}
				displayName = name
			} else {
				name, err := loginWithDevice(client)
				if err != nil {
					return err
				}
				displayName = name
			}

			// Save config
			cfg := &platform.PlatformConfig{
				ServerURL:    url,
				SessionToken: client.SessionToken,
			}
			// Preserve existing defaults if re-logging in
			if existing, err := platform.LoadConfig(); err == nil {
				cfg.DefaultOrg = existing.DefaultOrg
				cfg.DefaultResource = existing.DefaultResource
				cfg.CoderBinary = existing.CoderBinary
			}
			if err := platform.SaveConfig(cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}

			successMsg("Logged in as %s.", displayName)
			return nil
		},
	}

	cmd.Flags().StringVar(&serverURL, "server", "", "Codewire server URL")
	cmd.Flags().BoolVar(&usePassword, "password", false, "Use email/password login instead of browser")
	return cmd
}

func loginWithPassword(client *platform.Client) (string, error) {
	email, err := prompt("Email: ")
	if err != nil {
		return "", err
	}
	password, err := promptPassword("Password: ")
	if err != nil {
		return "", err
	}

	resp, err := client.Login(email, password)
	if err != nil {
		return "", fmt.Errorf("login failed: %w", err)
	}

	// Handle 2FA
	if resp.TwoFactorRequired {
		code, err := prompt("2FA Code: ")
		if err != nil {
			return "", err
		}
		authResp, err := client.ValidateTOTP(code, resp.TwoFactorToken)
		if err != nil {
			return "", fmt.Errorf("2FA validation failed: %w", err)
		}
		if authResp.Session == nil {
			return "", fmt.Errorf("no session returned after 2FA")
		}
	} else if resp.Session == nil {
		return "", fmt.Errorf("no session returned")
	}

	name := ""
	if resp.User != nil {
		name = resp.User.Name
		if name == "" {
			name = resp.User.Email
		}
	}
	return name, nil
}

func loginWithDevice(client *platform.Client) (string, error) {
	dauth, err := client.DeviceAuthorize()
	if err != nil {
		return "", fmt.Errorf("device auth failed: %w", err)
	}

	fmt.Println("Opening browser to authorize...")
	fmt.Printf("If browser doesn't open, visit: %s\n", dauth.VerificationURI)
	fmt.Printf("Your code: %s\n", dauth.UserCode)

	_ = openBrowser(dauth.VerificationURI)

	// Poll for approval
	interval := time.Duration(dauth.Interval) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	expiresIn := dauth.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		resp, statusCode, err := client.DeviceToken(dauth.DeviceCode)
		if err != nil {
			if statusCode == 410 {
				return "", fmt.Errorf("device code expired")
			}
			if statusCode == 403 {
				return "", fmt.Errorf("authorization denied")
			}
			// Network error or other transient failure, retry
			continue
		}

		if statusCode == 202 {
			// Still pending
			if resp.Status == "slow_down" {
				interval *= 2
			}
			continue
		}

		// Success — verify token was actually set
		if client.SessionToken == "" {
			return "", fmt.Errorf("device auth approved but no session token received (status %d, session_token=%q)", statusCode, resp.SessionToken)
		}

		if resp.User != nil {
			name := resp.User.Name
			if name == "" {
				name = resp.User.Email
			}
			return name, nil
		}
		return "(unknown)", nil
	}

	return "", fmt.Errorf("timed out waiting for authorization")
}

func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Sign out of Codewire",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := platform.NewClient()
			if err != nil {
				// Not logged in, just clean up config
				_ = platform.DeleteConfig()
				successMsg("Logged out.")
				return nil
			}
			_ = client.Logout()
			_ = platform.DeleteConfig()
			successMsg("Logged out.")
			return nil
		},
	}
}

func whoamiCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show current user and server",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := platform.NewClient()
			if err != nil {
				return err
			}

			resp, err := client.GetSession()
			if err != nil {
				return fmt.Errorf("session check failed: %w", err)
			}
			if resp.User == nil {
				return fmt.Errorf("not logged in (session expired?)")
			}

			if jsonOutput {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(resp.User)
			}

			cfg, _ := platform.LoadConfig()
			fmt.Printf("%-10s %s (%s)\n", bold("User:"), resp.User.Name, resp.User.Email)
			fmt.Printf("%-10s %s\n", bold("Server:"), client.ServerURL)
			if cfg != nil && cfg.DefaultOrg != "" {
				fmt.Printf("%-10s %s\n", bold("Org:"), cfg.DefaultOrg)
			}
			if cfg != nil && cfg.DefaultResource != "" {
				fmt.Printf("%-10s %s\n", bold("Resource:"), cfg.DefaultResource)
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output as JSON")
	return cmd
}

func orgsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "orgs",
		Short: "Manage organizations",
	}
	cmd.AddCommand(orgsListCmd(), orgsCreateCmd(), orgsDeleteCmd(), orgsInviteCmd())
	return cmd
}

func orgsListCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List organizations",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := platform.NewClient()
			if err != nil {
				return err
			}

			orgs, err := client.ListOrgs()
			if err != nil {
				return fmt.Errorf("list orgs: %w", err)
			}

			if jsonOutput {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(orgs)
			}

			if len(orgs) == 0 {
				fmt.Println("No organizations found.")
				return nil
			}

			cfg, _ := platform.LoadConfig()
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			tableHeader(w, "NAME", "SLUG", "ROLE", "RESOURCES")
			for _, org := range orgs {
				marker := ""
				if cfg != nil && cfg.DefaultOrg == org.ID {
					marker = " *"
				}
				fmt.Fprintf(w, "%s%s\t%s\t%s\t%d\n", org.Name, marker, org.Slug, org.Role, len(org.Resources))
			}
			return w.Flush()
		},
	}

	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output as JSON")
	return cmd
}

func resourcesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resources",
		Short: "Manage resources",
	}
	cmd.AddCommand(resourcesListCmd(), resourcesGetCmd(), resourcesCreateCmd(), resourcesDeleteCmd(), resourceStatusCmd())
	return cmd
}

func resourcesListCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all resources",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := platform.NewClient()
			if err != nil {
				return err
			}

			resources, err := client.ListResources()
			if err != nil {
				return fmt.Errorf("list resources: %w", err)
			}

			if jsonOutput {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(resources)
			}

			if len(resources) == 0 {
				fmt.Println("No resources found.")
				return nil
			}

			cfg, _ := platform.LoadConfig()
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			tableHeader(w, "NAME", "SLUG", "TYPE", "STATUS", "HEALTH")
			for _, r := range resources {
				marker := ""
				if cfg != nil && cfg.DefaultResource == r.ID {
					marker = " *"
				}
				fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%s\n", r.Name, marker, r.Slug, r.Type, stateColor(r.Status), stateColor(r.HealthStatus))
			}
			return w.Flush()
		},
	}

	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output as JSON")
	return cmd
}

func resourcesGetCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "get <id-or-slug>",
		Short: "Get resource details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := platform.NewClient()
			if err != nil {
				return err
			}

			resource, err := client.GetResource(args[0])
			if err != nil {
				return fmt.Errorf("get resource: %w", err)
			}

			if jsonOutput {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(resource)
			}

			fmt.Printf("%-10s %s\n", bold("Name:"), resource.Name)
			fmt.Printf("%-10s %s\n", bold("Slug:"), resource.Slug)
			fmt.Printf("%-10s %s\n", bold("Type:"), resource.Type)
			fmt.Printf("%-10s %s\n", bold("Status:"), stateColor(resource.Status))
			fmt.Printf("%-10s %s\n", bold("Health:"), stateColor(resource.HealthStatus))
			fmt.Printf("%-10s %s\n", bold("Plan:"), resource.BillingPlan)
			fmt.Printf("%-10s %s\n", bold("Created:"), resource.CreatedAt)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output as JSON")
	return cmd
}
