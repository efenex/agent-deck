package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/recall/query"
	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

// A restarted native conversation of the same harness is allowed to reopen
// (docs/redeliver.md). The offset belongs to the old file; the new, small
// file keeps the legacy full scan.
func TestRVRedeliverOffsetRestartedConversation(t *testing.T) {
	f := newRedeliverFixture(t, true)
	r := f.record(t, sendqueue.StateFailed)
	if err := os.WriteFile(f.path, []byte(claudeUserLine("old", "[REDACTED-OTHER]", f.now.Add(-time.Hour))), 0600); err != nil {
		t.Fatal(err)
	}
	saveSendOffset(t, f.dir, r)
	f.target.ClaudeSessionID = "22222222-2222-4222-8222-222222222222"
	restarted := session.ClaudeTranscriptPathForInstance(f.target)
	if restarted == f.path {
		t.Fatal("fixture did not change the transcript path")
	}
	if err := os.MkdirAll(filepath.Dir(restarted), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(restarted, []byte(claudeUserLine("new", "[REDACTED-OTHER]", f.now)), 0600); err != nil {
		t.Fatal(err)
	}
	if got := f.run(t, r, "restart", true); got.Outcome != "pending" || got.Reason != "would_reopen" {
		t.Fatalf("restarted conversation: %s/%s, want pending/would_reopen", got.Outcome, got.Reason)
	}
}

// A complete row written before the offset was captured precedes the typing,
// so bounded proof must not claim it (base scanned from TranscriptFrom).
func TestRVProofOffsetExcludesPreSendRow(t *testing.T) {
	for _, large := range []bool{false, true} {
		for _, mode := range []string{"pre-send only", "pre-send then landed"} {
			t.Run(mode+map[bool]string{false: "/small", true: "/over cap"}[large], func(t *testing.T) {
				f := newRedeliverFixture(t, false)
				r := f.record(t, sendqueue.StateTyped)
				sent, err := time.Parse(time.RFC3339Nano, r.SentAt)
				if err != nil {
					t.Fatal(err)
				}
				history := ""
				if large {
					history = strings.Repeat(`{"type":"file-history-snapshot"}`+"\n", int(query.LandedScanBytes/30)+1)
				}
				history += claudeUserLine("prior", "[REDACTED]", sent.Add(-time.Second))
				if err := os.WriteFile(f.path, []byte(history), 0600); err != nil {
					t.Fatal(err)
				}
				saveSendOffset(t, f.dir, r)
				wantRow, wantReason := "", "no_landed_proof"
				if mode == "pre-send then landed" {
					history += claudeUserLine("one", "[REDACTED]", sent.Add(time.Second))
					wantRow, wantReason = "one", ""
				}
				if err := os.WriteFile(f.path, []byte(history), 0600); err != nil {
					t.Fatal(err)
				}
				row, reason := boundedSendProof(f.dir, r)
				if reason != wantReason || row.ID != wantRow {
					t.Fatalf("bounded proof: row=%q reason=%q, want row=%q reason=%q", row.ID, reason, wantRow, wantReason)
				}
			})
		}
	}
}
