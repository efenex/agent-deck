package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
)

func TestSettleFakeClaudeExitsOnEOF(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("requires bash")
	}
	for _, input := range []string{"", "\x1b"} {
		t.Run(fmt.Sprintf("input_%x", input), func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "fake-claude")
			writeFile(t, script, settleFakeClaudeScript, 0o600)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", script, "closed", dir, filepath.Join(dir, "keys"), filepath.Join(dir, "launches"))
			cmd.Stdin = strings.NewReader(input)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("settle fake outlived EOF: %v: %s", err, out)
			}
		})
	}
}

func TestDeliveryEnvCleanupKillsPaneGroup(t *testing.T) {
	for _, tool := range []string{"tmux", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("requires %s", tool)
		}
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	script := filepath.Join(dir, "fake-claude-stubborn.py")
	writeFile(t, script, `import os, signal, sys
signal.signal(signal.SIGHUP, signal.SIG_IGN)
signal.signal(signal.SIGTERM, signal.SIG_IGN)
child = os.fork()
if child:
    with open(sys.argv[1], 'w') as out:
        out.write(str(os.getpgrp()) + ' ' + str(child))
while True:
    signal.pause()
`, 0o600)
	var group, child int
	// Emergency cleanup is owned by the outer test, after the assertion. A
	// failing regression must not itself leak the deliberately stubborn fake.
	t.Cleanup(func() {
		if group > 1 {
			_ = syscall.Kill(-group, syscall.SIGKILL)
		}
	})
	t.Run("fixture", func(t *testing.T) {
		e := newDeliveryEnv(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Use a distinct HOME so Linux's /proc fallback cannot mask broken
		// pane cleanup. This also models wrappers changing their child's env.
		command := fmt.Sprintf("HOME=%s exec python3 -I %s %s", shellescape.Quote(dir), shellescape.Quote(script), shellescape.Quote(pidFile))
		cmd := exec.CommandContext(ctx, "tmux", "new-session", "-d", "-s", "stubborn", command)
		cmd.Env = e.env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("start stubborn pane: %v: %s", err, out)
		}
		issue2530WaitFor(t, 5*time.Second, "stubborn process group", func() bool {
			data, _ := os.ReadFile(pidFile)
			fields := strings.Fields(string(data))
			if len(fields) != 2 {
				return false
			}
			group, _ = strconv.Atoi(fields[0])
			child, _ = strconv.Atoi(fields[1])
			return group > 1 && child > 1
		})
	})
	if group <= 1 || child <= 1 {
		t.Fatal("fixture did not publish its process group")
	}
	issue2530WaitFor(t, 3*time.Second, "fake-claude group to exit before its test returns", func() bool {
		return errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) && errors.Is(syscall.Kill(child, 0), syscall.ESRCH)
	})
	group = 0 // Already reaped; do not signal a subsequently reused group ID.
}
