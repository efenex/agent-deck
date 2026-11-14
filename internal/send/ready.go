package send

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// DefaultAgentReadyTimeout is the readiness budget for launch / StartWithMessage.
// Matches `session send --timeout` default so cold-start TUIs (Cursor, Claude+MCP)
// are not cut off early.
const DefaultAgentReadyTimeout = 10 * time.Minute

// startupPromptConfirmations is how many consecutive polls must see the tool
// prompt before the startup-window bypass below accepts it.
//
// A prompt drawn during the startup window is not proof the tool will keep
// what we type next. Claude Code paints its composer box early and then keeps
// mounting - MCP servers connecting, hooks firing, the remote-control bridge
// dialing - and keystrokes delivered into that window can be dropped as the
// input component remounts. Because the send is a single tmux send-keys of the
// whole message, what survives is a *tail*: the caller then presses Enter on a
// fragment, the agent answers the fragment, and the session looks entirely
// healthy afterwards. verifyPromptConsumedAfterLaunch cannot catch it either -
// it polls until the composer is rendered and holds no draft, i.e. until
// *something* was submitted, and never compares what landed against what was
// sent, so a submitted fragment reads as a clean delivery.
//
// Observed on a Claude launch carrying a 2183-byte --message-file: 88 bytes
// arrived, the closing paragraph, and the session sat idle for 18 hours.
//
// One sighting was the old bar. Requiring the prompt to still be there three
// polls later costs 400ms on a launch and takes the bypass out of the window
// where the pane is still being repainted. It cannot deadlock: a prompt that
// is really up stays up, and the surrounding timeout is unchanged.
const startupPromptConfirmations = 3

// AgentReadyChecker abstracts the tmux surface readiness polling needs.
// *tmux.Session satisfies this interface.
type AgentReadyChecker interface {
	GetStatus() (string, error)
	CapturePaneFresh() (string, error)
}

// PromptGates enables tool-specific prompt visibility checks once GetStatus
// reports waiting/idle (or during the startup-window prompt probe below).
type PromptGates struct {
	ClaudeComposer bool // IsClaudeCompatible tools: require ❯ composer line
	CodexPrompt    bool // IsCodexCompatible tools: require codex>/› prompt
}

// WaitForAgentReady waits until the agent accepts keyboard input.
//
// Real tmux targets use a fresh, deadline-bound readiness snapshot instead of
// the aggregate TUI status. Startup still requires three consecutive prompt
// sightings. Background work only permits input with a current empty composer.
func WaitForAgentReady(target AgentReadyChecker, tool string, timeout time.Duration, gates PromptGates) error {
	const pollInterval = 200 * time.Millisecond
	if timeout <= 0 {
		timeout = DefaultAgentReadyTimeout
	}
	started := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), started.Add(timeout))
	defer cancel()
	diag := &ReadinessError{Stage: "tmux_readiness", LastStatus: "unknown", LastReason: "no_sample_before_deadline", StatusCounts: map[string]int{}}
	fail := func() error {
		diag.ElapsedMS = time.Since(started).Milliseconds()
		return diag
	}
	sawActive := false
	readyCount := 0
	startupPromptSeen := 0
	for attempt := 0; waitReadyInterval(ctx, pollInterval); attempt++ {
		// Real tmux targets implement the context-aware probe. The legacy
		// interface remains for in-memory checkers; their elapsed time is
		// counted too, and a late successful sample cannot authorize input.
		var snap tmux.ReadinessSnapshot
		var err error
		_, bounded := target.(interface {
			ProbeReadiness(context.Context) (tmux.ReadinessSnapshot, error)
		})
		if probe, ok := target.(interface {
			ProbeReadiness(context.Context) (tmux.ReadinessSnapshot, error)
		}); ok {
			snap, err = probe.ProbeReadiness(ctx)
		} else {
			snap.Status, err = target.GetStatus()
		}
		if snap.PaneID != "" {
			diag.PaneID = snap.PaneID
		}
		diag.ComposerPresent = nil
		if err != nil {
			if snap.Reason == "capture_failed" {
				diag.CaptureErrors++
			} else {
				diag.StatusErrors++
			}
			diag.LastReason = err.Error()
			startupPromptSeen = 0
			readyCount = 0
			continue
		}
		status := snap.Status
		diag.LastStatus, diag.LastReason = status, snap.Reason
		diag.LastStatusReason = snap.Reason
		if diag.LastReason == "" {
			diag.LastReason = "status_" + status
		}
		diag.StatusCounts[status]++
		if ctx.Err() != nil {
			return fail()
		}
		if status == "inactive" || status == "error" || status == "stopped" {
			if diag.LastReason == "" {
				diag.LastReason = "terminal_state"
			}
			return fail()
		}
		promptReady := true
		if status == "starting" || gates.ClaudeComposer || gates.CodexPrompt || tool == "cursor" {
			raw := snap.RawPane
			if !bounded {
				raw, err = target.CapturePaneFresh()
			}
			if err != nil {
				diag.CaptureErrors++
				diag.LastReason = "capture_failed: " + err.Error()
				readyCount, startupPromptSeen = 0, 0
				continue
			}
			present, reason := readyPrompt(raw, tool, gates)
			diag.ComposerPresent = &present
			promptReady = reason == ""
			if !promptReady {
				diag.LastReason = reason
				if reason == ComposerCodeDraft {
					// Return the same untyped, retryable refusal as the
					// delivery guard; do not occupy a queue attempt for the
					// entire readiness timeout behind an operator's draft.
					return fail()
				}
			}
		}
		if ctx.Err() != nil {
			return fail()
		}
		if status == "starting" {
			if promptReady {
				startupPromptSeen++
				if startupPromptSeen >= startupPromptConfirmations {
					if waitReadyInterval(ctx, 300*time.Millisecond) {
						return nil
					}
					return fail()
				}
				readyCount = 0
				continue
			}
			// A prompt that came and went was a transient paint, not a
			// tool waiting for input. Start counting again.
			startupPromptSeen = 0
			readyCount = 0
			continue
		}

		// Out of the startup window. A later return to "starting" is a fresh
		// startup, not a continuation of the run we were counting.
		startupPromptSeen = 0

		// Only explicit background evidence plus a current, empty composer
		// can relax an active pane. A historical prompt never overrides a
		// foreground spinner, unknown active status, draft or menu.
		backgroundReady := snap.Reason == "background_work" && gates.ClaudeComposer && promptReady
		if status == "active" && !backgroundReady {
			sawActive = true
			readyCount = 0
			continue
		}

		readyStatus := status == "waiting" || status == "idle" || backgroundReady
		if readyStatus && promptReady {
			readyCount++
		} else {
			readyCount = 0
		}

		alreadyReady := readyCount >= 10 && attempt >= 15
		if promptReady && ((sawActive && readyStatus) || alreadyReady) {
			if waitReadyInterval(ctx, 300*time.Millisecond) {
				return nil
			}
			return fail()
		}
	}

	return fail()
}

// ReadinessError is safe to include in JSON: it contains no pane or draft text.
type ReadinessError struct {
	Stage            string         `json:"stage"`
	LastStatus       string         `json:"last_status"`
	LastReason       string         `json:"last_reason"`
	LastStatusReason string         `json:"last_status_reason"`
	StatusCounts     map[string]int `json:"status_counts"`
	StatusErrors     int            `json:"status_errors"`
	CaptureErrors    int            `json:"capture_errors"`
	ComposerPresent  *bool          `json:"composer_present"`
	ElapsedMS        int64          `json:"elapsed_ms"`
	TargetID         string         `json:"target_id"`
	PaneID           string         `json:"pane_id"`
}

func (e *ReadinessError) Error() string {
	message := "agent not ready"
	if e.Stage == "defer_foreground" {
		// Keep the pre-diagnostics phrase: callers classify a deferral
		// timeout by "defer-if-busy:" + "still busy" in the error text.
		message = "defer-if-busy: target still busy (" + e.LastStatus + ")"
	}
	return fmt.Sprintf("%s after %s (stage=%s status=%s reason=%s target=%s pane=%s)",
		message, time.Duration(e.ElapsedMS)*time.Millisecond, e.Stage, e.LastStatus, e.LastReason, e.TargetID, e.PaneID)
}

func waitReadyInterval(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

func readyPrompt(raw, tool string, gates PromptGates) (bool, string) {
	content := tmux.StripANSI(raw)
	if gates.ClaudeComposer {
		present := currentReadyComposer(content)
		if !present {
			return false, ComposerCodeNoPrompt
		}
		if tmux.NewPromptDetector("claude").HasErrorBanner(content) {
			return true, "terminal_error_banner"
		}
		return true, ClaudeComposerReady(raw).Code
	}
	if gates.CodexPrompt || strings.EqualFold(tool, "codex") {
		if tmux.NewPromptDetector("codex").HasPrompt(content) {
			return true, ""
		}
		return false, ComposerCodeNoPrompt
	}
	if paneLooksBusy(content) {
		return false, ComposerCodeBusy
	}
	if tmux.NewPromptDetector(tool).HasPrompt(content) {
		return true, ""
	}
	return false, ComposerCodeNoPrompt
}

func paneShowsReadyPrompt(target AgentReadyChecker, tool string, gates PromptGates) bool {
	raw, err := target.CapturePaneFresh()
	if err != nil {
		return false
	}
	_, reason := readyPrompt(raw, tool, gates)
	return reason == ""
}

// currentReadyComposer accepts a framed input region at the bottom, or the
// older bare prompt layout only at the final input line (with its known
// permissions footer). It intentionally does not use the 40-line historical
// glyph fallback in CurrentComposerPrompt.
func currentReadyComposer(content string) bool {
	lines := strings.Split(strings.TrimRight(content, "\n \t"), "\n")
	if _, ok := framedComposerPrompt(lines); ok {
		_, bottom := ComposerBounds(lines)
		if len(lines)-bottom > 9 {
			return false
		}
		for _, line := range lines[bottom+1:] {
			if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
				return false
			}
		}
		return true
	}
	footerSeen := false
	for len(lines) > 0 {
		line := strings.TrimSpace(lines[len(lines)-1])
		lines = lines[:len(lines)-1]
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "⏵⏵") || line == "? for shortcuts" {
			footerSeen = true
			continue
		}
		return footerSeen && (strings.HasPrefix(line, "❯") || strings.HasPrefix(line, "›"))
	}
	return false
}

func paneLooksBusy(content string) bool {
	lower := strings.ToLower(content)
	return strings.Contains(lower, "ctrl+c to interrupt") ||
		strings.Contains(lower, "esc to interrupt")
}
