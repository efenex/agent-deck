package tmux

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// resetTitleSettings puts the package-level title state back to its zero value
// so each test starts from "nothing has been set".
func resetTitleSettings(t *testing.T) {
	t.Helper()
	reset := func() {
		titleFormat = atomic.Value{}
		titleSettingsProvider = atomic.Value{}
		titleSettingsOnce = sync.Once{}
		hideCwdPrefixInTitle.Store(false)
		hideCwdPrefixInTitleSet.Store(false)
	}
	reset()
	t.Cleanup(reset)
}

func titleStringFor(s *Session) string {
	args := s.buildTerminalTitleArgs()
	for i, a := range args {
		if a == "set-titles-string" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// A one-shot process — `agent-deck rename`, a move, `session start`, the web
// daemon — never ran the TUI's config load. Before the provider fallback it
// therefore rewrote set-titles-string back to the built-in template, silently
// undoing the operator's configured title until the TUI touched the session
// again.
func TestTitleSettings_ConfigFallbackAppliesWithoutExplicitSetters(t *testing.T) {
	resetTitleSettings(t)
	SetTitleSettingsProvider(func() (string, bool) { return "{group} / {name}", false })

	sess := &Session{Name: "s", DisplayName: "relay-fix", GroupPath: "luna/ha"}
	got := titleStringFor(sess)
	want := "#{@agentdeck_group_path} / #{@agentdeck_display_name}"
	if got != want {
		t.Fatalf("set-titles-string = %q, want %q", got, want)
	}
}

func TestTitleSettings_ConfigFallbackAppliesIncludeCwdPrefix(t *testing.T) {
	resetTitleSettings(t)
	SetTitleSettingsProvider(func() (string, bool) { return "", true })

	got := titleStringFor(&Session{Name: "s", DisplayName: "relay-fix"})
	if want := "#{@agentdeck_display_name}"; got != want {
		t.Fatalf("set-titles-string = %q, want %q", got, want)
	}
}

// The TUI calls the setters on startup and on every config reload; those must
// win over the fallback, including when they set the zero value deliberately.
func TestTitleSettings_ExplicitSettersWinOverProvider(t *testing.T) {
	resetTitleSettings(t)
	SetTitleSettingsProvider(func() (string, bool) { return "{group} / {name}", true })
	SetTitleFormat("")
	SetHideCwdPrefixInTitle(false)

	got := titleStringFor(&Session{Name: "s", DisplayName: "relay-fix", GroupPath: "luna/ha"})
	want := "[#{@agentdeck_project_name}] #{@agentdeck_display_name}"
	if got != want {
		t.Fatalf("explicit setters were overridden: set-titles-string = %q, want %q", got, want)
	}
}

// No provider registered (any binary that links internal/tmux without
// internal/session) must behave exactly as before.
func TestTitleSettings_NoProviderKeepsHistoricalFormat(t *testing.T) {
	resetTitleSettings(t)

	got := titleStringFor(&Session{Name: "s", DisplayName: "relay-fix"})
	want := "[#{@agentdeck_project_name}] #{@agentdeck_display_name}"
	if got != want {
		t.Fatalf("set-titles-string = %q, want %q", got, want)
	}
}

// The config read is bounded to once per process: it happens on a tmux write
// path that runs on every rename, move and reconnect.
func TestTitleSettings_ProviderReadOnce(t *testing.T) {
	resetTitleSettings(t)
	var calls int32
	SetTitleSettingsProvider(func() (string, bool) {
		atomic.AddInt32(&calls, 1)
		return "{name}", false
	})

	for i := 0; i < 5; i++ {
		if got := titleStringFor(&Session{Name: "s", DisplayName: "relay-fix"}); !strings.Contains(got, "@agentdeck_display_name") {
			t.Fatalf("unexpected title string: %q", got)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("provider called %d times, want exactly 1", n)
	}
}
