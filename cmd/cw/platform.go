package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/codewiresh/codewire/internal/platform"
)

func loginCmd() *cobra.Command {
	var serverURL string

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

			// Read credentials
			email, err := prompt("Email: ")
			if err != nil {
				return err
			}
			password, err := promptPassword("Password: ")
			if err != nil {
				return err
			}

			resp, err := client.Login(email, password)
			if err != nil {
				return fmt.Errorf("login failed: %w", err)
			}

			// Handle 2FA
			if resp.TwoFactorRequired {
				code, err := prompt("2FA Code: ")
				if err != nil {
					return err
				}
				authResp, err := client.ValidateTOTP(code, resp.TwoFactorToken)
				if err != nil {
					return fmt.Errorf("2FA validation failed: %w", err)
				}
				if authResp.Session == nil {
					return fmt.Errorf("no session returned after 2FA")
				}
			} else if resp.Session == nil {
				return fmt.Errorf("no session returned")
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

			name := ""
			if resp.User != nil {
				name = resp.User.Name
				if name == "" {
					name = resp.User.Email
				}
			}
			fmt.Printf("Logged in as %s\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&serverURL, "server", "", "Codewire server URL")
	return cmd
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
				fmt.Println("Logged out.")
				return nil
			}
			_ = client.Logout()
			_ = platform.DeleteConfig()
			fmt.Println("Logged out.")
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
			fmt.Printf("User:   %s (%s)\n", resp.User.Name, resp.User.Email)
			fmt.Printf("Server: %s\n", client.ServerURL)
			if cfg != nil && cfg.DefaultOrg != "" {
				fmt.Printf("Org:    %s\n", cfg.DefaultOrg)
			}
			if cfg != nil && cfg.DefaultResource != "" {
				fmt.Printf("Resource: %s\n", cfg.DefaultResource)
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
			fmt.Fprintln(w, "NAME\tSLUG\tROLE\tRESOURCES")
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
	cmd.AddCommand(resourcesListCmd(), resourcesGetCmd(), resourcesCreateCmd(), resourcesDeleteCmd())
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
			fmt.Fprintln(w, "NAME\tSLUG\tTYPE\tSTATUS\tHEALTH")
			for _, r := range resources {
				marker := ""
				if cfg != nil && cfg.DefaultResource == r.ID {
					marker = " *"
				}
				fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%s\n", r.Name, marker, r.Slug, r.Type, r.Status, r.HealthStatus)
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

			fmt.Printf("Name:     %s\n", resource.Name)
			fmt.Printf("Slug:     %s\n", resource.Slug)
			fmt.Printf("Type:     %s\n", resource.Type)
			fmt.Printf("Status:   %s\n", resource.Status)
			fmt.Printf("Health:   %s\n", resource.HealthStatus)
			fmt.Printf("Plan:     %s\n", resource.BillingPlan)
			fmt.Printf("Created:  %s\n", resource.CreatedAt)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output as JSON")
	return cmd
}
