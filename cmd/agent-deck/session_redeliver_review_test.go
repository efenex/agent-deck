package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/recall/query"
	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
)

// An agent resends a failed send under a new ID and the resend fails too.
// Whitespace-only differences are the same text to the transcript matcher.
// Reopening the older one and then the newer one must not queue both: that
// would type the same text twice into one transcript.
func TestRedeliverIdenticalFailedPairReopensOnce(t *testing.T) {
	for _, order := range []string{"older-first", "newer-first"} {
		t.Run(order, func(t *testing.T) {
			f := newRedeliverFixture(t, true)
			older := f.record(t, sendqueue.StateFailed)
			older.CreatedAt = f.now.Add(-5 * time.Minute).Format(time.RFC3339Nano)
			if err := sendqueue.Save(f.dir, older); err != nil {
				t.Fatal(err)
			}
			newer := f.record(t, sendqueue.StateFailed)
			first, second := older, newer
			if order == "newer-first" {
				first, second = newer, older
			}
			if got := f.run(t, first, "k1", false); got.Reason != "reopened" {
				t.Fatalf("first reopen: %+v", got)
			}
			got := f.run(t, second, "k2", false)
			a, _ := sendqueue.Load(f.dir, older.SendID)
			b, _ := sendqueue.Load(f.dir, newer.SendID)
			if a.State == sendqueue.StateQueued && b.State == sendqueue.StateQueued {
				t.Fatalf("both identical sends reopened (would type twice): second=%+v", got)
			}
			if got.Outcome != "blocked_unknown" || got.Reason != "content_present" {
				t.Fatalf("second reopen: %+v", got)
			}
		})
	}
}

// A queued (still untyped) identical send created before the failed one is
// a known pending copy too.
func TestRedeliverFailedBlockedByOlderPendingCopy(t *testing.T) {
	f := newRedeliverFixture(t, true)
	pending := f.record(t, sendqueue.StateQueued)
	pending.CreatedAt = f.now.Add(-5 * time.Minute).Format(time.RFC3339Nano)
	if err := sendqueue.Save(f.dir, pending); err != nil {
		t.Fatal(err)
	}
	failed := f.record(t, sendqueue.StateFailed)
	if got := f.run(t, failed, "k", false); got.Reason != "content_present" {
		t.Fatalf("reopened beside an older queued copy: %+v", got)
	}
}

// Workers type later queued sends without waiting for the previous one's
// transcript proof (startPendingWatchers). A Claude message typed mid-turn is
// enqueued at once and absorbed later; the absorption can follow the next,
// different send's offset. That later send must not hide the proof.
func TestLateAbsorptionAfterNextDifferentSendStillLands(t *testing.T) {
	f := newRedeliverFixture(t, false)
	sent := f.now.Add(-time.Minute)
	q := func(op, reason, content string, ts time.Time) string {
		return fmt.Sprintf(`{"type":"queue-operation","operation":%q,"reason":%q,"timestamp":%q,"content":%q}`+"\n",
			op, reason, ts.UTC().Format(time.RFC3339Nano), content)
	}
	enqueueA := q("enqueue", "", "[REDACTED]", sent)
	enqueueB := q("enqueue", "", "[REDACTED-OTHER]", sent.Add(time.Second))
	absorbA := q("remove", "absorbed_mid_turn", "[REDACTED]", sent.Add(20*time.Second))
	if err := os.WriteFile(f.path, []byte(enqueueA+enqueueB+absorbA), 0600); err != nil {
		t.Fatal(err)
	}
	a := f.record(t, sendqueue.StateTyped)
	a.SentAt, a.TranscriptFrom = sent.Format(time.RFC3339Nano), 0
	b := f.record(t, sendqueue.StateTyped)
	b.Message = "[REDACTED-OTHER]"
	b.SentAt, b.TranscriptFrom = sent.Add(time.Second).Format(time.RFC3339Nano), int64(len(enqueueA))
	for _, r := range []*sendqueue.Record{a, b} {
		if err := sendqueue.Save(f.dir, r); err != nil {
			t.Fatal(err)
		}
	}
	// The pre-R0 watcher's matcher proves this fixture; R0 must not lose it.
	if _, _, ok := query.FindLanded(context.Background(), "claude", f.path, 0, a.Message, sent); !ok {
		t.Fatal("fixture: base FindLanded does not prove the absorbed send")
	}
	got := f.run(t, a, "late", false)
	if got.Outcome != "landed" {
		t.Fatalf("absorbed send not proven past a different send's offset: %+v", got)
	}
}

// A failed reopen's presence scan covers created_at..now. A long-lived Claude
// transcript is usually larger than the 4 MiB scan bound even when the last
// hour is small. Report-only: fail-closed, but D3 reopens become unreachable.
func TestRedeliverReopenPresenceOnLargeTranscriptBlocks(t *testing.T) {
	f := newRedeliverFixture(t, true)
	old := claudeUserLine("old", strings.Repeat("x", 1024), f.now.Add(-48*time.Hour))
	var sb strings.Builder
	for sb.Len() < 5<<20 {
		sb.WriteString(old)
	}
	if err := os.WriteFile(f.path, []byte(sb.String()), 0600); err != nil {
		t.Fatal(err)
	}
	r := f.record(t, sendqueue.StateFailed)
	got := f.run(t, r, "big", false)
	// Documents a known limit: fail-closed, never a reopen without coverage.
	if got.Outcome != "blocked_unknown" || got.Reason != "scan_limit" {
		t.Fatalf("large-transcript reopen: %+v", got.RedeliveryDecision)
	}
}

// Two identical failed sends reopened concurrently under different keys hold
// different record locks; the per-target reopen lock serializes them.
func TestRedeliverIdenticalFailedPairConcurrentReopensOnce(t *testing.T) {
	f := newRedeliverFixture(t, true)
	held, ok, err := sendqueue.TryLock(f.dir, "reopen-"+f.target.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	r := f.record(t, sendqueue.StateFailed)
	if got := f.run(t, r, "held", false); got.Outcome != "pending" || got.Reason != "reopen_busy" {
		t.Fatalf("reopen ignored the per-target reopen lock: %+v", got)
	}
	_ = held.Release()
	for i := 0; i < 10; i++ {
		a, b := f.record(t, sendqueue.StateFailed), f.record(t, sendqueue.StateFailed)
		done := make(chan redeliverResult, 2)
		for j, rec := range []*sendqueue.Record{a, b} {
			go func(rec *sendqueue.Record, key string) {
				got, _ := redeliverSend("_test", f.dir, rec.SendID, key, false)
				done <- got
			}(rec, fmt.Sprintf("c%d-%d", i, j))
		}
		<-done
		<-done
		la, _ := sendqueue.Load(f.dir, a.SendID)
		lb, _ := sendqueue.Load(f.dir, b.SendID)
		if la.State == sendqueue.StateQueued && lb.State == sendqueue.StateQueued {
			t.Fatalf("iteration %d: both identical sends reopened", i)
		}
		// Retire this pair so the next iteration starts clean.
		for _, rec := range []*sendqueue.Record{la, lb} {
			rec.State = sendqueue.StateCancelled
			_ = sendqueue.Save(f.dir, rec)
		}
	}
}

func TestRedeliverKnownCopyIgnoresWhitespace(t *testing.T) {
	f := newRedeliverFixture(t, true)
	pending := f.record(t, sendqueue.StateQueued)
	pending.Message = "  [REDACTED]\n"
	if err := sendqueue.Save(f.dir, pending); err != nil {
		t.Fatal(err)
	}
	failed := f.record(t, sendqueue.StateFailed)
	if got := f.run(t, failed, "ws", false); got.Reason != "content_present" {
		t.Fatalf("whitespace variant not treated as a copy: %+v", got)
	}
}
