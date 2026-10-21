package main

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/send"
	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

// `session send --transport` accepts only the empty default and the two
// config values; a typo is a usage error, never a silent transport.
func TestValidSendTransportFlag(t *testing.T) {
	for v, want := range map[string]bool{"": true, "tmux": true, "auto": true, "socket": false, "TMUX": false, "tmx": false} {
		if got := validSendTransportFlag(v); got != want {
			t.Errorf("validSendTransportFlag(%q) = %v, want %v", v, got, want)
		}
	}
}

// --transport tmux with send_transport = "auto" in config types into the pane:
// the socket is never resolved, and the pin is not reported as a fallback.
func TestPerformSend_TransportFlagTmuxOverridesAutoConfig(t *testing.T) {
	mock := &mockSendRetryTarget{statuses: []string{"active"}, panes: []string{""}}
	inst := &session.Instance{ID: "i1", Title: "target", Tool: "claude", ClaudeSessionID: "sid"}
	resolved := false
	resolve := func(*session.Instance) (send.ClaudeSocketTarget, error) {
		resolved = true
		return send.ClaudeSocketTarget{}, nil
	}
	sendFn := func(send.ClaudeSocketTarget, string) (string, error) {
		t.Fatal("socket written despite --transport tmux")
		return "", nil
	}
	cfgValue := "auto"
	flagValue := "tmux"
	if flagValue != "" {
		cfgValue = flagValue // as handleSessionSend applies the flag
	}
	res, err := performSend(inst, mock, "hello", false, defaultSendTuning(), cfgValue, false, nil, resolve, sendFn)
	if err != nil {
		t.Fatalf("performSend: %v", err)
	}
	if res.transport != "tmux" || res.fallbackReason != "" {
		t.Fatalf("transport=%q fallbackReason=%q, want tmux and none", res.transport, res.fallbackReason)
	}
	if resolved {
		t.Fatal("socket target resolved for a tmux-pinned send")
	}
	if mock.sendKeysCalls == 0 && mock.sendChunkedCalls == 0 {
		t.Fatal("tmux target not exercised")
	}
}

func TestChildSendArgsCarriesTransport(t *testing.T) {
	args := childSendArgs("", "target", "/tmp/x.message", childSendOptions{transport: "tmux"})
	i := slices.Index(args, "--transport")
	if i < 0 || i+1 >= len(args) || args[i+1] != "tmux" {
		t.Fatalf("child argv %v lacks --transport tmux", args)
	}
	if slices.Contains(childSendArgs("", "target", "/tmp/x.message", childSendOptions{}), "--transport") {
		t.Fatal("unpinned child argv carries --transport")
	}
}

func TestSendQueueRecordKeepsTransport(t *testing.T) {
	dir, rec := queueControlFixture(t, sendqueue.StateQueued)
	if _, err := sendqueue.Update(dir, rec.SendID, time.Now(), func(r *sendqueue.Record) { r.Transport = "tmux" }); err != nil {
		t.Fatal(err)
	}
	got, err := sendqueue.Load(dir, rec.SendID)
	if err != nil || got.Transport != "tmux" {
		t.Fatalf("reloaded record transport = %q (%v)", got.Transport, err)
	}
}

// A queued send pinned with --transport hands the pin to its child, keeping
// the guarded/release options; an unpinned one still uses the plain child.
func TestTypeQueuedPassesTransportPin(t *testing.T) {
	prevWith, prevNormal, prevGuarded, prevRelease := sendChildWithOptions, sendChild, sendChildGuarded, sendChildRelease
	t.Cleanup(func() {
		sendChildWithOptions, sendChild, sendChildGuarded, sendChildRelease = prevWith, prevNormal, prevGuarded, prevRelease
	})
	ok := `{"success":true,"delivery":"delivered","submitted":true,"confirmation":"confirmed"}`
	write := func(t *testing.T, resultPath string) {
		if err := os.WriteFile(resultPath, []byte(ok), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	other := func(string, string, string, string) (int, func() int, error) {
		t.Fatal("pinned send used an unpinned child")
		return 0, nil, nil
	}

	cases := []struct {
		name, status string
		guarded      bool
		want         childSendOptions
	}{
		{"plain", "waiting", false, childSendOptions{transport: "tmux"}},
		{"guarded", "waiting", true, childSendOptions{transport: "tmux", requireInputPrompt: true}},
		{"release", queueReleaseStatus, false, childSendOptions{transport: "tmux", requireInputPrompt: true, noWait: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, rec := queueControlFixture(t, sendqueue.StateQueued)
			rec.Transport, rec.RequireInputPrompt = "tmux", tc.guarded
			if err := sendqueue.Save(dir, rec); err != nil {
				t.Fatal(err)
			}
			set := func(fn func(*sendqueue.Record)) error {
				stored, err := sendqueue.Update(dir, rec.SendID, time.Now(), fn)
				if err == nil {
					*rec = *stored
				}
				return err
			}
			sendChild, sendChildGuarded, sendChildRelease = other, other, other
			var got *childSendOptions
			sendChildWithOptions = func(_, _, _, resultPath string, opts childSendOptions) (int, func() int, error) {
				got = &opts
				write(t, resultPath)
				return 4242, func() int { return 0 }, nil
			}
			if !typeQueued("", dir, rec, &session.Instance{ID: rec.SessionID}, nil, tc.status, "", 0, set) {
				t.Fatal("typeQueued returned false")
			}
			if got == nil || *got != tc.want {
				t.Fatalf("child options = %+v, want %+v", got, tc.want)
			}
		})
	}

	t.Run("unpinned", func(t *testing.T) {
		dir, rec := queueControlFixture(t, sendqueue.StateQueued)
		set := func(fn func(*sendqueue.Record)) error {
			stored, err := sendqueue.Update(dir, rec.SendID, time.Now(), fn)
			if err == nil {
				*rec = *stored
			}
			return err
		}
		used := false
		sendChildWithOptions = func(string, string, string, string, childSendOptions) (int, func() int, error) {
			t.Fatal("unpinned send used the options child")
			return 0, nil, nil
		}
		sendChild = func(_, _, _, resultPath string) (int, func() int, error) {
			used = true
			write(t, resultPath)
			return 4242, func() int { return 0 }, nil
		}
		if !typeQueued("", dir, rec, &session.Instance{ID: rec.SessionID}, nil, "waiting", "", 0, set) || !used {
			t.Fatal("unpinned send did not use the plain child")
		}
	})
}
