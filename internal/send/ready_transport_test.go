package send

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

type snapshotReadyChecker struct {
	mockReadyChecker
	snapshot tmux.ReadinessSnapshot
	slow     bool
}

func (s *snapshotReadyChecker) ProbeReadiness(ctx context.Context) (tmux.ReadinessSnapshot, error) {
	if s.slow {
		<-ctx.Done()
		return s.snapshot, ctx.Err()
	}
	return s.snapshot, nil
}

func TestReadinessBoundedProbeAndDiagnostics(t *testing.T) {
	s := &snapshotReadyChecker{snapshot: tmux.ReadinessSnapshot{PaneID: "%42"}, slow: true}
	start := time.Now()
	err := WaitForAgentReady(s, "claude", 300*time.Millisecond, PromptGates{ClaudeComposer: true})
	var diagnostic *ReadinessError
	if !errors.As(err, &diagnostic) || diagnostic.StatusErrors != 1 || diagnostic.PaneID != "%42" {
		t.Fatalf("missing diagnostic: %#v", err)
	}
	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Fatalf("deadline did not cancel probe: %s", elapsed)
	}
	encoded, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"stage", "last_status", "last_reason", "status_counts", "status_errors", "capture_errors", "composer_present", "elapsed_ms", "target_id", "pane_id"} {
		if !strings.Contains(string(encoded), `"`+field+`"`) {
			t.Errorf("missing %s: %s", field, encoded)
		}
	}
}

func TestReadinessSavedFrames(t *testing.T) {
	// Synthetic structural fixtures, not the unavailable incident pane.
	for _, tc := range []struct {
		name, status, reason string
		ready                bool
	}{
		{"expired-monitor", "waiting", "no_busy_indicator", true},
		{"foreground-busy", "active", "foreground_busy", false},
		{"background-work", "active", "background_work", true},
		{"recovered", "waiting", "no_busy_indicator", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/readiness/" + tc.name + ".txt")
			if err != nil {
				t.Fatal(err)
			}
			present, reason := readyPrompt(string(raw), "claude", PromptGates{ClaudeComposer: true})
			if (present && reason == "") != tc.ready {
				t.Fatalf("present=%v reason=%q", present, reason)
			}
			s := &snapshotReadyChecker{snapshot: tmux.ReadinessSnapshot{Status: tc.status, Reason: tc.reason, RawPane: string(raw)}}
			err = WaitForAgentReady(s, "claude", 4*time.Second, PromptGates{ClaudeComposer: true})
			if (err == nil) != tc.ready {
				t.Fatalf("readiness = %v, want ready=%v", err, tc.ready)
			}
		})
	}
}

func TestReadinessRefusesHistoricalPromptAndUnsafeComposer(t *testing.T) {
	for name, raw := range map[string]string{
		"bare glyph": "⏺ old output\n❯",
		"historical": "❯ old input\n⏺ output after old prompt",
		"old framed": "────────────────────\n❯\n────────────────────\n⏺ output after old composer",
		"draft":      "────────────────────\n❯ unsent draft\n────────────────────",
		"menu":       "────────────────────\n❯\n────────────────────\n  ❯ 1. Yes\n  Enter to confirm",
		"auth":       "API Error: 401 authentication_error\n────────────────────\n❯\n────────────────────",
		"missing":    "Loading...",
	} {
		t.Run(name, func(t *testing.T) {
			_, reason := readyPrompt(raw, "claude", PromptGates{ClaudeComposer: true})
			if reason == "" {
				t.Fatal("unsafe frame accepted")
			}
		})
	}
}

func TestForegroundHookWaitingDoesNotProbeActivePane(t *testing.T) {
	err := WaitForForegroundReady(func() (string, bool) { return "waiting", true },
		func(context.Context) (tmux.ReadinessSnapshot, error) {
			t.Fatal("fresh foreground waiting must not require a pane")
			return tmux.ReadinessSnapshot{}, nil
		}, "claude", PromptGates{ClaudeComposer: true}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadinessDraftRefusesPromptly(t *testing.T) {
	s := &snapshotReadyChecker{snapshot: tmux.ReadinessSnapshot{
		Status: "waiting", RawPane: "────────────────────\n❯ operator draft\n────────────────────",
	}}
	started := time.Now()
	err := WaitForAgentReady(s, "claude", 2*time.Second, PromptGates{ClaudeComposer: true})
	var diagnostic *ReadinessError
	if !errors.As(err, &diagnostic) || diagnostic.LastReason != ComposerCodeDraft {
		t.Fatalf("draft refusal = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 600*time.Millisecond {
		t.Fatalf("draft held readiness instead of returning a retryable refusal: %s", elapsed)
	}
}

// A Claude hook older than the 2m freshness window is the normal state of an
// idle target woken on a 20m cadence. With no foreground-busy evidence in the
// pane, --defer-if-busy must not hold on composer shape: a validated socket
// send needs no composer, and keyboard delivery applies its own guards.
func TestForegroundStaleHookIdlePaneDoesNotRequireComposer(t *testing.T) {
	for name, snap := range map[string]tmux.ReadinessSnapshot{
		"draft":           {Status: "waiting", Reason: "no_busy_indicator", RawPane: "────────────────────\n❯ operator draft\n────────────────────"},
		"no composer":     {Status: "waiting", Reason: "no_busy_indicator", RawPane: "⏺ done\nunrecognised layout"},
		"background work": {Status: "active", Reason: "background_work", RawPane: "⏺ done\nunrecognised layout"},
	} {
		t.Run(name, func(t *testing.T) {
			err := WaitForForegroundReady(func() (string, bool) { return "waiting", false },
				func(context.Context) (tmux.ReadinessSnapshot, error) { return snap, nil },
				"claude", PromptGates{ClaudeComposer: true}, time.Second)
			if err != nil {
				t.Fatalf("stale hook + idle pane held: %v", err)
			}
		})
	}
}

// The same stale hook still holds on a running turn the send-side detector
// sees even when the tmux classifier did not (status waiting).
func TestForegroundStaleHookHoldsOnRunningTurnLine(t *testing.T) {
	raw, err := os.ReadFile("testdata/readiness/foreground-busy.txt")
	if err != nil {
		t.Fatal(err)
	}
	err = WaitForForegroundReady(func() (string, bool) { return "waiting", false },
		func(context.Context) (tmux.ReadinessSnapshot, error) {
			return tmux.ReadinessSnapshot{Status: "waiting", Reason: "no_busy_indicator", RawPane: string(raw)}, nil
		}, "claude", PromptGates{ClaudeComposer: true}, 100*time.Millisecond)
	var diagnostic *ReadinessError
	if !errors.As(err, &diagnostic) || diagnostic.LastReason != ComposerCodeBusy {
		t.Fatalf("running turn line did not hold: %v", err)
	}
}

// Scripts classify a deferral timeout from the error text by matching
// "defer-if-busy:" and "still busy"; the structured diagnostics must not drop
// the phrase WaitUntilNotBusy used.
func TestForegroundDeferErrorKeepsStillBusyPhrase(t *testing.T) {
	err := WaitForForegroundReady(func() (string, bool) { return "running", true },
		func(context.Context) (tmux.ReadinessSnapshot, error) { return tmux.ReadinessSnapshot{}, nil },
		"claude", PromptGates{ClaudeComposer: true}, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "defer-if-busy:") || !strings.Contains(err.Error(), "still busy") {
		t.Fatalf("deferral error text = %v", err)
	}
}

func TestForegroundRunningAndUnknownHold(t *testing.T) {
	for _, fresh := range []bool{true, false} {
		start := time.Now()
		err := WaitForForegroundReady(func() (string, bool) { return "running", fresh },
			func(context.Context) (tmux.ReadinessSnapshot, error) {
				return tmux.ReadinessSnapshot{Status: "active", RawPane: "❯ old\n⏺ still working"}, nil
			}, "claude", PromptGates{ClaudeComposer: true}, 100*time.Millisecond)
		if err == nil || time.Since(start) > 500*time.Millisecond {
			t.Fatalf("fresh=%v err=%v elapsed=%v", fresh, err, time.Since(start))
		}
	}
}
