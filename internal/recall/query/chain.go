package query

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ChainSchema names the `recall chain` result.
const ChainSchema = "agent-deck.recall.chain/v1"

// ChainSegment is one native conversation of a deck session. `/clear` and
// `/new` start a new conversation id under the same deck session; the
// session_links table keeps all of them. Segments run oldest first.
type ChainSegment struct {
	Seq           int    `json:"seq"`
	NativeID      string `json:"native_id"`
	Harness       string `json:"harness"`
	FirstSeen     int64  `json:"first_seen,omitempty"`
	LastSeen      int64  `json:"last_seen,omitempty"`
	FirstSeenAt   string `json:"first_seen_at,omitempty"`
	LastSeenAt    string `json:"last_seen_at,omitempty"`
	Current       bool   `json:"current"`
	Authoritative bool   `json:"authoritative"`
	// Path is the native transcript on this host; empty when it is not here.
	Path string `json:"path,omitempty"`
}

// ChainSession names the deck session a chain belongs to.
type ChainSession struct {
	ID      string `json:"id"`
	Title   string `json:"title,omitempty"`
	Harness string `json:"harness"`
}

// Chain is the `recall chain --json` result.
type Chain struct {
	Schema   string         `json:"schema"`
	Session  ChainSession   `json:"session"`
	Current  string         `json:"current,omitempty"`
	Segments []ChainSegment `json:"segments"`
}

// SetTimes fills the RFC3339 twins of the unix second fields.
func (s *ChainSegment) SetTimes() {
	s.FirstSeenAt, s.LastSeenAt = unixRFC3339(s.FirstSeen), unixRFC3339(s.LastSeen)
}

func unixRFC3339(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

const chainCursorPrefix = "seg:"

const chainBoundaryRaw = "chain/boundary"

// ChainBoundaryRow separates two segments of a chain timeline. It uses the
// system kind so a client that does not know chains still shows a line.
func ChainBoundaryRow(prev, next ChainSegment) Row {
	return Row{
		ID: "boundary:" + next.NativeID, Kind: "system", TS: unixRFC3339(next.FirstSeen), Title: "Conversation cleared",
		Seg: next.NativeID, RawType: chainBoundaryRaw,
		Meta: map[string]any{"boundary": "clear", "seg": next.NativeID, "prev_seg": prev.NativeID},
	}
}

// ReadChainRows reads a rows page across the segments of a /clear chain.
//
//   - No cursor: the last opts.Tail rows of the chain (all rows when Tail is
//     0), filling from the newest segment backwards.
//   - opts.Before: up to opts.Limit older rows. The cursor is either a row
//     cursor from an earlier page (it names its segment's file) or
//     "seg:<native id>", which starts at the tail of that segment. A page
//     that uses one segment up carries on into the previous one, so a
//     client only ever sees a row cursor, and an empty one at chain start.
//
// Segments without a transcript on this host (empty Path) are skipped. Every
// row carries Seg. A boundary row precedes the first row of each segment
// after the oldest. BeforeCursor is empty at the start of the chain.
func ReadChainRows(ctx context.Context, harness string, segs []ChainSegment, opts RowsOptions) (RowsTimeline, error) {
	var tl RowsTimeline
	if opts.Since != "" || opts.AgentID != "" {
		return tl, errors.New("recall rows: --chain cannot combine with --since or --agent")
	}
	if opts.Before == "" && opts.Limit > 0 && opts.Tail == 0 {
		return tl, errors.New("recall rows: --chain pages with --tail N, then --before C --limit N")
	}
	if err := ValidateRowsOptions(opts); err != nil {
		return tl, err
	}
	var here []ChainSegment
	for _, s := range segs {
		if s.Path != "" {
			here = append(here, s)
		}
	}
	if len(here) == 0 {
		return tl, fmt.Errorf("recall rows: no segment of this chain has a transcript on this machine: %w", os.ErrNotExist)
	}
	read := func(s ChainSegment, o RowsOptions) (RowsTimeline, error) {
		return ReadRows(ctx, RowsSource{Harness: harness, Path: s.Path}, o)
	}
	need, k := opts.Tail, len(here)-1
	first := RowsOptions{Tail: opts.Tail}
	if need == 0 {
		need = 1 << 30
		first.Tail = need
	}
	switch {
	case strings.HasPrefix(opts.Before, chainCursorPrefix):
		id := strings.TrimPrefix(opts.Before, chainCursorPrefix)
		k = -1
		for i, s := range here {
			if s.NativeID == id {
				k = i
			}
		}
		if k < 0 {
			return tl, &ResyncError{"unknown_segment"}
		}
		need, first = opts.Limit, RowsOptions{Tail: opts.Limit}
	case opts.Before != "":
		c, err := decodeRowsCursor(opts.Before)
		if err != nil {
			return tl, &ResyncError{"invalid_cursor"}
		}
		k = -1
		for i, s := range here {
			if s.Path == c.P {
				k = i
			}
		}
		if k < 0 {
			return tl, &ResyncError{"source_moved"}
		}
		need, first = opts.Limit, RowsOptions{Before: opts.Before, Limit: opts.Limit}
	}

	var pages [][]Row // newest segment first; reversed on output
	exhausted := false
	var oldestPage RowsTimeline
	for i, o := k, first; ; {
		page, err := read(here[i], o)
		if err != nil {
			return tl, err
		}
		if i == k {
			tl.ThroughCursor = page.ThroughCursor
		}
		rows := page.Turns
		for j := range rows {
			rows[j].Seg = here[i].NativeID
		}
		asked := o.Tail
		if o.Before != "" {
			asked = o.Limit
		}
		exhausted = len(rows) < asked
		if exhausted && i > 0 {
			rows = append([]Row{ChainBoundaryRow(here[i-1], here[i])}, rows...)
		}
		pages = append(pages, rows)
		oldestPage = page
		need -= len(page.Turns)
		if !exhausted || need <= 0 || i == 0 {
			k = i
			break
		}
		i--
		o = RowsOptions{Tail: need}
	}
	for i := len(pages) - 1; i >= 0; i-- {
		tl.Turns = append(tl.Turns, pages[i]...)
	}
	if !exhausted {
		tl.BeforeCursor = oldestPage.BeforeCursor
	}
	return tl, nil
}
