//go:build !windows

package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// watchdogScript waits for the watched pid to disappear, then kills every tmux
// server whose socket lives under $2 and removes $2. Poll, not wait: the pid is
// not our child. Sockets are addressed by absolute -S path, never -L, for the
// same reason as KillTmuxServersUnder.
const watchdogScript = `
while kill -0 "$1" 2>/dev/null; do sleep 0.5; done
for s in "$2"/* "$2"/tmux-*/*; do
	[ -n "$3" ] && [ -S "$s" ] && env -u TMUX -u TMUX_PANE -u TMUX_TMPDIR "$3" -S "$s" kill-server >/dev/null 2>&1
done
chmod -R u+w "$2" 2>/dev/null
rm -rf "$2"
`

// startTmuxDirWatchdog guards dir against the exit paths cleanup cannot see.
//
// IsolateTmuxSocket's cleanup only runs when the test binary returns from
// TestMain. It does not run when the binary is killed by -timeout (a panic exit),
// by SIGKILL, or when a re-exec'd helper process calls os.Exit — and every one of
// those leaves a tmux server holding a pty plus an unreachable ad-tmux-* dir
// behind, which is how a single full `go test ./...` stranded six servers and
// ~60 dirs. So an out-of-process watcher, in its own session so a Ctrl-C to the
// test's process group does not take it down, waits for this process to die and
// then does what cleanup would have done.
//
// The returned stop func ends the watcher once cleanup has run in-process.
// Best-effort: any failure to start leaves the in-process cleanup as the only
// line of defence, which is the pre-existing behaviour.
func startTmuxDirWatchdog(dir string) (stop func()) {
	return startDirWatchdog(dir, "ad-tmux-", true)
}

// ReapHomeOnExit is the same guard for a package's isolated HOME: the directory
// is removed (writable again first, since a go module cache is read-only) after
// this process dies by any route. dir's base name must start with
// "agent-deck-cmd-tests-home-". The returned stop func ends the watcher once the
// in-process cleanup has run.
func ReapHomeOnExit(dir string) (stop func()) {
	return startDirWatchdog(dir, "agent-deck-cmd-tests-home-", false)
}

// ReapTmuxDirOnExit guards a test's own short TMUX_TMPDIR (a "ad*" directory
// made directly under /tmp to fit sun_path) the same way: servers under it are
// killed and the dir removed after this process dies by any route, including the
// -timeout panic that skips the test's t.Cleanup. Anything else is ignored.
func ReapTmuxDirOnExit(dir string) (stop func()) {
	if filepath.Dir(filepath.Clean(dir)) != "/tmp" {
		return func() {}
	}
	return startDirWatchdog(dir, "ad", true)
}

func startDirWatchdog(dir, prefix string, killServers bool) (stop func()) {
	noop := func() {}
	// The watcher rm -rf's dir, so refuse anything that is not a private
	// directory of the expected shape.
	if !strings.HasPrefix(filepath.Base(dir), prefix) || isDefaultTmuxBase(dir) {
		return noop
	}
	// A test may have emptied PATH (the uninstall CLI tests do, to hide brew), so
	// neither binary is guaranteed to resolve. Without tmux there is no server to
	// kill but the dir still has to go; without sh on PATH, /bin/sh is POSIX.
	var tmux string
	if killServers {
		tmux, _ = exec.LookPath("tmux")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		sh = "/bin/sh"
	}
	cmd := exec.Command(sh, "-c", watchdogScript, "sh", strconv.Itoa(os.Getpid()), dir, tmux)
	// The script's own sleep/env/rm must resolve even when PATH was emptied.
	cmd.Env = append(envWithoutTmuxVars(), "PATH=/usr/bin:/bin:/usr/sbin:/sbin")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return noop
	}
	// No reaper goroutine: several packages assert on goroutine leaks
	// (goleak) around code that runs after TestMain isolated tmux. stop reaps
	// the child synchronously instead; if it never runs, this process is dying
	// anyway and the watcher is no longer our child to worry about.
	return func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}
