package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPrimaryWindowTarget(t *testing.T) {
	for _, name := range []string{"agentdeck_agent_123", "renamed-agent"} {
		s := &Session{Name: name}
		require.Equal(t, name+":^", s.primaryWindowTarget())
		require.Equal(t, name+":3", s.windowTarget(3))
	}
}

// A second window is active throughout delivery and verification. Exercise both
// base-index settings: the managed window is the first one, not necessarily 0.
func TestAgentDeliveryIgnoresActiveWindow(t *testing.T) {
	skipIfNoTmuxBinary(t)
	for _, base := range []int{0, 1} {
		t.Run(fmt.Sprintf("base=%d", base), func(t *testing.T) {
			s := NewSession("agent-delivery", t.TempDir())
			require.NoError(t, s.Start("printf 'AGENT_READY\\n'; cat"))
			t.Cleanup(func() { _ = s.Kill() })
			ctl := func(args ...string) string {
				t.Helper()
				out, err := s.tmuxCmd(args...).CombinedOutput()
				require.NoError(t, err, "%s", out)
				return strings.TrimSpace(string(out))
			}
			agent := ctl("display-message", "-p", "-t", s.Name+":^", "#{pane_id}")
			if base == 1 {
				ctl("set-option", "-t", s.Name, "base-index", "1")
				ctl("move-window", "-s", s.Name+":^", "-t", s.Name+":1")
			}
			aux := ctl("new-window", "-P", "-F", "#{pane_id}", "-t", s.Name, "printf 'AUX_READY\\n'; cat")
			require.Eventually(t, func() bool {
				return strings.Contains(ctl("capture-pane", "-p", "-t", agent), "AGENT_READY") && strings.Contains(ctl("capture-pane", "-p", "-t", aux), "AUX_READY")
			}, 3*time.Second, 20*time.Millisecond)

			require.NoError(t, s.SendKeysAndEnterChecked("AGENT_MESSAGE", s.CapturePaneFresh, func(pane string, err error) (bool, error) {
				require.NoError(t, err)
				require.Contains(t, pane, "AGENT_MESSAGE")
				require.NotContains(t, pane, "AUX_READY")
				return true, nil
			}))
			require.Contains(t, ctl("capture-pane", "-p", "-t", agent), "AGENT_MESSAGE")
			require.NotContains(t, ctl("capture-pane", "-p", "-t", aux), "AGENT_MESSAGE")
			for name, capture := range map[string]func() (string, error){
				"cached": s.CapturePane, "fresh": s.CapturePaneFresh,
				"history": s.CaptureFullHistory,
			} {
				s.invalidateCache()
				pane, err := capture()
				require.NoError(t, err, name)
				require.Contains(t, pane, "AGENT_READY", name)
				require.NotContains(t, pane, "AUX_READY", name)
			}
			primed, timedOut := primeCaptureChunk(s.SocketName, []*Session{s})
			require.False(t, timedOut)
			require.Equal(t, 1, primed)
			cached, err := s.CapturePane()
			require.NoError(t, err)
			require.Contains(t, cached, "AGENT_READY")
			pipe, err := NewControlPipe(s.Name, s.SocketName)
			require.NoError(t, err)
			defer pipe.Close()
			captured, err := pipe.CapturePaneVia()
			require.NoError(t, err)
			require.Contains(t, captured, "AGENT_READY")
			sender, err := s.OpenKeySender()
			require.NoError(t, err)
			defer sender.Close()
			require.NoError(t, sender.SendKeys("PERSISTENT_MESSAGE"))
			require.NoError(t, sender.SendEnter())
			require.Eventually(t, func() bool {
				return strings.Contains(ctl("capture-pane", "-p", "-t", agent), "PERSISTENT_MESSAGE")
			}, 3*time.Second, 20*time.Millisecond)
			require.NotContains(t, ctl("capture-pane", "-p", "-t", aux), "PERSISTENT_MESSAGE")
			snap, err := s.ProbeReadiness(context.Background())
			require.NoError(t, err)
			require.Equal(t, agent, snap.PaneID)
			require.Equal(t, aux, ctl("display-message", "-p", "-t", s.Name, "#{pane_id}"), "delivery must not switch focus")
			auxIndex := ctl("display-message", "-p", "-t", aux, "#{window_index}")
			var index int
			_, err = fmt.Sscan(auxIndex, &index)
			require.NoError(t, err)
			require.NoError(t, s.SendKeysAndEnterToWindow(index, "EXPLICIT_WINDOW"))
			require.Contains(t, ctl("capture-pane", "-p", "-t", aux), "EXPLICIT_WINDOW")
			require.NotContains(t, ctl("capture-pane", "-p", "-t", agent), "EXPLICIT_WINDOW")
			// Exercise paste-buffer and the large-payload pane-id/TTY probe,
			// not only the small literal send-keys transport.
			require.NoError(t, s.SendKeysAndEnter(strings.Repeat("LONG_AGENT_MESSAGE\n", 100)))
			require.Eventually(t, func() bool {
				return strings.Contains(ctl("capture-pane", "-p", "-t", agent), "LONG_AGENT_MESSAGE")
			}, 3*time.Second, 20*time.Millisecond)
			require.NotContains(t, ctl("capture-pane", "-p", "-t", aux), "LONG_AGENT_MESSAGE")
		})
	}
}
