package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
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
		name     string
		guarded  bool
		evidence map[string]interface{}
		outcome  string
	}{
		{"guarded phone send", true, map[string]interface{}{"transport": "tmux"}, "blocked_unknown"},
		{"guarded without evidence", true, nil, "blocked_unknown"},
		{"unguarded tmux send", false, map[string]interface{}{"transport": "tmux"}, "blocked_unknown"},
		{"unguarded without evidence", false, nil, "blocked_unknown"},
		{"guarded claiming socket", true, map[string]interface{}{"transport": "socket"}, "blocked_unknown"},
		{"socket send", false, map[string]interface{}{"transport": "socket"}, "landed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRedeliverFixture(t, false)
			r := f.record(t, sendqueue.StateTyped)
			r.SentAt, r.Settled = "2026-11-14T08:00:00Z", true
			r.RequireInputPrompt, r.DeliveryEvidence = tc.guarded, tc.evidence
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
