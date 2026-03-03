package main

import (
	"fmt"
	"os"
	"time"

	"github.com/codewiresh/codewire/internal/platform"
)

// handleCheckoutAndWait opens a browser for Stripe checkout, polls for payment
// completion, then waits for the resource to reach "running" status.
// If the resource doesn't require checkout, it skips straight to provisioning wait.
func handleCheckoutAndWait(client *platform.Client, result *platform.CreateResourceResult) error {
	if result.RequiresCheckout && result.CheckoutURL != "" {
		fmt.Println()
		fmt.Println("  Opening browser for payment...")
		fmt.Printf("  URL: %s\n", result.CheckoutURL)
		_ = openBrowser(result.CheckoutURL)

		fmt.Print("  Waiting for checkout...")
		resource, err := client.WaitForCheckout(result.ID, 2*time.Second, 5*time.Minute)
		if err != nil {
			fmt.Println()
			return fmt.Errorf("checkout not completed: %w\n  Run `cw setup` to try again", err)
		}

		if resource.BillingStatus == "active" || resource.BillingStatus == "trialing" {
			fmt.Println(" done")
		} else {
			fmt.Printf(" %s\n", resource.BillingStatus)
		}
	}

	fmt.Printf("  Provisioning %q (%s)\n\n", result.Name, result.Type)

	timeline := newProvisionTimeline()

	// Try SSE first, fall back to polling
	events := make(chan platform.ProvisionEvent, 64)
	sseErr := client.StreamProvisionEvents(result.ID, events)

	if sseErr != nil {
		// Fallback: poll with phase display
		fmt.Print("  Waiting for provisioning...")
		resource, err := client.WaitForResource(result.ID, "running", 5*time.Second, 10*time.Minute)
		if err != nil {
			fmt.Println()
			return fmt.Errorf("provisioning failed: %w\n  Check status with: cw resources get %s", err, result.ID)
		}
		_ = resource
		fmt.Printf(" ready (%s)\n", timeline.total())
		return nil
	}

	// SSE path: render live timeline
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			timeline.handleEvent(ev)
			if ev.Phase == "complete" && ev.Status == "completed" {
				return
			}
			if ev.Phase == "error" && ev.Status == "failed" {
				return
			}
		}
	}()

	// Wait for either SSE completion or timeout
	select {
	case <-done:
	case <-time.After(10 * time.Minute):
		fmt.Fprintf(os.Stderr, "\n  Timed out after 10 minutes.\n")
		return fmt.Errorf("provisioning timed out\n  Check status with: cw resources get %s", result.ID)
	}

	fmt.Println()
	if timeline.failed() {
		return fmt.Errorf("provisioning failed (%s)\n  Run `cw resources retry %s` to retry", timeline.total(), result.Slug)
	}

	fmt.Printf("  Ready! (%s)\n", timeline.total())
	return nil
}

// promptConfirm asks a yes/no question with a default of yes.
func promptConfirm(label string) (bool, error) {
	answer, err := promptDefault(label, "Y")
	if err != nil {
		return false, err
	}
	switch answer {
	case "Y", "y", "yes", "Yes", "YES":
		return true, nil
	default:
		return false, nil
	}
}
