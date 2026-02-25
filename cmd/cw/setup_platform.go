package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/codewiresh/codewire/internal/platform"
)

func platformSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Interactive Codewire setup wizard",
		Long:  "Connect to a Codewire server, sign in, and select your default organization and resource.",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Welcome to Codewire!")
			fmt.Println()

			// [1/4] Server URL
			defaultURL := "https://codewire.sh"
			if existing, err := platform.LoadConfig(); err == nil {
				defaultURL = existing.ServerURL
			}

			serverURL, err := promptDefault("[1/4] Server URL", defaultURL)
			if err != nil {
				return err
			}

			client := platform.NewClientWithURL(serverURL)

			// Check connectivity
			if err := client.Healthz(); err != nil {
				return fmt.Errorf("cannot connect to %s: %w", serverURL, err)
			}
			fmt.Printf("      Connected to %s\n", serverURL)
			fmt.Println()

			// [2/4] Login
			fmt.Println("[2/4] Sign in")
			email, err := prompt("      Email: ")
			if err != nil {
				return err
			}
			password, err := promptPassword("      Password: ")
			if err != nil {
				return err
			}

			resp, err := client.Login(email, password)
			if err != nil {
				return fmt.Errorf("login failed: %w", err)
			}

			if resp.TwoFactorRequired {
				code, err := prompt("      2FA Code: ")
				if err != nil {
					return err
				}
				authResp, err := client.ValidateTOTP(code, resp.TwoFactorToken)
				if err != nil {
					return fmt.Errorf("2FA failed: %w", err)
				}
				if authResp.User != nil {
					displayName := authResp.User.Name
					if displayName == "" {
						displayName = authResp.User.Email
					}
					fmt.Printf("      Logged in as %s\n", displayName)
				}
			} else {
				if resp.User != nil {
					displayName := resp.User.Name
					if displayName == "" {
						displayName = resp.User.Email
					}
					fmt.Printf("      Logged in as %s\n", displayName)
				}
			}
			fmt.Println()

			// [3/4] Select organization
			orgs, err := client.ListOrgs()
			if err != nil {
				return fmt.Errorf("list orgs: %w", err)
			}

			var selectedOrg platform.OrgWithRole
			if len(orgs) == 0 {
				fmt.Println("[3/4] No organizations found. Create one at your Codewire dashboard.")
				fmt.Println()
			} else if len(orgs) == 1 {
				selectedOrg = orgs[0]
				fmt.Printf("[3/4] Organization: %s (%s)\n", selectedOrg.Name, selectedOrg.Role)
				fmt.Println()
			} else {
				options := make([]string, len(orgs))
				for i, org := range orgs {
					options[i] = fmt.Sprintf("%s (%s, %d resources)", org.Name, org.Role, len(org.Resources))
				}
				idx, err := promptSelect("[3/4] Select organization:", options)
				if err != nil {
					return err
				}
				selectedOrg = orgs[idx]
				fmt.Printf("      Default org: %s\n", selectedOrg.Name)
				fmt.Println()
			}

			// [4/4] Select resource
			var selectedResourceID string
			if selectedOrg.ID != "" && len(selectedOrg.Resources) > 0 {
				resources := selectedOrg.Resources
				if len(resources) == 1 {
					selectedResourceID = resources[0].ID
					fmt.Printf("[4/4] Resource: %s (%s, %s)\n", resources[0].Name, resources[0].Type, resources[0].Status)
				} else {
					options := make([]string, len(resources))
					for i, r := range resources {
						options[i] = fmt.Sprintf("%-20s %-12s %-10s %s", r.Name, r.Type, r.Status, r.HealthStatus)
					}
					idx, err := promptSelect("[4/4] Select resource:", options)
					if err != nil {
						return err
					}
					selectedResourceID = resources[idx].ID
					fmt.Printf("      Default resource: %s\n", resources[idx].Name)
				}
			} else if selectedOrg.ID != "" {
				fmt.Println("[4/4] No resources in this organization.")
			}
			fmt.Println()

			// Save config
			cfg := &platform.PlatformConfig{
				ServerURL:       serverURL,
				SessionToken:    client.SessionToken,
				DefaultOrg:      selectedOrg.ID,
				DefaultResource: selectedResourceID,
			}
			if err := platform.SaveConfig(cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}

			fmt.Println("All set! Try:")
			fmt.Println("  cw whoami                          # Check your identity")
			fmt.Println("  cw orgs list                       # List organizations")
			fmt.Println("  cw resources list                  # List resources")
			return nil
		},
	}
}
