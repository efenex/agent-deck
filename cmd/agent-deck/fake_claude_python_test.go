package main

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestClaudeLikeFakeLifecycle(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("requires python3")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-I", "testdata/test_claude_like_fake.py", "-v")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fake lifecycle: %v\n%s", err, out)
	}
}
