package tmux

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadinessSeparatesBackgroundFromForeground(t *testing.T) {
	for _, tc := range []struct {
		fixture, reason string
	}{
		{"workflow-running", "background_work"},
		{"workflow-finished", "no_busy_indicator"},
		{"monitor-footer-busy-turn", "foreground_busy"},
		{"workflow-auth-401", "terminal_error_banner"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/background_work/" + tc.fixture + ".txt")
			require.NoError(t, err)
			s := &Session{Command: "claude"}
			snap := s.classifyReadiness(ReadinessSnapshot{RawPane: string(raw)}, "", "claude")
			require.Equal(t, tc.reason, snap.Reason)
		})
	}
}

func TestReadinessPreservesTerminalStartupFailure(t *testing.T) {
	s := &Session{Command: "claude", startupTimedOut: true}
	snap := s.classifyReadiness(ReadinessSnapshot{RawPane: "❯\n  ⏵⏵ bypass permissions on"}, "", "claude")
	require.Equal(t, "error", snap.Status)
	require.Equal(t, "startup_timed_out", snap.Reason)
}

func TestReadinessProbeCancelsTmuxClient(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "identity", true: "capture"}[capture], func(t *testing.T) {
			dir := t.TempDir()
			// exec replaces the shell, so cancellation reaps the only child.
			script := "#!/bin/sh\n"
			if capture {
				script += "case \"$*\" in *display-message*) printf '%%42\\t0\\tfixture\\tclaude\\n'; exit 0;; esac\n"
			}
			script += "exec /bin/sleep 30\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755))
			t.Setenv("PATH", dir)
			s := NewSession("bounded-readiness", t.TempDir())
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			snap, err := s.ProbeReadiness(ctx)
			require.Error(t, err)
			require.Less(t, time.Since(start), 500*time.Millisecond)
			if capture {
				require.Equal(t, "%42", snap.PaneID)
				require.Equal(t, "capture_failed", snap.Reason)
			}
		})
	}
}

func TestReadinessProbeCapturesExactPane(t *testing.T) {
	skipIfNoTmuxBinary(t)
	s := NewSession("readiness-pane", t.TempDir())
	require.NoError(t, s.Start("sleep 60"))
	t.Cleanup(func() { _ = s.Kill() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snap, err := s.ProbeReadiness(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, snap.PaneID)
	require.Equal(t, "starting", snap.Status)
	// A removed target's name must never resolve as a prefix of a sibling.
	prefix := &Session{Name: s.TmuxName()[:len(s.TmuxName())-2], SocketName: s.SocketName}
	snap, err = prefix.ProbeReadiness(ctx)
	require.True(t, err != nil || snap.Status == "inactive", "an absent exact target must fail closed")
	require.Empty(t, snap.PaneID)
}
