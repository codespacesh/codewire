package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/codewiresh/codewire/internal/platform"
)

// showCurrentWorkspace prints the current workspace status.
func showCurrentWorkspace() error {
	if !platform.HasConfig() {
		fmt.Println("Not in platform mode. Run 'cw setup' to connect to a Codewire server.")
		return nil
	}

	ws := platform.GetCurrentWorkspace()
	if ws == "" {
		fmt.Println("No active workspace. Run 'cw launch <repo-url>' to create one,")
		fmt.Println("or 'cw <name>' to switch to an existing workspace.")
		return nil
	}

	pc, err := platform.NewClient()
	if err != nil {
		return err
	}

	cfg, _ := platform.LoadConfig()
	if cfg == nil || cfg.DefaultResource == "" {
		fmt.Printf("  %s (no default resource configured)\n", ws)
		return nil
	}

	workspaces, err := pc.ListWorkspaces(cfg.DefaultResource)
	if err != nil {
		fmt.Printf("  %s (could not fetch status)\n", ws)
		return nil
	}

	for _, w := range workspaces.Workspaces {
		if w.Name == ws {
			fmt.Printf("  %s (%s)\n", w.Name, w.Status)
			return nil
		}
	}

	fmt.Printf("  %s (not found on current resource)\n", ws)
	return nil
}

// switchWorkspace validates the workspace exists and sets it as current.
func switchWorkspace(name string, jsonOutput bool) error {
	if !platform.HasConfig() {
		return fmt.Errorf("not in platform mode (run 'cw setup')")
	}

	pc, err := platform.NewClient()
	if err != nil {
		return err
	}

	cfg, _ := platform.LoadConfig()
	if cfg == nil || cfg.DefaultResource == "" {
		return fmt.Errorf("no default resource configured (run 'cw setup')")
	}

	workspaces, err := pc.ListWorkspaces(cfg.DefaultResource)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}

	var found *platform.WorkspaceSummary
	for _, w := range workspaces.Workspaces {
		if w.Name == name {
			found = &w
			break
		}
	}
	if found == nil {
		return fmt.Errorf("workspace %q not found on resource %s", name, cfg.DefaultResource)
	}

	if err := platform.SetCurrentWorkspace(name); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(found)
	}

	fmt.Printf("Switched to workspace %q (%s)\n", found.Name, found.Status)
	return nil
}
