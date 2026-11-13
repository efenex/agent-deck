package send

import (
	"context"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// WaitForForegroundReady uses freshly reloaded turn hooks, not the aggregate
// session status (which includes background work). Without a fresh hook it
// requires positive current input-region evidence. Unknown never means idle.
// probe must honor ctx; the real tmux implementation uses bounded subprocesses.
func WaitForForegroundReady(
	hook func() (string, bool),
	probe func(context.Context) (tmux.ReadinessSnapshot, error),
	tool string, gates PromptGates, timeout time.Duration,
) error {
	if timeout <= 0 {
		timeout = DefaultAgentReadyTimeout
	}
	started := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), started.Add(timeout))
	defer cancel()
	diag := &ReadinessError{Stage: "defer_foreground", StatusCounts: map[string]int{}}
	for {
		status, fresh := hook()
		diag.ComposerPresent = nil
		diag.LastReason = "fresh_foreground_hook"
		diag.LastStatusReason = "fresh_foreground_hook"
		ready := fresh && (status == "waiting" || status == "idle")
		if !fresh {
			snap, err := probe(ctx)
			diag.PaneID = snap.PaneID
			status, diag.LastReason = snap.Status, snap.Reason
			diag.LastStatusReason = snap.Reason
			if err != nil {
				if snap.Reason == "capture_failed" {
					diag.CaptureErrors++
				} else {
					diag.StatusErrors++
				}
				diag.LastReason = err.Error()
			} else if status != "inactive" && status != "error" && status != "starting" {
				present, reason := readyPrompt(snap.RawPane, tool, gates)
				diag.ComposerPresent = &present
				ready = reason == "" && (status == "waiting" || status == "idle" || snap.Reason == "background_work")
				if reason != "" {
					diag.LastReason = reason
				}
			}
		}
		diag.LastStatus = status
		diag.StatusCounts[status]++
		if ctx.Err() == nil && ready {
			return nil
		}
		if status == "inactive" || status == "stopped" || status == "error" || !waitReadyInterval(ctx, DeferPollInterval) {
			diag.ElapsedMS = time.Since(started).Milliseconds()
			return diag
		}
	}
}
