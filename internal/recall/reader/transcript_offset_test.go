package reader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscriptOffsetCaptureAndMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.jsonl")
	if got := CaptureTranscriptOffset(path); got != nil {
		t.Fatal("missing source has a snapshot")
	}
	if got := CaptureTranscriptOffset(filepath.Dir(path)); got != nil {
		t.Fatal("directory has a snapshot")
	}
	for _, data := range []string{"", "[REDACTED]\n"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		s := CaptureTranscriptOffset(path)
		if s == nil || s.Offset != int64(len(data)) || s.Path != path {
			t.Fatalf("capture: %+v", s)
		}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var saved TranscriptOffset
		if err := json.Unmarshal(b, &saved); err != nil {
			t.Fatal(err)
		}
		if saved != *s {
			t.Fatal("identity changed through JSON")
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("[REDACTED-APPENDED]\n"); err != nil {
			t.Fatal(err)
		}
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if !saved.Matches(f, path, info) {
			t.Fatal("append invalidated identity")
		}
		saved.Device++
		if saved.Matches(f, path, info) {
			t.Fatal("different device accepted")
		}
		_ = f.Close()
	}
}

func TestTranscriptOffsetPrefixFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.jsonl")
	prefix := strings.Repeat("x", int(transcriptIdentityBytes))
	if err := os.WriteFile(path, []byte(prefix+"[REDACTED-APPENDED]"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(prefix))
	s := TranscriptOffset{Path: path, Offset: int64(len(prefix)), PrefixBytes: int64(len(prefix)), PrefixHash: hex.EncodeToString(h[:]),
		AnchorBytes: int64(len(prefix)), AnchorHash: hex.EncodeToString(h[:])}
	if !s.Matches(f, path, info) {
		t.Fatal("prefix hash must match after growth")
	}
	for _, mutate := range []func(*TranscriptOffset){
		func(s *TranscriptOffset) { s.PrefixHash = "" },
		func(s *TranscriptOffset) { s.PrefixHash = strings.Repeat("0", 64) },
		func(s *TranscriptOffset) { s.PrefixBytes = 0 },
		func(s *TranscriptOffset) { s.PrefixBytes = -1 },
		func(s *TranscriptOffset) { s.PrefixBytes++ },
		func(s *TranscriptOffset) { s.Offset-- },
		func(s *TranscriptOffset) { s.Offset = -1 },
		func(s *TranscriptOffset) { s.Path += ".other" },
		func(s *TranscriptOffset) { s.AnchorHash = "" },
		func(s *TranscriptOffset) { s.AnchorHash = strings.Repeat("0", 64) },
		func(s *TranscriptOffset) { s.AnchorBytes-- },
	} {
		bad := s
		mutate(&bad)
		if bad.Matches(f, path, info) {
			t.Fatalf("invalid fallback accepted: %+v", bad)
		}
	}
}
