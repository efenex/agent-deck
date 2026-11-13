package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRemoteRedeliverAndNoResumeArgs(t *testing.T) {
	for _, args := range [][]string{{"session", "redeliver", "send-id", "--request-id", "K", "--check-only", "--json"}, {"session", "send-status", "send-id", "--no-resume", "--json"}} {
		got, err := remoteCommandArgs(args)
		if err != nil || !reflect.DeepEqual(got, args) {
			t.Fatalf("forward args: %v %v", got, err)
		}
	}
}

func TestRemoteRedeliverOwnerOnlyAndUnsupported(t *testing.T) {
	for _, mode := range []string{"new", "old", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			home, shim := t.TempDir(), t.TempDir()
			writeQueueRemoteConfig(t, home)
			log := filepath.Join(shim, "calls")
			script := `#!/bin/sh
for cmd; do :; done
printf '%s\n' "$cmd" >> ` + log + `
case "$cmd" in
  *"'session' 'redeliver'"*)
    case ` + mode + ` in
      old) printf 'Usage: agent-deck session <command>\n'; printf 'Error: unknown session command: redeliver\n' >&2; exit 1 ;;
      disconnect) exit 255 ;;
      new) printf '{"outcome":"pending","reason":"owner_decided"}\n'; exit 0 ;;
    esac ;;
esac
exit 99
`
			path := filepath.Join(shim, "ssh")
			if err := os.WriteFile(path, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
				t.Fatalf("shim syntax: %s %v", out, err)
			}
			t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
			stdout, stderr, code := runAgentDeck(t, home, "remote", "lab", "session", "redeliver", "01ARZ3NDEKTSV4RRFFQ69G5FAV", "--request-id", "tap-key", "--json")
			switch mode {
			case "new":
				if code != 0 || !strings.Contains(stdout, `"reason":"owner_decided"`) {
					t.Fatalf("owner: %d %s %s", code, stdout, stderr)
				}
			case "old":
				if code != 0 || !strings.Contains(stdout, `"outcome":"unsupported"`) || strings.Contains(stdout+stderr, "Usage:") {
					t.Fatalf("old owner: %d %s %s", code, stdout, stderr)
				}
			case "disconnect":
				if code == 0 {
					t.Fatalf("disconnect treated as success: %s %s", stdout, stderr)
				}
			}
			calls, _ := os.ReadFile(log)
			if strings.Count(strings.TrimSpace(string(calls)), "\n") != 0 || !strings.Contains(string(calls), "'--request-id' 'tap-key'") {
				t.Fatalf("remote retried or rewrote key: %s", calls)
			}
			_ = filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
				if err == nil && d.IsDir() && d.Name() == "sendqueue" {
					t.Error("remote call touched local queue", path)
				}
				return err
			})
		})
	}
}
