package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/codewiresh/codewire/internal/client"
	"github.com/codewiresh/codewire/internal/platform"
)

func platformListCmd() *cobra.Command {
	var jsonOutput bool
	var statusFilter string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all workspaces and sessions",
		Long:  "In platform mode: show workspaces grouped by org/resource with session counts.\nIn standalone mode: list local sessions.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// If not in platform mode, fall back to local session list
			if !platform.HasConfig() {
				target, err := resolveTarget()
				if err != nil {
					return err
				}
				if target.IsLocal() {
					if err := ensureNode(); err != nil {
						return err
					}
				}
				return client.List(target, jsonOutput, statusFilter)
			}

			pc, err := platform.NewClient()
			if err != nil {
				return err
			}

			orgs, err := pc.ListOrgs()
			if err != nil {
				return fmt.Errorf("list orgs: %w", err)
			}

			// Get all session heartbeats
			sessions, _ := pc.ListSessions("", "")
			sessionIndex := buildSessionIndex(sessions)

			if jsonOutput {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"organizations": orgs,
					"sessions":      sessions,
				})
			}

			if len(orgs) == 0 {
				fmt.Println("No organizations found.")
				return nil
			}

			for _, org := range orgs {
				if len(org.Resources) == 0 {
					continue
				}

				for _, res := range org.Resources {
					if res.Type != "coder" {
						continue
					}

					healthTag := res.HealthStatus
					if healthTag == "" {
						healthTag = "unknown"
					}

					fmt.Printf("# %s / %s (%s)\n", org.Slug, res.Name, healthTag)

					// Get workspaces for this resource
					workspaces, err := pc.ListWorkspaces(res.ID)
					if err != nil || len(workspaces.Workspaces) == 0 {
						fmt.Println("  (no workspaces)")
						fmt.Println()
						continue
					}

					for _, ws := range workspaces.Workspaces {
						sessionCount, activeCount := countSessions(sessionIndex, res.ID, ws.ID)
						sessionInfo := ""
						if sessionCount > 0 {
							sessionInfo = fmt.Sprintf("%d sessions (%d active)", sessionCount, activeCount)
						} else {
							sessionInfo = "0 sessions"
						}
						fmt.Printf("  %-20s %-10s %s\n", ws.Name, ws.Status, sessionInfo)
					}
					fmt.Println()
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output as JSON")
	cmd.Flags().StringVar(&statusFilter, "status", "all", "Filter by status (standalone mode): all, running, completed, killed")
	_ = cmd.RegisterFlagCompletionFunc("status", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"all", "running", "completed", "killed"}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

type sessionKey struct {
	ResourceID  string
	WorkspaceID string
}

func buildSessionIndex(entries []platform.SessionEntry) map[sessionKey][]platform.SessionSnapshot {
	idx := make(map[sessionKey][]platform.SessionSnapshot)
	for _, e := range entries {
		if e.Stale {
			continue
		}
		key := sessionKey{ResourceID: e.ResourceID, WorkspaceID: e.WorkspaceID}
		idx[key] = e.Sessions
	}
	return idx
}

func countSessions(idx map[sessionKey][]platform.SessionSnapshot, resourceID, workspaceID string) (total, active int) {
	key := sessionKey{ResourceID: resourceID, WorkspaceID: workspaceID}
	sessions, ok := idx[key]
	if !ok {
		return 0, 0
	}
	total = len(sessions)
	for _, s := range sessions {
		if s.Status == "running" {
			active++
		}
	}
	return total, active
}
