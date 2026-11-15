package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Keep the terminal open when the wrapper dies, and ignore HUP when the
// terminal closes: neither case may leave the fake spinning on an empty read.
func TestFakeClaudeExitsWithoutOwner(t *testing.T) {
	for _, mode := range []string{"stdin_closed", "parent_died"} {
		t.Run(mode, func(t *testing.T) {
			if _, err := exec.LookPath("python3"); err != nil {
				t.Skip("requires python3")
			}
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			src, err := os.ReadFile("testdata/claude_like_fake.py")
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, "claude"), string(src), 0o600)
			transcript := filepath.Join(dir, "session.jsonl")
			writeFile(t, filepath.Join(dir, "fake-running.json"), fmt.Sprintf("{%q:%q}", dir, transcript), 0o600)
			writeFile(t, transcript+".input-control.json", `{"id":"idle","phase":"idle"}`, 0o600)
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer slave.Close()
			log, err := os.Create(filepath.Join(dir, "output"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			// The shell remains the fake's parent; the Go test retains the PTY.
			cmd := exec.Command("sh", "-c", `trap '' HUP; exec 3<&0; python3 -I ./claude <&3 & echo $! > pid; wait`)
			cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, slave, log, log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			pid := 0
			t.Cleanup(func() {
				if t.Failed() {
					b, _ := os.ReadFile(log.Name())
					t.Logf("fake output: %s", b)
				}
				if pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			})
			issue2530WaitFor(t, 5*time.Second, "fake ready", func() bool {
				b, _ := os.ReadFile(filepath.Join(dir, "pid"))
				pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
				_, err := os.Stat(transcript + ".input-state.json")
				return pid > 0 && err == nil
			})
			// A framing-only chunk is not EOF after escape stripping.
			_, _ = master.Write([]byte("\x1b[200~\x1b[201~"))
			time.Sleep(150 * time.Millisecond)
			if !fakeClaudePIDRunning(t, pid) {
				t.Fatal("fake exited on a framing-only input chunk")
			}
			if mode == "stdin_closed" {
				_ = master.Close()
			} else {
				_ = cmd.Process.Kill()
			}
			issue2530WaitFor(t, 3*time.Second, "fake to exit after "+mode, func() bool {
				return !fakeClaudePIDRunning(t, pid)
			})
			b, _ := os.ReadFile(log.Name())
			if strings.Contains(string(b), "Traceback") {
				t.Fatalf("fake crashed instead of exiting cleanly: %s", b)
			}
		})
	}
}

func fakeClaudePIDRunning(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			return false
		}
		t.Fatalf("inspect fake pid %d: %v", pid, err)
	}
	state := strings.TrimSpace(string(out))
	return state != "" && !strings.HasPrefix(state, "Z")
}

func TestIssue2530CleanupReapsFakeClaude(t *testing.T) {
	// These never read stdin and run outside tmux, proving cleanup independently
	// of the Python fake's cooperative exit. A sibling fixture must survive.
	start := func(check *testing.T, root, name string) (*exec.Cmd, <-chan error) {
		check.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			check.Fatal(err)
		}
		writeFile(check, path, "import time\nopen(__file__ + '.ready', 'w').close()\nwhile True: time.sleep(1)\n", 0o600)
		cmd := exec.Command("python3", "-I", path)
		if err := cmd.Start(); err != nil {
			check.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait(); close(done) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
		issue2530WaitFor(check, 5*time.Second, "uncooperative fake ready", func() bool {
			_, err := os.Stat(path + ".ready")
			return err == nil
		})
		return cmd, done
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("requires python3")
	}
	sibling, _ := start(t, t.TempDir(), "p/.fake/claude")
	var owned []<-chan error
	t.Run("owner", func(t *testing.T) {
		e := newIssue2530Env(t)
		for _, name := range []string{"fakebin/claude", "p/.fake/claude"} {
			_, done := start(t, e.root, name)
			owned = append(owned, done)
		}
	})
	for _, done := range owned {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("fixture cleanup left a fake Claude running")
		}
	}
	if !fakeClaudePIDRunning(t, sibling.Process.Pid) {
		t.Error("fixture cleanup killed a sibling fixture's fake")
	}
}
