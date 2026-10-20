package query

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// chainSegs writes one transcript per entry (n user rows each) and returns
// the segments oldest first.
func chainSegs(t *testing.T, counts ...int) []ChainSegment {
	t.Helper()
	var segs []ChainSegment
	for i, n := range counts {
		var recs []map[string]any
		for j := 0; j < n; j++ {
			recs = append(recs, userRec(fmt.Sprintf("s%d-u%d", i, j), fmt.Sprintf("2026-10-%02dT10:%02d:00.000Z", i+1, j),
				fmt.Sprintf("seg %d row %d", i, j), map[string]any{"origin": map[string]any{"kind": "human"}}))
		}
		segs = append(segs, ChainSegment{Seq: i, NativeID: fmt.Sprintf("seg-%d", i), Harness: "claude", FirstSeen: int64(1000 + i*100), Path: writeTranscript(t, jsonl(t, recs...))})
	}
	return segs
}

func ids(rows []Row) string {
	var out []string
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return strings.Join(out, " ")
}

func TestReadChainRowsTailFillsAcrossSegments(t *testing.T) {
	segs := chainSegs(t, 2, 3, 2)
	ctx := context.Background()

	// Inside the newest segment: no boundary, before_cursor is a row cursor.
	tl, err := ReadChainRows(ctx, "claude", segs, RowsOptions{Tail: 1})
	if err != nil || ids(tl.Turns) != "s2-u1" || tl.Turns[0].Seg != "seg-2" || tl.ThroughCursor == "" || strings.HasPrefix(tl.BeforeCursor, "seg:") || tl.BeforeCursor == "" {
		t.Fatalf("tail 1: %v %+v", err, tl)
	}
	// Exactly the newest segment: it is not known to be exhausted yet.
	tl, err = ReadChainRows(ctx, "claude", segs, RowsOptions{Tail: 2})
	if err != nil || ids(tl.Turns) != "s2-u0 s2-u1" {
		t.Fatalf("tail 2: %v %+v", err, ids(tl.Turns))
	}
	// Past it: boundary, then the tail of the previous segment.
	tl, err = ReadChainRows(ctx, "claude", segs, RowsOptions{Tail: 4})
	if err != nil || ids(tl.Turns) != "s1-u1 s1-u2 boundary:seg-2 s2-u0 s2-u1" {
		t.Fatalf("tail 4: %v %s", err, ids(tl.Turns))
	}
	b := tl.Turns[2]
	if b.Kind != "system" || b.RawType != "chain/boundary" || b.Seg != "seg-2" || b.Meta["prev_seg"] != "seg-1" || b.Meta["boundary"] != "clear" {
		t.Fatalf("boundary row: %+v", b)
	}
	if tl.Turns[0].Seg != "seg-1" || tl.BeforeCursor == "" || strings.HasPrefix(tl.BeforeCursor, "seg:") {
		t.Fatalf("segment stamps / cursor: %+v", tl)
	}
	// Whole chain, no limit: every segment, a boundary before each later one,
	// and the chain start ends paging.
	tl, err = ReadChainRows(ctx, "claude", segs, RowsOptions{})
	want := "s0-u0 s0-u1 boundary:seg-1 s1-u0 s1-u1 s1-u2 boundary:seg-2 s2-u0 s2-u1"
	if err != nil || ids(tl.Turns) != want || tl.BeforeCursor != "" {
		t.Fatalf("whole chain: %v %s before=%q", err, ids(tl.Turns), tl.BeforeCursor)
	}
	// A tail that stops exactly where a segment is used up cannot tell, so the
	// cursor is a row cursor and the next page opens with the previous one.
	tl, err = ReadChainRows(ctx, "claude", segs, RowsOptions{Tail: 5})
	if err != nil || ids(tl.Turns) != "s1-u0 s1-u1 s1-u2 boundary:seg-2 s2-u0 s2-u1" || tl.BeforeCursor == "" || strings.HasPrefix(tl.BeforeCursor, "seg:") {
		t.Fatalf("tail 5: %v %s before=%q", err, ids(tl.Turns), tl.BeforeCursor)
	}
}

func TestReadChainRowsBeforeWalksBackToChainStart(t *testing.T) {
	segs := chainSegs(t, 2, 3, 2)
	ctx := context.Background()
	tl, err := ReadChainRows(ctx, "claude", segs, RowsOptions{Tail: 2})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	got = append(got, ids(tl.Turns))
	for i := 0; tl.BeforeCursor != ""; i++ {
		if i > 20 {
			t.Fatal("paging does not terminate")
		}
		if tl, err = ReadChainRows(ctx, "claude", segs, RowsOptions{Before: tl.BeforeCursor, Limit: 2}); err != nil {
			t.Fatal(err)
		}
		got = append([]string{ids(tl.Turns)}, got...)
	}
	all := strings.Join(got, " ")
	whole, _ := ReadChainRows(ctx, "claude", segs, RowsOptions{})
	if strings.TrimSpace(all) != ids(whole.Turns) {
		t.Fatalf("pages joined\n%s\nwhole\n%s", all, ids(whole.Turns))
	}
}

func TestReadChainRowsSegCursorStartsAtThatSegmentsTail(t *testing.T) {
	segs := chainSegs(t, 2, 3, 2)
	tl, err := ReadChainRows(context.Background(), "claude", segs, RowsOptions{Before: "seg:seg-1", Limit: 2})
	if err != nil || ids(tl.Turns) != "s1-u1 s1-u2" || tl.BeforeCursor == "" || strings.HasPrefix(tl.BeforeCursor, "seg:") {
		t.Fatalf("seg: cursor: %v %+v", err, ids(tl.Turns))
	}
	tl, err = ReadChainRows(context.Background(), "claude", segs, RowsOptions{Before: "seg:seg-0", Limit: 5})
	if err != nil || ids(tl.Turns) != "s0-u0 s0-u1" || tl.BeforeCursor != "" {
		t.Fatalf("seg: cursor at chain start: %v %+v before=%q", err, ids(tl.Turns), tl.BeforeCursor)
	}
}

// A page that uses a segment up carries on into the previous one; the
// boundary row sits between them.
func TestReadChainRowsPageSpillsIntoPreviousSegment(t *testing.T) {
	segs := chainSegs(t, 4, 1)
	ctx := context.Background()
	tl, err := ReadChainRows(ctx, "claude", segs, RowsOptions{Tail: 3})
	if err != nil || ids(tl.Turns) != "s0-u2 s0-u3 boundary:seg-1 s1-u0" || strings.HasPrefix(tl.BeforeCursor, "seg:") {
		t.Fatalf("tail 3: %v %s before=%q", err, ids(tl.Turns), tl.BeforeCursor)
	}
	next, err := ReadChainRows(ctx, "claude", segs, RowsOptions{Before: tl.BeforeCursor, Limit: 3})
	if err != nil || ids(next.Turns) != "s0-u0 s0-u1" || next.BeforeCursor != "" {
		t.Fatalf("before: %v %s before=%q", err, ids(next.Turns), next.BeforeCursor)
	}
	// The newest segment is one row: asking for one row does not show that
	// it is used up, the next page does and continues into seg-0.
	one, err := ReadChainRows(ctx, "claude", segs, RowsOptions{Tail: 1})
	if err != nil || ids(one.Turns) != "s1-u0" || one.BeforeCursor == "" || strings.HasPrefix(one.BeforeCursor, "seg:") {
		t.Fatalf("tail 1: %v %s before=%q", err, ids(one.Turns), one.BeforeCursor)
	}
	spill, err := ReadChainRows(ctx, "claude", segs, RowsOptions{Before: one.BeforeCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if ids(spill.Turns) != "s0-u2 s0-u3 boundary:seg-1" {
		t.Fatalf("spill: %s", ids(spill.Turns))
	}
	if spill.BeforeCursor == "" || strings.HasPrefix(spill.BeforeCursor, "seg:") {
		t.Fatalf("spill cursor %q", spill.BeforeCursor)
	}
}

func TestReadChainRowsSkipsSegmentsWithoutTranscript(t *testing.T) {
	segs := chainSegs(t, 2, 2, 2)
	segs[1].Path = ""
	tl, err := ReadChainRows(context.Background(), "claude", segs, RowsOptions{})
	if err != nil || ids(tl.Turns) != "s0-u0 s0-u1 boundary:seg-2 s2-u0 s2-u1" {
		t.Fatalf("%v %s", err, ids(tl.Turns))
	}
	if b := tl.Turns[2]; b.Meta["prev_seg"] != "seg-0" {
		t.Fatalf("boundary names the neighbour that is present: %+v", b)
	}
	for _, s := range segs {
		segs[s.Seq].Path = ""
	}
	if _, err := ReadChainRows(context.Background(), "claude", segs, RowsOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no transcripts at all: %v", err)
	}
}

func TestReadChainRowsEmptyNewestSegment(t *testing.T) {
	segs := chainSegs(t, 2, 0)
	tl, err := ReadChainRows(context.Background(), "claude", segs, RowsOptions{Tail: 2})
	if err != nil || tl.ThroughCursor == "" {
		t.Fatalf("%v %+v", err, tl)
	}
	if !strings.Contains(ids(tl.Turns), "boundary:seg-1") || !strings.Contains(ids(tl.Turns), "s0-u1") {
		t.Fatalf("a /clear that has no rows yet still joins to the previous conversation: %s", ids(tl.Turns))
	}
}

func TestReadChainRowsRejectsWhatItCannotPage(t *testing.T) {
	segs := chainSegs(t, 2, 2)
	ctx := context.Background()
	for _, opts := range []RowsOptions{{Since: "x"}, {AgentID: "a"}, {Limit: 3}, {Before: "seg:seg-0"}} {
		if _, err := ReadChainRows(ctx, "claude", segs, opts); err == nil {
			t.Errorf("%+v accepted", opts)
		}
	}
	var re *ResyncError
	if _, err := ReadChainRows(ctx, "claude", segs, RowsOptions{Before: "seg:nope", Limit: 2}); !errors.As(err, &re) || re.Reason != "unknown_segment" {
		t.Errorf("unknown segment: %v", err)
	}
	if _, err := ReadChainRows(ctx, "claude", segs, RowsOptions{Before: "!!!", Limit: 2}); !errors.As(err, &re) || re.Reason != "invalid_cursor" {
		t.Errorf("garbage cursor: %v", err)
	}
	tl, _ := ReadRows(ctx, RowsSource{Harness: "claude", Path: writeTranscript(t, jsonl(t, userRec("x", "2026-10-01T00:00:00Z", "x", nil)))}, RowsOptions{Tail: 1})
	if _, err := ReadChainRows(ctx, "claude", segs, RowsOptions{Before: tl.BeforeCursor, Limit: 2}); !errors.As(err, &re) || re.Reason != "source_moved" {
		t.Errorf("cursor from a file outside the chain: %v", err)
	}
}
