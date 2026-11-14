package terminal

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRemoteSSHAttachCommandExecution(t *testing.T) {
	dir := t.TempDir()
	// Parse the local command, then emulate ssh joining its command operands
	// and passing them to the remote shell. Both quoting layers must survive.
	fakeSSH := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in
    -tt) shift ;;
    -o|-F) shift 2 ;;
    *) shift; break ;;
  esac
done
unset TERM COLORTERM
exec /bin/sh -c "$*"
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fakeSSH), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, colorTerm, executable, profile, session string }{
		{"truecolor", "truecolor", "capture", "work", "session"},
		{"24bit", "24bit", "capture", "work", "session"},
		{"unset", "", "capture", "work", "session"},
		{"odd", "odd'; exit 99; #", "capture", "work", "session"},
		{"quoted", "", "capture ' executable", "profile ' quoted", "session ' quoted"},
		{"metachar", "", "capture $(touch PWNED)", "profile `touch PWNED`", "session;$HOME\\*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executable := filepath.Join(dir, tc.executable)
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$TERM\" \"${COLORTERM-}\" \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			command := BuildAttachCommand(AttachRequest{
				Name: tc.session,
				Remote: &RemoteAttach{
					Host: "fixture", AgentDeckPath: executable, Profile: tc.profile,
				},
			})
			cmd := exec.Command("/bin/sh", "-c", command)
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "TERM=xterm-ghostty"}
			if tc.colorTerm != "" {
				cmd.Env = append(cmd.Env, "COLORTERM="+tc.colorTerm)
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("command failed: %v: %s", err, output)
			}
			// The raw launcher advertises the embedded emulator's capabilities.
			want := []string{"xterm-256color", "truecolor", "-p", tc.profile, "session", "attach", tc.session}
			got := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("captured = %q, want %q", got, want)
			}
		})
	}
}
