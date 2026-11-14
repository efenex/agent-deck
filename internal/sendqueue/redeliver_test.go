package sendqueue

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedeliverOnceConcurrentReplayAndConflict(t *testing.T) {
	dir, id := t.TempDir(), NewID(time.Now())
	var called atomic.Int32
	want := RedeliveryDecision{Outcome: "pending", Reason: "reopened"}
	apply := func() (RedeliveryDecision, error) { called.Add(1); return want, nil }
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, _, err := RedeliverOnce(dir, id, "stable", apply); err != nil || d != want {
				t.Errorf("decision: %+v %v", d, err)
			}
		}()
	}
	wg.Wait()
	if called.Load() != 1 {
		t.Fatalf("operation ran %d times", called.Load())
	}
	if d, replay, err := RedeliverOnce(dir, id, "stable", apply); err != nil || !replay || d != want || called.Load() != 1 {
		t.Fatalf("replay: %+v %v %v", d, replay, err)
	}
	if d, _, err := RedeliverOnce(dir, NewID(time.Now()), "stable", apply); err != nil || d.Outcome != "conflict" || called.Load() != 1 {
		t.Fatalf("conflict: %+v %v", d, err)
	}
	// Another resolved profile owns a separate key namespace.
	if _, replay, err := RedeliverOnce(t.TempDir(), id, "stable", apply); err != nil || replay || called.Load() != 2 {
		t.Fatalf("profile isolation: %v %v", replay, err)
	}
}

func TestRedeliverOnceCrashReservationAndPrune(t *testing.T) {
	dir, id := t.TempDir(), NewID(time.Now())
	crash := errors.New("injected crash after key fsync")
	if _, _, err := RedeliverOnce(dir, id, "crash", func() (RedeliveryDecision, error) { return RedeliveryDecision{}, crash }); !errors.Is(err, crash) {
		t.Fatal(err)
	}
	never := func() (RedeliveryDecision, error) {
		t.Error("replayed an interrupted operation")
		return RedeliveryDecision{}, nil
	}
	if d, replay, err := RedeliverOnce(dir, id, "crash", never); err != nil || !replay || d.Outcome != "pending" || d.Reason != "request_registered" {
		t.Fatalf("crash replay: %+v %v %v", d, replay, err)
	}
	if err := Save(dir, &Record{SendID: id, State: StateFailed, UpdatedAt: time.Now().Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	Prune(dir, time.Now().Add(-RetainFinished))
	if _, err := Load(dir, id); !errors.Is(err, ErrUnknown) {
		t.Fatal("record was not pruned", err)
	}
	if d, replay, err := RedeliverOnce(dir, id, "crash", never); err != nil || !replay || d.Outcome != "pending" {
		t.Fatalf("key lost on prune: %+v %v %v", d, replay, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "redeliver-keys"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, _ := entry.Info()
		if info.Mode().Perm() != 0600 {
			t.Errorf("key permissions: %o", info.Mode().Perm())
		}
	}
}

func TestRedeliverRequestValidation(t *testing.T) {
	for _, key := range []string{"", " leading", "trailing ", string(make([]byte, 257))} {
		if ValidRedeliveryRequestID(key) {
			t.Errorf("accepted invalid key length=%d", len(key))
		}
	}
	// Path-looking keys are hashes; they cannot escape the queue.
	dir := t.TempDir()
	if _, _, err := RedeliverOnce(dir, NewID(time.Now()), "../../path", func() (RedeliveryDecision, error) { return RedeliveryDecision{Outcome: "refused"}, nil }); err != nil {
		t.Fatal(err)
	}
}
