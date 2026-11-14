package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/costs"
	"github.com/asheshgoplani/agent-deck/internal/recall/query"
	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

// R0 never emits a typed/submitted send. Its only record writes are a
// failed-to-queued reopen (record lock) and landed proof (watch lock).
type redeliverSnapshot struct {
	SessionID    string `json:"session_id"`
	State        string `json:"state"`
	Attempts     int    `json:"attempts"`
	Redeliveries int    `json:"redeliveries"`
	Settled      bool   `json:"settled"`
	LandedRowID  string `json:"landed_row_id,omitempty"`
	LandedAt     string `json:"landed_at,omitempty"`
}

type redeliverResult struct {
	sendqueue.RedeliveryDecision
	SendID    string             `json:"send_id"`
	RequestID string             `json:"request_id"`
	Profile   string             `json:"profile"`
	CheckOnly bool               `json:"check_only,omitempty"`
	Replayed  bool               `json:"replayed,omitempty"`
	Snapshot  *redeliverSnapshot `json:"snapshot,omitempty"`
}

func handleSessionRedeliver(profile string, args []string) {
	fs := flag.NewFlagSet("session redeliver", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	key := fs.String("request-id", "", "Stable operation key (required; reuse it after a lost response)")
	check := fs.Bool("check-only", false, "Inspect eligibility/proof without changing records, binding keys or starting workers")
	fs.Usage = func() {
		fmt.Println("Usage: agent-deck session redeliver <send-id> --request-id K [--check-only] [--json]")
		fmt.Println("Reconcile late landings; release queued input or reopen a recent untyped failure with a guarded child.")
		fmt.Println("Typed/submitted sends are only rescanned, never re-sent. Cancelled sends stay cancelled.")
		fmt.Println("Outcomes: already_landed, landed, pending, refused, blocked_unknown, unsupported, not_found, conflict.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		exitCLI(2)
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" || !sendqueue.ValidRedeliveryRequestID(*key) {
		fs.Usage()
		exitCLI(2)
	}
	out := NewCLIOutput(*jsonOutput, false)
	profile, err := session.ResolveProfileForStorage(profile)
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		exitCLI(1)
	}
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		exitCLI(1)
	}
	dir := sendQueueDir(storage)
	_ = storage.Close()
	result, err := redeliverSend(profile, dir, fs.Arg(0), *key, *check)
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		exitCLI(1)
	}
	b, _ := json.Marshal(result)
	var fields map[string]interface{}
	_ = json.Unmarshal(b, &fields)
	out.Success(fmt.Sprintf("%s: %s", result.SendID, result.Outcome), fields)
}

func redeliveryDecision(outcome, reason string) sendqueue.RedeliveryDecision {
	return sendqueue.RedeliveryDecision{Outcome: outcome, Reason: reason}
}

func redeliverSend(profile, dir, id, key string, check bool) (redeliverResult, error) {
	result := redeliverResult{SendID: id, RequestID: key, Profile: profile, CheckOnly: check}
	var target string
	decide := func() (sendqueue.RedeliveryDecision, error) {
		d, kick, err := decideRedelivery(profile, dir, id, check, time.Now())
		target = kick
		return d, err
	}
	var err error
	if check {
		result.RedeliveryDecision, result.Replayed, err = sendqueue.ReadRedeliveryRequest(dir, id, key)
		if err == nil && !result.Replayed {
			result.RedeliveryDecision, err = decide()
		}
	} else {
		result.RedeliveryDecision, result.Replayed, err = sendqueue.RedeliverOnce(dir, id, key, decide)
	}
	if err != nil {
		return result, err
	}
	// Publication and key persistence precede the kick, and all record locks
	// have been released. Replays never authorize work, even after a crash.
	if target != "" && !check && !result.Replayed {
		if err := startSendWorker(profile, target); err != nil {
			// The decision remains pending and durable. A normal worker kick
			// (or send-status without --no-resume) can resume this queued send.
			fmt.Fprintln(os.Stderr, "agent-deck: start redelivery worker:", err)
		}
	}
	if rec, loadErr := sendqueue.Load(dir, id); loadErr == nil {
		result.Snapshot = &redeliverSnapshot{SessionID: rec.SessionID, State: rec.State, Attempts: rec.Attempts,
			Redeliveries: rec.Redeliveries, Settled: rec.Settled, LandedRowID: rec.LandedRowID, LandedAt: rec.LandedAt}
	} else if !errors.Is(loadErr, sendqueue.ErrUnknown) {
		return result, loadErr
	}
	return result, nil
}

func decideRedelivery(profile, dir, id string, check bool, now time.Time) (sendqueue.RedeliveryDecision, string, error) {
	answer := func(outcome, reason string) (sendqueue.RedeliveryDecision, string, error) {
		return redeliveryDecision(outcome, reason), "", nil
	}
	rec, err := sendqueue.Load(dir, id)
	if errors.Is(err, sendqueue.ErrUnknown) {
		return answer("not_found", "send_not_found")
	}
	if err != nil {
		return sendqueue.RedeliveryDecision{}, "", err
	}
	switch rec.State {
	case sendqueue.StateLanded:
		// Legacy claims suppress input forever, including those produced by
		// old watchers sharing a row. Surface that weaker attribution.
		reason := ""
		if records, err := sendqueue.ListForProof(dir); err != nil {
			reason = "proof_records_unreadable"
		} else if landedRowClaimed(records, rec, rec.LandedRowID) {
			reason = "shared_landed_row"
		}
		return answer("already_landed", reason)
	case sendqueue.StateCancelled:
		return answer("refused", "cancelled")
	}
	if rec.WaitWhileStopped {
		return answer("unsupported", "routed_send_policy")
	}
	if !query.SupportsDirectRows(rowsHarness(rec.Tool)) {
		return answer("unsupported", "tool_unverifiable")
	}
	instances, err := loadSessions(profile)
	if err != nil {
		if rec.State == sendqueue.StateTyping {
			return answer("pending", "delivery_in_progress")
		}
		return answer("blocked_unknown", "target_unreadable")
	}
	inst := instanceByID(instances, rec.SessionID)
	if inst != nil && (inst.IsSSH() || !query.SupportsDirectRows(rowsHarness(inst.Tool))) {
		return answer("unsupported", "target_unverifiable")
	}
	switch rec.State {
	case sendqueue.StateTyping:
		return answer("pending", "delivery_in_progress")
	case sendqueue.StateQueued:
		if check {
			return answer("pending", "would_release")
		}
		// The existing control protocol keeps the worker as the only writer
		// of queued records. Never save the snapshot we just inspected.
		d, err := releaseForRedelivery(profile, dir, rec)
		return d, "", err
	case sendqueue.StateTyped, sendqueue.StateSubmitted:
		d, err := reconcileRedelivery(profile, dir, rec, check)
		return d, "", err
	case sendqueue.StateFailed:
		return reopenFailedSend(profile, dir, rec, inst, instances, check, now)
	default:
		return answer("blocked_unknown", "unknown_state")
	}
}

// releaseForRedelivery uses the ordinary release control/result files. Like
// queue release, only the caller that created the request consumes its result.
// A timeout leaves the control intact and reports pending, never non-delivery.
func releaseForRedelivery(profile, dir string, rec *sendqueue.Record) (sendqueue.RedeliveryDecision, error) {
	err := requestQueueControl(dir, rec.SendID, "release")
	if os.IsExist(err) {
		return redeliveryDecision("pending", "control_in_progress"), nil
	}
	if err != nil {
		return sendqueue.RedeliveryDecision{}, err
	}
	if err := startSendWorker(profile, rec.SessionID); err != nil {
		return redeliveryDecision("pending", "worker_start_failed"), nil
	}
	end := time.Now().Add(envDuration("AGENTDECK_REDELIVER_WAIT", 5*time.Second))
	for {
		if b, err := os.ReadFile(sendqueue.ControlResultPath(dir, rec.SendID)); err == nil {
			var result queueControlResult
			if json.Unmarshal(b, &result) == nil {
				_ = os.Remove(sendqueue.ControlResultPath(dir, rec.SendID))
				_ = os.Remove(sendqueue.ControlPath(dir, rec.SendID))
				switch result.Outcome {
				case "refused", "cancelled", "not_found":
					return redeliveryDecision("refused", "release_"+result.Outcome), nil
				case "unconfirmed", "unknown":
					return redeliveryDecision("blocked_unknown", "release_unconfirmed"), nil
				default:
					return redeliveryDecision("pending", "release_processed"), nil
				}
			}
		}
		// Ordinary dequeue may have left queued before the control file was
		// created. Match queue release's readOutcome fallback and retire only
		// our own control; it must not reactivate on a later failed reopen.
		current, err := sendqueue.Load(dir, rec.SendID)
		if err == nil && current.State != sendqueue.StateQueued && current.State != sendqueue.StateTyping &&
			current.TargetStatus != queueReleaseStatus && current.State != sendqueue.StateCancelled {
			_ = os.Remove(sendqueue.ControlPath(dir, rec.SendID))
			if current.State == sendqueue.StateFailed {
				return redeliveryDecision("refused", "release_failed"), nil
			}
			return redeliveryDecision("pending", "delivery_in_progress"), nil
		}
		if !time.Now().Before(end) {
			return redeliveryDecision("pending", "release_requested"), nil
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func reopenFailedSend(profile, dir string, rec *sendqueue.Record, inst *session.Instance, instances []*session.Instance, check bool, now time.Time) (sendqueue.RedeliveryDecision, string, error) {
	answer := func(outcome, reason string) (sendqueue.RedeliveryDecision, string, error) {
		return redeliveryDecision(outcome, reason), "", nil
	}
	if !check {
		lock, ok, err := sendqueue.TryRecordLock(dir, rec.SendID)
		if err != nil {
			return sendqueue.RedeliveryDecision{}, "", err
		}
		if !ok {
			return answer("pending", "record_busy")
		}
		defer releaseQueueLock(lock)
		// Reopens of different send IDs to one target serialize their
		// known-copy check and Save: two identical failed sends reopened
		// concurrently must not both see the other as failed and both queue.
		reopen, ok, err := sendqueue.TryLock(dir, "reopen-"+rec.SessionID)
		if err != nil {
			return sendqueue.RedeliveryDecision{}, "", err
		}
		if !ok {
			return answer("pending", "reopen_busy")
		}
		defer releaseQueueLock(reopen)
		current, err := sendqueue.Load(dir, rec.SendID)
		if errors.Is(err, sendqueue.ErrUnknown) {
			return answer("not_found", "send_not_found")
		}
		if err != nil {
			return sendqueue.RedeliveryDecision{}, "", err
		}
		if current.State != sendqueue.StateFailed {
			return answer("pending", "state_changed")
		}
		rec = current
	}
	if inst == nil || inst.ID != rec.SessionID || !inst.Exists() {
		return answer("refused", "target_not_running")
	}
	if rowsHarness(inst.Tool) != rowsHarness(rec.Tool) {
		return answer("refused", "target_changed")
	}
	cfg, err := loadUserConfigForSend()
	if err != nil {
		return answer("refused", "config_unreadable")
	}
	age := cfg.GetRedeliverMaxAge()
	created, err := time.Parse(time.RFC3339Nano, rec.CreatedAt)
	if err != nil || age <= 0 || created.After(now) || now.Sub(created) > age {
		return answer("refused", "reopen_age_exceeded")
	}
	// Reopening grants no budget bypass, including to a record originally
	// queued with --ignore-budget. The ordinary child checks again at input.
	if budgetForSend(profile, inst, instances, nil).Action == costs.BudgetActionStop {
		return answer("refused", "budget_exceeded")
	}
	records, err := sendqueue.ListForProof(dir)
	if err != nil {
		return answer("blocked_unknown", "proof_records_unreadable")
	}
	for _, other := range records {
		if knownCopy(other, rec, created) {
			return answer("blocked_unknown", "content_present")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	path := session.LiveTranscriptPath(inst, instances)
	presence := query.ScanLanded(ctx, rowsHarness(inst.Tool), path, rec.Message,
		query.LandedWindow{To: -1, NotBefore: created})
	if presence.Present {
		return answer("blocked_unknown", "content_present")
	}
	if !presence.Complete {
		return answer("blocked_unknown", presence.Reason)
	}
	for _, path := range rec.Images {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return answer("refused", "attachment_missing")
		}
	}
	if check {
		return answer("pending", "would_reopen")
	}
	previous := *rec
	rec.State, rec.Verdict, rec.Reason = sendqueue.StateQueued, "queued", ""
	rec.Deadline = now.Add(sendqueue.DefaultRetryBudget).UTC().Format(time.RFC3339Nano)
	rec.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	rec.RequireInputPrompt, rec.IgnoreBudget, rec.Settled = true, false, false
	rec.Redeliveries++
	rec.UnavailableRefusals = 0
	// D3: the stable deck session may now run a different native conversation.
	// Old attempt evidence is replaced only by the worker at typing time.
	if err := sendqueue.Save(dir, rec); err != nil {
		return sendqueue.RedeliveryDecision{}, "", err
	}
	queuedSendChanged(profile, &previous, rec)
	return redeliveryDecision("pending", "reopened"), rec.SessionID, nil
}

// knownCopy reports whether other is a tracked copy of rec's text that may
// still be typed, or was typed after rec was created. Creation order alone is
// not enough: an older identical send that was itself reopened (or is still
// queued) types after rec was created. An unparseable time counts as a copy.
func knownCopy(other, rec *sendqueue.Record, created time.Time) bool {
	if other.SendID == rec.SendID || other.SessionID != rec.SessionID || normalizedSendText(other.Message) != normalizedSendText(rec.Message) ||
		other.State == sendqueue.StateFailed || other.State == sendqueue.StateCancelled {
		return false
	}
	if other.State == sendqueue.StateQueued || other.State == sendqueue.StateTyping {
		return true
	}
	for _, ts := range []string{other.CreatedAt, other.SentAt} {
		if ts == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil || !at.Before(created) {
			return true
		}
	}
	return false
}

// normalizedSendText folds whitespace the way transcript matching does.
func normalizedSendText(s string) string { return strings.Join(strings.Fields(s), " ") }

// redeliverProofWindow also caps ordinary watchers: a later identical input
// must never retroactively prove an older lost send.
const redeliverProofWindow = 10 * time.Minute

func landedRowClaimed(records []*sendqueue.Record, rec *sendqueue.Record, row string) bool {
	if row == "" {
		return false
	}
	for _, other := range records {
		if other.SendID != rec.SendID && other.LandedRowID == row {
			return true
		}
	}
	return false
}

func boundedSendProof(dir string, rec *sendqueue.Record) (query.LandedCandidate, string) {
	var none query.LandedCandidate
	sent, err := time.Parse(time.RFC3339Nano, rec.SentAt)
	if err != nil {
		return none, "send_time_unknown"
	}
	records, err := sendqueue.ListForProof(dir)
	if err != nil {
		return none, "proof_records_unreadable"
	}
	// Only a later send of the same text can be confused with this one's row.
	// A different-text send is no fence: the worker types it without waiting
	// for this proof, so a mid-turn queued message is absorbed after it.
	to := int64(-1)
	want := normalizedSendText(rec.Message)
	for _, other := range records {
		if other.SendID == rec.SendID || other.SessionID != rec.SessionID || other.TranscriptPath != rec.TranscriptPath || other.TranscriptFrom < rec.TranscriptFrom ||
			normalizedSendText(other.Message) != want {
			continue
		}
		next, err := time.Parse(time.RFC3339Nano, other.SentAt)
		if err == nil && !next.Before(sent) && (to < 0 || other.TranscriptFrom < to) {
			to = other.TranscriptFrom
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	scan := query.ScanLanded(ctx, rowsHarness(rec.Tool), rec.TranscriptPath, rec.Message,
		query.LandedWindow{From: rec.TranscriptFrom, To: to, NotBefore: sent, NotAfter: sent.Add(redeliverProofWindow)})
	if !scan.Complete {
		return none, scan.Reason
	}
	if len(scan.Candidates) != 1 || scan.Pending {
		if scan.Present {
			return none, "content_present"
		}
		return none, "no_landed_proof"
	}
	row := scan.Candidates[0]
	if landedRowClaimed(records, rec, row.ID) {
		return none, "content_present"
	}
	return row, ""
}

func reconcileRedelivery(profile, dir string, rec *sendqueue.Record, check bool) (sendqueue.RedeliveryDecision, error) {
	if !check {
		lock, ok, err := sendqueue.TryLock(dir, "watch-"+rec.SendID)
		if err != nil {
			return sendqueue.RedeliveryDecision{}, err
		}
		if !ok {
			return redeliveryDecision("pending", "watch_in_progress"), nil
		}
		defer releaseQueueLock(lock)
	}
	return reconcileSendProof(profile, dir, rec, check)
}

// reconcileSendProof requires the watch lock for mutations. The short claims
// lock serializes proof scan + Save across all watchers/redeliveries; Prune
// shares the record lock. No worker-owned queued/typing record is written.
func reconcileSendProof(profile, dir string, rec *sendqueue.Record, check bool) (sendqueue.RedeliveryDecision, error) {
	if !check {
		lock, ok, err := sendqueue.TryRecordLock(dir, rec.SendID)
		if err != nil {
			return sendqueue.RedeliveryDecision{}, err
		}
		if !ok {
			return redeliveryDecision("pending", "record_busy"), nil
		}
		defer releaseQueueLock(lock)
		claims, ok, err := sendqueue.TryLock(dir, "landed-claims")
		if err != nil {
			return sendqueue.RedeliveryDecision{}, err
		}
		if !ok {
			return redeliveryDecision("pending", "proof_busy"), nil
		}
		defer releaseQueueLock(claims)
		current, err := sendqueue.Load(dir, rec.SendID)
		if errors.Is(err, sendqueue.ErrUnknown) {
			return redeliveryDecision("not_found", "send_not_found"), nil
		}
		if err != nil {
			return sendqueue.RedeliveryDecision{}, err
		}
		*rec = *current
	}
	if rec.State == sendqueue.StateLanded {
		return redeliveryDecision("already_landed", ""), nil
	}
	if rec.State != sendqueue.StateTyped && rec.State != sendqueue.StateSubmitted {
		return redeliveryDecision("pending", "state_changed"), nil
	}
	proof, reason := boundedSendProof(dir, rec)
	if reason != "" {
		return redeliveryDecision("blocked_unknown", reason), nil
	}
	if !check {
		previous := *rec
		rec.State, rec.Verdict, rec.Reason, rec.Settled = sendqueue.StateLanded, "delivered", "", false
		rec.LandedRowID, rec.LandedAt = proof.ID, proof.TS
		rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if sid := claudeSessionIDFromTranscript(rowsHarness(rec.Tool), rec.TranscriptPath); sid != "" {
			rec.ClaudeSessionID = sid
		}
		if err := sendqueue.Save(dir, rec); err != nil {
			return sendqueue.RedeliveryDecision{}, err
		}
		queuedSendChanged(profile, &previous, rec)
	}
	return redeliveryDecision("landed", ""), nil
}
