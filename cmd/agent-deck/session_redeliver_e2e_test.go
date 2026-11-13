package main

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real CLI, isolated tmux and the existing synthetic Claude fixture: a
// stopped-target failure recovers once, or is blocked by an agent's resend.
func TestRedeliverE2EFailedRestartAndResend(t *testing.T) {
	for _, resent := range []bool{false, true} {
		name := "reopen"
		if resent {
			name = "agent-resend"
		}
		t.Run(name, func(t *testing.T) {
			e := newDeliveryEnv(t)
			// This case restarts the fake in its existing conversation. The
			// general delivery fixture only needs fresh --session-id starts.
			wrapper := filepath.Join(e.root, "fakebin", "claude")
			writeFile(t, wrapper, strings.Replace(deliveryFakeClaude, "--session-id)", "--session-id|--resume)", 1), 0700)
			if out, err := exec.Command("sh", "-n", wrapper).CombinedOutput(); err != nil {
				t.Fatalf("fake wrapper syntax: %s %v", out, err)
			}
			f := e.startFake("r0-"+name, "idle", "")
			e.mustRun("session", "stop", f.id, "--json")
			out, diag, code := e.run("", nil, "session", "send", f.id, "[REDACTED]", "--queue", "--json")
			var failed sendRecordJSON
			if code != 1 || json.Unmarshal([]byte(out), &failed) != nil || failed.State != "failed" {
				t.Fatalf("stopped enqueue: %d %s %s", code, out, diag)
			}
			e.mustRun("session", "start", f.id, "--json")
			e.waitPane(f, "restarted prompt", func(p string) bool { return strings.Contains(p, "All four steps ran.") && strings.Contains(p, "❯") })
			e.hook(f, "SessionStart")
			if resent {
				copyID := e.queue(f, "[REDACTED]")
				if st := e.waitSettled(f, copyID, "[REDACTED]", 30*time.Second); st.State != "landed" {
					t.Fatalf("resend did not land: %+v", st)
				}
			}
			var result redeliverResult
			out = e.mustRun("session", "redeliver", failed.SendID, "--request-id", "operator-tap", "--json")
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err)
			}
			if resent {
				if result.Outcome != "blocked_unknown" || result.Reason != "content_present" {
					t.Fatal(result)
				}
			} else {
				if result.Outcome != "pending" || result.Reason != "reopened" {
					t.Fatal(result)
				}
				if st := e.waitSettled(f, failed.SendID, "[REDACTED]", 30*time.Second); st.State != "landed" {
					t.Fatalf("reopened send did not land: %+v", st)
				}
			}
			out = e.mustRun("session", "redeliver", failed.SendID, "--request-id", "operator-tap", "--json")
			var replay redeliverResult
			if err := json.Unmarshal([]byte(out), &replay); err != nil || !replay.Replayed || replay.RedeliveryDecision != result.RedeliveryDecision {
				t.Fatalf("replay: %+v %v", replay, err)
			}
			if n := f.received(t, "[REDACTED]"); n != 1 {
				t.Fatalf("received %d copies", n)
			}
		})
	}
}
