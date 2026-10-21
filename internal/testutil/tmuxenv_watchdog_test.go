//go:build !windows

package testutil

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const watchdogHelperEnv = "AGENT_DECK_TMUX_WATCHDOG_HELPER"

// TestTmuxWatchdogHelper is re-exec'd by the tests below. It isolates, starts a
// real server, reports the isolated dir, then ends WITHOUT running the cleanup
// IsolateTmuxSocket returned — the exit paths (os.Exit in a helper process, a
// -timeout panic, SIGKILL) that used to strand the server.
func TestTmuxWatchdogHelper(t *testing.T) {
	mode := os.Getenv(watchdogHelperEnv)
	if mode == "" {
		return
	}
	if mode == "home" || mode == "ad-home" {
		pattern := "agent-deck-cmd-tests-home-"
		if mode == "ad-home" {
			pattern = "ad-home-"
		}
		home, err := os.MkdirTemp("", pattern)
		if err != nil {
			t.Fatal(err)
		}
		// A go module cache is written read-only; unlink fails inside 0555 dirs.
		mod := filepath.Join(home, "go", "pkg", "mod", "x@v1")
		if err := os.MkdirAll(mod, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(mod, "f.go"), []byte("package x"), 0o444)
		_ = os.Chmod(mod, 0o555)
		_ = os.Chmod(filepath.Dir(mod), 0o555)
		ReapHomeOnExit(home)
		os.Stdout.WriteString(home + "\n")
		os.Exit(0)
	}
	if mode == "exit-nopath" {
		// The uninstall CLI tests empty PATH to hide brew; no tmux, no sh by name.
		_ = os.Setenv("PATH", t.TempDir())
	}
	_ = IsolateTmuxSocket()
	if mode == "exit-nopath" {
		os.Stdout.WriteString(os.Getenv("TMUX_TMPDIR") + "\n")
		os.Exit(0)
	}
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", "wd", "sleep 3600").CombinedOutput(); err != nil {
		t.Fatalf("start server: %v %s", err, out)
	}
	os.Stdout.WriteString(os.Getenv("TMUX_TMPDIR") + "\n")
	if mode == "exit" {
		os.Exit(0)
	}
	time.Sleep(time.Hour) // "kill": wait to be SIGKILLed
}

func runWatchdogHelper(t *testing.T, mode string) (dir string, helper *exec.Cmd) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	helper = exec.Command(os.Args[0], "-test.run=^TestTmuxWatchdogHelper$")
	helper.Env = append(os.Environ(), watchdogHelperEnv+"="+mode)
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("helper reported no isolated dir: %v", err)
	}
	dir = strings.TrimSpace(line)
	want := map[string]string{"home": "agent-deck-cmd-tests-home-", "ad-home": "ad-home-"}[mode]
	if want == "" {
		want = "ad-tmux-"
	}
	if !strings.HasPrefix(filepath.Base(dir), want) {
		t.Fatalf("helper isolated dir = %q, want a %s* dir", dir, want)
	}
	// Safety net: never leave the server behind even if an assertion fails.
	t.Cleanup(func() {
		KillTmuxServersUnder(dir)
		_ = exec.Command("chmod", "-R", "u+w", dir).Run()
		_ = os.RemoveAll(dir)
	})
	return dir, helper
}

func requireDirGone(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Report what survived: a live server is the case that matters.
	var live []string
	matches, _ := filepath.Glob(filepath.Join(dir, "tmux-*", "*"))
	for _, sock := range matches {
		if exec.Command("tmux", "-S", sock, "list-sessions").Run() == nil {
			live = append(live, sock)
		}
	}
	t.Fatalf("isolated dir %s still present 10s after its owner died; live servers: %v", dir, live)
}

func TestTmuxWatchdog_ReapsServerWhenOwnerOsExits(t *testing.T) {
	dir, helper := runWatchdogHelper(t, "exit")
	_ = helper.Wait()
	requireDirGone(t, dir)
}

func TestTmuxWatchdog_RemovesDirWhenOwnerHadNoTmuxOrShOnPATH(t *testing.T) {
	dir, helper := runWatchdogHelper(t, "exit-nopath")
	_ = helper.Wait()
	requireDirGone(t, dir)
}

func TestReapHomeOnExit_RemovesReadOnlyHomeWhenOwnerOsExits(t *testing.T) {
	dir, helper := runWatchdogHelper(t, "home")
	_ = helper.Wait()
	requireDirGone(t, dir)
}

func TestReapHomeOnExit_RemovesIsolateHomePrefix(t *testing.T) {
	dir, helper := runWatchdogHelper(t, "ad-home")
	_ = helper.Wait()
	requireDirGone(t, dir)
}

func TestTmuxWatchdog_ReapsServerWhenOwnerIsSIGKILLed(t *testing.T) {
	dir, helper := runWatchdogHelper(t, "kill")
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	requireDirGone(t, dir)
}

// The watcher must not outlive a clean cleanup, and must refuse a dir that is
// not a private ad-tmux-* dir (it rm -rf's what it is given).
func TestTmuxWatchdog_RefusesNonIsolatedDirs(t *testing.T) {
	notOurs, err := os.MkdirTemp("/tmp", "not-ours-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(notOurs) })
	for _, dir := range []string{"/tmp", "/private/tmp", t.TempDir(), notOurs} {
		stop := startTmuxDirWatchdog(dir)
		stop()
		stop = ReapHomeOnExit(dir)
		stop()
		stop = ReapTmuxDirOnExit(dir)
		stop()
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("startTmuxDirWatchdog(%q) must not touch it: %v", dir, err)
		}
	}
}
