package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/recall/query"
	"github.com/asheshgoplani/agent-deck/internal/recall/reader"
	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

func saveSendOffset(t *testing.T, dir string, r *sendqueue.Record) {
	t.Helper()
	info, err := os.Stat(r.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	dev, ino := reader.FileIdentity(info)
	// JSON keeps the capability regressions runnable against the base record.
	b, err := json.Marshal(map[string]any{"transcript_offset": map[string]any{
		"path": r.TranscriptPath, "offset": info.Size(), "device": dev, "inode": ino,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, r); err != nil {
		t.Fatal(err)
	}
	r.TranscriptFrom = info.Size()
	if err := sendqueue.Save(dir, r); err != nil {
		t.Fatal(err)
	}
}

func TestRedeliverReopenSendOffset(t *testing.T) {
	for _, mode := range []string{"absent", "present", "replaced", "truncated", "legacy", "over cap"} {
		t.Run(mode, func(t *testing.T) {
			f := newRedeliverFixture(t, true)
			r := f.record(t, sendqueue.StateFailed)
			history := strings.Repeat(`{"type":"file-history-snapshot"}`+"\n", int(query.LandedScanBytes/30)+1)
			if err := os.WriteFile(f.path, []byte(history), 0600); err != nil {
				t.Fatal(err)
			}
			if mode != "legacy" {
				saveSendOffset(t, f.dir, r)
			}
			wantOutcome, wantReason := "blocked_unknown", "scan_limit"
			switch mode {
			case "absent":
				wantOutcome, wantReason = "pending", "would_reopen"
			case "present":
				wantReason = "content_present"
				history += claudeUserLine("landed", "[REDACTED]", f.now)
			case "replaced":
				if err := os.Rename(f.path, f.path+".old"); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				history = ""
			case "over cap":
				history += history
			}
			if err := os.WriteFile(f.path, []byte(history), 0600); err != nil {
				t.Fatal(err)
			}
			got := f.run(t, r, "offset", true)
			if got.Outcome != wantOutcome || got.Reason != wantReason {
				t.Fatalf("offset reopen: %s/%s, want %s/%s", got.Outcome, got.Reason, wantOutcome, wantReason)
			}
		})
	}
}

func TestSendOffsetPersistedBeforeChild(t *testing.T) {
	f := newRedeliverFixture(t, false)
	r := f.record(t, sendqueue.StateQueued)
	history := claudeUserLine("old", "[REDACTED-OTHER]", f.now.Add(-time.Hour))
	if err := os.WriteFile(f.path, []byte(history), 0600); err != nil {
		t.Fatal(err)
	}
	set := func(fn func(*sendqueue.Record)) error {
		saved, err := sendqueue.Update(f.dir, r.SendID, time.Now(), fn)
		if err == nil {
			*r = *saved
		}
		return err
	}
	called := false
	sendChild = func(_, _, _, result string) (int, func() int, error) {
		called = true
		saved, err := sendqueue.Load(f.dir, r.SendID)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(saved)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Offset *struct {
				Path   string `json:"path"`
				Offset int64  `json:"offset"`
				Device uint64 `json:"device"`
				Inode  uint64 `json:"inode"`
			} `json:"transcript_offset"`
		}
		if err := json.Unmarshal(b, &wire); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(f.path)
		if err != nil {
			t.Fatal(err)
		}
		dev, ino := reader.FileIdentity(info)
		if wire.Offset == nil || wire.Offset.Path != f.path || wire.Offset.Offset != int64(len(history)) || wire.Offset.Device != dev || wire.Offset.Inode != ino || saved.TranscriptFrom != int64(len(history)) || saved.State != sendqueue.StateTyping {
			t.Errorf("typing transition did not persist the send-time identity and offset")
		}
		if err := os.WriteFile(result, []byte(`{"success":true,"submitted":true}`), 0600); err != nil {
			t.Fatal(err)
		}
		return 123, func() int { return 0 }, nil
	}
	// A stale caller stat must not get paired with the newly captured identity.
	if !typeQueued("_test", f.dir, r, f.target, []*session.Instance{f.target}, "waiting", f.path, 0, set) || !called {
		t.Fatal("send child was not called")
	}
}

func TestRedeliverProofSendOffset(t *testing.T) {
	for _, mode := range []string{"unique", "ambiguous", "claimed", "past window", "replaced", "next send"} {
		t.Run(mode, func(t *testing.T) {
			f := newRedeliverFixture(t, false)
			r := f.record(t, sendqueue.StateTyped)
			history := strings.Repeat(`{"type":"file-history-snapshot"}`+"\n", int(query.LandedScanBytes/30)+1)
			if err := os.WriteFile(f.path, []byte(history), 0600); err != nil {
				t.Fatal(err)
			}
			saveSendOffset(t, f.dir, r)
			sent, err := time.Parse(time.RFC3339Nano, r.SentAt)
			if err != nil {
				t.Fatal(err)
			}
			at := sent.Add(time.Second)
			if mode == "past window" {
				at = sent.Add(11 * time.Minute)
			}
			history += claudeUserLine("one", "[REDACTED]", at)
			wantReason := ""
			switch mode {
			case "ambiguous":
				history += claudeUserLine("two", "[REDACTED]", at.Add(time.Second))
				wantReason = "content_present"
			case "claimed":
				other := f.record(t, sendqueue.StateLanded)
				other.Message, other.LandedRowID = "[REDACTED-OTHER]", "one"
				if err := sendqueue.Save(f.dir, other); err != nil {
					t.Fatal(err)
				}
				wantReason = "content_present"
			case "past window":
				wantReason = "no_landed_proof"
			case "replaced":
				if err := os.Rename(f.path, f.path+".old"); err != nil {
					t.Fatal(err)
				}
				wantReason = "scan_limit"
			case "next send":
				other := f.record(t, sendqueue.StateTyped)
				other.SentAt = at.Add(time.Second).Format(time.RFC3339Nano)
				other.TranscriptFrom = int64(len(history))
				if err := sendqueue.Save(f.dir, other); err != nil {
					t.Fatal(err)
				}
				history += claudeUserLine("two", "[REDACTED]", at.Add(2*time.Second))
			}
			if err := os.WriteFile(f.path, []byte(history), 0600); err != nil {
				t.Fatal(err)
			}
			row, reason := boundedSendProof(f.dir, r)
			if reason != wantReason || (reason == "" && row.ID != "one") {
				t.Fatalf("bounded offset proof: row=%s reason=%s, want %s", row.ID, reason, wantReason)
			}
		})
	}
}
