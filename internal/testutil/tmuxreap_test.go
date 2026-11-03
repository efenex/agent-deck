package testutil

import (
	"os"
	"os/exec"
	"testing"
)

// TestReapTmuxSocket_KillsServerAndRemovesSocket proves the cleanup the helper
// registers kills a live server on a private -L socket and unlinks the socket,
// without the test body doing any teardown of its own.
func TestReapTmuxSocket_KillsServerAndRemovesSocket(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	if os.Getenv(TestIsolationMarkerEnv) == "" {
		cleanup := IsolateTmuxSocket()
		defer cleanup()
	}

	const name = "reap-helper-test"
	var sock string
	t.Run("inner", func(t *testing.T) {
		ReapTmuxSocket(t, name)
		sock = reapSocketPath(reapTmuxEnv(), name)
		out, err := tmuxOutput(reapTmuxEnv(), "-L", name, "new-session", "-d", "-s", "x", "sleep 300")
		if err != nil {
			t.Fatalf("start server: %v %s", err, out)
		}
		if _, err := os.Stat(sock); err != nil {
			t.Fatalf("socket not at predicted path %s: %v", sock, err)
		}
		// Return without any manual cleanup: the helper must do it.
	})
	if _, err := tmuxOutput(reapTmuxEnv(), "-L", name, "list-sessions"); err == nil {
		t.Fatal("server still alive after the subtest's cleanups ran")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket file %s still present (err=%v)", sock, err)
	}
}
