package query

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanLandedBoundedEvidence(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	user := func(id, role string, at time.Time) string {
		return fmt.Sprintf(`{"type":%q,"uuid":%q,"timestamp":%q,"message":{"role":%q,"content":"[REDACTED]"}}`+"\n", role, id, at.Format(time.RFC3339Nano), role)
	}
	queue := func(op, reason string) string {
		return fmt.Sprintf(`{"type":"queue-operation","operation":%q,"reason":%q,"timestamp":%q,"content":"[REDACTED]"}`+"\n", op, reason, now.Format(time.RFC3339Nano))
	}
	for _, tc := range []struct {
		name, data        string
		from, to          int64
		matches           int
		present, complete bool
	}{
		{"one", user("u1", "user", now), 0, -1, 1, true, true},
		{"two identical sends", user("u1", "user", now) + user("u2", "user", now), 0, -1, 2, true, true},
		{"assistant quote", user("a1", "assistant", now), 0, -1, 0, false, true},
		{"old", user("old", "user", now.Add(-time.Hour)), 0, -1, 0, false, true},
		{"late human copy", user("later", "user", now.Add(11*time.Minute)), 0, -1, 0, false, true},
		{"next send boundary", user("u1", "user", now) + user("u2", "user", now), 0, int64(len(user("u1", "user", now))), 1, true, true},
		{"from offset", user("u1", "user", now) + user("u2", "user", now), int64(len(user("u1", "user", now))), -1, 1, true, true},
		{"pending", queue("enqueue", ""), 0, -1, 0, true, true},
		{"absorbed", queue("enqueue", "") + queue("remove", "absorbed into turn"), 0, -1, 1, true, true},
		{"removed", queue("enqueue", "") + queue("remove", "user removed"), 0, -1, 0, false, true},
		{"dequeued replaced", queue("enqueue", "") + queue("dequeue", "") + user("u1", "user", now), 0, -1, 1, true, true},
		{"malformed", "{broken}\n" + user("u1", "user", now), 0, -1, 1, true, false},
		{"torn tail", user("u1", "user", now) + `{"type":`, 0, -1, 1, true, false},
		{"shortened", "", 10, -1, 0, false, false},
		{"midline cursor", user("u1", "user", now), 5, -1, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.jsonl")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", LandedWindow{From: tc.from, To: tc.to, NotBefore: now, NotAfter: now.Add(10 * time.Minute)})
			if len(got.Candidates) != tc.matches || got.Present != tc.present || got.Complete != tc.complete {
				t.Fatalf("scan = %+v", got)
			}
		})
	}
}

func TestScanLandedCodexAndCoverage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	line := `{"type":"response_item","timestamp":"2026-11-13T12:00:00Z","payload":{"type":"message","role":"user","id":"msg-1","content":[{"type":"input_text","text":"[REDACTED]"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	got := ScanLanded(context.Background(), "codex", path, "[REDACTED]", LandedWindow{To: -1})
	if !got.Complete || len(got.Candidates) != 1 || got.Candidates[0].ID != "msg-1" {
		t.Fatalf("codex: %+v", got)
	}
	got = ScanLanded(context.Background(), "codex", path, "[REDACTED]", LandedWindow{To: -1, MaxBytes: 10})
	if got.Complete || got.Reason != "scan_limit" {
		t.Fatalf("bounded scan: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := ScanLanded(ctx, "codex", path, "[REDACTED]", LandedWindow{To: -1}); got.Complete {
		t.Fatalf("cancelled scan: %+v", got)
	}
	for _, tool := range []string{"shell", "opencode", "gemini"} {
		if got := ScanLanded(context.Background(), tool, path, "[REDACTED]", LandedWindow{To: -1}); got.Complete || got.Reason != "unsupported" {
			t.Fatalf("%s: %+v", tool, got)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := ScanLanded(context.Background(), "codex", path, "[REDACTED]", LandedWindow{To: -1}); got.Complete {
		t.Fatal("missing source claimed absent")
	}
}
