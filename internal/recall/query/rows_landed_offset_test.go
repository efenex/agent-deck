package query

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/recall/reader"
)

// Use the additive JSON shape so these regression tests also run on the base,
// where the unknown field is ignored, rather than failing to compile there.
func offsetWindow(t *testing.T, path string, w LandedWindow) LandedWindow {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dev, ino := reader.FileIdentity(info)
	b, err := json.Marshal(map[string]any{"TranscriptOffset": map[string]any{
		"path": path, "offset": info.Size(), "device": dev, "inode": ino,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func appendOffsetRows(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

func offsetUser(id, text string, at time.Time) string {
	return fmt.Sprintf(`{"type":"user","uuid":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n", id, at.Format(time.RFC3339Nano), text)
}

func offsetHistory() string {
	return strings.Repeat(`{"type":"file-history-snapshot"}`+"\n", int(LandedScanBytes/30)+1)
}

func TestScanLandedSendOffset(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, suffix string
		matches      int
	}{
		{"absent", "", 0},
		{"present", offsetUser("one", "[REDACTED]", now), 1},
		{"ambiguous", offsetUser("one", "[REDACTED]", now) + offsetUser("two", "[REDACTED]", now.Add(time.Second)), 2},
		{"past NotAfter", offsetUser("late", "[REDACTED]", now.Add(11*time.Minute)), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.jsonl")
			if err := os.WriteFile(path, []byte(offsetHistory()), 0600); err != nil {
				t.Fatal(err)
			}
			w := offsetWindow(t, path, LandedWindow{To: -1, NotBefore: now, NotAfter: now.Add(10 * time.Minute)})
			appendOffsetRows(t, path, tc.suffix)
			got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", w)
			if !got.Complete || got.Present != (tc.matches > 0) || len(got.Candidates) != tc.matches {
				t.Fatalf("offset coverage: %+v", got)
			}
		})
	}
}

func TestScanLandedSendOffsetFailsClosed(t *testing.T) {
	for _, mode := range []string{"legacy", "replaced", "truncated", "different path", "over cap", "backstep too long"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.jsonl")
			data := offsetHistory()
			if mode == "backstep too long" {
				data += `{"type":"user","padding":"` + strings.Repeat("x", 128<<10)
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			w := offsetWindow(t, path, LandedWindow{To: -1})
			switch mode {
			case "legacy":
				w = LandedWindow{To: -1}
			case "replaced":
				// Retain the old inode so immediate inode reuse cannot mask replacement.
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			case "different path":
				if err := os.Link(path, path+".link"); err != nil {
					t.Fatal(err)
				}
				path += ".link"
			case "over cap":
				appendOffsetRows(t, path, data)
			case "backstep too long":
				appendOffsetRows(t, path, "\"}\n")
			}
			got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", w)
			if got.Complete || got.Reason != "scan_limit" {
				t.Fatalf("unsafe offset: %+v", got)
			}
		})
	}
}

func TestScanLandedSendOffsetBackstep(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	row := offsetUser("concurrent", "[REDACTED]", now)
	for _, cut := range []int{0, len(row) / 2, len(row) - 1, len(row)} {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.jsonl")
			if err := os.WriteFile(path, []byte(offsetHistory()+row[:cut]), 0600); err != nil {
				t.Fatal(err)
			}
			w := offsetWindow(t, path, LandedWindow{To: -1, NotBefore: now})
			appendOffsetRows(t, path, row[cut:])
			got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", w)
			if !got.Complete || len(got.Candidates) != 1 || got.Candidates[0].ID != "concurrent" {
				t.Fatalf("concurrent row lost at offset: %+v", got)
			}
		})
	}
}

func TestScanLandedSendOffsetBoundsAndPending(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	first := offsetUser("one", "[REDACTED]", now)
	second := offsetUser("two", "[REDACTED]", now.Add(time.Second))
	queue := fmt.Sprintf(`{"type":"queue-operation","operation":"enqueue","timestamp":%q,"content":"[REDACTED]"}`+"\n", now.Format(time.RFC3339Nano))
	for _, tc := range []struct {
		name, suffix string
		limit, to    int64
		count        int
		pending      bool
		reason       string
	}{
		{"at cap with backstep", first, int64(len(first)), -1, 1, false, ""},
		{"past cap", first, int64(len(first) - 1), -1, 0, false, "scan_limit"},
		{"next send fence", first + second, int64(len(first)), int64(len(first)), 1, false, ""},
		{"torn upper fence", first + second, int64(len(first)), int64(len(first) - 1), 0, false, "source_incomplete"},
		{"pending", queue, 0, -1, 0, true, ""},
		{"malformed", "{broken}\n" + first, 0, -1, 1, false, "source_malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.jsonl")
			prefix := offsetHistory()
			if err := os.WriteFile(path, []byte(prefix), 0600); err != nil {
				t.Fatal(err)
			}
			to := tc.to
			if to >= 0 {
				to += int64(len(prefix))
			}
			w := offsetWindow(t, path, LandedWindow{From: int64(len(prefix)), To: to, MaxBytes: tc.limit, NotBefore: now})
			appendOffsetRows(t, path, tc.suffix)
			got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", w)
			if got.Complete != (tc.reason == "") || got.Reason != tc.reason || len(got.Candidates) != tc.count || got.Pending != tc.pending {
				t.Fatalf("bounds or pending changed: %+v", got)
			}
		})
	}
}

func TestScanLandedSendOffsetCodexEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	w := offsetWindow(t, path, LandedWindow{To: -1})
	appendOffsetRows(t, path, `{"type":"response_item","timestamp":"2026-11-13T12:00:00Z","payload":{"type":"message","role":"user","id":"msg-1","content":[{"type":"input_text","text":"[REDACTED]"}]}}`+"\n")
	got := ScanLanded(context.Background(), "codex", path, "[REDACTED]", w)
	if !got.Complete || len(got.Candidates) != 1 || got.Candidates[0].ID != "msg-1" {
		t.Fatalf("empty file offset: %+v", got)
	}
}
