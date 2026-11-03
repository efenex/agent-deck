package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ReapTmuxSocket registers a t.Cleanup that kills the tmux server listening on
// the private `-L <socketName>` socket and removes the socket file.
//
// Call it IMMEDIATELY after choosing the socket name — before anything that can
// start the server and before any t.Fatal — so the reap runs on every exit path
// of the test (pass, t.Fatal, t.FailNow, failed setup). Cleanups run LIFO, so
// registering it early makes it run last, after any `session stop` cleanup the
// test registered later.
//
// The server is addressed under the test process's own environment minus
// TMUX / TMUX_PANE, so TMUX_TMPDIR (the dir IsolateTmuxSocket made) resolves
// the socket to the same place as the tmux the test's subprocesses start,
// provided those subprocesses keep TMUX_TMPDIR (see isStrippedTmuxVar in
// cmd/agent-deck). "No server running" is success: there is nothing to reap.
//
// The helper refuses to run unless the process is isolated
// (TestIsolationMarkerEnv set), so it can never be pointed at the operator's
// real tmux directory by a misconfigured test.
//
// A server that survives kill-server fails the test: a leaked server holds a
// pty per pane and its socket outlives the run.
func ReapTmuxSocket(t testing.TB, socketName string) {
	t.Helper()
	if socketName == "" || strings.ContainsRune(socketName, filepath.Separator) {
		t.Fatalf("ReapTmuxSocket: %q is not a bare -L socket name", socketName)
	}
	t.Cleanup(func() {
		if os.Getenv(TestIsolationMarkerEnv) == "" {
			t.Errorf("ReapTmuxSocket(%q): process is not tmux-isolated; refusing to touch the real tmux dir", socketName)
			return
		}
		env := reapTmuxEnv()
		out, err := tmuxOutput(env, "-L", socketName, "kill-server")
		if err != nil && !noTmuxServer(out) {
			t.Errorf("tmux -L %s kill-server: %v: %s", socketName, err, strings.TrimSpace(out))
		}
		if _, err := tmuxOutput(env, "-L", socketName, "list-sessions"); err == nil {
			t.Errorf("tmux server still alive on -L %s after kill-server", socketName)
		}
		// kill-server unlinks the socket; a server that died by itself or by
		// signal can leave the file behind, so remove it explicitly.
		_ = os.Remove(reapSocketPath(env, socketName))
	})
}

// reapTmuxEnv is the process environment without the variables that would make
// tmux ignore TMUX_TMPDIR (TMUX) or target a pane (TMUX_PANE).
func reapTmuxEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// reapSocketPath mirrors tmux's own resolution of `-L name`:
// ${TMUX_TMPDIR:-/tmp}/tmux-<uid>/<name>.
func reapSocketPath(env []string, name string) string {
	base := "/tmp"
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "TMUX_TMPDIR="); ok && v != "" {
			base = v
		}
	}
	return filepath.Join(base, fmt.Sprintf("tmux-%d", os.Getuid()), name)
}

func tmuxOutput(env []string, args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// noTmuxServer reports tmux output meaning "nothing is listening there".
func noTmuxServer(out string) bool {
	return strings.Contains(out, "no server running") ||
		strings.Contains(out, "No such file or directory") ||
		strings.Contains(out, "Connection refused")
}
