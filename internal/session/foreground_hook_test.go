package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestForegroundHookRequiresFreshDiskObservation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	inst := &Instance{ID: "foreground-ready", Tool: "claude"}
	inst.UpdateHookStatus(&HookStatus{Status: "waiting", Event: "Stop", UpdatedAt: time.Now()})
	_, fresh := inst.GetHookStatus()
	require.True(t, fresh)
	_, fresh = ReloadForegroundHookStatus(inst)
	require.False(t, fresh, "missing file must not reuse cached waiting")
	path := filepath.Join(GetHooksDir(), inst.ID+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	writeHookStatusAt(t, path, "running", "")
	status, fresh := ReloadForegroundHookStatus(inst)
	require.True(t, fresh)
	require.Equal(t, "running", status)
	require.NoError(t, os.Remove(path))
	_, fresh = ReloadForegroundHookStatus(inst)
	require.False(t, fresh)
}

func TestForegroundHookRejectsForeignIdenticalEvidence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	inst := &Instance{ID: "foreground-foreign", Tool: "claude", ClaudeSessionID: "owned", ProjectPath: t.TempDir()}
	path := filepath.Join(GetHooksDir(), inst.ID+".json")
	writeHookStatusAt(t, path, "waiting", "foreign-without-conversation")
	hs := readHookStatusFile(inst.ID)
	require.NotNil(t, hs)
	inst.UpdateHookStatus(&HookStatus{Status: hs.Status, Event: hs.Event, UpdatedAt: hs.UpdatedAt, SessionID: "owned"})
	status, fresh := ReloadForegroundHookStatus(inst)
	require.False(t, fresh, "rejected hook must not inherit coincident cached waiting")
	require.Empty(t, status)
	require.Equal(t, "owned", inst.ClaudeSessionID)
}
