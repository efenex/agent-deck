package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func writeLookupRollout(t testing.TB, home, day, sid string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "10", day)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-"+day+"T12-00-00-"+sid+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func rolloutLookupFleet(t testing.TB, history int) []*Instance {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	// Historical rollouts, not registered sessions: this is the independent
	// dimension that the empty-transcript navigation benchmarks missed.
	for i := 0; i < history; i++ {
		writeLookupRollout(t, home, fmt.Sprintf("%02d", i%28+1), fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
	}
	fleet := make([]*Instance, 40)
	for i := range fleet {
		sid := fmt.Sprintf("11111111-1111-4111-8111-%012d", i)
		writeLookupRollout(t, home, "30", sid)
		fleet[i] = &Instance{Tool: "codex", CodexSessionID: sid}
	}
	return fleet
}

func TestCodexRolloutLookupWarmFleet(t *testing.T) {
	fleet := rolloutLookupFleet(t, 1000)
	check := func() {
		for _, inst := range fleet {
			if !inst.CanForkCodex() {
				t.Fatal("flushed rollout must remain forkable")
			}
		}
	}
	check()
	// A warm capability pass must not allocate directory listings proportional
	// to historical rollouts. Leave ample room for exact-path stat and ID checks.
	allocs := testing.AllocsPerRun(3, check)
	if allocs > 40*64 {
		t.Fatalf("warm 40-session capability pass allocated %.0f objects; budget 2560 (repeated history scan)", allocs)
	}
}

func TestCodexRolloutLookupFilesystemChanges(t *testing.T) {
	home := t.TempDir()
	other := t.TempDir()
	sid := "11111111-2222-4333-8444-555555555555"
	if got := codexRolloutPathInHome(sid, home); got != "" {
		t.Fatalf("unflushed rollout: %q", got)
	}
	path := writeLookupRollout(t, home, "29", sid)
	if got := codexRolloutPathInHome(" "+sid+" ", home); got != path {
		t.Fatalf("new rollout: got %q want %q", got, path)
	}
	if got := codexRolloutPathInHome(sid, other); got != "" {
		t.Fatalf("path leaked across homes: %q", got)
	}
	otherPath := writeLookupRollout(t, other, "30", sid)
	if got := codexRolloutPathInHome(sid, other); got != otherPath {
		t.Fatalf("other home: %q", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := codexRolloutPathInHome(sid, home); got != "" {
		t.Fatalf("removed rollout remains discoverable: %q", got)
	}
	moved := writeLookupRollout(t, home, "30", sid)
	if got := codexRolloutPathInHome(sid, home); got != moved {
		t.Fatalf("moved rollout: got %q want %q", got, moved)
	}
	if got := codexRolloutPathInHome("", home); got != "" {
		t.Fatalf("empty ID: %q", got)
	}
}

func TestCodexRolloutLookupConcurrent(t *testing.T) {
	home := t.TempDir()
	sid := "11111111-2222-4333-8444-555555555555"
	path := writeLookupRollout(t, home, "30", sid)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				if got := codexRolloutPathInHome(sid, home); got != path {
					t.Errorf("lookup: got %q want %q", got, path)
				}
			}
		})
	}
	wg.Wait()
}

func BenchmarkCodexRolloutLookup40(b *testing.B) {
	for _, history := range []int{231, 1000} {
		b.Run(fmt.Sprintf("history=%d", history), func(b *testing.B) {
			fleet := rolloutLookupFleet(b, history)
			for _, inst := range fleet {
				if !inst.CanForkCodex() {
					b.Fatal("fixture is not forkable")
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				for _, inst := range fleet {
					if !inst.CanForkCodex() {
						b.Fatal("flushed rollout must remain forkable")
					}
				}
			}
		})
	}
}

func TestCodexRolloutLookupGlobSemantics(t *testing.T) {
	home := t.TempDir()
	sid := "11111111-2222-4333-8444-555555555555"
	later := writeLookupRollout(t, home, "30", sid)
	// Non-literal IDs retain the original glob's current first-match behavior.
	if got := codexRolloutPathInHome("*", home); got != later {
		t.Fatalf("wildcard: %q", got)
	}
	earlier := writeLookupRollout(t, home, "29", sid)
	if got := codexRolloutPathInHome("*", home); got != earlier {
		t.Fatalf("new earlier match: %q", got)
	}
	if got := codexRolloutPathInHome("[", home); got != "" {
		t.Fatalf("malformed pattern: %q", got)
	}
	if err := os.Remove(earlier); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "missing-target"), earlier); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got := codexRolloutPathInHome(sid, home); got != earlier {
			t.Fatalf("glob's dangling symlink entry: %q", got)
		}
	}
}
