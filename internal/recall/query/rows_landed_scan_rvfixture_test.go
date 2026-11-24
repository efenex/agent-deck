package query

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Reviewer fixture (synthetic, structure only). Real Claude transcripts write
// queued_command / hook attachments after a typed prompt row with a timestamp
// up to minutes earlier. Such an attachment as the tail's first complete record
// must not prove that nothing fresh precedes the tail.
func TestRVTailAnchorAfterInvertedTimestamp(t *testing.T) { rvInvertedCases(t, false) }

// Sibling: the late-proof path. A second identical send inside the tail makes the
// window ambiguous; hiding the first one must not yield a unique Complete claim.
func TestRVTailAnchorHidesAmbiguity(t *testing.T) { rvInvertedCases(t, true) }

func rvInvertedCases(t *testing.T, second bool) {
	for _, withinCap := range []bool{false, true} {
		t.Run(fmt.Sprintf("within_cap=%v", withinCap), func(t *testing.T) { rvInverted(t, second, withinCap) })
	}
}

func rvInverted(t *testing.T, second, withinCap bool) {
	created := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	userRow := func(id, content string, at time.Time) string {
		return fmt.Sprintf(`{"type":"user","uuid":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n", id, at.Format(time.RFC3339Nano), content)
	}
	attach := func(id string, at time.Time) string {
		return fmt.Sprintf(`{"type":"attachment","uuid":%q,"timestamp":%q,"attachment":{"type":"queued_command","prompt":"[REDACTED-OTHER]"}}`+"\n", id, at.Format(time.RFC3339Nano))
	}
	landed := userRow("landed", "[REDACTED]", created.Add(time.Second)) // the send did land
	anchor := attach("anchor", created.Add(-time.Minute))               // written later, stamped earlier
	filler := userRow("f", strings.Repeat("[REDACTED-OTHER]", 4096), created.Add(2*time.Minute))
	bodyLimit := LandedScanBytes
	if withinCap {
		bodyLimit -= 1024
	}
	var body strings.Builder
	body.WriteString(anchor)
	if second {
		body.WriteString(userRow("landed2", "[REDACTED]", created.Add(5*time.Second)))
	}
	padMin := len(userRow("p", "", created.Add(3*time.Minute)))
	for int64(body.Len()+len(filler)+padMin) <= bodyLimit {
		body.WriteString(filler)
	}
	pad := bodyLimit - int64(body.Len()) // last row sized so the tail starts exactly at anchor
	padRow := userRow("p", strings.Repeat("x", int(pad)-padMin), created.Add(3*time.Minute))
	body.WriteString(padRow)
	prefix := offsetHistory() + userRow("old", "[REDACTED-OTHER]", created.Add(-time.Hour))
	data := prefix + landed + body.String()
	path := filepath.Join(t.TempDir(), "native.jsonl")
	if err := os.WriteFile(path, []byte(prefix), 0600); err != nil {
		t.Fatal(err)
	}
	w := offsetWindow(t, path, LandedWindow{To: -1, NotBefore: created, PeerEnvelope: true})
	appendOffsetRows(t, path, landed+body.String())
	if second {
		w.NotAfter = created.Add(10 * time.Minute)
	}
	full := LandedWindow{To: -1, NotBefore: w.NotBefore, NotAfter: w.NotAfter, PeerEnvelope: true}
	full.MaxBytes = int64(len(data)) + 1
	fullGot := ScanLanded(context.Background(), "claude", path, "[REDACTED]", full)
	if !fullGot.Complete || !fullGot.Present {
		t.Fatalf("full scan should see the landed row: %+v", fullGot)
	}
	got := ScanLanded(context.Background(), "claude", path, "[REDACTED]", w)
	if withinCap {
		expected := 1
		if second {
			expected = 2
		}
		if !got.Complete || !got.Present || len(got.Candidates) != expected {
			t.Fatalf("offset must preserve all candidates despite timestamp inversion: %+v", got)
		}
	} else if got.Complete || got.Reason != "scan_limit" {
		t.Fatalf("post-offset span exceeds cap: %+v", got)
	}
	if second {
		if len(fullGot.Candidates) != 2 {
			t.Fatalf("full scan should be ambiguous: %+v", fullGot)
		}
		if got.Complete && len(got.Candidates) == 1 {
			t.Fatalf("FALSE COVERAGE: tail claims a unique landed row, full scan has two: tail=%+v", got)
		}
		return
	}
	if got.Complete && !got.Present {
		t.Fatalf("FALSE COVERAGE: tail declared complete and absent while the send landed before it: %+v", got)
	}
}
