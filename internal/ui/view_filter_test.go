package ui

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func TestLoadSavedViewFilter(t *testing.T) {
	S := func(ss ...session.Status) []session.Status { return ss }
	open := S(session.StatusRunning, session.StatusWaiting, session.StatusIdle, session.StatusStarting, session.StatusQueued)
	cases := []struct {
		name     string
		uiState  string
		display  session.DisplaySettings
		statuses []session.Status
		archived bool
		time     session.TimeFilterMode
	}{
		{name: "never saved", statuses: nil},
		{name: "all", uiState: `{"preview_mode":0}`, statuses: nil},
		{name: "open today", uiState: `{"status_filter":"active","time_filter_mode":1}`, statuses: open, time: session.TimeFilterToday},
		{name: "open keeps stopped when configured", uiState: `{"status_filter":"active"}`,
			display:  session.DisplaySettings{ActiveFilterExcludes: []string{"error"}},
			statuses: append(append([]session.Status{}, open...), session.StatusStopped)},
		{name: "open second press hides stopped", uiState: `{"status_filter":"active","active_filter_hide_stopped":true}`,
			display: session.DisplaySettings{ActiveFilterExcludes: []string{"error"}}, statuses: open},
		{name: "idle bucket", uiState: `{"status_filter":"idle"}`, statuses: S(session.StatusIdle, session.StatusStarting, session.StatusQueued)},
		{name: "archived", uiState: `{"status_filter":"archived"}`, statuses: []session.Status{}, archived: true},
		{name: "default_filter fallback", display: session.DisplaySettings{DefaultFilter: "active"}, statuses: open},
		{name: "out-of-range time", uiState: `{"time_filter_mode":99}`, statuses: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := statedb.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.Migrate(); err != nil {
				t.Fatal(err)
			}
			if tc.uiState != "" {
				if err := db.SetMeta("ui_state", tc.uiState); err != nil {
					t.Fatal(err)
				}
			}
			f, err := LoadSavedViewFilter(db, tc.display)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f.Statuses, tc.statuses) || f.Archived != tc.archived || f.TimeFilter != tc.time {
				t.Fatalf("got statuses=%v archived=%v time=%v; want %v %v %v", f.Statuses, f.Archived, f.TimeFilter, tc.statuses, tc.archived, tc.time)
			}
		})
	}
}
