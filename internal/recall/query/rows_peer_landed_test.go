package query

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func peerFixture(t *testing.T, name string) (string, Row) {
	t.Helper()
	path := filepath.Join("testdata", "rows", "claude-"+name+".jsonl")
	rows, _ := readRowsT(t, "claude", path, RowsOptions{})
	if len(rows) != 1 {
		t.Fatalf("fixture row count = %d", len(rows))
	}
	return path, rows[0]
}

func TestR1SocketUserFixtures(t *testing.T) {
	for _, name := range []string{"socket-user", "socket-tagged-user", "socket-string-user"} {
		t.Run(name, func(t *testing.T) {
			path, row := peerFixture(t, name)
			want := "[REDACTED]"
			if name != "socket-user" {
				want = "[agent-deck from:synthetic-sender]\n" + want
			}
			if row.Kind != "user" {
				t.Errorf("socket user classified %q", row.Kind)
			}
			at, _ := time.Parse(time.RFC3339, row.TS)
			id, _, ok := FindLanded(context.Background(), "claude", path, 0, want, at)
			if !ok || id != row.ID {
				t.Error("socket user did not prove landing")
			}
			scan := ScanLanded(context.Background(), "claude", path, want, LandedWindow{To: -1, NotBefore: at, NotAfter: at.Add(time.Minute), PeerEnvelope: true})
			if !scan.Complete || scan.Pending || len(scan.Candidates) != 1 || scan.Candidates[0].ID != row.ID {
				t.Errorf("bounded scan: %+v", scan)
			}
			// A send typed into the pane never produces this row.
			typed := ScanLanded(context.Background(), "claude", path, want, LandedWindow{To: -1, NotBefore: at, NotAfter: at.Add(time.Minute)})
			if !typed.Complete || typed.Present || len(typed.Candidates) != 0 {
				t.Errorf("envelope proved a pane send: %+v", typed)
			}
		})
	}
}

func TestR1ForkOriginFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "rows", "claude-fork-origin-row.json"))
	if err != nil {
		t.Fatal(err)
	}
	var row Row
	if err := json.Unmarshal(b, &row); err != nil {
		t.Fatal(err)
	}
	// The origin field belongs to the fork's presentation schema. Landing
	// must use the native user shape and complete envelope, not trust origin.
	if !landedMatch(&row, normalizeLanded("[agent-deck from:synthetic-sender]\n[REDACTED]")) {
		t.Fatal("fork origin user row did not match")
	}
}

func TestR1PeerEnvelopeRejectsNonProof(t *testing.T) {
	_, source := peerFixture(t, "socket-tagged-user")
	want := normalizeLanded("[agent-deck from:synthetic-sender]\n[REDACTED]")
	for _, tc := range []struct{ name, kind, raw, body string }{
		{"assistant quote", "assistant", "assistant", source.Body},
		{"tool result", "tool", "user", source.Body},
		{"hook", "system", "attachment/hook_additional_context", source.Body},
		{"compact summary", "system", "user", "[REDACTED]"},
		{"bare meta", "system", "user", "[agent-deck from:synthetic-sender]\n[REDACTED]"},
		{"leading prose", "user", "user", "[REDACTED-OTHER]\n" + source.Body},
		{"trailing prose", "user", "user", source.Body + "\n[REDACTED-OTHER]"},
		{"missing footer", "user", "user", strings.Split(source.Body, "\n\n")[0]},
		{"partial body", "user", "user", strings.Replace(source.Body, "[REDACTED]", "[REDACTED] [REDACTED-OTHER]", 1)},
		{"wrong sender", "user", "user", strings.Replace(source.Body, "synthetic-sender", "other-sender", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := Row{Kind: tc.kind, RawType: tc.raw, Body: tc.body}
			if landedMatch(&row, want) {
				t.Fatal("non-proof matched")
			}
		})
	}
	if landedMatch(&source, normalizeLanded("[REDACTED]")) {
		t.Fatal("discarded sender tag")
	}
	source.Kind, source.Title = "system", "Compaction summary"
	if landedMatch(&source, want) {
		t.Fatal("compaction summary proved a peer send")
	}
}

func TestR1PeerEnvelopePastedContent(t *testing.T) {
	_, source := peerFixture(t, "socket-user")
	source.Body = strings.Replace(source.Body, "[REDACTED]", "<pasted_content id=\"r1\">\n[REDACTED]\n</pasted_content id=\"r1\">", 1)
	if !landedMatch(&source, normalizeLanded("[REDACTED]")) {
		t.Fatal("wrapped paste did not match")
	}
}

// Q4t's envelope writer has two mid-turn reply suffixes. Exercise these
// directly, without assuming that a mid-turn attachment is a native user row.
func TestR1PeerEnvelopeMidTurn(t *testing.T) {
	_, source := peerFixture(t, "socket-user")
	for _, suffix := range []string{
		" After completing your current task, decide whether/how to respond.",
		" After completing your current task, decide whether/how to respond (reply via SendMessage to the `from=` address).",
	} {
		body := strings.Replace(source.Body, "sent a message:\n", "sent a message while you were working:\n", 1) + suffix
		if kind := claudeUserTextKind(body, true); kind != "user" {
			t.Errorf("peer envelope classified %q", kind)
		}
		row := Row{Kind: "user", RawType: "user", Body: body}
		if !landedMatch(&row, "[REDACTED]") {
			t.Error("mid-turn envelope did not match")
		}
	}
}
