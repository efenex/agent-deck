package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/sandboxguard"
)

// HomeIsolationMarkerEnv is set during HOME+XDG isolation. Runtime guards (and
// the pathsafety guard test) read this to confirm a test context is sandboxed.
const HomeIsolationMarkerEnv = "AGENT_DECK_TEST_HOME_ISOLATED"

// ToolConfigDirEnvs are the agent tools' own config-dir overrides. Each one
// wins over $HOME in agent-deck's resolvers (GetClaudeConfigDir,
// getCodexHomeDir, getCopilotHomeDir, getDeepSeekHomeDir, geminiUserDir,
// PiExtensionsDir), and agent-deck itself exports CLAUDE_CONFIG_DIR (and
// DSH_HOME for DeepSeek sessions) into the sessions it starts. So a `go test`
// run from inside a worker session, or from a shell that exports any of them,
// inherits a path into the developer's live tool config: hook installs,
// settings.json and trusted-folder writes then land there even though HOME
// was swapped. IsolateHome clears them so they fall back under the isolated
// HOME, like the XDG dirs. The list lives in sandboxguard so the guard's
// failure message can name the variable a refused path came through.
var ToolConfigDirEnvs = sandboxguard.ToolConfigDirEnvs

// IsolateHome makes it safe for tests to resolve and write agent-deck runtime
// paths (~/.agent-deck/config.json, profiles/<p>/state.db, worker-scratch,
// logs, hooks) without ever touching the developer's real home directory.
//
// WHY THIS EXISTS (2026-06-04 data-loss incident — third of its class):
//
// `go test` resolves runtime paths via the HOME env var (os.UserHomeDir reads
// $HOME on Unix), NOT via the test's working directory. So running the suite
// from inside a git worktree still wrote to the real ~/.agent-deck. Test
// isolation up to that point relied solely on AGENTDECK_PROFILE=_test, which
// only scopes the *profile subdirectory* — GetAgentDeckDir(), config.json,
// worker-scratch/, and logs/ all still resolved under the real HOME. The
// concrete trigger: internal/ui's TestMain set AGENTDECK_PROFILE=_test but did
// NOT override HOME/XDG, so an un-sandboxed `go test ./internal/ui/...` wiped
// the live profile index + config.
//
// IsolateHome closes that gap by pointing HOME at a fresh per-call temp dir,
// CLEARING every XDG_* base dir so they track HOME, and setting
// AGENTDECK_PROFILE=_test as a second belt. It mirrors IsolateTmuxSocket
// (internal/testutil/tmuxenv.go).
//
// WHY XDG IS CLEARED, NOT PINNED (2026-06-07 ~96-test isolation regression):
//
// IsolateHome runs once per package (from TestMain), so its temp dir is SHARED
// by every test in the package. After #1294's XDG refactor, path resolution
// prefers an XDG location *if the file already exists* on disk. Pinning
// XDG_CONFIG_HOME to <pkg-tempdir>/.config meant one test writing config.toml
// there left a stale file that LATER tests then read instead of their own
// fresh slate — config bled across tests (catalog "empty", profile resolving
// to `default`, ~96 cross-package failures). Worse, the many tests that swap
// only HOME via t.TempDir() left XDG_CONFIG_HOME still pointing at the shared
// package dir, so their per-test config redirection silently did nothing.
//
// By clearing XDG_* instead, every base dir falls back to $HOME/.config,
// $HOME/.local/share, etc. (see agentpaths.xdgDir). Now any test that swaps
// HOME — which is the overwhelmingly common pattern — automatically gets a
// fully isolated, clean config/data/cache slate, with zero per-test wiring.
// Tests that genuinely need a specific XDG dir still set it explicitly via
// t.Setenv (auto-restored), which takes precedence.
//
// It sets:
//   - HOME             -> <tempdir>            (os.UserHomeDir source on Unix)
//   - XDG_CONFIG_HOME  -> ""  (cleared; resolves under $HOME/.config)
//   - XDG_DATA_HOME    -> ""  (cleared; resolves under $HOME/.local/share)
//   - XDG_CACHE_HOME   -> ""  (cleared; resolves under $HOME/.cache)
//   - XDG_STATE_HOME   -> ""  (cleared; resolves under $HOME/.local/state)
//   - CLAUDE_CONFIG_DIR, CODEX_HOME, ... -> "" (cleared; see ToolConfigDirEnvs)
//   - AGENTDECK_PROFILE -> _test
//   - AGENT_DECK_TEST_HOME_ISOLATED -> 1  (marker for guard/runtime checks)
//
// Call it from every package-level TestMain that transitively resolves an
// agent-deck path:
//
//	func TestMain(m *testing.M) {
//	    cleanupHome := testutil.IsolateHome()
//	    defer cleanupHome()
//	    cleanupTmux := testutil.IsolateTmuxSocket()
//	    defer cleanupTmux()
//	    os.Exit(m.Run())
//	}
//
// Returns a cleanup function that removes the temp dir and restores the
// original env so the parent process is not permanently altered.
//
// WHY THE GO CACHES ARE PINNED AND THE CLEANUP CHMODS (2026-10-21 ~2 GiB/run
// leak): any test that shells out to `go` with HOME=<ad-home-*> would fill
// $HOME/go/pkg/mod there. The module cache is written 0555/0444, so a plain
// os.RemoveAll fails and every full run stranded ~28 such dirs (~2 GiB). The
// fix is layered: GOMODCACHE/GOCACHE keep resolving to the developer's real
// caches (resolved before HOME moves), GOFLAGS gains -modcacherw so anything
// that does land in the sandbox stays removable, the cleanup restores owner
// write bits before removing, and an out-of-process watcher (see
// ReapHomeOnExit) removes the dir when this process dies without running the
// cleanup (-timeout panic, SIGKILL, goleak's os.Exit).
func IsolateHome() func() {
	type snap struct {
		key string
		val string
		had bool
	}

	keys := []string{
		"HOME",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"XDG_CACHE_HOME",
		"XDG_STATE_HOME",
		"AGENTDECK_PROFILE",
		HomeIsolationMarkerEnv,
		"GOMODCACHE",
		"GOCACHE",
		"GOFLAGS",
	}
	keys = append(keys, ToolConfigDirEnvs...)

	snaps := make([]snap, 0, len(keys))
	for _, k := range keys {
		v, had := os.LookupEnv(k)
		snaps = append(snaps, snap{key: k, val: v, had: had})
	}

	dir, err := os.MkdirTemp("", "ad-home-")
	if err != nil {
		// We must never fall back to the real HOME. A PID-keyed path under
		// /tmp is still safely off the real home.
		dir = fmt.Sprintf("/tmp/agent-deck-test-home-fallback-%d", os.Getpid())
		_ = os.MkdirAll(dir, 0o700)
	}

	// Resolve the Go caches against the REAL home before HOME moves; afterwards
	// `go` would default them to <ad-home>/go/pkg/mod and <ad-home>/Library/....
	pinGoCaches()

	_ = os.Setenv("HOME", dir)
	// Clear (do NOT pin) the XDG base dirs so they fall back to $HOME/*. This
	// keeps the per-package shared HOME from accumulating stale XDG config/data
	// across tests, and lets the common "swap HOME via t.TempDir()" pattern
	// isolate config automatically. See the doc comment above.
	_ = os.Unsetenv("XDG_CONFIG_HOME")
	_ = os.Unsetenv("XDG_DATA_HOME")
	_ = os.Unsetenv("XDG_CACHE_HOME")
	_ = os.Unsetenv("XDG_STATE_HOME")
	for _, k := range ToolConfigDirEnvs {
		_ = os.Unsetenv(k)
	}
	_ = os.Setenv("AGENTDECK_PROFILE", "_test")
	_ = os.Setenv(HomeIsolationMarkerEnv, "1")

	stopReaper := ReapHomeOnExit(dir)
	return func() {
		for _, s := range snaps {
			restoreEnv(s.key, s.val, s.had)
		}
		RemoveAllWritable(dir)
		stopReaper()
	}
}

// pinGoCaches exports GOMODCACHE and GOCACHE as the toolchain resolves them for
// the current (real) HOME, unless the caller already set them, and adds
// -modcacherw to GOFLAGS. Best effort: without a usable `go` the -modcacherw
// flag and the writable-chmod cleanup still keep the sandbox removable.
func pinGoCaches() {
	if os.Getenv("GOMODCACHE") == "" || os.Getenv("GOCACHE") == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOMODCACHE", "GOCACHE")
		cmd.WaitDelay = time.Second
		var caches map[string]string
		if out, err := cmd.Output(); err == nil && json.Unmarshal(out, &caches) == nil {
			for _, key := range []string{"GOMODCACHE", "GOCACHE"} {
				if os.Getenv(key) == "" && filepath.IsAbs(caches[key]) {
					_ = os.Setenv(key, caches[key])
				}
			}
		}
	}
	flags := os.Getenv("GOFLAGS")
	if !strings.Contains(flags, "-modcacherw") {
		_ = os.Setenv("GOFLAGS", strings.TrimSpace(flags+" -modcacherw"))
	}
}

// RemoveAllWritable is os.RemoveAll for a tree that may contain read-only
// directories (the Go module cache is written 0555/0444): it restores owner
// write permission on every directory first, as `go clean -modcache` does.
func RemoveAllWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintf(os.Stderr, "remove test home %s: %v\n", root, err)
	}
}
