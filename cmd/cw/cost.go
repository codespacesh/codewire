package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/codewiresh/codewire/internal/platform"
)

func costCmd() *cobra.Command {
	var (
		jsonOutput bool
		orgFilter  string
	)

	cmd := &cobra.Command{
		Use:   "cost",
		Short: "Show usage and billing overview",
		Long:  "Display resource usage and billing information across organizations.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !platform.HasConfig() {
				return fmt.Errorf("not in platform mode (run 'cw setup')")
			}

			pc, err := platform.NewClient()
			if err != nil {
				return err
			}

			orgs, err := pc.ListOrgs()
			if err != nil {
				return fmt.Errorf("list orgs: %w", err)
			}

			if jsonOutput {
				return costJSON(pc, orgs, orgFilter)
			}

			for _, org := range orgs {
				if orgFilter != "" && org.Slug != orgFilter && org.ID != orgFilter {
					continue
				}
				if len(org.Resources) == 0 {
					continue
				}

				overview, err := pc.GetBillingOverview(org.ID)
				if err != nil {
					fmt.Printf("# %s (billing unavailable)\n\n", org.Slug)
					continue
				}

				planInfo := overview.PlanDisplayName
				if planInfo == "" {
					planInfo = overview.Plan
				}
				costStr := fmt.Sprintf("$%d/mo", overview.TotalMonthlyCostCents/100)

				fmt.Printf("# %s (%s — %s)\n", org.Slug, planInfo, costStr)
				fmt.Printf("  Status: %s", overview.Status)
				if overview.CurrentPeriodEnd != nil {
					fmt.Printf("    Period ends: %s", *overview.CurrentPeriodEnd)
				}
				fmt.Println()

				if overview.IncludedDevs > 0 {
					extra := ""
					if overview.ExtraSeatPriceCents > 0 {
						extra = fmt.Sprintf(" ($%d/extra)", overview.ExtraSeatPriceCents/100)
					}
					fmt.Printf("  Seats: %d/%d included%s\n", overview.SeatCount, overview.IncludedDevs, extra)
				}
				fmt.Println()

				w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
				fmt.Fprintf(w, "  Resource\tCPU hrs\tMem GB·hrs\tDisk GB·hrs\tOverage\n")

				var totalCPU, totalMem, totalDisk float64
				var totalOverageCents int

				for _, res := range org.Resources {
					if res.Type != "coder" {
						continue
					}

					usage, err := pc.GetResourceUsage(res.ID)
					if err != nil {
						fmt.Fprintf(w, "  %s\t-\t-\t-\t-\n", res.Name)
						continue
					}

					fmt.Fprintf(w, "  %s\t%.1f\t%.1f\t%.1f\t$%.2f\n",
						res.Name, usage.CPUHours, usage.MemoryGBHours, usage.DiskGBHours,
						float64(usage.Overage.TotalCents)/100)

					totalCPU += usage.CPUHours
					totalMem += usage.MemoryGBHours
					totalDisk += usage.DiskGBHours
					totalOverageCents += usage.Overage.TotalCents
				}

				fmt.Fprintf(w, "  \t─────\t──────────\t───────────\t───────\n")
				fmt.Fprintf(w, "  Total\t%.1f\t%.1f\t%.1f\t$%.2f\n",
					totalCPU, totalMem, totalDisk, float64(totalOverageCents)/100)
				w.Flush()

				estimated := float64(overview.TotalMonthlyCostCents+totalOverageCents) / 100
				fmt.Printf("\n  Estimated total this month: $%.2f\n\n", estimated)
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output as JSON")
	cmd.Flags().StringVar(&orgFilter, "org", "", "Filter to a specific organization (slug or ID)")
	return cmd
}

func costJSON(pc *platform.Client, orgs []platform.OrgWithRole, orgFilter string) error {
	type orgCost struct {
		Org     platform.OrgWithRole              `json:"org"`
		Billing *platform.BillingOverview          `json:"billing,omitempty"`
		Usage   map[string]*platform.ResourceUsage `json:"usage,omitempty"`
	}

	var results []orgCost
	for _, org := range orgs {
		if orgFilter != "" && org.Slug != orgFilter && org.ID != orgFilter {
			continue
		}
		entry := orgCost{Org: org, Usage: map[string]*platform.ResourceUsage{}}
		if overview, err := pc.GetBillingOverview(org.ID); err == nil {
			entry.Billing = overview
		}
		for _, res := range org.Resources {
			if res.Type != "coder" {
				continue
			}
			if usage, err := pc.GetResourceUsage(res.ID); err == nil {
				entry.Usage[res.ID] = usage
			}
		}
		results = append(results, entry)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}
