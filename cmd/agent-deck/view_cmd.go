package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/ui"
)

// viewJSON is the `agent-deck view --json` payload: the TUI's persisted
// session-list filter, resolved so a client can apply it to `list --json`
// (status, archived, last_activity_at) without knowing the TUI's rules.
type viewJSON struct {
	StatusFilter string `json:"status_filter"`
	// Statuses admitted by the status filter; null means every status.
	Statuses        []string `json:"statuses"`
	Archived        bool     `json:"archived"`
	TimeFilter      string   `json:"time_filter"`
	TimeFilterLabel string   `json:"time_filter_label"`
	// ActiveSince is the RFC 3339 lower bound on last_activity_at; omitted
	// when the time filter is "all".
	ActiveSince string `json:"active_since,omitempty"`
}

// timeFilterSlugs are the stable JSON names of session.TimeFilterMode values.
var timeFilterSlugs = map[session.TimeFilterMode]string{
	session.TimeFilterAll:    "all",
	session.TimeFilterToday:  "today",
	session.TimeFilter3Days:  "3d",
	session.TimeFilter7Days:  "7d",
	session.TimeFilter30Days: "30d",
}

func handleView(profile string, args []string) {
	fs := flag.NewFlagSet("view", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	fs.Usage = func() {
		fmt.Println("Usage: agent-deck view [--json]")
		fmt.Println()
		fmt.Println("Show the session-list filter the TUI last saved for this profile: the")
		fmt.Println("status filter (! @ # $ % and archived) and the time range (*). The")
		fmt.Println("statuses and active_since fields apply it to `list --json` output.")
		fmt.Println()
		fmt.Println("Options:")
		fs.PrintDefaults()
	}
	if err := parseCLIFlags(fs, normalizeArgs(fs, args)); err != nil {
		exitCLI(1)
	}
	out := NewCLIOutput(*jsonOutput, false)

	var display session.DisplaySettings
	if cfg, err := session.LoadUserConfig(); err == nil && cfg != nil {
		display = cfg.Display
	}
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		exitCLI(1)
	}
	defer storage.Close()
	f, err := ui.LoadSavedViewFilter(storage.GetDB(), display)
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		exitCLI(1)
	}

	v := viewJSON{
		StatusFilter:    f.StatusFilter,
		Archived:        f.Archived,
		TimeFilter:      timeFilterSlugs[f.TimeFilter],
		TimeFilterLabel: f.TimeFilter.Label(),
	}
	if f.Statuses != nil {
		v.Statuses = make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			v.Statuses[i] = string(st)
		}
	}
	if since, ok := f.Since(time.Now()); ok {
		v.ActiveSince = since.Format(time.RFC3339)
	}

	status := "all"
	switch {
	case f.Archived:
		status = "archived"
	case v.Statuses != nil:
		status = fmt.Sprintf("%s (%s)", f.StatusFilter, strings.Join(v.Statuses, ", "))
	}
	human := fmt.Sprintf("Status: %s\nTime:   %s", status, v.TimeFilterLabel)
	if v.ActiveSince != "" {
		human += " (since " + v.ActiveSince + ")"
	}
	out.Print(human+"\n", v)
}
