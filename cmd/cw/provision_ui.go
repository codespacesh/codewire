package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/codewiresh/codewire/internal/platform"
)

// provisionTimeline renders a live provisioning timeline to the terminal.
type provisionTimeline struct {
	phases    []phaseState
	podStatus *podStatusState
	startTime time.Time
	lineCount int
}

type phaseState struct {
	phase   string
	message string
	status  string // started, completed, failed
	started time.Time
	elapsed time.Duration
}

type podStatusState struct {
	podName      string
	podStatus    string
	restartCount int
	logTail      string
}

func newProvisionTimeline() *provisionTimeline {
	return &provisionTimeline{
		startTime: time.Now(),
	}
}

// handleEvent processes an incoming provision event and re-renders.
func (t *provisionTimeline) handleEvent(ev platform.ProvisionEvent) {
	if ev.Phase == "pod_status" {
		t.handlePodStatus(ev)
		t.render()
		return
	}

	switch ev.Status {
	case "started":
		t.phases = append(t.phases, phaseState{
			phase:   ev.Phase,
			message: ev.Message,
			status:  "started",
			started: time.Now(),
		})
	case "completed":
		for i := len(t.phases) - 1; i >= 0; i-- {
			if t.phases[i].phase == ev.Phase && t.phases[i].status == "started" {
				t.phases[i].status = "completed"
				t.phases[i].elapsed = time.Since(t.phases[i].started)
				break
			}
		}
	case "failed":
		for i := len(t.phases) - 1; i >= 0; i-- {
			if t.phases[i].phase == ev.Phase {
				t.phases[i].status = "failed"
				t.phases[i].elapsed = time.Since(t.phases[i].started)
				break
			}
		}
	}

	t.render()
}

func (t *provisionTimeline) handlePodStatus(ev platform.ProvisionEvent) {
	var meta map[string]any
	if len(ev.Metadata) > 0 {
		json.Unmarshal(ev.Metadata, &meta)
	}
	if meta == nil {
		return
	}

	ps := &podStatusState{}
	if v, ok := meta["pod_name"].(string); ok {
		ps.podName = v
	}
	if v, ok := meta["pod_status"].(string); ok {
		ps.podStatus = v
	}
	if v, ok := meta["restart_count"].(float64); ok {
		ps.restartCount = int(v)
	}
	if v, ok := meta["log_tail"].(string); ok {
		ps.logTail = v
	}
	t.podStatus = ps
}

// render clears previous output and redraws the timeline.
func (t *provisionTimeline) render() {
	// Move cursor up to clear previous render
	if t.lineCount > 0 {
		fmt.Fprintf(os.Stderr, "\033[%dA\033[J", t.lineCount)
	}

	var lines []string

	for _, p := range t.phases {
		var icon, timing string
		switch p.status {
		case "completed":
			icon = "  " + greenErr("✓")
			timing = fmt.Sprintf("%s", p.elapsed.Truncate(time.Second))
		case "failed":
			icon = "  " + redErr("✗")
			timing = fmt.Sprintf("FAILED (%s)", p.elapsed.Truncate(time.Second))
		case "started":
			icon = "  " + yellowErr("◌")
			timing = fmt.Sprintf("%s...", time.Since(p.started).Truncate(time.Second))
		}
		lines = append(lines, fmt.Sprintf("%s %-30s %s", icon, p.message, timing))
	}

	// Pod status
	if t.podStatus != nil {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf("  Pod: %s", t.podStatus.podName))
		statusLine := fmt.Sprintf("  Status: %s", t.podStatus.podStatus)
		if t.podStatus.restartCount > 0 {
			statusLine += fmt.Sprintf(" (%d restarts)", t.podStatus.restartCount)
		}
		lines = append(lines, statusLine)
		if t.podStatus.logTail != "" {
			logLines := strings.Split(t.podStatus.logTail, "\n")
			last := logLines[len(logLines)-1]
			if len(last) > 80 {
				last = last[:80] + "..."
			}
			lines = append(lines, fmt.Sprintf("  Last log: %s", last))
		}
	}

	output := strings.Join(lines, "\n") + "\n"
	fmt.Fprint(os.Stderr, output)
	t.lineCount = len(lines)
}

// total returns the total elapsed time.
func (t *provisionTimeline) total() time.Duration {
	return time.Since(t.startTime).Truncate(time.Second)
}

// failed returns true if any phase failed.
func (t *provisionTimeline) failed() bool {
	for _, p := range t.phases {
		if p.status == "failed" {
			return true
		}
	}
	return false
}
