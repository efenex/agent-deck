package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

// childEvidence is what a queue child leaves in DeliveryEvidence for res:
// the `session send --json` success payload, read back from its result file.
func childEvidence(t *testing.T, res *sendDeliveryResult) map[string]interface{} {
	t.Helper()
	if res == nil {
		return nil
	}
	inst := session.NewInstanceWithTool("evidence-target", t.TempDir(), "claude")
	b, err := json.Marshal(sendSuccessData(inst, "[REDACTED]", *res, false))
	if err != nil {
		t.Fatal(err)
	}
	var evidence map[string]interface{}
	if err := json.Unmarshal(b, &evidence); err != nil {
		t.Fatal(err)
	}
	if outcome, _ := classifyChild(evidence, 0); outcome != childTyped && outcome != childSubmitted {
		t.Fatalf("child evidence classified %v", outcome)
	}
	return evidence
}

var (
	socketChild = &sendDeliveryResult{delivery: deliveryQueuedSocket, transport: "socket", socketMsgID: "synthetic-msg"}
	tmuxChild   = &sendDeliveryResult{delivery: deliveryDelivered, transport: "tmux"}
)

// A send typed into the pane never produces a Claude peer envelope. An
// envelope with the same text is another session's message (a native Claude
// peer, or another socket send), so it must not prove a tmux send landed:
// for short phone replies ("ok", "yes") that would hide a real loss.
func TestRedeliverPeerEnvelopeProvesOnlySocketSends(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "recall", "query", "testdata", "rows", "claude-socket-user.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		guarded bool
		child   *sendDeliveryResult
		outcome string
	}{
		{"guarded phone send", true, tmuxChild, "blocked_unknown"},
		{"guarded without evidence", true, nil, "blocked_unknown"},
		{"unguarded tmux send", false, tmuxChild, "blocked_unknown"},
		{"unguarded without evidence", false, nil, "blocked_unknown"},
		{"guarded claiming socket", true, socketChild, "blocked_unknown"},
		{"socket send", false, socketChild, "landed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRedeliverFixture(t, false)
			r := f.record(t, sendqueue.StateTyped)
			r.SentAt, r.Settled = "2026-11-14T08:00:00Z", true
			r.RequireInputPrompt, r.DeliveryEvidence = tc.guarded, childEvidence(t, tc.child)
			if err := os.WriteFile(f.path, b, 0600); err != nil {
				t.Fatal(err)
			}
			if err := sendqueue.Save(f.dir, r); err != nil {
				t.Fatal(err)
			}
			got := f.run(t, r, "transport-proof", false)
			if got.Outcome != tc.outcome {
				t.Fatalf("outcome = %s (%s), want %s", got.Outcome, got.Reason, tc.outcome)
			}
			if f.kicks.Load() != 0 {
				t.Fatal("proof scan emitted")
			}
		})
	}
}
