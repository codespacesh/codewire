package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/codewiresh/codewire/internal/platform"
)

func orgsCreateCmd() *cobra.Command {
	var slug string

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new organization",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if slug == "" {
				slug = slugify(name)
			}

			pc, err := platform.NewClient()
			if err != nil {
				return err
			}

			org, err := pc.CreateOrg(&platform.CreateOrgRequest{
				Name: name,
				Slug: slug,
			})
			if err != nil {
				return fmt.Errorf("create org: %w", err)
			}

			fmt.Printf("Created organization %q (slug: %s)\n", org.Name, org.Slug)
			return nil
		},
	}

	cmd.Flags().StringVar(&slug, "slug", "", "URL-safe slug (default: auto-generated from name)")
	return cmd
}

func orgsDeleteCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "delete <id-or-slug>",
		Short: "Delete an organization",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pc, err := platform.NewClient()
			if err != nil {
				return err
			}

			// Resolve the org to get its name and ID
			orgID, err := resolveOrgID(pc, args[0])
			if err != nil {
				return err
			}

			if !yes {
				// Look up org name for confirmation
				orgs, err := pc.ListOrgs()
				if err != nil {
					return err
				}
				var orgName string
				for _, o := range orgs {
					if o.ID == orgID {
						orgName = o.Name
						break
					}
				}
				if orgName == "" {
					orgName = args[0]
				}
				if err := confirmDelete("organization", orgName); err != nil {
					return err
				}
			}

			if err := pc.DeleteOrg(orgID); err != nil {
				return fmt.Errorf("delete org: %w", err)
			}

			fmt.Println("Organization deleted.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompt")
	return cmd
}

func orgsInviteCmd() *cobra.Command {
	var role, orgFlag string

	cmd := &cobra.Command{
		Use:   "invite <email>",
		Short: "Invite a member to an organization",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			email := args[0]

			pc, err := platform.NewClient()
			if err != nil {
				return err
			}

			orgID, err := resolveOrgID(pc, orgFlag)
			if err != nil {
				return err
			}

			inv, err := pc.CreateInvitation(orgID, &platform.InviteMemberRequest{
				Email: email,
				Role:  role,
			})
			if err != nil {
				return fmt.Errorf("invite: %w", err)
			}

			fmt.Printf("Invited %s as %s\n", inv.Email, inv.Role)
			return nil
		},
	}

	cmd.Flags().StringVar(&role, "role", "member", "Role to assign (owner, admin, member)")
	cmd.Flags().StringVar(&orgFlag, "org", "", "Organization ID or slug (default: from config)")
	return cmd
}
