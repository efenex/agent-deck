package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsolateHomeClearsToolConfigDirs pins that IsolateHome does not leave a
// way back to the developer's live tool config. Each tool config-dir variable
// beats $HOME in agent-deck's resolvers, and agent-deck exports
// CLAUDE_CONFIG_DIR into the sessions it starts, so a test run started from a
// worker session used to inherit the real ~/.claude (or ~/.codex, ...) even
// with HOME swapped. After IsolateHome every one of them must be unset, and
// cleanup must put the caller's values back.
func TestIsolateHomeClearsToolConfigDirs(t *testing.T) {
	live := t.TempDir() // stands in for the developer's live config dirs
	want := map[string]string{}
	for _, k := range ToolConfigDirEnvs {
		want[k] = filepath.Join(live, strings.ToLower(k))
		t.Setenv(k, want[k])
	}

	cleanup := IsolateHome()
	home := os.Getenv("HOME")
	for _, k := range ToolConfigDirEnvs {
		if v, ok := os.LookupEnv(k); ok {
			cleanup()
			t.Fatalf("after IsolateHome %s=%q is still set; agent-deck resolves it ahead of the isolated HOME %q, so tests would write into the live dir", k, v, home)
		}
	}
	if home == "" || strings.HasPrefix(home, live) {
		cleanup()
		t.Fatalf("IsolateHome set HOME=%q, want a fresh temp dir", home)
	}
	cleanup()

	for _, k := range ToolConfigDirEnvs {
		if got := os.Getenv(k); got != want[k] {
			t.Errorf("cleanup did not restore %s: got %q, want %q", k, got, want[k])
		}
	}
}

// TestIsolateHomeClearsDSHHome: DSH_HOME beats ~/.dsh in getDeepSeekHomeDir
// and agent-deck exports it into DeepSeek sessions, so a test run started
// from one inherited the live DeepSeek home until IsolateHome cleared it.
func TestIsolateHomeClearsDSHHome(t *testing.T) {
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "live-dsh"))
	cleanup := IsolateHome()
	v, ok := os.LookupEnv("DSH_HOME")
	cleanup()
	if ok {
		t.Fatalf("after IsolateHome DSH_HOME=%q is still set", v)
	}
}

// A go module cache is written 0555/0444; the cleanup IsolateHome returns must
// still remove a HOME that has one in it (the 2026-10-21 ~2 GiB/run leak).
func TestIsolateHome_CleanupRemovesReadOnlyModuleCache(t *testing.T) {
	cleanup := IsolateHome()
	home := os.Getenv("HOME")
	if !strings.HasPrefix(filepath.Base(home), "ad-home-") {
		t.Fatalf("HOME = %q, want an ad-home-* dir", home)
	}
	mod := filepath.Join(home, "go", "pkg", "mod", "x@v1")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "f.go"), []byte("package x"), 0o444); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(mod, 0o555)
	_ = os.Chmod(filepath.Dir(mod), 0o555)

	cleanup()

	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("IsolateHome cleanup left %s behind (stat err=%v)", home, err)
	}
}

// Anything `go` does under the sandboxed HOME must not dirty it: the caches
// stay on the real ones and a module cache that does land there is writable.
func TestIsolateHome_KeepsGoCachesOutOfSandbox(t *testing.T) {
	t.Setenv("GOMODCACHE", "")
	t.Setenv("GOCACHE", "")
	t.Setenv("GOFLAGS", "-count=1")
	cleanup := IsolateHome()
	defer cleanup()
	home := os.Getenv("HOME")
	for _, key := range []string{"GOMODCACHE", "GOCACHE"} {
		if v := os.Getenv(key); v != "" && strings.HasPrefix(v, home) {
			t.Errorf("%s = %q is inside the sandbox HOME %q", key, v, home)
		}
	}
	if f := os.Getenv("GOFLAGS"); !strings.Contains(f, "-modcacherw") || !strings.Contains(f, "-count=1") {
		t.Errorf("GOFLAGS = %q, want existing flags plus -modcacherw", f)
	}
}
