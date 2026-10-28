package main

import (
	"errors"
	"fmt"
	"sort"

	"github.com/asheshgoplani/agent-deck/internal/recall/query"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// buildChain lists the native conversations a deck session has been, oldest
// first, from its session_links rows. `/clear` and `/new` start a new
// conversation id under the same deck session. The live conversation always
// comes last, even when a resume moved it back to an older id. A session
// whose links were never recorded still answers with its current id.
func buildChain(inst *session.Instance, peers []*session.Instance, links []statedb.SessionLink) query.Chain {
	h := rowsHarness(inst.Tool)
	current := firstNonEmpty(inst.ClaudeSessionID, inst.CodexSessionID)
	chain := query.Chain{Schema: query.ChainSchema, Session: query.ChainSession{ID: inst.ID, Title: inst.Title, Harness: h}, Segments: []query.ChainSegment{}}
	var segs []query.ChainSegment
	seen := map[string]bool{}
	for _, l := range links {
		if l.Harness != h || seen[l.NativeID] {
			continue
		}
		seen[l.NativeID] = true
		segs = append(segs, query.ChainSegment{NativeID: l.NativeID, Harness: h, FirstSeen: l.FirstSeen, LastSeen: l.LastSeen, Authoritative: l.Authoritative})
	}
	if current != "" && !seen[current] {
		segs = append(segs, query.ChainSegment{NativeID: current, Harness: h, Authoritative: true})
	}
	if current == "" {
		// No live id recorded: the authoritative link, if any, is current.
		for _, s := range segs {
			if s.Authoritative {
				current = s.NativeID
			}
		}
	}
	sort.SliceStable(segs, func(i, j int) bool {
		a, b := segs[i], segs[j]
		if (a.NativeID == current) != (b.NativeID == current) {
			return b.NativeID == current
		}
		if a.FirstSeen != b.FirstSeen {
			return a.FirstSeen < b.FirstSeen
		}
		if a.LastSeen != b.LastSeen {
			return a.LastSeen < b.LastSeen
		}
		return a.NativeID < b.NativeID
	})
	for i := range segs {
		s := &segs[i]
		s.Seq, s.Current = i, s.NativeID == current && current != ""
		s.SetTimes()
		if s.Current {
			if p, err := liveTranscriptPath(inst, peers); err == nil {
				s.Path = p
			}
		}
		if s.Path == "" {
			if _, p := nativeTranscriptByID(s.NativeID); p != "" {
				s.Path = p
			}
		}
	}
	chain.Current, chain.Segments = current, append(chain.Segments, segs...)
	return chain
}

// resolveChain finds the deck session behind <session> and builds its chain.
// An old conversation id (one a /clear left behind) resolves through
// session_links to the session that held it. It never opens the recall
// index, so it works with [recall] off.
func resolveChain(profile, ref string) (query.Chain, *session.Instance, *session.Storage, error) {
	storage, instances, _, err := loadSessionData(profile)
	if err != nil {
		return query.Chain{}, nil, nil, err
	}
	chain, inst, err := chainOf(storage, instances, ref)
	if err != nil {
		storage.Close()
		return query.Chain{}, nil, nil, err
	}
	return chain, inst, storage, nil
}

func chainOf(storage *session.Storage, instances []*session.Instance, ref string) (query.Chain, *session.Instance, error) {
	inst := lookupRowsInstance(ref, instances)
	db := storage.GetDB()
	if inst == nil && db != nil {
		for _, h := range []string{"claude", "codex"} {
			ids, _ := db.SessionIDsByNativeID(h, ref)
			if len(ids) == 1 {
				for _, i := range instances {
					if i.ID == ids[0] {
						inst = i
					}
				}
			}
		}
	}
	if inst == nil {
		return query.Chain{}, nil, fmt.Errorf("no session or conversation matches %q", ref)
	}
	if h := rowsHarness(inst.Tool); !query.SupportsDirectRows(h) {
		return query.Chain{}, nil, fmt.Errorf("%w: %s", query.ErrRowsUnsupported, inst.Tool)
	}
	var links []statedb.SessionLink
	if db != nil {
		var err error
		if links, err = db.ListSessionLinks(inst.ID); err != nil {
			return query.Chain{}, nil, err
		}
	}
	return buildChain(inst, instances, links), inst, nil
}

func handleRecallChain(profile string, args []string) {
	fs := newRecallFlagSet("recall chain")
	jsonOutput := fs.Bool("json", false, "Output canonical JSON")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: agent-deck recall chain <session> --json")
		fmt.Fprintln(fs.Output(), "The native conversations a deck session has been, oldest first: /clear and /new start a new one under the same session.")
		fmt.Fprintln(fs.Output(), "<session>: deck session id, title or id prefix, or a conversation id the session ever held. Works with [recall] off.")
		fmt.Fprintln(fs.Output(), "Output: {schema, session, current, segments[]}; each segment has native_id, first_seen/last_seen (unix and RFC3339), current, authoritative and path (only when the transcript is on this host). The live conversation is last. Read-only; reads state.db only.")
		fmt.Fprintln(fs.Output(), "Page rows across the chain with: agent-deck recall timeline <session> --json --chain --tail N")
		fs.PrintDefaults()
	}
	if !parseRecallFlags(fs, args) {
		return
	}
	out := NewCLIOutput(*jsonOutput, false)
	if !*jsonOutput || fs.NArg() != 1 {
		fs.Usage()
		exitCLI(2)
	}
	chain, _, storage, err := resolveChain(profile, fs.Arg(0))
	if err == nil {
		storage.Close()
	}
	if err != nil {
		code := ErrCodeNotFound
		if errors.Is(err, query.ErrRowsUnsupported) {
			code = ErrCodeInvalidOperation
		}
		out.Error(err.Error(), code)
		exitCLI(1)
	}
	out.printJSON(chain)
}
