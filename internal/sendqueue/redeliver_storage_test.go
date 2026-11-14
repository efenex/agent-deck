package sendqueue

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPruneHonorsRecordLock(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	r := &Record{SendID: NewID(now), State: StateFailed, UpdatedAt: now.Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)}
	if err := Save(dir, r); err != nil {
		t.Fatal(err)
	}
	lock, ok, err := TryLock(dir, "record-"+r.SendID)
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer lock.Release()
	Prune(dir, now.Add(-RetainFinished))
	if _, err := Load(dir, r.SendID); err != nil {
		t.Fatalf("prune deleted a record being reopened: %v", err)
	}
	r.State = StateQueued
	if err := Save(dir, r); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	Prune(dir, now.Add(-RetainFinished))
	if got, err := Load(dir, r.SendID); err != nil || got.State != StateQueued {
		t.Fatalf("prune removed reopened record: %+v %v", got, err)
	}
}

func TestSaveConcurrentAtomic(t *testing.T) {
	dir := t.TempDir()
	id := NewID(time.Now())
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 10; j++ {
				if err := Save(dir, &Record{SendID: id, State: StateTyped, Message: strings.Repeat("[REDACTED]", 1024)}); err != nil {
					t.Errorf("concurrent save: %v", err)
					return
				}
				if r, err := Load(dir, id); err != nil || len(r.Message) != 10240 {
					t.Errorf("torn record: %v", err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("left temp file %s", e.Name())
		}
	}
}
