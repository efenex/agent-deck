package query

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/recall/reader"
)

// LandedScanBytes bounds both late-proof scans and failed-reopen presence
// checks. Incomplete coverage is never evidence that content is absent.
const LandedScanBytes int64 = 4 << 20

// LandedWindow restricts a scan to [From, To) and a timestamp interval. A
// matching TranscriptOffset extends coverage past MaxBytes: when [From, To)
// exceeds the cap, the scan starts at the row straddling the offset instead.
// To < 0 means the current EOF. MaxBytes <= 0 uses LandedScanBytes.
// PeerEnvelope also accepts a Claude peer envelope around the text. Set it
// for a send delivered over Claude's socket, or when any copy of the text
// counts (a presence check); never as proof for a send typed into the pane.
type LandedWindow struct {
	From, To            int64
	NotBefore, NotAfter time.Time
	MaxBytes            int64
	PeerEnvelope        bool
	// TranscriptOffset proves byte coverage from the send even when the full
	// file exceeds MaxBytes. It must match the current path and file identity.
	TranscriptOffset *reader.TranscriptOffset
}

// LandedCandidate contains no transcript content.
type LandedCandidate struct {
	ID string
	TS string
}

// LandedScan distinguishes absence from incomplete coverage. Present also
// includes matching input still queued in the receiver: reopening that would
// duplicate it. Several Candidates establish presence, not unique attribution.
type LandedScan struct {
	Candidates []LandedCandidate
	Present    bool
	Pending    bool
	Complete   bool
	Reason     string
}

// ScanLanded uses the existing native row parser and full-message matcher,
// but never stops at the first match. It tracks queue removal/absorption and
// checks the entire bounded window before claiming a unique landed row.
func ScanLanded(ctx context.Context, harness, path, text string, w LandedWindow) LandedScan {
	unknown := func(reason string) LandedScan { return LandedScan{Reason: reason} }
	if !SupportsDirectRows(harness) {
		return unknown("unsupported")
	}
	want := normalizeLanded(text)
	if want == "" {
		return unknown("empty_content")
	}
	// An offset taken in another file (a restarted native conversation)
	// says nothing about this one, which keeps the legacy rules.
	if w.TranscriptOffset != nil && w.TranscriptOffset.Path != path {
		w.TranscriptOffset = nil
	}
	f, err := os.Open(path)
	if err != nil {
		if w.TranscriptOffset != nil {
			return unknown("scan_limit")
		}
		return unknown("source_unreadable")
	}
	defer f.Close()
	info, err := f.Stat()
	if w.TranscriptOffset != nil && (err != nil || !w.TranscriptOffset.Matches(f, path, info)) {
		return unknown("scan_limit")
	}
	if err != nil || !info.Mode().IsRegular() || w.From < 0 || info.Size() < w.From {
		return unknown("source_shortened")
	}
	to := w.To
	if to < 0 {
		to = info.Size()
	}
	if to > info.Size() || to < w.From {
		return unknown("source_shortened")
	}
	limit := w.MaxBytes
	if limit <= 0 {
		limit = LandedScanBytes
	}
	start := w.From
	result := LandedScan{Complete: true}
	if offset := w.TranscriptOffset; offset != nil {
		// Never infer byte coverage from timestamps: native rows can be
		// written out of timestamp order. Count the cap from the saved EOF;
		// only the fixed backstep may read bytes before it.
		if offset.Offset < w.From || offset.Offset > to {
			return unknown("scan_limit")
		}
		offsetStart, ok := landedOffsetStart(f, offset.Offset)
		if to-w.From > limit {
			if !ok || to-offset.Offset > limit {
				return unknown("scan_limit")
			}
			// Keep the legacy tail as well: a row of an earlier attempt can
			// sit before this attempt's offset, and the legacy tail scan saw
			// it. Proof scans start at the offset (From), so they gain nothing.
			start = max(w.From, tailStart(f, to, limit))
		}
		// The offset never narrows the legacy window; it only adds the row
		// straddling it and, over the cap, completes the coverage.
		if ok && offsetStart < start {
			start = offsetStart
		}
	} else if to-start > limit {
		start = tailStart(f, to, limit)
		result.Complete, result.Reason = false, "scan_limit"
	}
	// A saved offset must be a line boundary. Starting mid-record could hide
	// queued copies or reinterpret a replaced/truncated source as fresh input.
	if start > 0 {
		var b [1]byte
		if _, err := f.ReadAt(b[:], start-1); err != nil || b[0] != '\n' {
			return unknown("source_boundary")
		}
	}
	fresh := func(ts string) bool {
		t, err := time.Parse(time.RFC3339Nano, ts)
		return err == nil && (w.NotBefore.IsZero() || !t.Before(w.NotBefore.Add(-landedClockSkew))) &&
			(w.NotAfter.IsZero() || !t.After(w.NotAfter))
	}
	// Store only matches, never transcript bodies. Queue rows can be removed
	// or replaced by a native user row later in this same scan.
	matches := make(map[string]LandedCandidate)
	pending := make(map[string]bool)
	p := newRowParser(harness, rowParserState{})
	end, err := scanLines(ctx, f, start, to, func(line []byte, _ int64) error {
		if !json.Valid(line) {
			result.Complete, result.Reason = false, "source_malformed"
			return nil
		}
		for _, fr := range p.line(line) {
			if fr.Row != nil && fr.Frame == "row" && landedMatchPeer(fr.Row, want, w.PeerEnvelope) {
				if _, err := time.Parse(time.RFC3339Nano, fr.Row.TS); err != nil {
					result.Complete, result.Reason = false, "source_timestamp"
				}
			}
			switch {
			case fr.Frame == "remove":
				delete(matches, fr.ID)
				delete(pending, fr.ID)
			case fr.Row == nil:
			case fr.Frame == "update" && pending[fr.Row.ID] && fr.Row.Queued != nil && !*fr.Row.Queued:
				delete(pending, fr.Row.ID)
				if fresh(fr.Row.TS) {
					matches[fr.Row.ID] = LandedCandidate{ID: fr.Row.ID, TS: fr.Row.TS}
				}
			case fr.Frame != "row" || !landedMatchPeer(fr.Row, want, w.PeerEnvelope):
			case fresh(fr.Row.TS):
				if fr.Row.Queued != nil && *fr.Row.Queued {
					pending[fr.Row.ID] = true
				} else {
					matches[fr.Row.ID] = LandedCandidate{ID: fr.Row.ID, TS: fr.Row.TS}
				}
			}
		}
		return nil
	})
	if err != nil || end != to {
		result.Complete, result.Reason = false, "source_incomplete"
	}
	// Appending is allowed; replacement or truncation while scanning is not.
	if current, err := os.Stat(path); err != nil || !os.SameFile(info, current) || current.Size() < to {
		result.Complete, result.Reason = false, "source_changed"
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Complete, result.Reason = false, "scan_timeout"
	}
	for _, candidate := range matches {
		result.Candidates = append(result.Candidates, candidate)
	}
	result.Pending = len(pending) > 0
	result.Present = len(matches) > 0 || result.Pending
	return result
}

// landedOffsetStart includes the row straddling EOF at send time. A row
// completed before the capture was written before anything was typed, so it is
// never this send's row and stays outside the window (a proof scan must not
// claim it). Never skip a partial row: if its start is outside this bounded
// backstep, fail closed.
func landedOffsetStart(f *os.File, offset int64) (int64, bool) {
	const backstep int64 = 64 << 10
	start := max(int64(0), offset-backstep)
	b := make([]byte, offset-start)
	if len(b) == 0 {
		return 0, true
	}
	if _, err := f.ReadAt(b, start); err != nil {
		return 0, false
	}
	if b[len(b)-1] == '\n' {
		return offset, true
	}
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		return start + int64(i) + 1, true
	}
	return 0, start == 0
}
