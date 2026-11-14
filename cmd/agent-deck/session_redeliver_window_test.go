package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/recall/query"
	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
)

// Proof-window attribution once only same-text sends fence the window
// (fc0a2cae). Every case runs in both reconcile orders: the claims lock makes
// the first reconcile win, so attribution must not depend on who runs first
// unless the test says so.

type windowSend struct {
	text string
	sent time.Time
	from int64
}

func windowRecords(t *testing.T, f *redeliverFixture, sends ...windowSend) []*sendqueue.Record {
	t.Helper()
	var out []*sendqueue.Record
	for _, s := range sends {
		r := f.record(t, sendqueue.StateTyped)
		r.Settled = true
		r.Message, r.SentAt, r.TranscriptFrom = s.text, s.sent.Format(time.RFC3339Nano), s.from
		if err := sendqueue.Save(f.dir, r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func windowLoad(t *testing.T, f *redeliverFixture, r *sendqueue.Record) *sendqueue.Record {
	t.Helper()
	got, err := sendqueue.Load(f.dir, r.SendID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func windowQueueOp(op, reason, content string, ts time.Time) string {
	return fmt.Sprintf(`{"type":"queue-operation","operation":%q,"reason":%q,"timestamp":%q,"content":%q}`+"\n",
		op, reason, ts.UTC().Format(time.RFC3339Nano), content)
}

// A "yes", a different-text X, then B "yes". The X offset is no fence, but
// B's is: A never takes B's row, whether or not A's own row exists.
func TestProofWindowIdenticalPairAcrossDifferentSend(t *testing.T) {
	for _, aLands := range []bool{true, false} {
		for _, order := range []string{"a-first", "b-first"} {
			t.Run(fmt.Sprintf("a_lands=%v/%s", aLands, order), func(t *testing.T) {
				f := newRedeliverFixture(t, false)
				t0 := f.now.Add(-5 * time.Minute)
				var rowA string
				if aLands {
					rowA = claudeUserLine("row-a", "[REDACTED]", t0)
				}
				rowX := claudeUserLine("row-x", "[REDACTED-OTHER]", t0.Add(time.Minute))
				rowB := claudeUserLine("row-b", "[REDACTED]", t0.Add(2*time.Minute))
				if err := os.WriteFile(f.path, []byte(rowA+rowX+rowB), 0600); err != nil {
					t.Fatal(err)
				}
				recs := windowRecords(t, f,
					windowSend{"[REDACTED]", t0, 0},
					windowSend{"[REDACTED-OTHER]", t0.Add(time.Minute), int64(len(rowA))},
					windowSend{"[REDACTED]", t0.Add(2 * time.Minute), int64(len(rowA + rowX))})
				a, b := recs[0], recs[2]
				first, second := a, b
				if order == "b-first" {
					first, second = b, a
				}
				f.run(t, first, "k1", false)
				f.run(t, second, "k2", false)
				la, lb := windowLoad(t, f, a), windowLoad(t, f, b)
				if lb.State != sendqueue.StateLanded || lb.LandedRowID != "row-b" {
					t.Fatalf("B not proven by its own row: %+v", lb)
				}
				switch {
				case aLands && (la.State != sendqueue.StateLanded || la.LandedRowID != "row-a"):
					t.Fatalf("A not proven by its own row: %+v", la)
				case !aLands && la.State == sendqueue.StateLanded:
					t.Fatalf("lost A took B's row: %+v", la)
				}
			})
		}
	}
}

// A is typed mid-turn and enqueued; B (same text) is typed at a later offset
// and lost; A's absorption is written after B's offset. B's scan starts with
// an empty parser, so the absorption shows up as an orphan absorbed row.
//
// Only one physical absorption exists, so at most one send may land.
// Base FindLanded proves both (A via the enqueue/update pair, B via the
// orphan row). At the tip A is fenced (pending only) and B takes the row:
// right count, wrong attribution. This predates fc0a2cae (an any-text fence
// is also a same-text fence) and is pinned here as known behaviour.
func TestProofWindowOrphanAbsorptionNeverProvesTwo(t *testing.T) {
	for _, order := range []string{"a-first", "b-first"} {
		t.Run(order, func(t *testing.T) {
			f := newRedeliverFixture(t, false)
			t0 := f.now.Add(-5 * time.Minute)
			enqA := windowQueueOp("enqueue", "", "[REDACTED]", t0)
			absorbA := windowQueueOp("remove", "absorbed_mid_turn", "[REDACTED]", t0.Add(time.Minute))
			if err := os.WriteFile(f.path, []byte(enqA+absorbA), 0600); err != nil {
				t.Fatal(err)
			}
			recs := windowRecords(t, f,
				windowSend{"[REDACTED]", t0, 0},
				windowSend{"[REDACTED]", t0.Add(30 * time.Second), int64(len(enqA))})
			a, b := recs[0], recs[1]
			ctx := context.Background()
			_, _, baseA := query.FindLanded(ctx, "claude", f.path, 0, a.Message, t0)
			_, _, baseB := query.FindLanded(ctx, "claude", f.path, b.TranscriptFrom, b.Message, t0.Add(30*time.Second))
			if !baseA || !baseB {
				t.Fatalf("fixture: base FindLanded proves A=%v B=%v, want both", baseA, baseB)
			}
			first, second := a, b
			if order == "b-first" {
				first, second = b, a
			}
			f.run(t, first, "k1", false)
			f.run(t, second, "k2", false)
			la, lb := windowLoad(t, f, a), windowLoad(t, f, b)
			if la.State == sendqueue.StateLanded && lb.State == sendqueue.StateLanded {
				t.Fatalf("one absorption proved two sends: A=%s B=%s", la.LandedRowID, lb.LandedRowID)
			}
			if la.State == sendqueue.StateLanded || lb.State != sendqueue.StateLanded {
				t.Fatalf("known attribution changed: A=%s B=%s (row %s)", la.State, lb.State, lb.LandedRowID)
			}
		})
	}
}

// B (same text, later) has an offset that cannot fence A: typed while the
// transcript path was unknown, so the watcher filled the path in later with
// transcript_from 0. A lost A can then take B's row if it reconciles first.
// One row, one landed send either way: right count, attribution depends on
// order. Predates fc0a2cae (same skip conditions); pinned as known behaviour.
func TestProofWindowUnfenceableSiblingNeverProvesTwo(t *testing.T) {
	for _, order := range []string{"a-first", "b-first"} {
		t.Run(order, func(t *testing.T) {
			f := newRedeliverFixture(t, false)
			t0 := f.now.Add(-5 * time.Minute)
			rowP := claudeUserLine("row-prev", "[REDACTED-OTHER]", t0.Add(-time.Minute))
			rowB := claudeUserLine("row-b", "[REDACTED]", t0.Add(time.Minute))
			if err := os.WriteFile(f.path, []byte(rowP+rowB), 0600); err != nil {
				t.Fatal(err)
			}
			recs := windowRecords(t, f,
				windowSend{"[REDACTED]", t0, int64(len(rowP))},
				windowSend{"[REDACTED]", t0.Add(time.Minute), 0})
			a, b := recs[0], recs[1]
			first, second := a, b
			if order == "b-first" {
				first, second = b, a
			}
			f.run(t, first, "k1", false)
			f.run(t, second, "k2", false)
			la, lb := windowLoad(t, f, a), windowLoad(t, f, b)
			if la.State == sendqueue.StateLanded && lb.State == sendqueue.StateLanded {
				t.Fatalf("one row proved two sends: A=%s B=%s", la.LandedRowID, lb.LandedRowID)
			}
			winner := map[string]*sendqueue.Record{"a-first": la, "b-first": lb}[order]
			if winner.State != sendqueue.StateLanded || winner.LandedRowID != "row-b" {
				t.Fatalf("first reconcile did not take the only row: A=%s B=%s", la.State, lb.State)
			}
		})
	}
}

// The behaviour fc0a2cae changes: an untracked identical row (a person
// retyping, or a plain non-queued `session send`, which keeps no record)
// after a different-text tracked send. 5de87986 fenced it at the different
// send's offset; the tip lets it prove the lost send within the 10-minute
// window, as base FindLanded always did (with no window). Past the window
// it never proves.
func TestProofWindowUntrackedRetypeAfterDifferentSend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after time.Duration
		want  string
	}{
		{"within window", 3 * time.Minute, sendqueue.StateLanded},
		{"past window", 11 * time.Minute, sendqueue.StateTyped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRedeliverFixture(t, false)
			t0 := f.now.Add(-12 * time.Minute)
			rowX := claudeUserLine("row-x", "[REDACTED-OTHER]", t0.Add(time.Minute))
			rowH := claudeUserLine("row-human", "[REDACTED]", t0.Add(tc.after))
			if err := os.WriteFile(f.path, []byte(rowX+rowH), 0600); err != nil {
				t.Fatal(err)
			}
			recs := windowRecords(t, f,
				windowSend{"[REDACTED]", t0, 0},
				windowSend{"[REDACTED-OTHER]", t0.Add(time.Minute), 0})
			a := recs[0]
			if _, _, ok := query.FindLanded(context.Background(), "claude", f.path, 0, a.Message, t0); !ok {
				t.Fatal("fixture: base FindLanded does not prove the retype")
			}
			f.run(t, a, "k", false)
			if got := windowLoad(t, f, a); got.State != tc.want {
				t.Fatalf("lost send with untracked retype %s: state %s, want %s", tc.name, got.State, tc.want)
			}
		})
	}
}

// A different-text failed send to the same target that finds the per-target
// reopen lock held gets pending/reopen_busy. The decision is bound to its key,
// so a replay of that key never reopens; the record stays failed and a new
// key is needed. Pinned so a change to this is deliberate.
func TestRedeliverReopenBusyIsStickyPerKey(t *testing.T) {
	f := newRedeliverFixture(t, true)
	held, ok, err := sendqueue.TryLock(f.dir, "reopen-"+f.target.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	r := f.record(t, sendqueue.StateFailed)
	if got := f.run(t, r, "busy", false); got.Outcome != "pending" || got.Reason != "reopen_busy" {
		_ = held.Release()
		t.Fatalf("held reopen lock: %+v", got)
	}
	_ = held.Release()
	got := f.run(t, r, "busy", false)
	if !got.Replayed || got.Reason != "reopen_busy" || got.Snapshot == nil || got.Snapshot.State != sendqueue.StateFailed {
		t.Fatalf("replay after release: %+v snapshot=%+v", got, got.Snapshot)
	}
	if got := f.run(t, r, "fresh", false); got.Reason != "reopened" {
		t.Fatalf("new key after release: %+v", got)
	}
}
