package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestSessionRowSSHBadgeBoundaries(t *testing.T) {
	for _, selected := range []bool{false, true} {
		for _, tc := range []struct {
			width int
			want  string
		}{
			{28, ""}, {29, "[ssh:…]"}, {30, "[ssh:w…]"},
			{39, "[ssh:worker@ver…]"}, {40, "[ssh:work…]"},
			{54, "[ssh:worker@very-long-h…]"}, {55, "[ssh:worker@very-long-...]"},
			{59, "[ssh:worker@very-long-...]"}, {60, "[ssh:worker@very-long-...]"},
		} {
			t.Run(fmt.Sprintf("width%d-selected%t", tc.width, selected), func(t *testing.T) {
				row := renderSSHBadgeRow(t, tc.width, selected, "remote-session-long-title", "worker@very-long-hostname.example", false, false)
				if tc.want == "" {
					if strings.Contains(row, "[ssh:") {
						t.Errorf("badge must drop below minimum: %q", row)
					}
				} else if !strings.Contains(row, tc.want) {
					t.Errorf("want %q in %q", tc.want, row)
				}
				if !strings.Contains(row, "remote-sess") {
					t.Errorf("title floor lost: %q", row)
				}
				assertSSHBadgeGolden(t, fmt.Sprintf("width%d-selected%t", tc.width, selected), row)
			})
		}
	}
}

func TestSessionRowSSHBadgeUnicodeAndPriority(t *testing.T) {
	for _, selected := range []bool{false, true} {
		for _, host := range []string{"host", "界界界界界界界界界界界界", "e\u0301quipe@long-host.example"} {
			for width := 24; width <= 65; width++ {
				t.Run(fmt.Sprintf("%s-width%d-selected%t", host, width, selected), func(t *testing.T) {
					row := renderSSHBadgeRow(t, width, selected, "remote-session-long-title", host, false, false)
					if !strings.Contains(row, "remote-sess") {
						t.Errorf("title floor lost: %q", row)
					}
					if i := strings.Index(row, "[ssh:"); i >= 0 && !strings.Contains(row[i:], "]") {
						t.Errorf("unclosed SSH badge: %q", row)
					}
				})
			}
		}
		for _, width := range []int{32, 42, 60, 80} {
			row := renderSSHBadgeRow(t, width, selected, session.MaestroSessionTitle, "worker@very-long-hostname.example", true, true)
			if !strings.Contains(row, "[SUPERVISOR] [YOLO]") {
				t.Errorf("critical badges lost: %q", row)
			}
			assertSSHBadgeGolden(t, fmt.Sprintf("critical-width%d-selected%t", width, selected), row)
		}
		for _, tc := range []struct{ name, host, want string }{
			{"short", "host", "[ssh:host]"},
			{"wide", "界界界界界界界界", "[ssh:界界…]"},
			{"combining", "e\u0301quipe@long-host.example", "[ssh:e\u0301qui…]"},
		} {
			row := renderSSHBadgeRow(t, 40, selected, "remote-session-long-title", tc.host, false, false)
			if !strings.Contains(row, tc.want) {
				t.Errorf("want %q in %q", tc.want, row)
			}
			assertSSHBadgeGolden(t, fmt.Sprintf("%s-selected%t", tc.name, selected), row)
		}
		row := renderSSHBadgeRow(t, 60, selected, "remote-session-long-title", "worker@very-long-hostname.example", false, true)
		if !strings.Contains(row, "[feature/long...] [ssh:worker…]") || !strings.Contains(row, "remote-sess") {
			t.Errorf("worktree priority or title floor lost: %q", row)
		}
		assertSSHBadgeGolden(t, fmt.Sprintf("worktree-selected%t", selected), row)
	}
}

func renderSSHBadgeRow(t *testing.T, width int, selected bool, title, host string, yolo, worktree bool) string {
	t.Helper()
	h := NewHome()
	inst := &session.Instance{ID: "ssh-width", Title: title, Tool: "claude", Status: session.StatusWaiting, SSHHost: host}
	if yolo {
		inst.Tool = "gemini"
		inst.GeminiYoloMode = &yolo
	}
	if worktree {
		inst.WorktreePath = "/tmp/worktree"
		inst.WorktreeBranch = "feature/long-worktree-branch"
	}
	state := sessionRenderState{title: inst.Title, tool: inst.Tool, status: inst.Status}
	var b strings.Builder
	h.renderSessionItem(&b, session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, IsLastInGroup: true}, selected, map[string]sessionRenderState{inst.ID: state}, width)
	row := strings.TrimSuffix(stripAnsi(b.String()), "\n")
	if !utf8.ValidString(row) || cellWidth(row) > width {
		t.Errorf("invalid row or width exceeds %d: %q", width, row)
	}
	return row
}

func assertSSHBadgeGolden(t *testing.T, name, row string) {
	t.Helper()
	path := filepath.Join("testdata", "ssh_badge_width", name+".txt")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(row+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != row+"\n" {
		t.Errorf("golden mismatch: want %q, got %q", want, row+"\n")
	}
}

func BenchmarkSessionRowSSHBadge(b *testing.B) {
	h := NewHome()
	inst := &session.Instance{ID: "ssh-bench", Title: "remote-session-long-title", Tool: "claude", Status: session.StatusWaiting, SSHHost: "worker@very-long-hostname.example"}
	item := session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, IsLastInGroup: true}
	snapshot := map[string]sessionRenderState{inst.ID: {title: inst.Title, tool: inst.Tool, status: inst.Status}}
	for _, width := range []int{40, 60} {
		b.Run(fmt.Sprintf("width%d", width), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var row strings.Builder
				h.renderSessionItem(&row, item, false, snapshot, width)
			}
		})
	}
}
