package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
)

// This regression compiles against the R0 base: the old watcher claimed an
// already-used user row for a second identical send, silently hiding a loss.
func TestQueuedWatchDoesNotShareLandedRow(t *testing.T) {
	t.Setenv("AGENTDECK_SEND_LAND_WINDOW", "30ms")
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "native.jsonl")
	now := time.Now().UTC()
	if err := os.WriteFile(path, []byte(claudeUserLine("one-row", "[REDACTED]", now)), 0600); err != nil {
		t.Fatal(err)
	}
	first := &sendqueue.Record{SendID: sendqueue.NewID(now.Add(-time.Minute)), SessionID: "target", Tool: "claude", State: "landed", LandedRowID: "one-row", TranscriptPath: path, SentAt: now.Add(-time.Minute).Format(time.RFC3339Nano)}
	second := &sendqueue.Record{SendID: sendqueue.NewID(now), SessionID: "target", Tool: "claude", State: "typed", Message: "[REDACTED]", TranscriptPath: path, SentAt: now.Format(time.RFC3339Nano)}
	if err := sendqueue.Save(dir, first); err != nil {
		t.Fatal(err)
	}
	if err := sendqueue.Save(dir, second); err != nil {
		t.Fatal(err)
	}
	watchQueuedSend("_test", dir, second.SendID)
	got, err := sendqueue.Load(dir, second.SendID)
	if err != nil || got.State == "landed" || got.LandedRowID != "" {
		t.Fatalf("one native row proved two identical sends: %+v %v", got, err)
	}
}
