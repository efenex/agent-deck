package query

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Review fixtures for send-time offsets: an identity that still passes
// device/inode and size checks must not turn a different byte history into
// coverage, and an offset must never narrow a legacy window that fits.

func rvOtherFiller(n int64) string {
	line := `{"type":"summary","leafUuid":"[REDACTED]"}` + "\n"
	return strings.Repeat(line, int(n)/len(line)+1)
}

// Same inode, truncated and regrown past the saved offset: the matching row
// now sits before the offset and the window after it is empty of matches.
func TestRVOffsetTruncateRegrow(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "native.jsonl")
	if err := os.WriteFile(path, []byte(offsetHistory()), 0600); err != nil {
		t.Fatal(err)
	}
	w := offsetWindow(t, path, LandedWindow{To: -1, NotBefore: now, PeerEnvelope: true})
	before, _ := os.Stat(path)
	regrown := offsetUser("landed", "[REDACTED]", now.Add(time.Second)) + rvOtherFiller(before.Size())
	if err := os.WriteFile(path, []byte(regrown), 0600); err != nil { // O_TRUNC keeps the inode
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) || after.Size() < before.Size() {
		t.Fatal("fixture did not keep the inode and regrow past the offset")
	}
	if full := ScanLanded(context.Background(), "claude", path, "[REDACTED]", LandedWindow{To: -1, NotBefore: now, PeerEnvelope: true, MaxBytes: after.Size() + 1}); !full.Present {
		t.Fatalf("full scan must see the row: %+v", full)
	}
	got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", w)
	if got.Complete || got.Reason != "scan_limit" {
		t.Fatalf("truncate+regrow gave a verdict (false coverage): %+v", got)
	}
}

// A new file that reuses the old inode number. Only checkable where the
// filesystem hands the inode straight back.
func TestRVOffsetInodeReuse(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "native.jsonl")
	if err := os.WriteFile(path, []byte(offsetHistory()), 0600); err != nil {
		t.Fatal(err)
	}
	w := offsetWindow(t, path, LandedWindow{To: -1, NotBefore: now, PeerEnvelope: true})
	before, _ := os.Stat(path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(offsetUser("landed", "[REDACTED]", now.Add(time.Second))+rvOtherFiller(before.Size())), 0600); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if before.Sys().(*syscall.Stat_t).Ino != after.Sys().(*syscall.Stat_t).Ino {
		t.Skip("filesystem did not reuse the inode")
	}
	got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", w)
	if got.Complete || got.Reason != "scan_limit" {
		t.Fatalf("inode reuse gave a verdict (false coverage): %+v", got)
	}
}

// Presence on a small file: legacy covers from byte 0, so a row of an
// earlier attempt (before this attempt's offset) must still block.
func TestRVOffsetDoesNotNarrowLegacyWindow(t *testing.T) {
	created := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "native.jsonl")
	data := offsetUser("attempt1", "[REDACTED]", created.Add(10*time.Second)) + offsetUser("other", "[REDACTED-OTHER]", created.Add(20*time.Second))
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	w := offsetWindow(t, path, LandedWindow{To: -1, NotBefore: created, PeerEnvelope: true})
	legacy := ScanLanded(context.Background(), "claude", path, "[REDACTED]", LandedWindow{To: -1, NotBefore: created, PeerEnvelope: true})
	got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", w)
	if !legacy.Complete || !legacy.Present || !got.Complete || !got.Present {
		t.Fatalf("offset narrowed a fitting legacy window: legacy=%+v offset=%+v", legacy, got)
	}
}
