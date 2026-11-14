package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
)

// The receiver fixtures come from the writer, not from live conversations.
// Exercise the actual command as well as the query-level matcher tests.
func TestR1SocketRedeliverCLI(t *testing.T) {
	for _, name := range []string{"socket-user", "socket-tagged-user", "socket-string-user"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			id := addSessionJSON(t, home, "r1-synthetic", "claude")
			message := "[REDACTED]"
			if name != "socket-user" {
				message = "[agent-deck from:synthetic-sender]\n" + message
			}
			stdout, _, _ := runAgentDeck(t, home, "session", "send", id, message, "--queue", "--json")
			var receipt struct {
				SendID string `json:"send_id"`
			}
			if err := json.Unmarshal([]byte(stdout), &receipt); err != nil || receipt.SendID == "" {
				t.Fatal("missing synthetic queue receipt")
			}
			dir := filepath.Dir(queuedRecordPathForRedeliverTest(t, home, receipt.SendID))
			rec, err := sendqueue.Load(dir, receipt.SendID)
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join("..", "..", "internal", "recall", "query", "testdata", "rows", "claude-"+name+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			rec.State, rec.Settled, rec.Tool = sendqueue.StateTyped, true, "claude"
			rec.SentAt, rec.Message = "2026-11-14T08:00:00Z", message
			rec.DeliveryEvidence = childEvidence(t, socketChild)
			rec.TranscriptPath = filepath.Join(home, "synthetic.jsonl")
			if err := os.WriteFile(rec.TranscriptPath, b, 0600); err != nil {
				t.Fatal(err)
			}
			if err := sendqueue.Save(dir, rec); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := runAgentDeck(t, home, "session", "redeliver", rec.SendID, "--request-id", "r1-socket", "--json")
			var result redeliverResult
			if code != 0 || json.Unmarshal([]byte(stdout), &result) != nil {
				t.Fatalf("command failed: %d %s", code, stderr)
			}
			if result.Outcome != "landed" || result.Snapshot.LandedRowID != name {
				t.Fatalf("UTC=%s outcome=%s reason=%s row=%s", time.Now().UTC().Format(time.RFC3339), result.Outcome, result.Reason, result.Snapshot.LandedRowID)
			}
		})
	}
}

func TestR1SocketProofGuards(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "recall", "query", "testdata", "rows", "claude-socket-user.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	at, _ := time.Parse(time.RFC3339, "2026-11-14T08:00:00Z")
	for _, mode := range []string{"one", "claimed", "multiple", "time", "offset"} {
		t.Run(mode, func(t *testing.T) {
			f := newRedeliverFixture(t, false)
			r := f.record(t, sendqueue.StateTyped)
			r.SentAt, r.Settled = at.Format(time.RFC3339Nano), true
			r.DeliveryEvidence = childEvidence(t, socketChild)
			data := append([]byte(nil), b...)
			switch mode {
			case "claimed":
				owner := f.record(t, sendqueue.StateLanded)
				owner.SentAt, owner.LandedRowID = at.Add(-time.Minute).Format(time.RFC3339Nano), "socket-user"
				if err := sendqueue.Save(f.dir, owner); err != nil {
					t.Fatal(err)
				}
			case "multiple":
				var row map[string]any
				if err := json.Unmarshal(b, &row); err != nil {
					t.Fatal(err)
				}
				row["uuid"] = "socket-other"
				other, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, append(other, '\n')...)
			case "time":
				r.SentAt = at.Add(-11 * time.Minute).Format(time.RFC3339Nano)
			case "offset":
				r.TranscriptFrom = int64(len(data))
			}
			if err := os.WriteFile(f.path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := sendqueue.Save(f.dir, r); err != nil {
				t.Fatal(err)
			}
			got := f.run(t, r, "r1-guard", false)
			if mode == "one" {
				if got.Outcome != "landed" {
					t.Fatalf("socket not proven: %+v", got.RedeliveryDecision)
				}
			} else if got.Outcome != "blocked_unknown" {
				t.Fatalf("proof guard bypassed: %+v", got.RedeliveryDecision)
			}
			if f.kicks.Load() != 0 {
				t.Fatal("proof scan emitted")
			}
		})
	}
}
