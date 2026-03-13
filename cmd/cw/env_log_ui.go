package main

import (
	"fmt"
	"os"
	"time"

	"github.com/codewiresh/codewire/internal/platform"
)

// followEnvironmentLogs connects to the SSE stream and prints each log event.
// Stops on terminal events (phase=="complete" or status=="failed").
func followEnvironmentLogs(client *platform.Client, orgID, envID string) error {
	events := make(chan platform.EnvironmentLog, 64)
	if err := client.StreamEnvironmentLogs(orgID, envID, events); err != nil {
		return fmt.Errorf("connect to log stream: %w", err)
	}

	phases := make(map[string]time.Time) // phase → start time

	for ev := range events {
		renderEnvLogEvent(ev, phases)
		if isTerminalEvent(ev) {
			return nil
		}
	}
	return nil
}

// renderEnvLogEvent prints a single log event as an append-only line.
func renderEnvLogEvent(ev platform.EnvironmentLog, phases map[string]time.Time) {
	switch ev.Status {
	case "started":
		phases[ev.Phase] = time.Now()
		fmt.Fprintf(os.Stderr, "  %s %s...\n", yellowErr("◌"), ev.Message)
	case "completed":
		elapsed := ""
		if start, ok := phases[ev.Phase]; ok {
			elapsed = fmt.Sprintf("  %s", time.Since(start).Truncate(time.Second))
		}
		fmt.Fprintf(os.Stderr, "  %s %s%s\n", greenErr("✓"), ev.Message, elapsed)
	case "warning":
		fmt.Fprintf(os.Stderr, "  %s %s\n", yellowErr("!"), ev.Message)
	case "failed":
		fmt.Fprintf(os.Stderr, "  %s %s\n", redErr("✗"), ev.Message)
	default:
		fmt.Fprintf(os.Stderr, "  · %s\n", ev.Message)
	}
}

// isTerminalEvent returns true if this event signals the end of the stream.
func isTerminalEvent(ev platform.EnvironmentLog) bool {
	return ev.Phase == "complete" || ev.Status == "failed"
}
