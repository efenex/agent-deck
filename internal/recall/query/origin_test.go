package query

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func jsonl(t *testing.T, recs ...map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func userRec(uuid, ts string, content any, extra map[string]any) map[string]any {
	r := map[string]any{"type": "user", "uuid": uuid, "timestamp": ts, "message": map[string]any{"role": "user", "content": content}}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func writeTranscript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPeerFromText(t *testing.T) {
	for text, want := range map[string]string{
		"[agent-deck from:abc-123]\nbrief":                                    "abc-123",
		"  \n[agent-deck from:abc-123] ping":                                  "abc-123",
		"\n\n<pasted_content id=\"3dd1\">\n[agent-deck from:def-4]\nbody":     "def-4",
		"Another Claude session sent a message:\n[agent-deck from:zzz-9]\nhi": "zzz-9",
		`<agent-message from="a90eb253">` + "\n[Subagent hand-back] report":   "a90eb253",
		"please mention [agent-deck from:abc-123] in the docs":                "",
		"[agent-deck from:]":   "",
		"[agent-deck from:abc": "",
		"hello":                "",
		"Another Claude session sent a message:\n[tower-poke] run /tick now":     "",
		"<pasted_content id=\"1\">\nno tag here, [agent-deck from:late] in text": "",
	} {
		if got := peerFromText(text); got != want {
			t.Errorf("peerFromText(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestDeriveOrigin(t *testing.T) {
	human, tn := nativeOrigin{Kind: "human"}, nativeOrigin{Kind: "task-notification"}
	for _, c := range []struct {
		name, kind, text string
		meta             bool
		no               nativeOrigin
		want             string
	}{
		{"native human", "user", "fix it", false, human, "human"},
		{"tag beats native human", "user", "[agent-deck from:p1]\nhi", false, human, "peer:p1"},
		{"native task-notification", "system", "<task-notification>x</task-notification>", false, tn, "task-notification"},
		{"native peer with from", "system", "hi", true, nativeOrigin{Kind: "peer", From: "a1"}, "peer:a1"},
		{"native peer unknown sender", "system", "[tower-poke]", true, nativeOrigin{Kind: "peer"}, "peer:unknown"},
		{"native peer, tag names the sender", "system", "[agent-deck from:p2]\nx", true, nativeOrigin{Kind: "peer", From: "unknown"}, "peer:p2"},
		{"native kind we do not know", "system", "x", true, nativeOrigin{Kind: "channel"}, "system"},
		{"old transcript: plain text", "user", "hello", false, nativeOrigin{}, "human"},
		{"old transcript: task notification", "system", "<task-notification>\n<task-id>1", false, nativeOrigin{}, "task-notification"},
		{"old transcript: stop hook feedback", "system", "Stop hook feedback:\nChild session completed", true, nativeOrigin{}, "hook"},
		{"old transcript: inbox", "system", "[INBOX] 2 unread", false, nativeOrigin{}, "hook"},
		{"old transcript: slash command", "command", "<command-name>/board</command-name>", false, nativeOrigin{}, "human"},
		{"old transcript: skill body", "system", "Base directory for this skill: /x", true, nativeOrigin{}, "system"},
		{"old transcript: caveat", "system", "<local-command-caveat>x", true, nativeOrigin{}, "system"},
		{"old transcript: agent-message", "system", `<agent-message from="a9">x`, true, nativeOrigin{}, "peer:a9"},
	} {
		if got := deriveOrigin(c.kind, c.text, c.meta, c.no); got != c.want {
			t.Errorf("%s: deriveOrigin = %q, want %q", c.name, got, c.want)
		}
	}
}

func originOf(t *testing.T, rows []Row, id string) Row {
	t.Helper()
	r, ok := rowByID(rows, id)
	if !ok {
		t.Fatalf("no row %s in %d rows", id, len(rows))
	}
	return r
}

// TestRowsOriginFromClaudeRecords drives every source of origin through the
// reader: user records with and without Claude's own origin, peer tags, queue
// operations, queued_command attachments, hook output.
func TestRowsOriginFromClaudeRecords(t *testing.T) {
	body := jsonl(t,
		userRec("u-human", "2026-10-20T10:00:00.000Z", "fix the build", map[string]any{"origin": map[string]any{"kind": "human"}}),
		userRec("u-peer", "2026-10-20T10:00:01.000Z", "[agent-deck from:6582bf29-1791957861]\n[opus1] done", map[string]any{"origin": map[string]any{"kind": "human"}}),
		userRec("u-pasted", "2026-10-20T10:00:02.000Z", "\n\n<pasted_content id=\"3dd1\">\n[agent-deck from:243999f1-1788246811]\nTwo things", map[string]any{"origin": map[string]any{"kind": "human"}}),
		userRec("u-tn", "2026-10-20T10:00:03.000Z", "<task-notification>\n<task-id>b1</task-id>\n<summary>Monitor event</summary>\n</task-notification>",
			map[string]any{"origin": map[string]any{"kind": "task-notification", "producer": "session-task"}}),
		userRec("u-hook", "2026-10-20T10:00:04.000Z", "Stop hook feedback:\nChild session(s) completed", map[string]any{"isMeta": true}),
		userRec("u-skill", "2026-10-20T10:00:05.000Z", "Base directory for this skill: /x\n", map[string]any{"isMeta": true}),
		userRec("u-cmd", "2026-10-20T10:00:06.000Z", "<command-message>board</command-message>\n<command-name>/board</command-name>", nil),
		userRec("u-old", "2026-10-20T10:00:07.000Z", "an old transcript has no origin", nil),
		map[string]any{"type": "attachment", "uuid": "q-peer", "timestamp": "2026-10-20T10:00:08.000Z", "attachment": map[string]any{
			"type": "queued_command", "commandMode": "prompt", "isMeta": true,
			"prompt": "Another Claude session sent a message:\n[agent-deck from:zzz-9]\nhi",
			"origin": map[string]any{"kind": "peer", "from": "unknown", "verifiedPeerPid": 5},
		}},
		map[string]any{"type": "attachment", "uuid": "q-sub", "timestamp": "2026-10-20T10:00:09.000Z", "attachment": map[string]any{
			"type": "queued_command", "commandMode": "prompt",
			"prompt": `<agent-message from="a90eb253bd81f608a">` + "\n[Subagent hand-back] report",
			"origin": map[string]any{"kind": "peer", "from": "a90eb253bd81f608a"},
		}},
		map[string]any{"type": "attachment", "uuid": "q-tn", "timestamp": "2026-10-20T10:00:10.000Z", "attachment": map[string]any{
			"type": "queued_command", "commandMode": "task-notification",
			"prompt": "<task-notification>\n<task-id>b2</task-id>\n<summary>done</summary>\n</task-notification>",
			"origin": map[string]any{"kind": "task-notification", "producer": "session-task"},
		}},
		map[string]any{"type": "attachment", "uuid": "hk", "timestamp": "2026-10-20T10:00:11.000Z", "attachment": map[string]any{
			"type": "hook_additional_context", "hookName": "SessionStart", "content": []string{"rules"}}},
		map[string]any{"type": "system", "subtype": "stop_hook_summary", "uuid": "sh", "timestamp": "2026-10-20T10:00:12.000Z", "stopReason": "blocked"},
		map[string]any{"type": "queue-operation", "operation": "enqueue", "timestamp": "2026-10-20T10:00:13.000Z", "content": "typed while busy"},
		map[string]any{"type": "queue-operation", "operation": "enqueue", "timestamp": "2026-10-20T10:00:14.000Z", "content": "[agent-deck from:pq-1]\nqueued peer"},
		map[string]any{"type": "assistant", "uuid": "as", "timestamp": "2026-10-20T10:00:15.000Z",
			"message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "ok"}}}},
	)
	tl, err := ReadRows(context.Background(), RowsSource{Harness: "claude", Path: writeTranscript(t, body)}, RowsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"u-human": "human", "u-peer": "peer:6582bf29-1791957861", "u-pasted": "peer:243999f1-1788246811", "u-tn": "task-notification",
		"u-hook": "hook", "u-skill": "system", "u-cmd": "human", "u-old": "human",
		"q-peer": "peer:zzz-9", "q-sub": "peer:a90eb253bd81f608a", "q-tn": "task-notification",
		"hk": "hook", "sh": "hook",
		"queue:2026-10-20T10:00:13.000Z:" + contentHash("typed while busy"):                    "human",
		"queue:2026-10-20T10:00:14.000Z:" + contentHash("[agent-deck from:pq-1]\nqueued peer"): "peer:pq-1",
		"as": "",
	}
	for id, origin := range want {
		if got := originOf(t, tl.Turns, id).Origin; got != origin {
			t.Errorf("row %s origin = %q, want %q", id, got, origin)
		}
	}
	// A peer's message arrives as a human-typed one, so kind stays user:
	// origin is what tells them apart.
	if r := originOf(t, tl.Turns, "u-peer"); r.Kind != "user" {
		t.Errorf("peer message kind = %q", r.Kind)
	}
	// Assistant and tool rows carry no origin at all.
	for _, r := range tl.Turns {
		if (r.Kind == "assistant" || r.Kind == "tool") && r.Origin != "" {
			t.Errorf("%s row %s has origin %q", r.Kind, r.ID, r.Origin)
		}
	}
	// The field is additive: omitted when empty.
	b, _ := json.Marshal(originOf(t, tl.Turns, "as"))
	if strings.Contains(string(b), "origin") {
		t.Errorf("assistant row serializes an origin: %s", b)
	}
}

func TestRowsOriginCodexAndV1(t *testing.T) {
	rows := rowsFromTurns([]Turn{
		{Kind: "message", Role: "user", Text: "[agent-deck from:p7]\nhello"},
		{Kind: "message", Role: "user", Text: "typed"},
		{Kind: "message", Role: "assistant", Text: "ok"},
	})
	if rows[0].Origin != "peer:p7" || rows[1].Origin != "human" || rows[2].Origin != "" {
		t.Fatalf("v1 turn origins: %+v", rows)
	}
	tl, err := ReadRows(context.Background(), RowsSource{Harness: "codex", Path: filepath.Join("testdata", "rows", "codex-rollout.jsonl")}, RowsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, r := range tl.Turns {
		if r.Kind == "user" {
			users++
			if r.Origin != "human" {
				t.Errorf("codex user row %s origin %q", r.ID, r.Origin)
			}
		}
	}
	if users == 0 {
		t.Fatal("fixture has no codex user rows")
	}
}

// TestMergeRowKeepsOriginAndSeg: update frames patch origin and seg like
// every other field, and an absent patch leaves them alone.
func TestMergeRowKeepsOriginAndSeg(t *testing.T) {
	dst := Row{ID: "r", Origin: "human", Seg: "s1"}
	MergeRow(&dst, Row{ID: "r", Body: "x"})
	if dst.Origin != "human" || dst.Seg != "s1" {
		t.Fatalf("empty patch cleared fields: %+v", dst)
	}
	MergeRow(&dst, Row{ID: "r", Origin: "peer:a", Seg: "s2"})
	if dst.Origin != "peer:a" || dst.Seg != "s2" {
		t.Fatalf("patch did not apply: %+v", dst)
	}
}

func shellRecs() []map[string]any {
	return []map[string]any{
		userRec("sh-in", "2026-10-20T11:00:00.000Z", "<bash-input> gh pr merge 31 && flux reconcile</bash-input>", nil),
		userRec("sh-out", "2026-10-20T11:00:01.000Z", "<bash-stdout>a -&gt; b\n&lt;ok&gt;</bash-stdout><bash-stderr></bash-stderr>", nil),
		userRec("sh2-in", "2026-10-20T11:01:00.000Z", "<bash-input>false</bash-input>", nil),
		userRec("sh2-out", "2026-10-20T11:01:01.000Z", "<bash-stdout></bash-stdout><bash-stderr>error: nope</bash-stderr>", nil),
	}
}

// TestRowsUserShellIsOneBashRow: a `!` line and its output become one bash
// row, like the Bash tool's tool_use plus tool_result, marked user_shell.
func TestRowsUserShellIsOneBashRow(t *testing.T) {
	tl, err := ReadRows(context.Background(), RowsSource{Harness: "claude", Path: writeTranscript(t, jsonl(t, shellRecs()...))}, RowsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Turns) != 2 {
		t.Fatalf("want one row per shell line, got %d: %+v", len(tl.Turns), tl.Turns)
	}
	ok, bad := tl.Turns[0], tl.Turns[1]
	if ok.ID != "sh-in" || ok.Kind != "bash" || ok.Origin != "human" || ok.Meta["user_shell"] != true ||
		ok.Detail != "gh pr merge 31 && flux reconcile" || ok.Title != ok.Detail ||
		ok.Finished == nil || !*ok.Finished || ok.IsError == nil || *ok.IsError || ok.Body != "a -> b\n<ok>" {
		t.Fatalf("finished shell row: %+v", ok)
	}
	if bad.ID != "sh2-in" || bad.IsError == nil || !*bad.IsError || bad.Body != "error: nope" {
		t.Fatalf("stderr-only shell row: %+v", bad)
	}
}

// TestRowsUserShellOutputWithoutInput: when the input record is outside the
// window the output still shows up, as a finished bash row.
func TestRowsUserShellOutputWithoutInput(t *testing.T) {
	recs := shellRecs()[1:2]
	tl, err := ReadRows(context.Background(), RowsSource{Harness: "claude", Path: writeTranscript(t, jsonl(t, recs...))}, RowsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Turns) != 1 || tl.Turns[0].Kind != "bash" || tl.Turns[0].Origin != "human" || tl.Turns[0].Meta["user_shell"] != true || tl.Turns[0].Finished == nil || !*tl.Turns[0].Finished {
		t.Fatalf("orphan shell output: %+v", tl.Turns)
	}
}

// TestRowsUserShellSurvivesFollowResume: the pending input id rides in the
// follow cursor, so the output record after a reconnect still updates the
// row instead of appearing as a second one.
func TestRowsUserShellSurvivesFollowResume(t *testing.T) {
	recs := shellRecs()
	p := newRowParser("claude", rowParserState{})
	line := func(p *rowParser, r map[string]any) []RowFrame {
		b, _ := json.Marshal(r)
		return p.line(b)
	}
	if fr := line(p, recs[0]); len(fr) != 1 || fr[0].Frame != "row" {
		t.Fatalf("input frames: %+v", fr)
	}
	state, err := json.Marshal(p.st)
	if err != nil {
		t.Fatal(err)
	}
	var restored rowParserState
	if err := json.Unmarshal(state, &restored); err != nil {
		t.Fatal(err)
	}
	fr := line(newRowParser("claude", restored), recs[1])
	if len(fr) != 1 || fr[0].Frame != "update" || fr[0].Row.ID != "sh-in" || fr[0].Row.Finished == nil || !*fr[0].Row.Finished {
		t.Fatalf("output after resume: %+v", fr)
	}
}

func TestRowsUserShellThroughFollow(t *testing.T) {
	path := writeTranscript(t, jsonl(t, shellRecs()[:1]...))
	tl, err := ReadRows(context.Background(), RowsSource{Harness: "claude", Path: path}, RowsOptions{})
	if err != nil || len(tl.Turns) != 1 {
		t.Fatalf("first read: %v %+v", err, tl.Turns)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(jsonl(t, shellRecs()[1])); err != nil {
		t.Fatal(err)
	}
	f.Close()
	next, err := ReadRows(context.Background(), RowsSource{Harness: "claude", Path: path}, RowsOptions{Since: tl.ThroughCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Turns) != 0 || len(next.Updates) != 1 || next.Updates[0].ID != "sh-in" || next.Updates[0].Body != "a -> b\n<ok>" {
		t.Fatalf("--since after the output record: turns=%d updates=%+v", len(next.Turns), next.Updates)
	}
}

func TestBashShellOutput(t *testing.T) {
	for _, c := range []struct {
		name, text, want string
		isErr            bool
	}{
		{"stdout only", "<bash-stdout>a &amp; b</bash-stdout><bash-stderr></bash-stderr>", "a & b", false},
		{"stderr only is an error", "<bash-stdout></bash-stdout><bash-stderr>boom</bash-stderr>", "boom", true},
		{"both streams join, not an error", "<bash-stdout>out\n</bash-stdout><bash-stderr>warn</bash-stderr>", "out\nwarn", false},
		{"escaped markup in stderr", "<bash-stderr>&lt;x&gt;</bash-stderr>", "<x>", true},
	} {
		got, isErr := bashShellOutput(c.text)
		if got != c.want || isErr != c.isErr {
			t.Errorf("%s: bashShellOutput = (%q, %v), want (%q, %v)", c.name, got, isErr, c.want, c.isErr)
		}
	}
}

func TestDeriveOriginPrecedence(t *testing.T) {
	human := nativeOrigin{Kind: "human"}
	for _, c := range []struct {
		name, kind, text string
		meta             bool
		no               nativeOrigin
		want             string
	}{
		{"native human beats stop-hook text", "user", "Stop hook feedback:\nx", false, human, "human"},
		{"agent-message envelope beats native task-notification", "system", `<agent-message from="a9">x`, true, nativeOrigin{Kind: "task-notification"}, "peer:a9"},
		{"bash kind without native origin is human", "bash", "<bash-input>ls</bash-input>", false, nativeOrigin{}, "human"},
	} {
		if got := deriveOrigin(c.kind, c.text, c.meta, c.no); got != c.want {
			t.Errorf("%s: deriveOrigin = %q, want %q", c.name, got, c.want)
		}
	}
}
