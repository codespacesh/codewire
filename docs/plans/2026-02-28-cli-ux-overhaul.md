# CLI UX Overhaul Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add workspace-noun-first syntax (`cw api run -- cmd`) and full org/resource/billing CRUD to the `cw` CLI so users never need the web UI.

**Architecture:** Pre-cobra arg interception in `main()` checks if `os.Args[1]` is an unknown command; if so, shifts it into a package-level `workspaceOverride` variable that workspace-aware commands check first. New CRUD commands follow the existing `platform.Client.do()` HTTP pattern. A shared `slugify()` helper auto-generates slugs. Delete confirmations require typing the resource name.

**Tech Stack:** Go 1.23+, cobra, `internal/platform` HTTP client

**Repo:** `~/src/codewire/codewire-cli/`

---

### Task 1: Workspace prefix interception + `isKnownCommand` helper

**Files:**
- Modify: `cmd/cw/main.go:26-32` (add `workspaceOverride` var)
- Modify: `cmd/cw/main.go:113-116` (add interception before `rootCmd.Execute()`)
- Create: `cmd/cw/workspace_prefix_test.go`

**Step 1: Write the test**

Create `cmd/cw/workspace_prefix_test.go`:

```go
package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestIsKnownCommand(t *testing.T) {
	root := &cobra.Command{Use: "cw"}
	root.AddCommand(&cobra.Command{Use: "run", Aliases: []string{}})
	root.AddCommand(&cobra.Command{Use: "list"})
	root.AddCommand(&cobra.Command{Use: "stop", Aliases: []string{"halt"}})
	root.AddCommand(&cobra.Command{Use: "orgs"})
	root.AddCommand(&cobra.Command{Use: "launch"})

	tests := []struct {
		name string
		want bool
	}{
		{"run", true},
		{"list", true},
		{"stop", true},
		{"halt", true},  // alias
		{"orgs", true},
		{"launch", true},
		{"api", false},        // workspace name
		{"my-project", false}, // workspace name
		{"help", true},        // built-in
		{"version", true},     // built-in
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isKnownCommand(root, tt.name)
			if got != tt.want {
				t.Errorf("isKnownCommand(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd ~/src/codewire/codewire-cli && go test ./cmd/cw/ -run TestIsKnownCommand -v`
Expected: FAIL — `isKnownCommand` undefined

**Step 3: Implement `isKnownCommand` and workspace interception**

In `cmd/cw/main.go`, add the package-level variable after the existing `var` block at line 26:

```go
var (
	version = "dev"

	serverFlag        string
	tokenFlag         string
	workspaceOverride string // set by workspace prefix interception (e.g. "cw api run")
)
```

Add this function anywhere in `main.go` (e.g. after `main()`):

```go
// isKnownCommand checks if name matches any registered cobra command or alias.
func isKnownCommand(root *cobra.Command, name string) bool {
	// Built-in cobra commands
	if name == "help" || name == "version" || name == "completion" {
		return true
	}
	for _, cmd := range root.Commands() {
		if cmd.Name() == name {
			return true
		}
		for _, alias := range cmd.Aliases {
			if alias == name {
				return true
			}
		}
	}
	return false
}
```

In `main()`, replace lines 113-116 with:

```go
	// Workspace prefix interception: "cw api run -- cmd" → workspaceOverride="api"
	// Only when: platform mode, >= 3 args, first arg is not a known command or flag.
	if len(os.Args) >= 3 && platform.HasConfig() {
		candidate := os.Args[1]
		if !strings.HasPrefix(candidate, "-") && !isKnownCommand(rootCmd, candidate) {
			workspaceOverride = candidate
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
```

**Step 4: Run test to verify it passes**

Run: `cd ~/src/codewire/codewire-cli && go test ./cmd/cw/ -run TestIsKnownCommand -v`
Expected: PASS

**Step 5: Verify build**

Run: `cd ~/src/codewire/codewire-cli && go build ./cmd/cw/ && go vet ./cmd/cw/`
Expected: No errors

**Step 6: Commit**

```bash
cd ~/src/codewire/codewire-cli
git add cmd/cw/main.go cmd/cw/workspace_prefix_test.go
git commit -m "feat: add workspace prefix arg interception"
```

---

### Task 2: `resolveWorkspace` helper + workspace validation

**Files:**
- Modify: `cmd/cw/workspace_context.go`
- Modify: `cmd/cw/workspace_prefix_test.go` (add test)

**Step 1: Write the test**

Add to `cmd/cw/workspace_prefix_test.go`:

```go
func TestResolveWorkspaceName(t *testing.T) {
	// Test priority: override > explicit > current
	orig := workspaceOverride
	defer func() { workspaceOverride = orig }()

	workspaceOverride = "from-override"
	name := resolveWorkspaceName("from-explicit")
	if name != "from-override" {
		t.Errorf("expected override, got %q", name)
	}

	workspaceOverride = ""
	name = resolveWorkspaceName("from-explicit")
	if name != "from-explicit" {
		t.Errorf("expected explicit, got %q", name)
	}

	workspaceOverride = ""
	name = resolveWorkspaceName("")
	if name != "" {
		t.Errorf("expected empty, got %q", name)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd ~/src/codewire/codewire-cli && go test ./cmd/cw/ -run TestResolveWorkspaceName -v`
Expected: FAIL — `resolveWorkspaceName` undefined

**Step 3: Implement `resolveWorkspaceName`**

Add to `cmd/cw/workspace_context.go` (after existing functions):

```go
// resolveWorkspaceName returns the workspace name from:
// 1. workspaceOverride (set by "cw api run" prefix interception)
// 2. explicit positional arg
// 3. empty string (caller should fall back to GetCurrentWorkspace or error)
func resolveWorkspaceName(explicit string) string {
	if workspaceOverride != "" {
		return workspaceOverride
	}
	if explicit != "" {
		return explicit
	}
	return ""
}
```

**Step 4: Run test to verify it passes**

Run: `cd ~/src/codewire/codewire-cli && go test ./cmd/cw/ -run TestResolveWorkspaceName -v`
Expected: PASS

**Step 5: Commit**

```bash
cd ~/src/codewire/codewire-cli
git add cmd/cw/workspace_context.go cmd/cw/workspace_prefix_test.go
git commit -m "feat: add resolveWorkspaceName helper"
```

---

### Task 3: Wire `workspaceOverride` into `run` command

**Files:**
- Modify: `cmd/cw/main.go:210-230` (runCmd platform mode section)

**Step 1: Update the platform-mode branch in `runCmd`**

In `cmd/cw/main.go`, find the `runCmd()` function. Replace the platform-mode block (around lines 210-230) that currently reads:

```go
		// Platform mode: run in remote workspace via coder ssh
		if platform.HasConfig() && platform.GetCurrentWorkspace() != "" {
			dash := cmd.ArgsLenAtDash()
			if dash == -1 {
				return fmt.Errorf("command required after --\n\nUsage: cw run [workspace] -- <command> [args...]")
			}

			// Parse: cw run [workspace] -- command...
			wsName := platform.GetCurrentWorkspace()
			if dash >= 1 {
				wsName = args[0] // explicit workspace override
			}
```

With:

```go
		// Platform mode: run in remote workspace via coder ssh
		if platform.HasConfig() {
			dash := cmd.ArgsLenAtDash()
			if dash == -1 {
				return fmt.Errorf("command required after --\n\nUsage: cw run [workspace] -- <command> [args...]")
			}

			// Parse: cw run [workspace] -- command...
			// Priority: workspaceOverride > positional arg > current workspace
			explicit := ""
			if dash >= 1 {
				explicit = args[0]
			}
			wsName := resolveWorkspaceName(explicit)
			if wsName == "" {
				wsName = platform.GetCurrentWorkspace()
			}
			if wsName == "" {
				return fmt.Errorf("no workspace specified\n\nUsage: cw <workspace> run -- <command>\n   or: cw run <workspace> -- <command>\n   or: set current workspace with 'cw <name>'")
			}
```

**Step 2: Verify build**

Run: `cd ~/src/codewire/codewire-cli && go build ./cmd/cw/ && go vet ./cmd/cw/`
Expected: No errors

**Step 3: Commit**

```bash
cd ~/src/codewire/codewire-cli
git add cmd/cw/main.go
git commit -m "feat: wire workspaceOverride into run command"
```

---

### Task 4: Wire `workspaceOverride` into open, start, stop commands

**Files:**
- Modify: `cmd/cw/workspaces.go` (openCmd, workspaceStartCmd, workspaceStopCmd)

**Step 1: Update `openCmd`**

In `cmd/cw/workspaces.go`, find `openCmd()`. Replace the workspace resolution block:

```go
		wsName := ""
		if len(args) > 0 {
			wsName = args[0]
		} else {
			wsName = platform.GetCurrentWorkspace()
			if wsName == "" {
				return fmt.Errorf("no workspace specified and no current workspace set")
			}
		}
```

With:

```go
		explicit := ""
		if len(args) > 0 {
			explicit = args[0]
		}
		wsName := resolveWorkspaceName(explicit)
		if wsName == "" {
			wsName = platform.GetCurrentWorkspace()
		}
		if wsName == "" {
			return fmt.Errorf("no workspace specified\n\nUsage: cw <workspace> open\n   or: cw open <workspace>")
		}
```

**Step 2: Update `workspaceStartCmd`**

Same pattern — replace the workspace resolution in `workspaceStartCmd()`:

```go
		explicit := ""
		if len(args) > 0 {
			explicit = args[0]
		}
		wsName := resolveWorkspaceName(explicit)
		if wsName == "" {
			wsName = platform.GetCurrentWorkspace()
		}
		if wsName == "" {
			return fmt.Errorf("no workspace specified\n\nUsage: cw <workspace> start\n   or: cw start <workspace>")
		}
```

**Step 3: Update `workspaceStopCmd`**

Same pattern for `workspaceStopCmd()`:

```go
		explicit := ""
		if len(args) > 0 {
			explicit = args[0]
		}
		wsName := resolveWorkspaceName(explicit)
		if wsName == "" {
			wsName = platform.GetCurrentWorkspace()
		}
		if wsName == "" {
			return fmt.Errorf("no workspace specified\n\nUsage: cw <workspace> stop\n   or: cw stop <workspace>")
		}
```

**Step 4: Verify build**

Run: `cd ~/src/codewire/codewire-cli && go build ./cmd/cw/ && go vet ./cmd/cw/`
Expected: No errors

**Step 5: Commit**

```bash
cd ~/src/codewire/codewire-cli
git add cmd/cw/workspaces.go
git commit -m "feat: wire workspaceOverride into open/start/stop commands"
```

---

### Task 5: `cw list` with workspace-prefix drill-down

**Files:**
- Modify: `cmd/cw/platform_list.go`

**Step 1: Add workspace-specific session listing**

In `cmd/cw/platform_list.go`, at the top of the `RunE` function (inside the platform mode branch, right after the `!platform.HasConfig()` check), add a check for `workspaceOverride`:

```go
			// Platform mode
			pc, err := platform.NewClient()
			if err != nil {
				return err
			}

			// If workspace override is set (e.g. "cw api list"), show sessions for that workspace
			if workspaceOverride != "" {
				return listWorkspaceSessions(pc, workspaceOverride, jsonOutput)
			}
```

Add the `listWorkspaceSessions` function at the bottom of the file:

```go
func listWorkspaceSessions(pc *platform.Client, wsName string, jsonOutput bool) error {
	cfg, err := platform.LoadConfig()
	if err != nil {
		return err
	}
	if cfg.DefaultResource == "" {
		return fmt.Errorf("no default resource configured (run 'cw setup')")
	}

	// Verify workspace exists
	workspaces, err := pc.ListWorkspaces(cfg.DefaultResource)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	var wsID string
	for _, ws := range workspaces.Workspaces {
		if ws.Name == wsName {
			wsID = ws.ID
			break
		}
	}
	if wsID == "" {
		return fmt.Errorf("workspace %q not found", wsName)
	}

	sessions, err := pc.ListWorkspaceSessions(cfg.DefaultResource, wsID)
	if err != nil {
		// Session listing may fail if workspace has no agent — show empty
		sessions = nil
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sessions)
	}

	if len(sessions) == 0 {
		fmt.Printf("No sessions in workspace %q.\n", wsName)
		return nil
	}

	fmt.Printf("ID  %-36s  %-10s  %s\n", "COMMAND", "STATUS", "NAME")
	for _, s := range sessions {
		cmd := s.Command
		if len(cmd) > 36 {
			cmd = cmd[:33] + "..."
		}
		name := s.Name
		if name == "" {
			name = "—"
		}
		fmt.Printf("%-3d %-36s  %-10s  %s\n", s.ID, cmd, s.Status, name)
	}
	return nil
}
```

**Step 2: Update the unified list to mark current workspace**

In the existing workspace listing loop (around line 90-98), update the workspace name display to mark current:

```go
				currentWs := platform.GetCurrentWorkspace()
				for _, ws := range workspaces.Workspaces {
					sessionCount, activeCount := countSessions(sessionIndex, res.ID, ws.ID)
					marker := " "
					if ws.Name == currentWs {
						marker = "*"
					}
					sessionInfo := ""
					if sessionCount > 0 {
						sessionInfo = fmt.Sprintf("%d sessions (%d active)", sessionCount, activeCount)
					} else {
						sessionInfo = "0 sessions"
					}
					fmt.Printf("  %s %-19s %-10s %s\n", marker, ws.Name, ws.Status, sessionInfo)
				}
```

**Step 3: Verify build**

Run: `cd ~/src/codewire/codewire-cli && go build ./cmd/cw/ && go vet ./cmd/cw/`
Expected: No errors

**Step 4: Commit**

```bash
cd ~/src/codewire/codewire-cli
git add cmd/cw/platform_list.go
git commit -m "feat: cw list shows unified view, cw <ws> list drills into sessions"
```

---

### Task 6: Shared helpers — `slugify`, `resolveOrgID`, `confirmDelete`

**Files:**
- Create: `cmd/cw/platform_helpers.go`
- Create: `cmd/cw/platform_helpers_test.go`

**Step 1: Write the test**

Create `cmd/cw/platform_helpers_test.go`:

```go
package main

import "testing"

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Acme Corp", "acme-corp"},
		{"My Cool Project", "my-cool-project"},
		{"  hello  world  ", "hello-world"},
		{"foo--bar", "foo-bar"},
		{"UPPER CASE", "upper-case"},
		{"special!@#chars", "special-chars"},
		{"-leading-trailing-", "leading-trailing"},
		{"a", "a"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := slugify(tt.input)
			if got != tt.want {
				t.Errorf("slugify(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd ~/src/codewire/codewire-cli && go test ./cmd/cw/ -run TestSlugify -v`
Expected: FAIL — `slugify` undefined

**Step 3: Implement helpers**

Create `cmd/cw/platform_helpers.go`:

```go
package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/codewiresh/codewire/internal/platform"
)

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// slugify converts a name to a URL-safe slug.
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = nonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	// Collapse consecutive hyphens
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if len(s) > 48 {
		s = s[:48]
		s = strings.TrimRight(s, "-")
	}
	return s
}

// resolveOrgID resolves an org ID from a flag value or config default.
// If orgFlag is a slug, it looks up the ID via ListOrgs.
func resolveOrgID(pc *platform.Client, orgFlag string) (string, error) {
	if orgFlag == "" {
		cfg, err := platform.LoadConfig()
		if err != nil {
			return "", fmt.Errorf("no org specified (pass --org or run 'cw setup')")
		}
		if cfg.DefaultOrg == "" {
			return "", fmt.Errorf("no default org configured (pass --org or run 'cw setup')")
		}
		return cfg.DefaultOrg, nil
	}

	// Could be an ID (UUID) or slug — try listing orgs to resolve
	orgs, err := pc.ListOrgs()
	if err != nil {
		return "", fmt.Errorf("list orgs: %w", err)
	}
	for _, org := range orgs {
		if org.ID == orgFlag || org.Slug == orgFlag {
			return org.ID, nil
		}
	}
	return "", fmt.Errorf("organization %q not found", orgFlag)
}

// confirmDelete prompts the user to type the name to confirm deletion.
// Returns nil if confirmed, error otherwise.
func confirmDelete(resourceType, name string) error {
	fmt.Printf("Type %q to confirm deletion of %s %q: ", name, resourceType, name)
	input, err := prompt("")
	if err != nil {
		return err
	}
	if strings.TrimSpace(input) != name {
		return fmt.Errorf("confirmation failed — aborting")
	}
	return nil
}
```

Note: the `confirmDelete` function re-uses `prompt("")` from `cmd/cw/prompt.go` but prints its own label directly. Fix the call — use a raw stdin read instead:

Actually, looking at `prompt()`, it prints the label then reads. So `confirmDelete` should just do:

```go
func confirmDelete(resourceType, name string) error {
	input, err := prompt(fmt.Sprintf("Type %q to confirm deletion of %s %q: ", name, resourceType, name))
	if err != nil {
		return err
	}
	if strings.TrimSpace(input) != name {
		return fmt.Errorf("confirmation failed — aborting")
	}
	return nil
}
```

**Step 4: Run test to verify it passes**

Run: `cd ~/src/codewire/codewire-cli && go test ./cmd/cw/ -run TestSlugify -v`
Expected: PASS

**Step 5: Verify build**

Run: `cd ~/src/codewire/codewire-cli && go build ./cmd/cw/ && go vet ./cmd/cw/`
Expected: No errors

**Step 6: Commit**

```bash
cd ~/src/codewire/codewire-cli
git add cmd/cw/platform_helpers.go cmd/cw/platform_helpers_test.go
git commit -m "feat: add slugify, resolveOrgID, confirmDelete helpers"
```

---

### Task 7: Org CRUD — types + client methods

**Files:**
- Modify: `internal/platform/types.go`
- Modify: `internal/platform/orgs.go`

**Step 1: Add new types**

Add to `internal/platform/types.go` after the `OrgWithRole` definition:

```go
type CreateOrgRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type OrgInvitation struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

type InviteMemberRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}
```

**Step 2: Add client methods**

Add to `internal/platform/orgs.go`:

```go
// CreateOrg creates a new organization.
func (c *Client) CreateOrg(req *CreateOrgRequest) (*Organization, error) {
	var org Organization
	if err := c.do("POST", "/api/v1/organizations", req, &org); err != nil {
		return nil, err
	}
	return &org, nil
}

// DeleteOrg deletes an organization by ID.
func (c *Client) DeleteOrg(orgID string) error {
	return c.do("DELETE", "/api/v1/organizations/"+orgID, nil, nil)
}

// CreateInvitation invites a member to an organization.
func (c *Client) CreateInvitation(orgID string, req *InviteMemberRequest) (*OrgInvitation, error) {
	var inv OrgInvitation
	if err := c.do("POST", "/api/v1/organizations/"+orgID+"/invitations", req, &inv); err != nil {
		return nil, err
	}
	return &inv, nil
}
```

**Step 3: Verify build**

Run: `cd ~/src/codewire/codewire-cli && go build ./... && go vet ./...`
Expected: No errors

**Step 4: Commit**

```bash
cd ~/src/codewire/codewire-cli
git add internal/platform/types.go internal/platform/orgs.go
git commit -m "feat: add org CRUD client methods and types"
```

---

### Task 8: Org CRUD — CLI commands

**Files:**
- Create: `cmd/cw/platform_mutate.go`
- Modify: `cmd/cw/platform.go:159-166` (wire subcommands into `orgsCmd`)

**Step 1: Create the command file**

Create `cmd/cw/platform_mutate.go`:

```go
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
```

**Step 2: Wire into `orgsCmd`**

In `cmd/cw/platform.go`, replace the `orgsCmd()` function:

```go
func orgsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "orgs",
		Short: "Manage organizations",
	}
	cmd.AddCommand(orgsListCmd(), orgsCreateCmd(), orgsDeleteCmd(), orgsInviteCmd())
	return cmd
}
```

**Step 3: Verify build**

Run: `cd ~/src/codewire/codewire-cli && go build ./cmd/cw/ && go vet ./cmd/cw/`
Expected: No errors

**Step 4: Commit**

```bash
cd ~/src/codewire/codewire-cli
git add cmd/cw/platform_mutate.go cmd/cw/platform.go
git commit -m "feat: add cw orgs create/delete/invite commands"
```

---

### Task 9: Resource CRUD — types + client methods

**Files:**
- Modify: `internal/platform/types.go`
- Modify: `internal/platform/resources.go`

**Step 1: Add new types**

Add to `internal/platform/types.go`:

```go
type CreateResourceRequest struct {
	OrgID string `json:"orgId"`
	Type  string `json:"type"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	Plan  string `json:"plan,omitempty"`
}

type CreateResourceResult struct {
	PlatformResource
	CheckoutURL string `json:"checkout_url,omitempty"`
}
```

**Step 2: Add client methods**

Add to `internal/platform/resources.go`:

```go
// CreateResource creates a new resource.
func (c *Client) CreateResource(req *CreateResourceRequest) (*CreateResourceResult, error) {
	var result CreateResourceResult
	if err := c.do("POST", "/api/v1/resources", req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DeleteResource deletes a resource by ID or slug.
func (c *Client) DeleteResource(idOrSlug string) error {
	return c.do("DELETE", "/api/v1/resources/"+idOrSlug, nil, nil)
}
```

**Step 3: Verify build**

Run: `cd ~/src/codewire/codewire-cli && go build ./... && go vet ./...`
Expected: No errors

**Step 4: Commit**

```bash
cd ~/src/codewire/codewire-cli
git add internal/platform/types.go internal/platform/resources.go
git commit -m "feat: add resource CRUD client methods and types"
```

---

### Task 10: Resource CRUD — CLI commands

**Files:**
- Modify: `cmd/cw/platform_mutate.go` (append commands)
- Modify: `cmd/cw/platform.go` (wire into `resourcesCmd`)

**Step 1: Add resource commands**

Append to `cmd/cw/platform_mutate.go`:

```go
func resourcesCreateCmd() *cobra.Command {
	var name, slug, orgFlag, resType, plan string

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new resource",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if resType == "" {
				return fmt.Errorf("--type is required (coder, codewire-relay)")
			}
			if slug == "" {
				slug = slugify(name)
			}

			pc, err := platform.NewClient()
			if err != nil {
				return err
			}

			orgID, err := resolveOrgID(pc, orgFlag)
			if err != nil {
				return err
			}

			result, err := pc.CreateResource(&platform.CreateResourceRequest{
				OrgID: orgID,
				Type:  resType,
				Name:  name,
				Slug:  slug,
				Plan:  plan,
			})
			if err != nil {
				return fmt.Errorf("create resource: %w", err)
			}

			fmt.Printf("Created resource %q (slug: %s, status: %s)\n", result.Name, result.Slug, result.Status)

			if result.CheckoutURL != "" {
				fmt.Printf("Checkout URL: %s\n", result.CheckoutURL)
				fmt.Println("Opening in browser...")
				_ = openBrowser(result.CheckoutURL)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Resource name (required)")
	cmd.Flags().StringVar(&slug, "slug", "", "URL-safe slug (default: auto-generated from name)")
	cmd.Flags().StringVar(&orgFlag, "org", "", "Organization ID or slug (default: from config)")
	cmd.Flags().StringVar(&resType, "type", "", "Resource type: coder, codewire-relay (required)")
	cmd.Flags().StringVar(&plan, "plan", "", "Billing plan (optional)")
	return cmd
}

func resourcesDeleteCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "delete <id-or-slug>",
		Short: "Delete a resource",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				// Resolve name for confirmation
				pc, err := platform.NewClient()
				if err != nil {
					return err
				}
				resource, err := pc.GetResource(args[0])
				if err != nil {
					return fmt.Errorf("resource not found: %w", err)
				}
				if err := confirmDelete("resource", resource.Name); err != nil {
					return err
				}
			}

			pc, err := platform.NewClient()
			if err != nil {
				return err
			}
			if err := pc.DeleteResource(args[0]); err != nil {
				return fmt.Errorf("delete resource: %w", err)
			}

			fmt.Println("Resource deleted.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompt")
	return cmd
}
```

**Step 2: Wire into `resourcesCmd`**

In `cmd/cw/platform.go`, update `resourcesCmd()`:

```go
func resourcesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resources",
		Short: "Manage resources",
	}
	cmd.AddCommand(resourcesListCmd(), resourcesGetCmd(), resourcesCreateCmd(), resourcesDeleteCmd())
	return cmd
}
```

**Step 3: Verify build**

Run: `cd ~/src/codewire/codewire-cli && go build ./cmd/cw/ && go vet ./cmd/cw/`
Expected: No errors

**Step 4: Commit**

```bash
cd ~/src/codewire/codewire-cli
git add cmd/cw/platform_mutate.go cmd/cw/platform.go
git commit -m "feat: add cw resources create/delete commands"
```

---

### Task 11: Billing checkout command

**Files:**
- Create: `cmd/cw/billing_cmd.go`
- Modify: `internal/platform/billing.go` (new types + method)
- Modify: `internal/platform/types.go` (new types)
- Modify: `cmd/cw/main.go` (wire `billingCmd()` into root)

**Step 1: Add types and client method**

Add to `internal/platform/types.go`:

```go
type ResourceCheckoutRequest struct {
	Plan       string `json:"plan"`
	SuccessURL string `json:"success_url"`
	CancelURL  string `json:"cancel_url"`
}

type CheckoutURLResponse struct {
	CheckoutURL string `json:"checkout_url"`
}
```

Add to `internal/platform/billing.go`:

```go
func (c *Client) CreateResourceCheckout(resourceID string, req *ResourceCheckoutRequest) (*CheckoutURLResponse, error) {
	var resp CheckoutURLResponse
	err := c.do("POST", fmt.Sprintf("/api/v1/resources/%s/billing/checkout", resourceID), req, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}
```

**Step 2: Create billing command**

Create `cmd/cw/billing_cmd.go`:

```go
package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/codewiresh/codewire/internal/platform"
)

func billingCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "billing",
		Short: "Billing and checkout",
	}
	cmd.AddCommand(billingCheckoutCmd())
	return cmd
}

func billingCheckoutCmd() *cobra.Command {
	var plan string

	cmd := &cobra.Command{
		Use:   "checkout <resource-id-or-slug>",
		Short: "Open Stripe checkout for a resource",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if plan == "" {
				return fmt.Errorf("--plan is required (starter, pro, team)")
			}

			pc, err := platform.NewClient()
			if err != nil {
				return err
			}

			cfg, _ := platform.LoadConfig()
			dashboardURL := cfg.ServerURL + "/dashboard"

			resp, err := pc.CreateResourceCheckout(args[0], &platform.ResourceCheckoutRequest{
				Plan:       plan,
				SuccessURL: dashboardURL,
				CancelURL:  dashboardURL,
			})
			if err != nil {
				return fmt.Errorf("checkout: %w", err)
			}

			fmt.Printf("Opening checkout for plan %q...\n", plan)
			fmt.Printf("URL: %s\n", resp.CheckoutURL)
			_ = openBrowser(resp.CheckoutURL)
			return nil
		},
	}

	cmd.Flags().StringVar(&plan, "plan", "", "Billing plan: starter, pro, team (required)")
	return cmd
}
```

**Step 3: Wire into main**

In `cmd/cw/main.go`, add `billingCmd()` to the `rootCmd.AddCommand(...)` call, in the Platform section after `costCmd()`:

```go
		costCmd(),
		billingCmd(),
```

**Step 4: Verify build**

Run: `cd ~/src/codewire/codewire-cli && go build ./cmd/cw/ && go vet ./cmd/cw/`
Expected: No errors

**Step 5: Commit**

```bash
cd ~/src/codewire/codewire-cli
git add cmd/cw/billing_cmd.go cmd/cw/main.go internal/platform/billing.go internal/platform/types.go
git commit -m "feat: add cw billing checkout command"
```

---

### Task 12: Update homepage terminal demo

**Files:**
- Modify: `~/src/codewire/codewire/apps/web/src/routes/index.tsx`

**Step 1: Update the terminal demo**

In `apps/web/src/routes/index.tsx`, find the terminal demo div (the one with the three colored dots and monospace font). Replace its content to show the full onboarding + workspace prefix flow:

```tsx
              <div>
                <span style={{ color: "var(--accent)" }}>$</span>{" "}
                <span style={{ color: "var(--fg-primary)" }}>cw setup</span>
              </div>
              <div style={{ color: "var(--fg-tertiary)" }}>[1/4] Server URL [https://codewire.sh]: </div>
              <div style={{ color: "var(--fg-tertiary)" }}>      Connected to https://codewire.sh</div>
              <div style={{ color: "var(--fg-tertiary)" }}>[2/4] Sign in</div>
              <div style={{ color: "var(--fg-tertiary)" }}>[3/4] Organization: Acme Corp (owner)</div>
              <div style={{ color: "var(--fg-tertiary)" }}>[4/4] Resource: production (coder, running)</div>
              <br />
              <div>
                <span style={{ color: "var(--accent)" }}>$</span>{" "}
                <span style={{ color: "var(--fg-primary)" }}>cw launch github.com/acme/api</span>
              </div>
              <div style={{ color: "var(--fg-tertiary)" }}>Creating workspace "api"...</div>
              <div style={{ color: "var(--fg-tertiary)" }}>Waiting for workspace to start... <span style={{ color: "var(--success)" }}>running</span></div>
              <br />
              <div>
                <span style={{ color: "var(--accent)" }}>$</span>{" "}
                <span style={{ color: "var(--fg-primary)" }}>cw api run -- claude -p "fix the auth bug in src/middleware"</span>
              </div>
              <div style={{ color: "var(--fg-tertiary)" }}>▸ session 1 started</div>
              <br />
              <div>
                <span style={{ color: "var(--accent)" }}>$</span>{" "}
                <span style={{ color: "var(--fg-primary)" }}>cw api list</span>
              </div>
              <div style={{ display: "grid", gridTemplateColumns: "2.5rem 20rem 5.5rem", color: "var(--fg-tertiary)", fontSize: "12px" }}>
                <span>ID</span><span>COMMAND</span><span>STATUS</span>
              </div>
              <div style={{ display: "grid", gridTemplateColumns: "2.5rem 20rem 5.5rem", fontSize: "12px" }}>
                <span>1</span><span>claude -p "fix the auth..."</span><span style={{ color: "var(--success)" }}>running</span>
              </div>
```

**Step 2: Verify the web app builds**

Run: `cd ~/src/codewire/codewire/apps/web && npx tsc --noEmit 2>&1 | head -5` (or just check the dev server if running)

**Step 3: Commit**

```bash
cd ~/src/codewire/codewire
git add apps/web/src/routes/index.tsx
git commit -m "feat: update homepage terminal demo for workspace prefix syntax"
```

---

## Files Changed Summary

| File | Action |
|------|--------|
| `cmd/cw/main.go` | `workspaceOverride` var, arg interception, `isKnownCommand`, `billingCmd()` wiring |
| `cmd/cw/workspace_prefix_test.go` | New — tests for `isKnownCommand` and `resolveWorkspaceName` |
| `cmd/cw/workspace_context.go` | Add `resolveWorkspaceName()` |
| `cmd/cw/workspaces.go` | Use `resolveWorkspaceName()` in open/start/stop |
| `cmd/cw/platform_list.go` | Workspace drill-down, current workspace marker |
| `cmd/cw/platform_helpers.go` | New — `slugify`, `resolveOrgID`, `confirmDelete` |
| `cmd/cw/platform_helpers_test.go` | New — `TestSlugify` |
| `cmd/cw/platform_mutate.go` | New — org create/delete/invite, resource create/delete |
| `cmd/cw/platform.go` | Wire subcommands into `orgsCmd`/`resourcesCmd` |
| `cmd/cw/billing_cmd.go` | New — `billing checkout` command |
| `internal/platform/types.go` | 6 new types |
| `internal/platform/orgs.go` | `CreateOrg`, `DeleteOrg`, `CreateInvitation` |
| `internal/platform/resources.go` | `CreateResource`, `DeleteResource` |
| `internal/platform/billing.go` | `CreateResourceCheckout` |
| `apps/web/src/routes/index.tsx` | Updated terminal demo |

## Verification

After all tasks, run:

1. `cd ~/src/codewire/codewire-cli && go build ./... && go vet ./...` — compiles clean
2. `go test ./cmd/cw/ -v` — all tests pass
3. `go test ./internal/... -v` — all existing tests still pass
4. Manual test (requires running docker compose):
   - `./cw setup` → wizard completes
   - `./cw orgs create "Test Org"` → org created
   - `./cw orgs list` → shows Test Org
   - `./cw resources create --type coder --name test-coder` → resource created
   - `./cw resources list` → shows test-coder
   - `./cw workspaces` → lists workspaces
   - `./cw list` → unified view
   - `./cw nonexistent run -- cmd` → "workspace not found"
   - `./cw orgs delete test-org --yes` → deleted
