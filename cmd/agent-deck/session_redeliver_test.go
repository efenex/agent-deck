package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/costs"
	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

type redeliverFixture struct {
	dir, path string
	target    *session.Instance
	now       time.Time
	kicks     atomic.Int32
	config    session.UserConfig
}

func newRedeliverFixture(t *testing.T, running bool) *redeliverFixture {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENTDECK_REDELIVER_WAIT", "10ms")
	f := &redeliverFixture{dir: t.TempDir(), now: time.Now().UTC()}
	f.target = session.NewInstanceWithTool("r0-target", t.TempDir(), "claude")
	f.target.ClaudeSessionID = "11111111-1111-4111-8111-111111111111"
	f.path = session.ClaudeTranscriptPathForInstance(f.target)
	if err := os.MkdirAll(filepath.Dir(f.path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if running {
		skipIfNoTmuxBinaryCLI(t)
		sess := f.target.GetTmuxSession()
		if err := sess.Start("bash"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sess.Kill() })
	}
	prevLoad, prevKick, prevConfig, prevBudget := loadSessions, startSendWorker, loadUserConfigForSend, budgetForSend
	t.Cleanup(func() {
		loadSessions, startSendWorker, loadUserConfigForSend, budgetForSend = prevLoad, prevKick, prevConfig, prevBudget
	})
	loadSessions = func(string) ([]*session.Instance, error) { return []*session.Instance{f.target}, nil }
	startSendWorker = func(string, string) error { f.kicks.Add(1); return nil }
	loadUserConfigForSend = func() (*session.UserConfig, error) { return &f.config, nil }
	budgetForSend = func(string, *session.Instance, []*session.Instance, *session.Storage) costs.BudgetResult {
		return costs.BudgetResult{Action: costs.BudgetActionNone}
	}
	prevNormal, prevGuard, prevRelease := sendChild, sendChildGuarded, sendChildRelease
	t.Cleanup(func() { sendChild, sendChildGuarded, sendChildRelease = prevNormal, prevGuard, prevRelease })
	forbid := func(string, string, string, string) (int, func() int, error) {
		t.Error("redelivery emitted directly")
		return 0, nil, errors.New("forbidden")
	}
	sendChild, sendChildGuarded, sendChildRelease = forbid, forbid, forbid
	return f
}

func (f *redeliverFixture) record(t *testing.T, state string) *sendqueue.Record {
	t.Helper()
	id, err := sendqueue.NextID(f.dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r := &sendqueue.Record{SendID: id, SessionID: f.target.ID, State: state, Tool: "claude", Message: "[REDACTED]", Attempts: 2,
		CreatedAt: f.now.Add(-2 * time.Minute).Format(time.RFC3339Nano), UpdatedAt: f.now.Format(time.RFC3339Nano),
		SentAt: f.now.Add(-time.Minute).Format(time.RFC3339Nano), TranscriptPath: f.path, Deadline: f.now.Add(-time.Minute).Format(time.RFC3339Nano)}
	if err := sendqueue.Save(f.dir, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *redeliverFixture) run(t *testing.T, r *sendqueue.Record, key string, check bool) redeliverResult {
	t.Helper()
	got, err := redeliverSend("_test", f.dir, r.SendID, key, check)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRedeliverStateTable(t *testing.T) {
	for _, tc := range []struct {
		state, outcome string
		settled        bool
		kicks          int32
	}{
		{"landed", "already_landed", false, 0}, {"queued", "pending", false, 1}, {"typing", "pending", false, 0},
		{"typed", "blocked_unknown", false, 0}, {"submitted", "blocked_unknown", false, 0},
		{"typed", "blocked_unknown", true, 0}, {"submitted", "blocked_unknown", true, 0},
		{"failed", "pending", false, 1}, {"cancelled", "refused", false, 0}, {"future-state", "blocked_unknown", false, 0},
	} {
		t.Run(tc.state+"/"+map[bool]string{true: "settled", false: "active"}[tc.settled], func(t *testing.T) {
			f := newRedeliverFixture(t, tc.state == "failed")
			r := f.record(t, tc.state)
			r.Settled = tc.settled
			if err := sendqueue.Save(f.dir, r); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(f.dir, r.SendID+".json"))
			got := f.run(t, r, "state", false)
			if got.Outcome != tc.outcome || f.kicks.Load() != tc.kicks {
				t.Fatalf("got %+v kicks=%d", got, f.kicks.Load())
			}
			if tc.state != "failed" {
				after, _ := os.ReadFile(filepath.Join(f.dir, r.SendID+".json"))
				if !bytes.Equal(before, after) {
					t.Fatal("rewrote a worker-owned or terminal record")
				}
			}
			if tc.state == "cancelled" && got.Reason != "cancelled" {
				t.Fatal(got)
			}
		})
	}
}

func TestRedeliverCheckOnlyHasNoSideEffects(t *testing.T) {
	for _, state := range []string{"landed", "queued", "typing", "typed", "submitted", "failed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			f := newRedeliverFixture(t, state == "failed")
			r := f.record(t, state)
			if err := os.WriteFile(f.path, []byte(claudeUserLine("proof", "[REDACTED]", f.now)), 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(f.dir, r.SendID+".json"))
			entries, _ := os.ReadDir(f.dir)
			got := f.run(t, r, "pure", true)
			after, _ := os.ReadFile(filepath.Join(f.dir, r.SendID+".json"))
			afterEntries, _ := os.ReadDir(f.dir)
			if !bytes.Equal(before, after) || len(entries) != len(afterEntries) || f.kicks.Load() != 0 {
				t.Fatalf("check-only mutated: %+v", got)
			}
			if (state == "typed" || state == "submitted") && got.Outcome != "landed" {
				t.Fatal(got)
			}
		})
	}
}

func TestRedeliverFailedGuards(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(*redeliverFixture, *sendqueue.Record)
	}{
		{"old", "reopen_age_exceeded", func(f *redeliverFixture, r *sendqueue.Record) {
			r.CreatedAt = f.now.Add(-61 * time.Minute).Format(time.RFC3339Nano)
		}},
		{"invalid age", "reopen_age_exceeded", func(f *redeliverFixture, r *sendqueue.Record) { f.config.Send.RedeliverMaxAge = "unlimited" }},
		{"bad created", "reopen_age_exceeded", func(f *redeliverFixture, r *sendqueue.Record) { r.CreatedAt = "broken" }},
		{"budget", "budget_exceeded", func(f *redeliverFixture, r *sendqueue.Record) {
			r.IgnoreBudget = true
			budgetForSend = func(string, *session.Instance, []*session.Instance, *session.Storage) costs.BudgetResult {
				return costs.BudgetResult{Action: costs.BudgetActionStop}
			}
		}},
		{"content", "content_present", func(f *redeliverFixture, r *sendqueue.Record) {
			_ = os.WriteFile(f.path, []byte(claudeUserLine("resend", "[REDACTED]", f.now)), 0600)
		}},
		{"missing transcript", "source_unreadable", func(f *redeliverFixture, r *sendqueue.Record) { _ = os.Remove(f.path) }},
		{"partial transcript", "source_incomplete", func(f *redeliverFixture, r *sendqueue.Record) { _ = os.WriteFile(f.path, []byte(`{"type":`), 0600) }},
		{"attachment", "attachment_missing", func(f *redeliverFixture, r *sendqueue.Record) {
			r.Images = []string{filepath.Join(f.dir, "missing.png")}
		}},
		{"different deck session", "target_not_running", func(f *redeliverFixture, r *sendqueue.Record) { r.SessionID = "another-deck-session" }},
		{"pending resend", "content_present", func(f *redeliverFixture, r *sendqueue.Record) { f.record(t, sendqueue.StateQueued) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRedeliverFixture(t, true)
			r := f.record(t, sendqueue.StateFailed)
			tc.change(f, r)
			if err := sendqueue.Save(f.dir, r); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(f.dir, r.SendID+".json"))
			got := f.run(t, r, "guard", false)
			if got.Reason != tc.reason || f.kicks.Load() != 0 {
				t.Fatalf("guard: %+v kicks=%d", got, f.kicks.Load())
			}
			after, _ := os.ReadFile(filepath.Join(f.dir, r.SendID+".json"))
			if !bytes.Equal(before, after) {
				t.Fatal("refusal mutated failed record")
			}
		})
	}
	t.Run("stopped", func(t *testing.T) {
		f := newRedeliverFixture(t, false)
		r := f.record(t, "failed")
		if got := f.run(t, r, "stop", false); got.Reason != "target_not_running" {
			t.Fatal(got)
		}
	})
}

func TestRedeliverFailedSameIDCurrentNativeGuardAndDeadline(t *testing.T) {
	f := newRedeliverFixture(t, true)
	f.config.Send.RedeliverMaxAge = "2h"
	r := f.record(t, "failed")
	r.CreatedAt = f.now.Add(-90 * time.Minute).Format(time.RFC3339Nano)
	r.ClaudeSessionID = "22222222-2222-4222-8222-222222222222" // previous native conversation
	r.TranscriptPath = filepath.Join(f.dir, "old.jsonl")
	r.IgnoreBudget = true
	if err := sendqueue.Save(f.dir, r); err != nil {
		t.Fatal(err)
	}
	got := f.run(t, r, "reopen", false)
	current, err := sendqueue.Load(f.dir, r.SendID)
	if err != nil {
		t.Fatal(err)
	}
	deadline, _ := time.Parse(time.RFC3339Nano, current.Deadline)
	if got.Reason != "reopened" || current.State != "queued" || current.Attempts != 2 || current.Redeliveries != 1 || !current.RequireInputPrompt || current.IgnoreBudget || !deadline.After(f.now.Add(29*time.Minute)) {
		t.Fatalf("reopen: %+v %+v", got, current)
	}
	// Consume the reopened record through the existing typing boundary. Only
	// the guarded child may run, with the current native path and fresh evidence.
	guarded := false
	sendChildGuarded = func(_, _, _, result string) (int, func() int, error) {
		guarded = true
		_ = os.WriteFile(result, []byte(`{"success":true,"submitted":true}`), 0600)
		return 123, func() int { return 0 }, nil
	}
	set := func(fn func(*sendqueue.Record)) error {
		saved, err := sendqueue.Update(f.dir, r.SendID, time.Now(), fn)
		if err == nil {
			*current = *saved
		}
		return err
	}
	if !typeQueued("_test", f.dir, current, f.target, []*session.Instance{f.target}, "waiting", f.path, 0, set) || !guarded || current.State != "submitted" || current.TranscriptPath != f.path {
		t.Fatalf("guarded child: %+v", current)
	}
	// The operation's decision is immutable while the snapshot advances.
	replay := f.run(t, r, "reopen", false)
	if !replay.Replayed || replay.RedeliveryDecision != got.RedeliveryDecision || replay.Snapshot.State != "submitted" || f.kicks.Load() != 1 {
		t.Fatalf("replay: %+v", replay)
	}
	checked := f.run(t, r, "reopen", true)
	if !checked.Replayed || checked.RedeliveryDecision != got.RedeliveryDecision || f.kicks.Load() != 1 {
		t.Fatalf("check replay: %+v", checked)
	}
	other := f.record(t, "cancelled")
	if conflict := f.run(t, other, "reopen", true); conflict.Outcome != "conflict" {
		t.Fatalf("check ignored binding: %+v", conflict)
	}
}

func TestRedeliverUnsupportedTargets(t *testing.T) {
	for _, tool := range []string{"opencode", "shell", "gemini", "ssh", "routed"} {
		for _, state := range []string{"queued", "typing", "typed", "submitted", "failed"} {
			t.Run(tool+"/"+state, func(t *testing.T) {
				f := newRedeliverFixture(t, false)
				r := f.record(t, state)
				switch tool {
				case "ssh":
					f.target.SSHHost = "remote-placeholder"
				case "routed":
					r.WaitWhileStopped = true
				default:
					r.Tool = tool
					f.target.Tool = tool
				}
				if err := sendqueue.Save(f.dir, r); err != nil {
					t.Fatal(err)
				}
				if got := f.run(t, r, "unsupported", false); got.Outcome != "unsupported" || f.kicks.Load() != 0 {
					t.Fatal(got)
				}
			})
		}
	}
}

func TestRedeliverLateProofAndAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rows         int
		want, reason string
	}{
		{"late one", 1, "landed", ""}, {"two identical", 2, "blocked_unknown", "content_present"}, {"no proof", 0, "blocked_unknown", "no_landed_proof"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRedeliverFixture(t, false)
			r := f.record(t, "submitted")
			r.Settled = true
			r.SentAt = f.now.Add(-3 * time.Minute).Format(time.RFC3339Nano)
			if err := sendqueue.Save(f.dir, r); err != nil {
				t.Fatal(err)
			}
			var rows string
			for i := 0; i < tc.rows; i++ {
				rows += claudeUserLine(string(rune('a'+i)), "[REDACTED]", f.now)
			}
			_ = os.WriteFile(f.path, []byte(rows), 0600)
			got := f.run(t, r, "proof", false)
			if got.Outcome != tc.want || got.Reason != tc.reason || f.kicks.Load() != 0 {
				t.Fatal(got)
			}
			if tc.want == "landed" && (got.Snapshot.State != "landed" || got.Snapshot.LandedRowID != "a") {
				t.Fatal(got)
			}
			b, _ := json.Marshal(got)
			if strings.Contains(string(b), "[REDACTED]") || strings.Contains(string(b), f.path) {
				t.Fatal("content or path in result")
			}
		})
	}
}

func TestRedeliverBoundedAndClaimedRows(t *testing.T) {
	for _, kind := range []string{"time", "next-offset", "claimed", "corrupt-record"} {
		t.Run(kind, func(t *testing.T) {
			f := newRedeliverFixture(t, false)
			r := f.record(t, "typed")
			r.Settled = true
			row := claudeUserLine("unique", "[REDACTED]", f.now)
			switch kind {
			case "time":
				r.SentAt = f.now.Add(-11 * time.Minute).Format(time.RFC3339Nano)
			case "next-offset":
				other := f.record(t, "submitted")
				other.SentAt = f.now.Format(time.RFC3339Nano)
				_ = sendqueue.Save(f.dir, other)
			case "claimed":
				other := f.record(t, "landed")
				other.LandedRowID = "unique"
				other.SentAt = f.now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
				_ = sendqueue.Save(f.dir, other)
			case "corrupt-record":
				_ = os.WriteFile(filepath.Join(f.dir, sendqueue.NewID(time.Now())+".json"), []byte("{"), 0600)
			}
			_ = sendqueue.Save(f.dir, r)
			_ = os.WriteFile(f.path, []byte(row), 0600)
			if got := f.run(t, r, "fenced", false); got.Outcome != "blocked_unknown" || f.kicks.Load() != 0 {
				t.Fatal(got)
			}
		})
	}
}

func TestRedeliverHeldWatchAndRecordLocks(t *testing.T) {
	for _, state := range []string{"typed", "failed"} {
		t.Run(state, func(t *testing.T) {
			f := newRedeliverFixture(t, state == "failed")
			r := f.record(t, state)
			name := "watch-" + r.SendID
			if state == "failed" {
				name = "record-" + r.SendID
			}
			lock, ok, err := sendqueue.TryLock(f.dir, name)
			if err != nil || !ok {
				t.Fatal(err)
			}
			defer lock.Release()
			if got := f.run(t, r, "busy", false); got.Outcome != "pending" || f.kicks.Load() != 0 {
				t.Fatal(got)
			}
		})
	}
}

func TestRedeliverConcurrentKeysAndPrune(t *testing.T) {
	f := newRedeliverFixture(t, true)
	f.config.Send.RedeliverMaxAge = "240h"
	for n := 0; n < 15; n++ {
		r := f.record(t, "failed")
		r.Message += " " + string(rune('a'+n))
		r.CreatedAt = f.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)
		r.UpdatedAt = f.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)
		_ = sendqueue.Save(f.dir, r)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); sendqueue.Prune(f.dir, f.now.Add(-sendqueue.RetainFinished)) }()
		for _, key := range []string{"a", "b"} {
			go func(key string) {
				defer wg.Done()
				result, err := redeliverSend("_test", f.dir, r.SendID, r.SendID+key, false)
				if err != nil {
					t.Error(err)
					return
				}
				if result.Reason == "reopened" && (result.Snapshot == nil || result.Snapshot.Redeliveries != 1) {
					t.Errorf("reopen lost: %+v", result)
				}
			}(key)
		}
		wg.Wait()
		if current, err := sendqueue.Load(f.dir, r.SendID); err == nil && current.Redeliveries > 1 {
			t.Fatalf("multiple reopens: %+v", current)
		}
	}
}

func TestRedeliverConcurrentProofClaimsAndLegacyFlag(t *testing.T) {
	f := newRedeliverFixture(t, false)
	first, second := f.record(t, "typed"), f.record(t, "submitted")
	// Distinct deck aliases of one native source still share row claims.
	second.SessionID = "alias-target"
	if err := sendqueue.Save(f.dir, second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, []byte(claudeUserLine("shared", "[REDACTED]", f.now)), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, rec := range []*sendqueue.Record{first, second} {
		wg.Add(1)
		go func(rec *sendqueue.Record) {
			defer wg.Done()
			if _, err := redeliverSend("_test", f.dir, rec.SendID, rec.SendID, false); err != nil {
				t.Error(err)
			}
		}(rec)
	}
	wg.Wait()
	landed := 0
	for _, rec := range []*sendqueue.Record{first, second} {
		current, err := sendqueue.Load(f.dir, rec.SendID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == "landed" {
			landed++
		}
	}
	if landed != 1 {
		t.Fatalf("shared row claimed by %d records", landed)
	}
	// An old binary could already have stored two claims. Do not reopen or
	// downgrade either one, but make the weaker legacy attribution visible.
	for _, rec := range []*sendqueue.Record{first, second} {
		rec.State = "landed"
		rec.LandedRowID = "shared"
		if err := sendqueue.Save(f.dir, rec); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.run(t, second, "legacy", false); got.Outcome != "already_landed" || got.Reason != "shared_landed_row" {
		t.Fatal(got)
	}
}

func TestRedeliverCodexKeepsUnresolvedSubmissionMarker(t *testing.T) {
	f := newRedeliverFixture(t, true)
	f.target.Tool = "codex"
	f.target.CodexSessionID = "33333333-3333-4333-8333-333333333333"
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(codexHome, "sessions", "2026", "11", "13", "rollout-2026-11-13T12-00-00-"+f.target.CodexSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"33333333-3333-4333-8333-333333333333","thread_source":"user"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	marker, err := session.PrepareCodexSubmissionMarker(f.target.ID, f.target.CodexSessionID, "", f.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := marker.MarkTransportAmbiguous(f.now); err != nil {
		t.Fatal(err)
	}
	r := f.record(t, "failed")
	r.Tool = "codex"
	r.TranscriptPath = filepath.Join(f.dir, "old-rollout.jsonl")
	if err := sendqueue.Save(f.dir, r); err != nil {
		t.Fatal(err)
	}
	if got := f.run(t, r, "codex-reopen", false); got.Reason != "reopened" {
		t.Fatal(got)
	}
	if _, err := session.ReconcileCodexSubmissionMarker(f.target.ID, f.target.CodexSessionID, "", nil); err == nil || !strings.Contains(err.Error(), "unresolved Codex submission") {
		t.Fatalf("redelivery cleared the marker: %v", err)
	}
	// Positive proof is still the exact bound rollout user row, never a
	// newer turn or the mere absence of a submission marker.
	r.State = "submitted"
	r.Settled = true
	r.TranscriptPath = path
	if err := sendqueue.Save(f.dir, r); err != nil {
		t.Fatal(err)
	}
	if got := f.run(t, r, "codex-miss", false); got.Outcome != "blocked_unknown" {
		t.Fatal(got)
	}
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	line := `{"type":"response_item","timestamp":"` + f.now.Format(time.RFC3339Nano) + `","payload":{"type":"message","role":"user","id":"codex-proof","content":[{"type":"input_text","text":"[REDACTED]"}]}}` + "\n"
	if _, err := fh.WriteString(line); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	if got := f.run(t, r, "codex-proof", false); got.Outcome != "landed" || got.Snapshot.LandedRowID != "codex-proof" {
		t.Fatal(got)
	}
}
