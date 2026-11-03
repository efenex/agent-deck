package ui

// Per-session accent: the `color` field (agent-deck session set <id> color X)
// used to tint only the title of an unselected row. These tests pin the wider
// contract: a coloured marker on the row, the accent surviving row selection,
// and the preview pane's title/border/divider taking the selected session's
// accent. With no colour set, nothing changes.

import (
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	accentFG = "38;2;255;0;0" // truecolor foreground for #FF0000
	accentBG = "48;2;255;0;0" // truecolor background for #FF0000
)

func accentRow(t *testing.T, color string, selected bool) string {
	t.Helper()
	forceTrueColorProfile()
	inst := &session.Instance{ID: "acc-1", Title: "tower", Tool: "claude", Color: color}
	h := &Home{width: 140}
	item := session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, Path: "test", IsLastInGroup: true}
	snap := map[string]sessionRenderState{inst.ID: {status: session.StatusRunning, tool: "claude"}}
	var b strings.Builder
	h.renderSessionItem(&b, item, selected, snap, h.width)
	return b.String()
}

func TestSessionAccent_UnselectedRowHasMarker(t *testing.T) {
	row := accentRow(t, "#FF0000", false)
	if !strings.Contains(row, sessionAccentMarker) {
		t.Fatalf("accent row missing marker %q: %q", sessionAccentMarker, row)
	}
	if !strings.Contains(row, accentFG) {
		t.Fatalf("accent row missing red foreground: %q", row)
	}
}

func TestSessionAccent_NoColorRowUnchanged(t *testing.T) {
	for _, selected := range []bool{false, true} {
		row := accentRow(t, "", selected)
		if strings.Contains(row, sessionAccentMarker) {
			t.Fatalf("selected=%v: no-accent row must not carry marker: %q", selected, row)
		}
		if strings.Contains(row, accentBG) || strings.Contains(row, accentFG) {
			t.Fatalf("selected=%v: no-accent row picked up accent escapes: %q", selected, row)
		}
	}
}

func TestSessionAccent_SelectedRowKeepsAccent(t *testing.T) {
	row := accentRow(t, "#FF0000", true)
	if !strings.Contains(row, accentBG) {
		t.Fatalf("selected accent row lost accent highlight (want %q): %q", accentBG, row)
	}
	if !strings.Contains(row, accentFG) {
		t.Fatalf("selected accent row lost accent arrow: %q", row)
	}
}

func TestSessionAccent_RowWidthUnchanged(t *testing.T) {
	for _, selected := range []bool{false, true} {
		plain := ansi.StringWidth(accentRow(t, "", selected))
		tinted := ansi.StringWidth(accentRow(t, "#FF0000", selected))
		if plain != tinted {
			t.Fatalf("selected=%v: accent changed row width %d -> %d", selected, plain, tinted)
		}
	}
}

func TestSessionAccent_PreviewAccentFollowsCursor(t *testing.T) {
	red := &session.Instance{ID: "r", Title: "red", Color: "#FF0000"}
	plain := &session.Instance{ID: "p", Title: "plain"}
	h := &Home{flatItems: []session.Item{
		{Type: session.ItemTypeGroup},
		{Type: session.ItemTypeSession, Session: red},
		{Type: session.ItemTypeSession, Session: plain},
	}}
	cases := []struct {
		cursor int
		want   lipgloss.Color
	}{{0, ""}, {1, lipgloss.Color("#FF0000")}, {2, ""}, {9, ""}, {-1, ""}}
	for _, c := range cases {
		h.cursor = c.cursor
		if got := h.previewAccent(); got != c.want {
			t.Errorf("cursor %d: previewAccent = %q, want %q", c.cursor, got, c.want)
		}
	}
}

func TestSessionAccent_PanelTitle(t *testing.T) {
	forceTrueColorProfile()
	h := &Home{}
	def := h.renderPanelTitle("PREVIEW", 20)
	if got := h.renderPanelTitleAccent("PREVIEW", 20, ""); got != def {
		t.Fatalf("empty accent must equal the default title:\n got %q\nwant %q", got, def)
	}
	acc := h.renderPanelTitleAccent("PREVIEW", 20, lipgloss.Color("#FF0000"))
	if !strings.Contains(acc, accentFG) {
		t.Fatalf("accent title missing red: %q", acc)
	}
	if ansi.StringWidth(acc) != ansi.StringWidth(def) {
		t.Fatalf("accent changed title width")
	}
}
