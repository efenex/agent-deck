package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSSHAttachCommandExecution(t *testing.T) {
	for _, tc := range []struct{ name, terminal, want string }{
		{"portable", "screen", "screen"},
		{"unknown", "xterm-ghostty", "xterm-256color"},
		{"hostile", "x; touch /unwanted", "xterm-256color"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", tc.terminal)
			executable := filepath.Join(t.TempDir(), "capture executable")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$TERM\" \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			runner := &SSHRunner{Host: "fixture", AgentDeckPath: executable, Profile: "profile ' quoted"}
			args := runner.buildAttachArgs("session ' quoted")
			output, err := exec.Command("/bin/sh", "-c", args[len(args)-1]).CombinedOutput()
			if err != nil {
				t.Fatalf("command failed: %v: %s", err, output)
			}
			want := []string{tc.want, "-p", "profile ' quoted", "session", "attach", "session ' quoted"}
			got := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("captured = %q, want %q", got, want)
			}
		})
	}
}

func TestSSHAttachCOLORTERM(t *testing.T) {
	for _, tc := range []struct{ name, value, want string }{
		{"truecolor", "truecolor", "truecolor"},
		{"24bit", "24bit", "24bit"},
		{"unset", "", ""},
		{"odd", "truecolor'; exit 99; #", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", "tmux-256color")
			t.Setenv("COLORTERM", tc.value)
			if tc.name == "unset" {
				if err := os.Unsetenv("COLORTERM"); err != nil {
					t.Fatal(err)
				}
			}
			executable := filepath.Join(t.TempDir(), "capture ' executable")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$TERM\" \"${COLORTERM-}\" \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			runner := &SSHRunner{Host: "fixture", AgentDeckPath: executable, Profile: "profile ' quoted"}
			args := runner.buildAttachArgs("session ' quoted")
			remoteCmd := args[len(args)-1]
			if got := strings.Contains(remoteCmd, "COLORTERM="); got != (tc.want != "") {
				t.Fatalf("COLORTERM assignment present = %v: %q", got, remoteCmd)
			}
			cmd := exec.Command("/bin/sh", "-c", remoteCmd)
			// SSH does not normally forward the local COLORTERM. Start the
			// simulated remote shell without it so inheritance cannot mask a bug.
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("command failed: %v: %s", err, output)
			}
			want := []string{"tmux-256color", tc.want, "-p", "profile ' quoted", "session", "attach", "session ' quoted"}
			got := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("captured = %q, want %q", got, want)
			}
		})
	}
}
