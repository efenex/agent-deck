package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
)

func TestRedeliverCLIAbsent(t *testing.T) {
	home := t.TempDir()
	stdout, stderr, code := runAgentDeck(t, home, "session", "redeliver", "01ARZ3NDEKTSV4RRFFQ69G5FAV", "--request-id", "r0-absent", "--json")
	var result struct {
		Outcome string `json:"outcome"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &result) != nil || result.Outcome != "not_found" {
		t.Fatalf("redeliver absent: exit %d %s %s", code, stdout, stderr)
	}
}

func TestSendStatusNoResumeCLI(t *testing.T) {
	home := t.TempDir()
	id := addSessionJSON(t, home, "r0-stopped", "claude")
	stdout, _, _ := runAgentDeck(t, home, "session", "send", id, "[REDACTED]", "--queue", "--json")
	var receipt struct {
		SendID string `json:"send_id"`
	}
	if err := json.Unmarshal([]byte(stdout), &receipt); err != nil || receipt.SendID == "" {
		t.Fatalf("receipt: %v %s", err, stdout)
	}
	stdout, stderr, code := runAgentDeck(t, home, "session", "send-status", receipt.SendID, "--no-resume", "--json")
	var result struct {
		State string `json:"state"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &result) != nil || result.State != "failed" {
		t.Fatalf("no-resume: exit %d %s %s", code, stdout, stderr)
	}
	path := queuedRecordPathForRedeliverTest(t, home, receipt.SendID)
	dir := filepath.Dir(path)
	rec, err := sendqueue.Load(dir, receipt.SendID)
	if err != nil {
		t.Fatal(err)
	}
	rec.State = sendqueue.StateQueued
	if err := sendqueue.Save(dir, rec); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = runAgentDeck(t, home, "session", "send-status", receipt.SendID, "--no-resume", "--json")
	if code != 0 || json.Unmarshal([]byte(stdout), &result) != nil || result.State != "queued" {
		t.Fatalf("queued no-resume: %d %s %s", code, stdout, stderr)
	}
	time.Sleep(250 * time.Millisecond)
	if current, err := sendqueue.Load(dir, receipt.SendID); err != nil || current.State != "queued" {
		t.Fatalf("no-resume started a worker: %+v %v", current, err)
	}
	// Preserve the old default: a plain status read resumes this stopped
	// target's queued send, and its worker fails it untyped.
	_, _, code = runAgentDeck(t, home, "session", "send-status", receipt.SendID, "--json")
	if code != 0 {
		t.Fatal("default status failed")
	}
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		current, err := sendqueue.Load(dir, receipt.SendID)
		if err == nil && current.State == "failed" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("default status did not resume the worker")
}

func queuedRecordPathForRedeliverTest(t *testing.T, home, id string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == id+".json" && filepath.Base(filepath.Dir(path)) == "sendqueue" {
			found = path
		}
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("queue fixture missing: %v", err)
	}
	return found
}

func TestRedeliverCLIReplaySurvivesProcessAndRecordRewrite(t *testing.T) {
	home := t.TempDir()
	id := addSessionJSON(t, home, "r0-replay", "claude")
	stdout, _, _ := runAgentDeck(t, home, "session", "send", id, "[REDACTED]", "--queue", "--json")
	var receipt struct {
		SendID string `json:"send_id"`
	}
	if err := json.Unmarshal([]byte(stdout), &receipt); err != nil || receipt.SendID == "" {
		t.Fatalf("receipt: %s %v", stdout, err)
	}
	call := func(sendID string) redeliverResult {
		stdout, stderr, code := runAgentDeck(t, home, "session", "redeliver", sendID, "--request-id", "stable-key", "--json")
		var got redeliverResult
		if code != 0 || json.Unmarshal([]byte(stdout), &got) != nil {
			t.Fatalf("redeliver: %d %s %s", code, stdout, stderr)
		}
		return got
	}
	first := call(receipt.SendID)
	if first.Outcome != "refused" || first.Reason != "target_not_running" {
		t.Fatal(first)
	}
	dir := filepath.Dir(queuedRecordPathForRedeliverTest(t, home, receipt.SendID))
	r, err := sendqueue.Load(dir, receipt.SendID)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an old worker re-marshalling the record with no operation
	// fields; the key file must survive separately.
	r.State = sendqueue.StateLanded
	r.LandedRowID = "synthetic-row"
	if err := sendqueue.Save(dir, r); err != nil {
		t.Fatal(err)
	}
	again := call(receipt.SendID)
	if !again.Replayed || again.RedeliveryDecision != first.RedeliveryDecision || again.Snapshot.State != "landed" {
		t.Fatal(again)
	}
	if conflict := call("01ARZ3NDEKTSV4RRFFQ69G5FAV"); conflict.Outcome != "conflict" {
		t.Fatal(conflict)
	}
	for _, args := range [][]string{{"session", "redeliver", receipt.SendID, "--json"}, {"session", "redeliver", receipt.SendID, "--request-id", "k", "--message", "[REDACTED]"}} {
		if _, _, code := runAgentDeck(t, home, args...); code != 2 {
			t.Fatalf("invalid args accepted: %v exit %d", args, code)
		}
	}
}
