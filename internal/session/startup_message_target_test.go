package session

import (
	"strings"
	"testing"
)

func TestStartupMessageTargetsOriginatingPane(t *testing.T) {
	startupNameConfig(t, "[claude]\ncommand='claude'\n")
	inst := &Instance{Tool: "claude", Title: "Target", ID: "target", ProjectPath: t.TempDir()}
	command := inst.buildClaudeCommandWithMessage("claude", "hello")
	// Capture, literal text and Enter must all address the pane in which the
	// startup shell runs, even if the operator switches windows during sleep.
	if got := strings.Count(command, `-t "$TMUX_PANE"`); got != 3 {
		t.Fatalf("want all three delivery operations pinned to TMUX_PANE, got %d: %s", got, command)
	}
	if strings.Contains(command, "SESSION_NAME") {
		t.Fatalf("startup delivery must not resolve the session's active window: %s", command)
	}
}
