package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
	"github.com/stretchr/testify/require"
)

// 2026-10-20 socket steal: when the socket the poller probes is suspect
// (unreachable, lost, or held by a newer server), every session on it reads
// as gone at once. A pass must hold their last status instead of flipping the
// fleet to error, and alert once.
func TestSocketSteal_MassVanishDoesNotFlipToError(t *testing.T) {
	skipIfNoTmuxBinary(t)
	var alerts []tmux.ServerRecoveryEvent
	restore := tmux.SetSocketGuardForTest(
		func(string) (bool, string) { return true, "test: socket held by a newer server" },
		func(ev tmux.ServerRecoveryEvent) { alerts = append(alerts, ev) })
	t.Cleanup(restore)

	insts := stealTestInstances(t, 4)
	var pass StatusUpdatePass
	for _, i := range insts {
		_ = pass.UpdateStatus(i)
	}
	for _, i := range insts {
		require.Equal(t, StatusRunning, i.Status, "%s must keep its last status on a suspect socket", i.Title)
	}
	holding := 0
	for _, ev := range alerts {
		if ev.Action == "holding-status" {
			holding++
		}
	}
	require.Equal(t, 1, holding, "one alert for the whole pass, got %+v", alerts)
}

// Control: on a healthy socket the same absence is a verdict.
func TestSocketSteal_HealthySocketAbsenceStillFlips(t *testing.T) {
	skipIfNoTmuxBinary(t)
	restore := tmux.SetSocketGuardForTest(func(string) (bool, string) { return false, "" }, func(tmux.ServerRecoveryEvent) {})
	t.Cleanup(restore)

	insts := stealTestInstances(t, 2)
	var pass StatusUpdatePass
	for _, i := range insts {
		_ = pass.UpdateStatus(i)
	}
	for _, i := range insts {
		require.NotEqual(t, StatusRunning, i.Status, "%s: a real vanish on a healthy socket must not be held", i.Title)
	}
}

// stealTestInstances returns running shell sessions whose tmux sessions do not
// exist: the session package's isolated TMUX_TMPDIR has no server for them.
func stealTestInstances(t *testing.T, n int) []*Instance {
	t.Helper()
	var out []*Instance
	for k := 0; k < n; k++ {
		i := NewInstance(fmt.Sprintf("steal-hold-%d", k), t.TempDir())
		i.Tool = "shell"
		i.tmuxSession = tmux.NewSession(i.Title, i.ProjectPath)
		i.tmuxSession.SocketName = "adsteal-hold" // one shared server, like the real fleet
		i.Status = StatusRunning
		i.statusSampledLive = true
		i.CreatedAt = time.Now().Add(-time.Hour)
		i.lastStartTime = time.Now().Add(-time.Hour)
		out = append(out, i)
	}
	return out
}
