package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/codewiresh/codewire/internal/platform"
)

func tmplParentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "template",
		Short:   "Manage environment templates",
		Aliases: []string{"tmpl"},
	}
	cmd.AddCommand(templateListCmd())
	cmd.AddCommand(templateCreateCmd())
	cmd.AddCommand(templateInfoCmd())
	cmd.AddCommand(templateRmCmd())
	return cmd
}

func templateListCmd() *cobra.Command {
	var envType string

	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List environment templates",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			templates, err := client.ListEnvTemplates(orgID, envType)
			if err != nil {
				return fmt.Errorf("list templates: %w", err)
			}

			if len(templates) == 0 {
				fmt.Println("No templates found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			tableHeader(w, "NAME", "LANGUAGE", "IMAGE", "OFFICIAL", "CPU/MEM/DISK")
			for _, t := range templates {
				slug := "--"
				if t.Slug != nil {
					slug = *t.Slug
				}
				lang := "--"
				if t.Language != nil && *t.Language != "" {
					lang = *t.Language
				}
				official := ""
				if t.Official {
					official = "yes"
				}
				resources := fmt.Sprintf("%dm/%dMB/%dGB", t.DefaultCPUMillicores, t.DefaultMemoryMB, t.DefaultDiskGB)
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					slug, lang, t.Name, official, resources)
			}
			return w.Flush()
		},
	}

	cmd.Flags().StringVar(&envType, "type", "", "Filter by type (coder, sandbox)")
	return cmd
}

func templateCreateCmd() *cobra.Command {
	var (
		name          string
		image         string
		install       string
		startup       string
		cpu           int
		memory        int
		disk          int
		ttl           string
		description   string
		secretProject string
	)

	cmd := &cobra.Command{
		Use:   "create <slug>",
		Short: "Create an environment template",
		Long: `Create a template with a short name (slug) for reuse.

Examples:
  cw template create my-app --image go --install "go mod download"
  cw template create my-app --image go --secrets my-project`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]

			if image == "" {
				return fmt.Errorf("--image is required")
			}

			// Image shorthand: expand bare names (e.g. "full" → ghcr.io/codewiresh/full:latest).
			if image != "" && !containsSlash(image) {
				image = expandImageRef(image)
			}

			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			req := &platform.CreateTemplateRequest{
				Type:          "sandbox",
				Name:          slug,
				Slug:          slug,
				Description:   description,
				Image:         image,
				InstallCommand: install,
				StartupScript: startup,
				SecretProject: secretProject,
			}
			if cpu > 0 {
				req.DefaultCPUMillicores = &cpu
			}
			if memory > 0 {
				req.DefaultMemoryMB = &memory
			}
			if disk > 0 {
				req.DefaultDiskGB = &disk
			}
			if ttl != "" {
				d, err := time.ParseDuration(ttl)
				if err != nil {
					return fmt.Errorf("invalid --ttl duration: %w", err)
				}
				secs := int(d.Seconds())
				req.DefaultTTLSeconds = &secs
			}

			if name != "" {
				req.Name = name
			}

			tmpl, err := client.CreateEnvTemplate(orgID, req)
			if err != nil {
				return fmt.Errorf("create template: %w", err)
			}

			successMsg("Template created: %s.", tmpl.Name)
			fmt.Printf("  ID:    %s\n", tmpl.ID)
			fmt.Printf("  Type:  %s\n", tmpl.Type)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Display name (defaults to slug)")
	cmd.Flags().StringVar(&image, "image", "", "Container image (shorthand: go, node, python)")
	cmd.Flags().StringVar(&install, "install", "", "Default install command")
	cmd.Flags().StringVar(&startup, "startup", "", "Default startup script")
	cmd.Flags().IntVar(&cpu, "cpu", 0, "Default CPU in millicores")
	cmd.Flags().IntVar(&memory, "memory", 0, "Default memory in MB")
	cmd.Flags().IntVar(&disk, "disk", 0, "Default disk in GB")
	cmd.Flags().StringVar(&ttl, "ttl", "", "Default TTL (e.g. 1h, 30m)")
	cmd.Flags().StringVar(&description, "description", "", "Template description")
	cmd.Flags().StringVar(&secretProject, "secrets", "", "Default secret project to bind")
	return cmd
}

func templateInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <slug-or-id>",
		Short: "Show template details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			// Try to find by slug first by listing all templates.
			templates, err := client.ListEnvTemplates(orgID, "")
			if err != nil {
				return fmt.Errorf("list templates: %w", err)
			}

			var tmpl *platform.EnvironmentTemplate
			for i, t := range templates {
				if t.ID == args[0] || (t.Slug != nil && *t.Slug == args[0]) {
					tmpl = &templates[i]
					break
				}
			}
			if tmpl == nil {
				return fmt.Errorf("template not found: %s", args[0])
			}

			slug := "--"
			if tmpl.Slug != nil {
				slug = *tmpl.Slug
			}
			lang := "--"
			if tmpl.Language != nil && *tmpl.Language != "" {
				lang = *tmpl.Language
			}

			fmt.Printf("%-10s %s\n", bold("ID:"), dim(tmpl.ID))
			fmt.Printf("%-10s %s\n", bold("Name:"), tmpl.Name)
			fmt.Printf("%-10s %s\n", bold("Slug:"), slug)
			fmt.Printf("%-10s %s\n", bold("Type:"), tmpl.Type)
			fmt.Printf("%-10s %s\n", bold("Language:"), lang)
			fmt.Printf("%-10s %v\n", bold("Official:"), tmpl.Official)
			fmt.Printf("%-10s %s\n", bold("Build:"), tmpl.BuildStatus)
			fmt.Printf("CPU:      %dm\n", tmpl.DefaultCPUMillicores)
			fmt.Printf("Memory:   %dMB\n", tmpl.DefaultMemoryMB)
			fmt.Printf("Disk:     %dGB\n", tmpl.DefaultDiskGB)
			return nil
		},
	}
}

func templateRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <id>",
		Short:   "Delete an environment template",
		Aliases: []string{"delete"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			orgID, client, err := getDefaultOrg()
			if err != nil {
				return err
			}

			if err := client.DeleteEnvTemplate(orgID, args[0]); err != nil {
				return fmt.Errorf("delete template: %w", err)
			}
			successMsg("Template %s deleted.", args[0])
			return nil
		},
	}
}

func containsSlash(s string) bool {
	for _, c := range s {
		if c == '/' {
			return true
		}
	}
	return false
}
