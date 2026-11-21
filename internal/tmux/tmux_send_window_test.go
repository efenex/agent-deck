// Regression tests for issue #1369: quick-approve (`a`) must be able to deliver
// "1"+Enter to a SPECIFIC tmux window — the one showing the Claude prompt —
// not just the session's active window. SendKeysAndEnterToWindow targets
// "<session>:<windowIndex>"; ordinary sends target the managed agent window,
// independently of the active window. These tests record the exact
// send-keys argv via the keySenderExec seam (see recordKeySender in
// tmux_vim_mode_test.go), so they run without a real tmux server.
package tmux

import (
	"fmt"
	"strings"
	"testing"
)

// TestSendKeysAndEnterToWindow_TargetsWindowIndex: the non-vim window send must
// emit exactly the literal paste then Enter, both aimed at "<session>:<idx>".
func TestSendKeysAndEnterToWindow_TargetsWindowIndex(t *testing.T) {
	calls := recordKeySender(t)

	s := &Session{Name: "multiwin"} // VimMode defaults to false
	if err := s.SendKeysAndEnterToWindow(3, "1"); err != nil {
		t.Fatalf("SendKeysAndEnterToWindow returned error: %v", err)
	}

	c := *calls
	if len(c) != 2 {
		t.Fatalf("expected 2 tmux calls (paste, Enter), got %d: %v", len(c), c)
	}
	for _, call := range c {
		if !strings.Contains(call, "-t multiwin:3") {
			t.Fatalf("call not targeted at window multiwin:3: %q", call)
		}
	}
	if !strings.Contains(c[0], "-l") || !strings.Contains(c[0], "-- 1") {
		t.Fatalf("first call must be the literal paste of \"1\": %q", c[0])
	}
	if sentKey(c[1]) != "Enter" {
		t.Fatalf("second call must be Enter, got %q (%v)", sentKey(c[1]), c)
	}
}

func TestDefaultKeyDelivery_TargetsAgentWindow(t *testing.T) {
	for _, vim := range []bool{false, true} {
		s := &Session{Name: "multiwin", VimMode: vim}
		for name, send := range map[string]func() error{
			"literal":         func() error { return s.SendKeys("hello") },
			"enter":           s.SendEnter,
			"interrupt":       s.SendCtrlC,
			"clear":           s.SendCtrlU,
			"checked enter":   func() error { return s.SendEnterChecked(nil) },
			"named":           func() error { return s.SendNamedKey("Tab") },
			"submit":          func() error { return s.SendKeysAndEnter("hello") },
			"checked submit":  func() error { return s.SendKeysAndEnterChecked("hello", nil, nil) },
			"guarded submit":  func() error { return s.SendKeysAndEnterCheckedGuarded("hello", nil, nil, nil) },
			"chunked":         func() error { return s.SendKeysChunked("hello") },
			"checked chunked": func() error { return s.SendKeysChunkedChecked("hello", nil) },
		} {
			t.Run(fmt.Sprintf("%s/vim=%v", name, vim), func(t *testing.T) {
				calls := recordKeySender(t)
				if err := send(); err != nil {
					t.Fatal(err)
				}
				if len(*calls) == 0 {
					t.Fatal("no keystrokes recorded")
				}
				for _, call := range *calls {
					if !strings.Contains(call, "-t multiwin:^ ") {
						t.Fatalf("keystroke must target agent window: %q", call)
					}
				}
			})
		}
	}
}

func TestPrimaryWindowKeyDelivery_TargetsFirstWindow(t *testing.T) {
	calls := recordKeySender(t)
	s := &Session{Name: "multiwin"}

	if err := s.SendKeysToPrimaryWindow("hello"); err != nil {
		t.Fatal(err)
	}
	if err := s.SendNamedKeyToPrimaryWindow("Tab"); err != nil {
		t.Fatal(err)
	}
	if err := s.SendEnterToPrimaryWindow(); err != nil {
		t.Fatal(err)
	}

	if len(*calls) != 3 {
		t.Fatalf("expected text, Tab, Enter calls; got %v", *calls)
	}
	for _, call := range *calls {
		if !strings.Contains(call, "-t multiwin:^") {
			t.Fatalf("primary-window call targeted active window: %q", call)
		}
	}
}

// TestSendKeysAndEnterToWindow_VimMode_GuardsThenTargetsWindow verifies the
// window send still honors the #1264 vim insert-guard (Escape, i) and that the
// guard, paste, and Enter are ALL aimed at the window target.
func TestSendKeysAndEnterToWindow_VimMode_GuardsThenTargetsWindow(t *testing.T) {
	calls := recordKeySender(t)

	s := &Session{Name: "vimwin", VimMode: true}
	if err := s.SendKeysAndEnterToWindow(2, "1"); err != nil {
		t.Fatalf("SendKeysAndEnterToWindow returned error: %v", err)
	}

	c := *calls
	if len(c) != 4 {
		t.Fatalf("expected 4 calls (Escape, i, paste, Enter), got %d: %v", len(c), c)
	}
	for _, call := range c {
		if !strings.Contains(call, "-t vimwin:2") {
			t.Fatalf("vim window send not targeted at vimwin:2: %q", call)
		}
	}
	if sentKey(c[0]) != "Escape" || sentKey(c[1]) != "i" {
		t.Fatalf("vim guard must be Escape then i first: %v", c)
	}
	if sentKey(c[3]) != "Enter" {
		t.Fatalf("final key must be Enter, got %q (%v)", sentKey(c[3]), c)
	}
}
