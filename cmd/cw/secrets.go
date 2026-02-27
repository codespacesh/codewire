package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/codewiresh/codewire/internal/platform"
)

func secretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage organization secrets",
	}
	cmd.AddCommand(secretsListCmd(), secretsSetCmd(), secretsDeleteCmd())
	return cmd
}

func secretsListCmd() *cobra.Command {
	var jsonOutput bool
	var orgID string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List secret keys (names only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := platform.NewClient()
			if err != nil {
				return err
			}

			oid, err := resolveOrgID(orgID)
			if err != nil {
				return err
			}

			secrets, err := client.ListSecrets(oid)
			if err != nil {
				return fmt.Errorf("list secrets: %w", err)
			}

			if jsonOutput {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(secrets)
			}

			if len(secrets) == 0 {
				fmt.Println("No secrets found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "KEY\tCREATED\tUPDATED")
			for _, s := range secrets {
				fmt.Fprintf(w, "%s\t%s\t%s\n", s.Key, s.CreatedAt, s.UpdatedAt)
			}
			return w.Flush()
		},
	}

	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output as JSON")
	cmd.Flags().StringVar(&orgID, "org", "", "Organization ID (defaults to configured org)")
	return cmd
}

func secretsSetCmd() *cobra.Command {
	var orgID string

	cmd := &cobra.Command{
		Use:   "set <KEY>",
		Short: "Set a secret (prompts for value)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := platform.NewClient()
			if err != nil {
				return err
			}

			oid, err := resolveOrgID(orgID)
			if err != nil {
				return err
			}

			key := args[0]
			value, err := promptPassword("Value: ")
			if err != nil {
				return err
			}
			if value == "" {
				return fmt.Errorf("value cannot be empty")
			}

			if err := client.SetSecret(oid, key, value); err != nil {
				return fmt.Errorf("set secret: %w", err)
			}

			fmt.Printf("Secret %s set.\n", key)
			return nil
		},
	}

	cmd.Flags().StringVar(&orgID, "org", "", "Organization ID (defaults to configured org)")
	return cmd
}

func secretsDeleteCmd() *cobra.Command {
	var orgID string

	cmd := &cobra.Command{
		Use:   "delete <KEY>",
		Short: "Delete a secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := platform.NewClient()
			if err != nil {
				return err
			}

			oid, err := resolveOrgID(orgID)
			if err != nil {
				return err
			}

			key := args[0]
			if err := client.DeleteSecret(oid, key); err != nil {
				return fmt.Errorf("delete secret: %w", err)
			}

			fmt.Printf("Secret %s deleted.\n", key)
			return nil
		},
	}

	cmd.Flags().StringVar(&orgID, "org", "", "Organization ID (defaults to configured org)")
	return cmd
}

// resolveOrgID returns the explicit org ID or falls back to the configured default.
func resolveOrgID(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	cfg, err := platform.LoadConfig()
	if err != nil {
		return "", fmt.Errorf("no --org flag and no default org configured (run 'cw setup')")
	}
	if cfg.DefaultOrg == "" {
		return "", fmt.Errorf("no --org flag and no default org configured (run 'cw setup')")
	}
	return cfg.DefaultOrg, nil
}
