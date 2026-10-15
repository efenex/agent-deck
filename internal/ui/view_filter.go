package ui

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// SavedViewFilter is the session-list filter the TUI last persisted for a
// profile (ui_state in state.db), resolved against the user's display config.
// It lets a client outside the TUI (`agent-deck view --json`, a remote web
// deck) show the same sessions the TUI does without re-deriving the rules.
type SavedViewFilter struct {
	// StatusFilter is the raw filter: "" (all), a status, "active" (%) or
	// "archived".
	StatusFilter string
	// Statuses lists every session status the filter admits; nil means no
	// status restriction. Empty for the archived view, which selects on the
	// archived flag instead of status.
	Statuses []session.Status
	// Archived is true when the TUI is browsing the archived list.
	Archived bool
	// TimeFilter is the recency filter (cycled by '*').
	TimeFilter session.TimeFilterMode
}

// viewFilterStatuses is every status a session can report, in display order.
var viewFilterStatuses = []session.Status{
	session.StatusRunning, session.StatusWaiting, session.StatusIdle,
	session.StatusStarting, session.StatusQueued, session.StatusError,
	session.StatusStopped,
}

// LoadSavedViewFilter reads the persisted view filter from db. A profile the
// TUI never saved state for yields the config's default_filter, as at TUI
// startup.
func LoadSavedViewFilter(db *statedb.StateDB, display session.DisplaySettings) (SavedViewFilter, error) {
	var state uiState
	if db != nil {
		val, err := db.GetMeta("ui_state")
		if err != nil {
			return SavedViewFilter{}, fmt.Errorf("read ui_state: %w", err)
		}
		if val != "" {
			if err := json.Unmarshal([]byte(val), &state); err != nil {
				return SavedViewFilter{}, fmt.Errorf("decode ui_state: %w", err)
			}
		}
	}

	// Mirror loadUIState/applyUIState and the default_filter fallback so the
	// predicate below is the TUI's own matchesStatusFilter.
	h := &Home{activeFilterExcludes: display.GetActiveFilterExcludes()}
	h.statusFilter = session.Status(state.StatusFilter)
	h.activeFilterHideStopped = state.StatusFilter == string(FilterModeActive) && state.ActiveFilterHideStopped
	if h.statusFilter == "" {
		h.statusFilter = session.Status(display.GetDefaultFilter())
	}

	f := SavedViewFilter{StatusFilter: string(h.statusFilter), TimeFilter: session.TimeFilterMode(state.TimeFilterMode)}
	if f.TimeFilter < session.TimeFilterAll || f.TimeFilter >= session.TimeFilterModeCount {
		f.TimeFilter = session.TimeFilterAll
	}
	switch h.statusFilter {
	case "":
	case FilterModeArchived:
		f.Archived = true
		f.Statuses = []session.Status{}
	default:
		f.Statuses = []session.Status{}
		for _, st := range viewFilterStatuses {
			if h.matchesStatusFilter(h.statusFilter, st) {
				f.Statuses = append(f.Statuses, st)
			}
		}
	}
	return f, nil
}

// Since is the earliest last-activity time the recency filter admits at now;
// ok is false when it has no lower bound.
func (f SavedViewFilter) Since(now time.Time) (time.Time, bool) {
	return f.TimeFilter.Since(now)
}
