package query

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/recall/reader"
)

// Second-review fixtures. They take the snapshot with the real capture
// function, so the window is exactly the one a send records.

func rv2Capture(t *testing.T, path string) *reader.TranscriptOffset {
	t.Helper()
	offset := reader.CaptureTranscriptOffset(path)
	if offset == nil {
		t.Fatal("no snapshot captured")
	}
	return offset
}

// A capture at a row boundary must keep every row written after it, however
// the writer groups rows into writes: in one write with an earlier row, after
// a row completed between capture and send, or finishing a row cut by the
// capture. Over the cap, so only the offset window is scanned.
func TestRV2OffsetBoundaryKeepsRowsWrittenAfterCapture(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	other := offsetUser("other", "[REDACTED-OTHER]", now)
	landed := offsetUser("landed", "[REDACTED]", now.Add(time.Second))
	for _, tc := range []struct {
		name, before, after string
	}{
		{"one write after boundary", "", other + landed},
		{"capture between rows", other, landed},
		{"capture cuts the earlier row", other[:len(other)/2], other[len(other)/2:] + landed},
		{"landed first in its write", "", landed + other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.jsonl")
			if err := os.WriteFile(path, []byte(offsetHistory()+tc.before), 0600); err != nil {
				t.Fatal(err)
			}
			offset := rv2Capture(t, path)
			appendOffsetRows(t, path, tc.after)
			proof := ScanLanded(context.Background(), "claude", path, "[REDACTED]", LandedWindow{
				From: offset.Offset, To: -1, NotBefore: now, NotAfter: now.Add(10 * time.Minute), TranscriptOffset: offset})
			if !proof.Complete || len(proof.Candidates) != 1 || proof.Candidates[0].ID != "landed" {
				t.Fatalf("proof lost the row written after the capture: %+v", proof)
			}
			presence := ScanLanded(context.Background(), "claude", path, "[REDACTED]", LandedWindow{
				To: -1, NotBefore: now, PeerEnvelope: true, TranscriptOffset: offset})
			if !presence.Complete || !presence.Present {
				t.Fatalf("presence lost the row written after the capture: %+v", presence)
			}
		})
	}
}

// A file under 4 KiB is pinned by its whole prefix: a same-inode rewrite that
// keeps those bytes keeps the snapshot valid and the row after it in view; a
// rewrite that changes one byte of it fails closed.
func TestRV2OffsetShortFileRewrite(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	prefix := offsetUser("a", "[REDACTED-OTHER]", now.Add(-time.Hour)) + offsetUser("b", "[REDACTED-OTHER]", now.Add(-time.Minute))
	landed := offsetUser("landed", "[REDACTED]", now.Add(time.Second))
	for _, tc := range []struct {
		name, rewrite string
		reason        string
	}{
		{"same prefix", prefix + landed, ""},
		{"changed prefix", strings.Replace(prefix, `"a"`, `"z"`, 1) + landed, "scan_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.jsonl")
			if err := os.WriteFile(path, []byte(prefix), 0600); err != nil {
				t.Fatal(err)
			}
			offset := rv2Capture(t, path)
			if offset.AnchorBytes != int64(len(prefix)) {
				t.Fatalf("short file anchor must cover the whole prefix: %d of %d", offset.AnchorBytes, len(prefix))
			}
			before, _ := os.Stat(path)
			if err := os.WriteFile(path, []byte(tc.rewrite), 0600); err != nil { // O_TRUNC keeps the inode
				t.Fatal(err)
			}
			if after, _ := os.Stat(path); !os.SameFile(before, after) {
				t.Fatal("fixture did not keep the inode")
			}
			got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", LandedWindow{
				From: offset.Offset, To: -1, NotBefore: now, TranscriptOffset: offset})
			if got.Reason != tc.reason {
				t.Fatalf("short file rewrite: %+v", got)
			}
			if tc.reason == "" && (len(got.Candidates) != 1 || got.Candidates[0].ID != "landed") {
				t.Fatalf("row after an unchanged prefix lost: %+v", got)
			}
		})
	}
}

// Offset zero has no anchor, so it must add nothing to the legacy rules: the
// whole file is the window, and over the cap that is scan_limit.
func TestRV2OffsetZeroIsLegacy(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	landed := offsetUser("landed", "[REDACTED]", now.Add(time.Second))
	for _, tc := range []struct {
		name, grow string
		reason     string
	}{
		{"fits", landed, ""},
		{"over cap", landed + offsetHistory(), "scan_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.jsonl")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			offset := rv2Capture(t, path)
			if offset.Offset != 0 {
				t.Fatalf("empty file offset = %d", offset.Offset)
			}
			appendOffsetRows(t, path, tc.grow)
			for _, w := range []LandedWindow{
				{To: -1, NotBefore: now, PeerEnvelope: true},
				{To: -1, NotBefore: now, PeerEnvelope: true, TranscriptOffset: offset},
			} {
				got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", w)
				if got.Reason != tc.reason || (tc.reason == "" && !got.Present) {
					t.Fatalf("offset zero (with offset: %v) differs from legacy: %+v", w.TranscriptOffset != nil, got)
				}
			}
		})
	}
}

// Over the cap, legacy presence scans the last 4 MiB and blocks on a row of
// an earlier attempt found there. The latest attempt's offset sits after that
// row; it must not turn the scan into a complete absence.
func TestRV2OffsetKeepsLegacyTailOverCap(t *testing.T) {
	created := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "native.jsonl")
	data := offsetHistory() + offsetUser("attempt1", "[REDACTED]", created.Add(10*time.Second))
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	offset := rv2Capture(t, path)
	appendOffsetRows(t, path, offsetUser("other", "[REDACTED-OTHER]", created.Add(20*time.Second)))
	legacy := ScanLanded(context.Background(), "claude", path, "[REDACTED]", LandedWindow{To: -1, NotBefore: created, PeerEnvelope: true})
	if !legacy.Present {
		t.Fatalf("fixture: legacy tail must see the earlier row: %+v", legacy)
	}
	got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", LandedWindow{To: -1, NotBefore: created, PeerEnvelope: true, TranscriptOffset: offset})
	if !got.Complete || !got.Present {
		t.Fatalf("offset narrowed the legacy tail over the cap: %+v", got)
	}
}

// Starting at the legacy tail carries queue state from before the offset into
// rows after it. An enqueue before the capture and its absorption after it must
// give the same answer as an unbounded scan, fresh or older than NotBefore.
func TestRV2OffsetTailQueueStateMatchesUnboundedScan(t *testing.T) {
	created := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	queue := func(op string, at time.Time) string {
		return fmt.Sprintf(`{"type":"queue-operation","operation":%q,"timestamp":%q,"content":"[REDACTED]"}`+"\n", op, at.Format(time.RFC3339Nano))
	}
	absorbed := func(at time.Time) string {
		return queue("remove", at)[:len(queue("remove", at))-2] + `,"reason":"absorbed_mid_turn"}` + "\n" +
			fmt.Sprintf(`{"type":"attachment","uuid":"q1","timestamp":%q,"attachment":{"type":"queued_command","prompt":"[REDACTED]","commandMode":"prompt","origin":{"kind":"human"}}}`+"\n", at.Format(time.RFC3339Nano))
	}
	for _, tc := range []struct {
		name    string
		enqueue time.Time
		after   string
	}{
		{"fresh enqueue, absorbed", created.Add(time.Second), absorbed(created.Add(5 * time.Second))},
		{"stale enqueue, absorbed", created.Add(-time.Hour), absorbed(created.Add(5 * time.Second))},
		{"fresh enqueue, dequeued", created.Add(time.Second), queue("dequeue", created.Add(5*time.Second)) + offsetUser("u1", "[REDACTED]", created.Add(5*time.Second))},
		{"stale enqueue, dequeued", created.Add(-time.Hour), queue("dequeue", created.Add(5*time.Second)) + offsetUser("u1", "[REDACTED]", created.Add(5*time.Second))},
		{"fresh enqueue, removed", created.Add(time.Second), queue("remove", created.Add(5*time.Second))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.jsonl")
			if err := os.WriteFile(path, []byte(offsetHistory()+queue("enqueue", tc.enqueue)), 0600); err != nil {
				t.Fatal(err)
			}
			offset := rv2Capture(t, path)
			appendOffsetRows(t, path, tc.after)
			full := ScanLanded(context.Background(), "claude", path, "[REDACTED]", LandedWindow{To: -1, NotBefore: created, PeerEnvelope: true, MaxBytes: 1 << 30})
			got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", LandedWindow{To: -1, NotBefore: created, PeerEnvelope: true, TranscriptOffset: offset})
			t.Logf("full={Present:%v Pending:%v} offset={Present:%v Pending:%v Complete:%v %s}", full.Present, full.Pending, got.Present, got.Pending, got.Complete, got.Reason)
			if !full.Complete || !got.Complete || got.Present != full.Present || got.Pending != full.Pending {
				t.Fatalf("offset scan disagrees with unbounded scan: full=%+v offset=%+v", full, got)
			}
		})
	}
}
