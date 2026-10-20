package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

const (
	chainOldID = "aaaaaaaa-1111-2222-3333-444444444444"
	chainMidID = "bbbbbbbb-1111-2222-3333-444444444444"
)

func chainLine(t *testing.T, kind, uuid, ts, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type": kind, "uuid": uuid, "timestamp": ts,
		"message": map[string]any{"role": kind, "content": []map[string]any{{"type": "text", "text": text}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// chainTestSession is rowsTestSession plus two earlier conversations: the
// deck session was /clear-ed twice, so session_links holds three ids. The
// live transcript keeps rowsTestSession's fixture rows.
func chainTestSession(t *testing.T) (home, id string) {
	t.Helper()
	home, id, live := rowsTestSession(t)
	dir := filepath.Dir(live)
	// The hook path records one session_links row per conversation id it
	// sees; replay that: old, mid, then the live one.
	var dbPath string
	_ = filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "state.db" && filepath.Base(filepath.Dir(p)) == "ch_support_test" {
			dbPath = p
		}
		return nil
	})
	if dbPath == "" {
		t.Fatal("no state.db under the test home")
	}
	db, err := statedb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seen := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	for _, native := range []string{chainOldID, chainMidID, "11111111-2222-3333-4444-555555555555"} {
		if err := db.WriteClaudeSessionBinding(id, native, seen); err != nil {
			t.Fatalf("bind %s: %v", native, err)
		}
		seen = seen.Add(24 * time.Hour)
	}
	old := chainLine(t, "user", "u-old-1", "2026-10-01T10:00:00.000Z", "old question") +
		chainLine(t, "assistant", "a-old-1", "2026-10-01T10:00:05.000Z", "old answer")
	mid := chainLine(t, "user", "u-mid-1", "2026-10-02T10:00:00.000Z", "mid question") +
		chainLine(t, "assistant", "a-mid-1", "2026-10-02T10:00:05.000Z", "mid answer") +
		chainLine(t, "user", "u-mid-2", "2026-10-02T10:01:00.000Z", "mid follow-up")
	for native, body := range map[string]string{chainOldID: old, chainMidID: mid} {
		if err := os.WriteFile(filepath.Join(dir, native+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home, id
}

type chainJSON struct {
	Schema  string `json:"schema"`
	Current string `json:"current"`
	Session struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Harness string `json:"harness"`
	} `json:"session"`
	Segments []struct {
		Seq           int    `json:"seq"`
		NativeID      string `json:"native_id"`
		Harness       string `json:"harness"`
		FirstSeen     int64  `json:"first_seen"`
		LastSeen      int64  `json:"last_seen"`
		FirstSeenAt   string `json:"first_seen_at"`
		Current       bool   `json:"current"`
		Authoritative bool   `json:"authoritative"`
		Path          string `json:"path"`
	} `json:"segments"`
}

func runChain(t *testing.T, home string, args ...string) (chainJSON, string, int) {
	t.Helper()
	stdout, stderr, code := runAgentDeck(t, home, append([]string{"recall", "chain"}, args...)...)
	var ch chainJSON
	if code == 0 {
		if err := json.Unmarshal([]byte(stdout), &ch); err != nil {
			t.Fatalf("chain JSON: %v %s %s", err, stdout, stderr)
		}
	}
	return ch, stdout + stderr, code
}

// TestRecallChainListsConversationsOldestFirst: the verb reads session_links
// only, so it answers with [recall] off, by deck id, title and by an old
// conversation id; the live conversation is last and current.
func TestRecallChainListsConversationsOldestFirst(t *testing.T) {
	home, id := chainTestSession(t)
	writeMacappConfig(t, home, "[recall]\nenabled = false\n")
	for _, ref := range []string{id, "rows-cli", chainOldID} {
		ch, out, code := runChain(t, home, ref, "--json")
		if code != 0 {
			t.Fatalf("chain %s: %d %s", ref, code, out)
		}
		if ch.Schema != "agent-deck.recall.chain/v1" || ch.Session.ID != id || ch.Session.Title != "rows-cli" || ch.Session.Harness != "claude" ||
			ch.Current != "11111111-2222-3333-4444-555555555555" || len(ch.Segments) != 3 {
			t.Fatalf("chain %s: %s", ref, out)
		}
		var got []string
		for i, s := range ch.Segments {
			got = append(got, s.NativeID)
			if s.Seq != i || s.Harness != "claude" || s.FirstSeen == 0 || s.FirstSeenAt == "" || s.Path == "" {
				t.Fatalf("segment %d incomplete: %+v", i, s)
			}
			if s.Current != (i == 2) || s.Authoritative != (i == 2) {
				t.Fatalf("segment %d current/authoritative wrong: %+v", i, s)
			}
		}
		if strings.Join(got, ",") != chainOldID+","+chainMidID+",11111111-2222-3333-4444-555555555555" {
			t.Fatalf("segments out of order: %v", got)
		}
	}
}

func TestRecallChainUnknownAndUsage(t *testing.T) {
	home, _ := chainTestSession(t)
	if _, out, code := runChain(t, home, "no-such-session", "--json"); code != 1 || !strings.Contains(out, "NOT_FOUND") {
		t.Fatalf("unknown session: %d %s", code, out)
	}
	if _, _, code := runChain(t, home, "rows-cli"); code != 2 {
		t.Fatalf("chain without --json: exit %d", code)
	}
	stdout, stderr, _ := runAgentDeck(t, home, "recall", "chain", "--help")
	if !strings.Contains(stdout+stderr, "Usage: agent-deck recall chain") {
		t.Fatalf("no usage: %s %s", stdout, stderr)
	}
}

// TestRecallChainWithoutLinksStillAnswers: a session whose links were never
// written answers with its current conversation id.
func TestRecallChainWithoutLinksStillAnswers(t *testing.T) {
	home, id, _ := rowsTestSession(t)
	ch, out, code := runChain(t, home, id, "--json")
	if code != 0 || len(ch.Segments) != 1 || !ch.Segments[0].Current || ch.Segments[0].Path == "" {
		t.Fatalf("single-conversation chain: %d %s", code, out)
	}
}

type chainTimelineJSON struct {
	Turns []struct {
		ID      string         `json:"id"`
		Kind    string         `json:"kind"`
		Seg     string         `json:"seg"`
		Body    string         `json:"body"`
		RawType string         `json:"raw_type"`
		Meta    map[string]any `json:"meta"`
	} `json:"turns"`
	ThroughCursor string `json:"through_cursor"`
	BeforeCursor  string `json:"before_cursor"`
}

func runChainTimeline(t *testing.T, home, id string, args ...string) (chainTimelineJSON, string, int) {
	t.Helper()
	argv := append([]string{"recall", "timeline", id, "--json", "--chain"}, args...)
	stdout, stderr, code := runAgentDeck(t, home, argv...)
	var tl chainTimelineJSON
	if code == 0 {
		if err := json.Unmarshal([]byte(stdout), &tl); err != nil {
			t.Fatalf("timeline JSON: %v %s", err, stdout)
		}
	}
	return tl, stdout + stderr, code
}

// TestRecallTimelineChainPagesAcrossClear walks the whole chain backwards
// with --tail then --before, the way a client scrolls up: no row twice, the
// boundary rows sit between conversations, and the last page ends the chain.
func TestRecallTimelineChainPagesAcrossClear(t *testing.T) {
	home, id := chainTestSession(t)
	// A tail inside the live conversation stays inside it.
	tl, out, code := runChainTimeline(t, home, id, "--tail", "2")
	if code != 0 || len(tl.Turns) != 2 || tl.ThroughCursor == "" {
		t.Fatalf("--tail 2: %d %s", code, out)
	}
	for _, r := range tl.Turns {
		if r.Seg != "11111111-2222-3333-4444-555555555555" {
			t.Fatalf("row of the live conversation carries seg %q", r.Seg)
		}
	}
	// A tail longer than the live conversation continues into the older ones.
	all, out, code := runChainTimeline(t, home, id, "--tail", "1000")
	if code != 0 || len(all.Turns) == 0 {
		t.Fatalf("--tail 1000: %d %s", code, out)
	}
	var texts []string
	for _, r := range all.Turns {
		if r.Kind == "user" || r.Kind == "assistant" {
			if r.Seg == chainOldID || r.Seg == chainMidID {
				texts = append(texts, r.Body)
			}
		}
	}
	if strings.Join(texts, "|") != "old question|old answer|mid question|mid answer|mid follow-up" {
		t.Fatalf("older conversations out of order: %v", texts)
	}
	var boundaries []string
	for _, r := range all.Turns {
		if strings.HasPrefix(r.ID, "boundary:") {
			if r.Kind != "system" || r.RawType != "chain/boundary" || r.Seg != strings.TrimPrefix(r.ID, "boundary:") || r.Meta["boundary"] != "clear" {
				t.Fatalf("boundary row malformed: %+v", r)
			}
			boundaries = append(boundaries, r.Seg)
		}
	}
	if strings.Join(boundaries, ",") != chainMidID+",11111111-2222-3333-4444-555555555555" {
		t.Fatalf("boundaries: %v", boundaries)
	}
	if all.BeforeCursor != "" {
		t.Fatalf("chain start should end paging, before_cursor %q", all.BeforeCursor)
	}

	// Scroll: tail 2, then pages of 2 until the cursor is empty.
	seen := map[string]bool{}
	var order []string
	page, out, code := runChainTimeline(t, home, id, "--tail", "2")
	for pages := 0; ; pages++ {
		if code != 0 || pages > 60 {
			t.Fatalf("paging stuck at page %d: %d %s", pages, code, out)
		}
		for i := len(page.Turns) - 1; i >= 0; i-- {
			r := page.Turns[i]
			if seen[r.ID] {
				t.Fatalf("row %s delivered twice", r.ID)
			}
			seen[r.ID] = true
			order = append(order, r.ID)
		}
		if page.BeforeCursor == "" {
			break
		}
		page, out, code = runChainTimeline(t, home, id, "--before", page.BeforeCursor, "--limit", "2")
	}
	if len(order) != len(all.Turns) {
		t.Fatalf("paging delivered %d rows, one read delivered %d", len(order), len(all.Turns))
	}
	for i, r := range all.Turns {
		if order[len(order)-1-i] != r.ID {
			t.Fatalf("paged order differs from the single read at %d", i)
		}
	}
}

func TestRecallTimelineChainRejectsBadCombinations(t *testing.T) {
	home, id := chainTestSession(t)
	for _, args := range [][]string{{"--since", "x"}, {"--agent", "a1"}, {"--v1"}, {"--limit", "5"}} {
		if _, out, code := runChainTimeline(t, home, id, args...); code == 0 {
			t.Fatalf("--chain %v accepted: %s", args, out)
		}
	}
	if _, out, code := runChainTimeline(t, home, id, "--before", "seg:cccccccc-0000-0000-0000-000000000000", "--limit", "3"); code != 1 || !strings.Contains(out, "RESYNC_REQUIRED") {
		t.Fatalf("unknown seg cursor: %d %s", code, out)
	}
}

// TestRecallTimelineChainSkipsMissingTranscripts: a conversation whose file
// is not on this host drops out of the page instead of failing it.
func TestRecallTimelineChainSkipsMissingTranscripts(t *testing.T) {
	home, id := chainTestSession(t)
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", chainMidID+".jsonl"))
	if len(matches) != 1 {
		t.Fatalf("mid transcript not found: %v", matches)
	}
	if err := os.Remove(matches[0]); err != nil {
		t.Fatal(err)
	}
	tl, out, code := runChainTimeline(t, home, id, "--tail", "1000")
	if code != 0 {
		t.Fatalf("missing segment failed the page: %d %s", code, out)
	}
	for _, r := range tl.Turns {
		if r.Seg == chainMidID {
			t.Fatalf("row of a missing transcript: %+v", r)
		}
	}
	ch, out, code := runChain(t, home, id, "--json")
	if code != 0 || len(ch.Segments) != 3 || ch.Segments[1].Path != "" {
		t.Fatalf("chain should still list the segment, without a path: %d %s", code, out)
	}
}

func TestRemoteRecallForwardsChain(t *testing.T) {
	for _, args := range [][]string{
		{"chain", "S", "--json"},
		{"timeline", "S", "--json", "--chain", "--tail", "50"},
		{"timeline", "S", "--json", "--chain", "--before", "seg:abc", "--limit", "50"},
	} {
		if err := validateRemoteRecallArgs(args); err != nil {
			t.Fatalf("%v refused: %v", args, err)
		}
	}
	if err := validateRemoteRecallArgs([]string{"chain", "S", "--json", "--bogus"}); err == nil {
		t.Fatal("chain accepted an unknown option")
	}
}
