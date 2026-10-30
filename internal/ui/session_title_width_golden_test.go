package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// TestSessionRowWidthBudget_Golden pins the session-list row width budget
// (#2201) at 80 and 60 terminal columns, using the same pane-width math the
// real TUI uses (sessionsPaneWidth) so the frames match what a user sees.
// Before the fix, a long title collapsed to a bare "…" at these widths while
// the account badge stayed fully visible; now the title keeps at least
// minSessionTitleWidth columns and the badge shrinks or drops instead.
// Regenerate with:
//
//	UPDATE_GOLDEN=1 go test ./internal/ui/ -run TestSessionRowWidthBudget_Golden
func TestSessionRowWidthBudget_Golden(t *testing.T) {
	steps := []struct {
		name          string
		terminalWidth int
		account       string
		fork          bool
	}{
		{"01-term80-inherited", 80, "", false},
		{"02-term60-inherited", 60, "", false},
		{"03-term80-named", 80, "personal", false},
		{"04-term60-named", 60, "personal", false},
		{"05-term80-fork", 80, "", true},
		{"06-term120-fork", 120, "", true},
		{"07-term200-fork", 200, "", true},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			h := NewHome()
			h.width, h.height = step.terminalWidth, 40
			listWidth := h.sessionsPaneWidth()
			inst := &session.Instance{
				ID:      "width",
				Title:   "a genuinely long session title that would elide the account suffix first",
				Tool:    "claude",
				Status:  session.StatusIdle,
				Account: step.account,
			}
			if step.fork {
				inst.WorktreePath = "/tmp/fork"
				inst.WorktreeBranch = "fork/claude-many-steps"
			}
			state := sessionRenderState{
				status:         session.StatusIdle,
				tool:           "claude",
				title:          inst.Title,
				account:        step.account,
				accountDisplay: newAccountPresentation(step.account, true),
			}
			var b strings.Builder
			h.renderSessionItem(&b, session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, Path: "work", IsLastInGroup: true}, false, map[string]sessionRenderState{inst.ID: state}, listWidth)
			got := fmt.Sprintf("terminal_width=%d list_width=%d\n%s", step.terminalWidth, listWidth, strings.TrimSuffix(stripAnsi(b.String()), "\n")+"\n")
			path := filepath.Join("testdata", "session_row_width_budget", step.name+".txt")
			if os.Getenv("UPDATE_GOLDEN") != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v (UPDATE_GOLDEN=1 to create)", path, err)
			}
			if string(want) != got {
				t.Fatalf("golden %s differs from the rendered row.\n--- want\n%s\n--- got\n%s", path, want, got)
			}
		})
	}
}

// The 40-column threshold used to restore the full branch badge and tool,
// consuming the title, especially with a timestamp or child indentation.
func TestSessionRowTitleFloorBoundaries(t *testing.T) {
	for _, width := range []int{39, 40, 42} {
		for _, timestamp := range []bool{false, true} {
			for _, nested := range []bool{false, true} {
				name := fmt.Sprintf("width%d-time%t-nested%t", width, timestamp, nested)
				t.Run(name, func(t *testing.T) {
					h := NewHome()
					h.showSessionTimestamps = timestamp
					for _, title := range []string{"luna-p678", "luna-p678-long-title"} {
						inst := &session.Instance{ID: "title-floor", Title: title, Tool: "claude", Status: session.StatusWaiting,
							WorktreePath: "/tmp/worktree", WorktreeBranch: "feature/luna/p678-prep-1030", CreatedAt: time.Now().Add(-2 * time.Hour)}
						item := session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, Path: "work", IsLastInGroup: true}
						if nested {
							item.Level = 2
							item.IsSubSession = true
							item.IsLastSubSession = true
							item.ParentIsLastInGroup = true
						}
						state := sessionRenderState{status: inst.Status, tool: inst.Tool, title: title}
						var b strings.Builder
						h.renderSessionItem(&b, item, false, map[string]sessionRenderState{inst.ID: state}, width)
						row := strings.TrimSuffix(stripAnsi(b.String()), "\n")
						// Independent readable-name oracle; do not bless a golden hiding it.
						wantTitle := title
						if title != "luna-p678" {
							wantTitle = "luna-p678-l"
						}
						if !strings.Contains(row, wantTitle) {
							t.Errorf("title floor missing: want %q in %q", wantTitle, row)
						}
						if cellWidth(row) > width {
							t.Errorf("row width %d exceeds %d: %q", cellWidth(row), width, row)
						}
						if title == "luna-p678" {
							continue
						}
						path := filepath.Join("testdata", "session_row_width_budget", name+".txt")
						got := row + "\n"
						if os.Getenv("UPDATE_GOLDEN") != "" {
							if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
								t.Fatal(err)
							}
						} else {
							want, err := os.ReadFile(path)
							if err != nil {
								t.Fatal(err)
							}
							if string(want) != got {
								t.Errorf("golden mismatch:\nwant %s\ngot %s", want, got)
							}
						}
					}
				})
			}
		}
	}
}

func TestSessionRowTitleFloorOptionalBadges(t *testing.T) {
	for _, tc := range []struct {
		width             int
		timestamp, nested bool
	}{
		{42, true, true}, {120, true, true}, {120, false, true}, {120, true, false}, {120, false, false},
	} {
		width := tc.width
		for _, selected := range []bool{false, true} {
			t.Run(fmt.Sprintf("width%d-selected%t-time%t-nested%t", width, selected, tc.timestamp, tc.nested), func(t *testing.T) {
				h := NewHome()
				h.showSessionTimestamps = tc.timestamp
				inst := &session.Instance{ID: "crowded", Title: "luna-p678", Tool: "claude", Status: session.StatusWaiting,
					WorktreePath: "/tmp/worktree", WorktreeBranch: "feature/luna/p678-prep-1030",
					Sandbox: &session.SandboxConfig{Enabled: true}, SSHHost: "worker@very-long-hostname",
					CreatedAt: time.Now().Add(-2 * time.Hour)}
				state := sessionRenderState{status: inst.Status, tool: inst.Tool, title: inst.Title,
					accountDisplay: newAccountPresentation("personal", true)}
				item := session.Item{Type: session.ItemTypeSession, Session: inst, Level: 2,
					IsSubSession: true, IsLastSubSession: true, ParentIsLastInGroup: true}
				if !tc.nested {
					item.Level = 1
					item.IsSubSession = false
				}
				var b strings.Builder
				h.renderSessionItem(&b, item, selected, map[string]sessionRenderState{inst.ID: state}, width)
				row := strings.TrimSuffix(stripAnsi(b.String()), "\n")
				if !strings.Contains(row, inst.Title) || cellWidth(row) > width {
					t.Fatalf("name or row width lost at %d: %q", width, row)
				}
				if width == 120 && strings.Contains(row, "2h ago") != tc.timestamp {
					t.Errorf("wide row timestamp enabled=%v: %q", tc.timestamp, row)
				}
				if width == 120 {
					for _, badge := range []string{"[sandbox]", "[ssh:", `[account:"personal"]`} {
						if !strings.Contains(row, badge) {
							t.Errorf("wide row lost %q: %q", badge, row)
						}
					}
				}
			})
		}
	}
}

// Short names must not pay for unused title-floor cells. Critical mode and
// supervisor indicators retain their pre-title-floor priority over the title.
func TestSessionRowTitleFloorReviewRegressions(t *testing.T) {
	for _, tc := range []struct {
		name, title, tool, branch, want string
		width                           int
		timestamp, yolo                 bool
	}{
		{name: "short-worktree", title: "luna-p678", tool: "claude", branch: "feature/luna/p678-prep-1030", width: 52, timestamp: true, want: "2h ago"},
		{name: "short-plain", title: "abc", tool: "claude", width: 24, timestamp: true, want: "2h ago"},
		{name: "branch-from-original", title: "luna-p678-long-title", tool: "claude", branch: "feature/luna/p678-prep-1030", width: 38, want: "[feature/luna/…]"},
		{name: "yolo", title: "luna-p678-long-title", tool: "gemini", width: 24, yolo: true, want: "[YOLO]"},
		{name: "supervisor", title: session.MaestroSessionTitle, tool: "claude", width: 28, want: "[SUPERVISOR]"},
		{name: "both-critical", title: session.MaestroSessionTitle, tool: "gemini", width: 32, yolo: true, want: "[SUPERVISOR] [YOLO]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHome()
			h.showSessionTimestamps = tc.timestamp
			inst := &session.Instance{ID: "review", Title: tc.title, Tool: tc.tool, Status: session.StatusWaiting, CreatedAt: time.Now().Add(-2 * time.Hour)}
			if tc.branch != "" {
				inst.WorktreePath = "/tmp/worktree"
				inst.WorktreeBranch = tc.branch
			}
			if tc.yolo {
				inst.GeminiYoloMode = &tc.yolo
			}
			state := sessionRenderState{title: inst.Title, tool: inst.Tool, status: inst.Status}
			var b strings.Builder
			h.renderSessionItem(&b, session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, IsLastInGroup: true}, false, map[string]sessionRenderState{inst.ID: state}, tc.width)
			row := strings.TrimSuffix(stripAnsi(b.String()), "\n")
			if !strings.Contains(row, tc.want) {
				t.Errorf("missing %q in %q", tc.want, row)
			}
			if tc.timestamp && !strings.Contains(row, tc.title) {
				t.Errorf("short title missing: %q", row)
			}
			if cellWidth(row) > tc.width {
				t.Errorf("row exceeds %d cells: %q", tc.width, row)
			}
		})
	}
}
