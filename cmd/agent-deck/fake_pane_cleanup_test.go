package main

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// cleanupFakePaneGroups stops the entire group, including children ignoring
// HUP, before closing the pane. An empty target is only used with an env's
// private server; named targets use TestMain's isolated tmux socket.
func cleanupFakePaneGroups(t *testing.T, env []string, socketName, target string) {
	t.Helper()
	args := []string{"list-panes", "-a"}
	if target != "" {
		args = []string{"list-panes", "-s", "-t", "=" + target}
	} else {
		isolated := false
		for _, entry := range env {
			if value, ok := strings.CutPrefix(entry, "TMUX_TMPDIR="); ok && value != "" {
				isolated = true
			}
		}
		if !isolated {
			t.Error("refusing pane cleanup without a private tmux directory")
			return
		}
	}
	if socketName != "" {
		args = append([]string{"-L", socketName}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", append(args, "-F", "#{pane_pid}")...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		// A stopped session or an env that never started tmux has no panes.
		missing := strings.Contains(string(out), "no server running") ||
			strings.Contains(string(out), "error connecting to") ||
			strings.Contains(string(out), "can't find session")
		if ctx.Err() != nil || !missing {
			if _, lookupErr := exec.LookPath("tmux"); lookupErr == nil {
				t.Errorf("list fake panes: %v: %s", err, out)
			}
		}
		return
	}
	groups := make(map[int]bool)
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 1 {
			t.Errorf("invalid fake pane PID %q", field)
			continue
		}
		group, err := syscall.Getpgid(pid)
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		// tmux makes its pane process the group leader. Never signal the
		// test runner's group or any unexpected group on a malformed reply.
		if err != nil || group != pid || group == syscall.Getpgrp() {
			t.Errorf("unsafe fake pane process group: pid=%d pgid=%d err=%v", pid, group, err)
			continue
		}
		if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill fake pane group %d: %v", group, err)
		}
		groups[group] = true
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(groups) > 0 && time.Now().Before(deadline) {
		for group := range groups {
			if errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
				delete(groups, group)
			}
		}
		if len(groups) > 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	for group := range groups {
		t.Errorf("fake pane group %d outlived test cleanup", group)
	}
}
