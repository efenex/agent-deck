package main

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
)

// Synthetic event-time experiment, not a measurement of production incidence.
// Only the watcher's wall-clock wait is compressed; rows retain their actual
// offsets from send time, including the two-minute and ten-minute boundaries.
func TestR1PhoneLateRescanMeasurement(t *testing.T) {
	t.Setenv("AGENTDECK_SEND_LAND_WINDOW", "")
	if sendLandWindow() != 2*time.Minute {
		t.Fatal("experiment assumes the production two-minute watch")
	}
	t.Setenv("AGENTDECK_SEND_LAND_WINDOW", "1ms")
	lost, recovered := 0, 0
	for _, tc := range []struct {
		name   string
		delay  time.Duration
		mode   string
		landed bool
	}{
		{"absorb_121s", 121 * time.Second, "absorb", true},
		{"absorb_180s", 180 * time.Second, "absorb", true},
		{"absorb_599s", 599 * time.Second, "absorb", true},
		{"absorb_601s", 601 * time.Second, "absorb", false},
		{"never_absorbed", 0, "none", false},
		{"multiple_rows", 180 * time.Second, "multiple", false},
		{"claimed_row", 180 * time.Second, "claimed", false},
		{"next_identical_send", 180 * time.Second, "fenced", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRedeliverFixture(t, false)
			sent := f.now.Add(-15 * time.Minute)
			q := func(op, reason string, at time.Time) string {
				return fmt.Sprintf(`{"type":"queue-operation","operation":%q,"reason":%q,"timestamp":%q,"content":"[REDACTED]"}`+"\n", op, reason, at.Format(time.RFC3339Nano))
			}
			prefix := q("enqueue", "", sent)
			if err := os.WriteFile(f.path, []byte(prefix), 0600); err != nil {
				t.Fatal(err)
			}
			r := f.record(t, sendqueue.StateTyped)
			r.SentAt, r.RequireInputPrompt = sent.Format(time.RFC3339Nano), true
			if err := sendqueue.Save(f.dir, r); err != nil {
				t.Fatal(err)
			}
			// At the two-minute snapshot only the enqueue exists. The real
			// watcher must settle typed, without a claimed row or emission.
			watchLanded("_test", f.dir, r, func(fn func(*sendqueue.Record)) error {
				fn(r)
				return sendqueue.Save(f.dir, r)
			})
			if !r.Settled || r.State != sendqueue.StateTyped || r.LandedRowID != "" {
				t.Fatal("watch did not settle typed")
			}
			lost++
			later := ""
			switch tc.mode {
			case "absorb", "fenced":
				later = q("remove", "absorbed_mid_turn", sent.Add(tc.delay))
			case "multiple":
				later = q("remove", "dequeued", sent.Add(tc.delay)) + claudeUserLine("r1-one", r.Message, sent.Add(tc.delay)) + claudeUserLine("r1-two", r.Message, sent.Add(tc.delay+time.Second))
			case "claimed":
				later = q("remove", "dequeued", sent.Add(tc.delay)) + claudeUserLine("r1-claimed", r.Message, sent.Add(tc.delay))
				owner := f.record(t, sendqueue.StateLanded)
				owner.LandedRowID = "r1-claimed"
				owner.SentAt = sent.Add(-time.Minute).Format(time.RFC3339Nano)
				if err := sendqueue.Save(f.dir, owner); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mode == "fenced" {
				next := f.record(t, sendqueue.StateTyped)
				next.SentAt, next.TranscriptFrom = sent.Add(150*time.Second).Format(time.RFC3339Nano), int64(len(prefix))
				if err := sendqueue.Save(f.dir, next); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(f.path, []byte(prefix+later), 0600); err != nil {
				t.Fatal(err)
			}
			got := f.run(t, r, "r1-measure", false)
			if (got.Outcome == "landed") != tc.landed {
				t.Fatalf("unexpected decision: %+v", got.RedeliveryDecision)
			}
			if got.Outcome == "landed" {
				recovered++
			}
			if f.kicks.Load() != 0 {
				t.Fatal("rescan started a sender")
			}
			t.Logf("UTC=%s settled=typed late_outcome=%s reason=%s", time.Now().UTC().Format(time.RFC3339), got.Outcome, got.Reason)
		})
	}
	t.Logf("UTC=%s synthetic_lost=%d recovered=%d blocked=%d", time.Now().UTC().Format(time.RFC3339), lost, recovered, lost-recovered)
}
