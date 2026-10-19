package session

import (
	"testing"
	"time"
)

// A session stopped from another process (`agent-deck session stop` in the CLI)
// is adopted by a long-lived TUI/web process as StatusStopped, but that process
// can still see the pane as alive for a moment (positive tmux cache hit, or a
// control-mode pipe that has not noticed the kill yet). Claude's SessionEnd hook
// ("dead") arrives at the same time. The "tmux exists again → stopped means the
// user restarted it" revival must not turn that SessionEnd into StatusError:
// a dead hook on a session we recorded as stopped is the stop, not a crash.
func TestUpdateStatus_StoppedStaysStoppedOnFreshDeadHook(t *testing.T) {
	skipIfNoTmuxBinary(t)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	inst := NewInstanceWithTool("stop-dead-hook", tmpHome, "claude")
	if err := inst.tmuxSession.Start("sleep 3600"); err != nil {
		t.Fatalf("tmux start: %v", err)
	}
	defer func() { _ = inst.tmuxSession.Kill() }()

	// Past the 1.5-second startup grace period inside UpdateStatus.
	time.Sleep(2 * time.Second)

	inst.mu.Lock()
	inst.Status = StatusStopped
	inst.hookStatus = "dead"
	inst.hookEvent = "SessionEnd"
	inst.hookLastUpdate = time.Now()
	inst.mu.Unlock()

	_ = inst.UpdateStatus()

	if got := inst.GetStatusThreadSafe(); got != StatusStopped {
		t.Fatalf("Status = %q, want %q: a fresh SessionEnd hook must not turn a stopped session into error", got, StatusStopped)
	}
}
