package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/mattn/go-isatty"
)

var (
	stderrColor bool
	stdoutColor bool
)

func init() {
	stderrColor = isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())
	stdoutColor = isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
	if os.Getenv("NO_COLOR") != "" {
		stderrColor = false
		stdoutColor = false
	}
}

// ANSI helpers — all check stdoutColor before emitting codes.

func bold(s string) string {
	if !stdoutColor {
		return s
	}
	return "\033[1m" + s + "\033[0m"
}

func dim(s string) string {
	if !stdoutColor {
		return s
	}
	return "\033[2m" + s + "\033[0m"
}

func green(s string) string {
	if !stdoutColor {
		return s
	}
	return "\033[32m" + s + "\033[0m"
}

func red(s string) string {
	if !stdoutColor {
		return s
	}
	return "\033[31m" + s + "\033[0m"
}

func yellow(s string) string {
	if !stdoutColor {
		return s
	}
	return "\033[33m" + s + "\033[0m"
}

// Stderr-targeted color helpers.

func greenErr(s string) string {
	if !stderrColor {
		return s
	}
	return "\033[32m" + s + "\033[0m"
}

func redErr(s string) string {
	if !stderrColor {
		return s
	}
	return "\033[31m" + s + "\033[0m"
}

func yellowErr(s string) string {
	if !stderrColor {
		return s
	}
	return "\033[33m" + s + "\033[0m"
}

// stateColor applies color to a state label for stdout.
func stateColor(state string) string {
	switch state {
	case "running", "started", "healthy":
		return green(state)
	case "error", "failed", "unhealthy":
		return red(state)
	case "creating", "pending", "provisioning", "starting", "stopping":
		return yellow(state)
	case "stopped", "destroyed":
		return dim(state)
	default:
		return state
	}
}

// successMsg prints "  ✓ message" to stderr with a green checkmark.
func successMsg(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "  %s %s\n", greenErr("✓"), msg)
}

// tableHeader writes a bold header row to a tabwriter (stdout).
func tableHeader(w *tabwriter.Writer, cols ...string) {
	bolded := make([]string, len(cols))
	for i, c := range cols {
		bolded[i] = bold(c)
	}
	for i, c := range bolded {
		if i > 0 {
			fmt.Fprint(w, "\t")
		}
		fmt.Fprint(w, c)
	}
	fmt.Fprintln(w)
}
