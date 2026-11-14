package tmux

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ReadinessSnapshot is a fresh, bounded observation for keyboard readiness.
// Background work is deliberately separate from the foreground input state;
// this observation never rewrites the status shown by the TUI.
type ReadinessSnapshot struct {
	Status, Reason string
	RawPane        string
	PaneID         string
	BackgroundWork bool
}

// ProbeReadiness bypasses the status/cache warm-up pipeline. Both subprocesses
// share the caller's deadline, and Output waits for their exit: no polling
// goroutine or tmux client is left running after this method returns.
func (s *Session) ProbeReadiness(ctx context.Context) (ReadinessSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var snap ReadinessSnapshot
	run := func(args ...string) ([]byte, error) {
		cmd := s.tmuxCmdContext(ctx, args...)
		// A child retaining a pipe must not add the usual two-second drain
		// allowance to a short readiness budget.
		cmd.WaitDelay = 10 * time.Millisecond
		return commandOutput(cmd)
	}
	meta, err := run("display-message", "-p", "-t", "="+s.TmuxName()+":", "#{pane_id}\t#{pane_dead}\t#{pane_current_command}\t#{pane_title}")
	if err != nil {
		if captureGoneFromErr(err) {
			snap.Status, snap.Reason = "inactive", "pane_gone"
			return snap, nil
		}
		return snap, fmt.Errorf("readiness pane identity: %w", err)
	}
	fields := strings.SplitN(strings.TrimRight(string(meta), "\r\n"), "\t", 4)
	if len(fields) != 4 || !strings.HasPrefix(fields[0], "%") {
		return snap, fmt.Errorf("readiness pane identity: invalid response")
	}
	snap.PaneID = fields[0]
	if fields[1] == "1" {
		snap.Status, snap.Reason = "inactive", "pane_dead"
		return snap, nil
	}
	// Address the exact pane whose identity we just read, even if the active
	// window changes between the two probes.
	raw, err := run("capture-pane", "-p", "-e", "-t", snap.PaneID)
	if err != nil {
		snap.Reason = "capture_failed"
		return snap, fmt.Errorf("readiness capture: %w", err)
	}
	snap.RawPane = string(raw)
	return s.classifyReadiness(snap, fields[3], fields[2]), nil
}

func (s *Session) classifyReadiness(snap ReadinessSnapshot, title, command string) ReadinessSnapshot {
	content := StripANSI(snap.RawPane)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startupTimedOut {
		snap.Status, snap.Reason = "error", "startup_timed_out"
		return snap
	}
	if s.hasErrorBannerIndicator(content) || s.classifySubstate(content) == SubstateModelUnavailable {
		snap.Status, snap.Reason = "error", "terminal_error_banner"
		return snap
	}
	// Preserve startup's three-sighting protection in send.WaitForAgentReady.
	if !s.startupAt.IsZero() && !s.startupTimedOut {
		snap.Status, snap.Reason = "starting", "startup"
		return snap
	}
	snap.BackgroundWork = s.isClaudeTool() && ParseClaudeBackgroundWork(content).PaneProven().Running()
	if s.hasBusyIndicator(content) || AnalyzePaneTitle(title, command) == TitleStateWorking {
		snap.Status, snap.Reason = "active", "foreground_busy"
	} else if snap.BackgroundWork {
		snap.Status, snap.Reason = "active", "background_work"
	} else {
		snap.Status, snap.Reason = "waiting", "no_busy_indicator"
	}
	return snap
}
