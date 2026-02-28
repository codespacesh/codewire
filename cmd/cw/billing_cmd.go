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
