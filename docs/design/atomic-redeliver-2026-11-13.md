# Atomic, idempotent re-delivery of a durable send

Status: R0 implementation contract accepted after Claude review and operator decisions.
Task: `agentdeck-redeliver-r0`. Date: 2026-11-13.

## R0: accepted scope (supersedes the original draft below)

The first phone-facing benefit is **late-landing reconciliation**, not another
send. A settled `typed`/`submitted` record can acquire transcript proof after
the ordinary two-minute watch ends. A miss never permits emission (D1a).

This document was copied from design commits `b2a68625` and `5ce1af69`.
Implementation starts at `f7017710f879f2a232c1616ff43dded8247573e1`.
The cited sendqueue, send/queue commands, native row readers, transport, native
submission marker and remote forwarding sources were re-read at that base;
their diff from `0549d06f` is empty. The historical line numbers below name
the original pin, not the new implementation.

```text
agent-deck -p PROFILE session redeliver SEND_ID --request-id K [--check-only] --json
agent-deck -p PROFILE session send-status SEND_ID --no-resume --json
agent-deck remote OWNER session redeliver SEND_ID --request-id K --json
```

R0 needs no expected revision, aggregate version, migration, capability verb,
attempt fence, receiver token or replacement payload (D6a). The request key is
required even for `--check-only`, but checks neither bind nor consume keys.
Unknown command on an older owner returns `unsupported`; no local fallback.

| Retained state | R0 action |
| --- | --- |
| `landed` | `already_landed`, always suppress input; flag `shared_landed_row` when another retained record claims its row |
| `queued` | Existing release control protocol; only the target worker changes/types the record |
| `typing` | `pending`, join without another emitter |
| `typed` / `submitted`, settled or not | Try `watch-<id>` lock; bounded scan only; unique proof upgrades to `landed`, otherwise `blocked_unknown`; held lock returns `pending` |
| `failed` | Under `record-<id>` flock, reload and check all guards, then same-ID `failed→queued`; kick worker after releasing the lock |
| `cancelled` | `refused`, reason `cancelled`, never reopen (D8a) |
| absent | `not_found`, never reconstruct |

**R0 write rule:** redeliver writes records only on `failed→queued` and
`typed/submitted→landed`. Never save a stale queued/typing snapshot. Ordinary
watchers share the same watch/row-claim protocol. Prune takes the record lock,
reloads, and rechecks finality and retention before deleting. Lock files are
not unlinked, avoiding split-lock inodes.

Proof scans read at most 4 MiB, with a three-second deadline, from the saved
offset through the offset of the next send of the same text to the same
target/source (when known; a different-text send is no fence),
and accept rows no later than `sent_at + 10 minutes`. The existing two-second
clock skew applies to the lower bound. A row already claimed by another
retained record is rejected. Several candidates or pending identical input
mean `content_present`, not unique landed proof. A shared claims flock
serializes scan plus publication between current watchers and redeliveries.
Malformed/partial/missing/truncated data or incomplete coverage stays unknown.
Old `landed` claims remain suppressive; no retroactive downgrade or migration.

Failed-reopen guards:

- Stable deck session ID must still exist and run. It may now have a different
  native conversation of the same harness (D3a). Routed successor sends are
  outside R0; no cross-deck-session retargeting.
- Default maximum age is **1 hour from created_at**, configured by
  `[send] redeliver_max_age = "1h"`. Invalid/nonpositive values disable reopening
  (D2). Cost budgets are checked again; reopening grants no budget bypass.
- Check the **current live native transcript**, from offset zero with
  `notBefore=created_at`. A bounded tail may find presence, but an omitted
  prefix means coverage is incomplete and reopening is blocked. Matching
  queued input and known later nonfailed sends also block with `content_present`.
- Refresh the deadline to the ordinary 30-minute retry budget, keep `Attempts`,
  increment additive `redeliveries`, and force `RequireInputPrompt=true` (D4a).
  The normal guarded child selects tmux, never socket, and keeps Codex's
  existing acceptance-marker protocol. No raw Enter, draft clearing or marker
  clearing is added. The original ULID can precede newer queued sends.
- OpenCode, shell and SSH placeholders are `unsupported` (D7a); no reader means
  no safe content-presence check. Terminal landed/cancelled records still
  suppress input regardless of tool.

Request bindings live outside the record in `sendqueue/redeliver-keys/`, keyed
by a hash of the caller key, so an older worker saving the record cannot erase
them. Same key returns the stored decision plus a current content-free
snapshot; another send conflicts. Before any action, fsync a pending key
reservation. A crash in the registration/action/result gap leaves
`pending/request_registered`; replay never reruns that action. Inspect the
snapshot and deliberately use a new key if appropriate. Bindings survive
Prune; R0 has no binding garbage collector. Save uses unique temporary files,
file fsync, rename and directory fsync. No v2 state is written.

**Guarantee boundary:** no automatic re-emission of possibly delivered input
for a given `send_id`. A manual or agent resend under another ID can duplicate
the content. The reopen presence check blocks visible copies and queued peer
records; it cannot exclude a human or untracked sender racing the check. Do not
advertise cross-ID or receiver-side exactly-once behavior.

Delivery order: **R0 agent-deck**, then the separate deck-remote R0 integration
and **R1** socket/fork-origin native-row fixtures and matcher work. Guarded phone
sends use tmux, so socket envelopes are not an explanation for those losses.
R1 must prove its shapes with synthetic/container fixtures before enabling
proof for unguarded socket traffic. D9b (deck-remote prefers row receipts and
labels content-only matches unverified) belongs to that separate integration.

**R2/D5:** a two-day spike on the existing synchronous Claude UserPromptSubmit
hook as a content-free receipt/dedup seam, then Codex. Verify queue timing and
blocking-hook ordering in a container. The hook exists; a receiver-enforced
dedup contract does not yet. Only positive spike evidence justifies any lost
send re-emission. The original slices 2 and 4–6, v2 migration, endpoint locks
and OpenCode adapter remain deferred behind that decision.

## Historical full design (deferred where it conflicts with R0)

The remainder preserves the original review context and pinned citations.
Its v2 API, migration and estimates are **not** R0 implementation requirements.

## Recommendation and guarantee boundary

Add `agent-deck session redeliver <send-id>` as a durable, same-ID operation.
The owner host performs reconciliation, eligibility checking, reservation and
delivery; deck-remote submits one request and displays its evidence. Never
implement this as `send-status` followed by a new `session send`.

Recommend **Option A: conservative recovery on existing transports**, with an
adapter contract that permits **Option B: receiver-enforced deduplication** later.
Ship A with automatic recovery of provably untyped failures and reconciliation
of ambiguous sends. Do not advertise automatic recovery of every historical
“lost” send. Recovering an ambiguous send without risking duplication requires
receiver support that this draft does not assume exists.

The central distinction is **absence of proof versus proof of non-delivery**.
A transcript miss, elapsed timeout, empty composer, dead sender or restarted
target does not prove the earlier input cannot still land. A lock in agent-deck
cannot serialize the harness's private input queue or a human keyboard.

The proposed guarantees are:

1. One logical message retains one `send_id`, immutable payload and target scope.
2. Repeating one operation key never authorizes another transport attempt.
3. At most one active emitter exists for that send across cooperating workers,
   clients and restarts. Under Option A, any unresolved attempt fences later
   ones. A known landed send never emits again.
4. An attempt that may have reached the receiver blocks further emission unless
   a receiver contract deduplicates it or conclusively fences/rejects it.
5. `landed` means positive evidence in the intended native transcript, not
   transport success, a spinner, a completed unrelated turn or model execution.
6. Atomicity covers the durable decision and ownership of emission. It is not a
   transaction spanning the filesystem and the third-party harness. Crash gaps
   remain `unknown`; they never silently become permission to resend.

This chooses safety over guaranteed eventual delivery when evidence is missing.
Option B can improve liveness; merely adding a sender-side idempotency key cannot.

## Evidence scope and pins

All unprefixed `path:line` references below name **agent-deck commit
`0549d06f93a542bae2360e8afc49768be0fd05ff`**, not the moving branch. Inspect with
`git show 0549d06f:<path> | nl -ba`. The supplied worktree started at
`f7017710f879f2a232c1616ff43dded8247573e1`; its intervening changes do not alter
the send/queue sources cited here. The design does not reset that worktree.

Other repositories are pinned separately:

- `DR:` = deck-remote `e1a2b94c5b26b1855e24362bfecff81c167a9aad`, inspected in
  `/Users/rdesmet/shared-work/git/ai-tooling/deck-remote`. `redeliver.go` and
  `askqueue.go` are unchanged since the supplied merge `aedcf01` (#13).
- `TWR:` = tower `1339d9b8f98d3d3194725c2aa70ec65a2c63fb67`, inspected in
  `/Users/rdesmet/shared-work/git/ai-tooling/tower`; `scripts/sendq.sh` is clean.

These are source findings, cross-checked against existing test assertions, not
new live reproductions. No live session commands, records, transcripts or tmux
input were used. Native tool behavior beyond these integrations needs isolated
acceptance testing. Proposed fields and contracts below are explicitly new.

## Current state machine

### Records, entry points and ownership

The outbox is one JSON record per send in the profile's `sendqueue/` directory.
It contains `send_id`, target identity, `message`, `images`, `attempts`, deadline,
child PID/result evidence, transcript path/offset, `landed_row_id`, `landed_at`,
`settled`, and a separate optional enqueue `key`. Its retained native identity
field is `claude_session_id`; there is no equivalent top-level Codex identity in
this record (`internal/sendqueue/sendqueue.go:67-118`).

`--queue` and ordinary asynchronous `--json` route into this outbox. Attachments
are copied before enqueue; the resulting message and paths are retained
(`cmd/agent-deck/session_send_cmd.go:299-319`;
`cmd/agent-deck/session_send_queue_cmd.go:43-96`). The CLI calls `Enqueue`, not
`EnqueueOnce`, with a 30-minute deadline and then starts the target worker
(`cmd/agent-deck/session_send_queue_cmd.go:157-192`,
`internal/sendqueue/sendqueue.go:39-40`). Internal `EnqueueOnce`
serializes its key mapping under `.keys.lock` and returns an existing retained
record; it does not reopen it or compare a new payload for equality
(`internal/sendqueue/enqueue.go:68-112`).

| Boundary | Current transition / behavior | Pinned evidence |
| --- | --- | --- |
| Enqueue | New ULID; `queued`, or `failed` if target not running and not `wait_while_stopped` | `internal/sendqueue/enqueue.go:121-142` |
| Scheduling | Per-target worker flock; oldest pending `queued`/`typing` first | `cmd/agent-deck/session_send_queue_cmd.go:319-350`, `cmd/agent-deck/session_send_queue_cmd.go:421-428` |
| Before child | Save `typing`, increment attempts, stamp `sent_at`, transcript offset, clear previous child evidence, then start child | `cmd/agent-deck/session_send_queue_cmd.go:735-800` |
| Definite refusal | `target_busy`, `composer_blocked`, `acceptance_refused`, or pretyping `menu_open` return to `queued`; persistent unavailable Codex identity can end `failed` | `cmd/agent-deck/session_send_queue_cmd.go:500-546` (classification), `cmd/agent-deck/session_send_queue_cmd.go:866-877` (transition) |
| Child success | Confirmed submission becomes `submitted`; other success becomes `typed`; even a typed record can carry `verdict=delivered` | `cmd/agent-deck/session_send_queue_cmd.go:843-865` |
| Ambiguous child | Crash, missing result and other uncertain outcomes become `typed`, never a safe-to-retry `failed` | `cmd/agent-deck/session_send_queue_cmd.go:517-546`, `cmd/agent-deck/session_send_queue_cmd.go:879-905` |
| Land watch | Matching native row becomes `landed`; unsupported tool or expired watch sets `settled=true` without retyping | `cmd/agent-deck/session_send_queue_cmd.go:909-951` |
| Terminal | `landed`, `failed`, `cancelled`, or any settled record is final | `internal/sendqueue/sendqueue.go:120-123` |
| Retention | Finished records retained seven days; pruning also deletes result, control, message and enqueue-key files | `internal/sendqueue/sendqueue.go:63-64`, `internal/sendqueue/sendqueue.go:322-341` |

Failed-before-typing paths also include target removal/stoppage, expired busy or
refusal budget, and enforced cost budget at the typing boundary
(`cmd/agent-deck/session_send_queue_cmd.go:598-612`,
`cmd/agent-deck/session_send_queue_cmd.go:635-693`,
`cmd/agent-deck/session_send_queue_cmd.go:740-755`). These are distinct from a
post-input hook rejection.

There is also a specialized routed-event path: `WaitWhileStopped` records can
follow an archived target's successor and have their target fields changed
(`cmd/agent-deck/session_send_queue_cmd.go:353-419`,
`cmd/agent-deck/session_send_queue_cmd.go:614-639`). Initial redelivery should
refuse these as `unsupported` with `routed_send_policy`, leaving their normal
handoff behavior intact. Extending immutable-target redelivery to routed events
requires a separately specified lineage policy.

### Queue release, status and locks

`queue release` only delivers a record still `queued`. It skips queue waiting
and backoff, skips the readiness wait (`--no-wait`) and relies on a guarded child with
`--require-input-prompt --no-wait`
(`cmd/agent-deck/session_queue_cmd.go:208-281`;
`cmd/agent-deck/session_send_queue_cmd.go:1042-1061`). The prompt guard forces
tmux even if Claude socket transport is enabled
(`cmd/agent-deck/session_send_transport.go:319-323`).

For records beyond queued, release/cancel return `cancelled`, `not_found` for
`failed`, `unknown` for `typing` or an attempted send lacking child evidence,
otherwise `already_sent`. Thus the prompt's typed/submitted shorthand has an
important missing-evidence exception. `delivered` from release means either
`submitted` or `landed`; it is not a transcript-only outcome
(`cmd/agent-deck/session_queue_cmd.go:174-205`,
`cmd/agent-deck/session_queue_cmd.go:384-386`).

The control file is created with `O_EXCL`; the worker writes a separate result,
and the calling CLI removes both when it consumes the result. A timeout reports
`unknown`; it does not establish cancellation
(`cmd/agent-deck/session_queue_cmd.go:147-171`,
`cmd/agent-deck/session_queue_cmd.go:402-445`). These files are not a persistent
request-key/result ledger suitable for repeatable re-delivery operations.

`send-status` loads and returns the record, exits 2 if unknown, and starts a
worker for a nonfinal record. It is **not read-only**; settled records do not get
a fresh proof scan through this path
(`cmd/agent-deck/session_send_queue_cmd.go:219-263`). The default transcript watch
window is two minutes (`cmd/agent-deck/session_send_queue_cmd.go:286-290`).

Three existing synchronization domains matter:

- Queue worker: target flock in the profile queue directory
  (`internal/sendqueue/sendqueue.go:345-378`).
- Watcher: separate `watch-<send-id>` flock; it also updates the record
  (`cmd/agent-deck/session_send_queue_cmd.go:468-487`).
- Transport: `AcquireSendLock(inst.ID)` across readiness, input and verification;
  Codex acceptance acquisition precedes this transport call
  (`cmd/agent-deck/session_send_transport.go:302-318`;
  `cmd/agent-deck/session_send_cmd.go:409-447`).

Record `Update` is load/mutate/save without its own lock; `Save` uses temporary
file plus rename, without an fsync in that function. Existing ownership rules
must be extended before allowing a new writer to reopen records or race a
watcher (`internal/sendqueue/sendqueue.go:231-244`,
`internal/sendqueue/sendqueue.go:296-304`).

### Existing proof and integration limits

`FindLanded` reads native Claude/Codex data from the saved offset; a user row or
command row must match whitespace-folded text (Claude pasted-content wrappers
are handled). A Claude queued row counts only once absorbed or replaced by its
user row. It checks timestamps with two seconds' skew. Missing files, truncated
files, unsupported formats and misses all return a false result; scan errors
are not exposed in its return type
(`internal/recall/query/rows_landed.go:10-99`). Direct rows currently support only
Claude and Codex (`internal/recall/query/rows_source.go:81-87`).

The watcher resolves a transcript path only while the saved path is empty; it
does not chase a changed nonempty path during the loop
(`cmd/agent-deck/session_send_queue_cmd.go:924-943`). The general rows cursor has
anchor validation for rewritten/shortened sources, which the new proof reader
can reuse (`internal/recall/query/rows_source.go:113-150`).

Claude socket success is an unacknowledged write with a freshly generated
`msg_id`. A write-phase failure is committed/ambiguous, not permission to fall
back and resend (`internal/send/claudesocket.go:346-418`;
`cmd/agent-deck/session_send_transport.go:269-275`). This code establishes no
receiver-side duplicate suppression contract. A stable `msg_id` alone would
not establish one either.

Codex already has a durable submission marker and acceptance lock. Ambiguous
markers stay unresolved until newer generation/input evidence or definitive
nontransport evidence; clearing checks the exact marker's ownership
(`internal/session/codex_submission_marker.go:27-44`,
`internal/session/codex_submission_marker.go:80-185`). Re-delivery must compose
with that protocol, not delete its marker to make a retry pass.

OpenCode has a Recall reader for session/message/part JSON storage, with user
roles and text parts. It is not wired into the queue's direct landed checker
(`internal/recall/reader/opencode.go:16-23`,
`internal/recall/reader/opencode.go:143-258`;
`internal/recall/query/rows_source.go:85-87`). Shell likewise has no supported
direct landed reader; the worker settles unsupported tools
(`cmd/agent-deck/session_send_queue_cmd.go:914-922`).

Remote forwarding admits send/status and queue controls on the owner host
(`cmd/agent-deck/remote_exec.go:19-40`, `cmd/agent-deck/remote_queue.go:9-21`).
An SSH-backed instance instead selects tmux, not a locally resolved Claude
socket (`cmd/agent-deck/session_send_transport.go:130-144`). Codex local rollout
resolution explicitly rejects a nonlocal source
(`internal/session/instance_transcript.go:71-86`). These two remote shapes must
remain distinct in the new API.

Tower's heuristic captures a pre-send byte offset, seeks a 12–40 character
ASCII slice (or slash name), then polls for roughly 45 seconds. It can label a
miss `lost` or a busy target `queued`; remote checking remains `unverified`
(`TWR:scripts/sendq.sh:50-68`, `TWR:scripts/sendq.sh:89-143`). This is useful
diagnostic evidence, not a safe negative proof: substring collisions, role
confusion, delayed writes and replacement files remain possible. Its optional
Enter retry is separate operator policy, not a primitive to transplant
(`TWR:scripts/sendq.sh:145-152`, `TWR:scripts/sendq.sh:235-244`).

Deck-remote currently releases only queued sends; it refuses failed/cancelled
and ambiguous states. Its UI maps settled typed/submitted to `lost`, which is
not an agent-deck record state (`DR:redeliver.go:21-60`,
`DR:askqueue.go:129-166`). The handler status-checks before release, validates
session/profile association and detaches the request from phone cancellation
(`DR:redeliver.go:63-133`). It removes final sends from the active tracker
(`DR:askqueue.go:277-294`); automatic releases are limited to queued records,
three attempts and fresh idle/empty-prompt hints (`DR:redeliver.go:16-19`,
`DR:redeliver.go:136-189`).

Independent source cross-checks: failed control outcomes are asserted at
`cmd/agent-deck/session_queue_cli_test.go:92-116`; crash recovery forbids retyping
at `cmd/agent-deck/session_send_queue_restart_test.go:55-128`; landed matching
tests distinguish absorbed/removed/old messages at
`internal/recall/query/rows_test.go:618-681`. These tests were read, not run.

## Options and decisions

| Decision | Option A | Option B | Recommendation |
| --- | --- | --- | --- |
| Safety/liveness contract | Sender serializes decisions; retry only proven safe; ambiguous input blocks | Receiver atomically deduplicates a delivery key and persists acceptance/transcript receipt | A now; B is required for unconditional recovery of ambiguous sends |
| Durable storage | Versioned JSON aggregate per send plus immutable attempt artifacts, all mutations under a record lock and fsync | Move sends/attempts/operations into SQLite transactions | A for this feature; existing filesystem layout keeps migration scoped. SQLite does not solve the harness crash gap |
| User surface | New `session redeliver` with explicit outcomes and operation key | Extend `queue release` to reopen terminal records | New verb; preserve release/cancel semantics and old clients |
| Correlation | Native row/request ID where available; exact payload plus frozen source fence otherwise, with ambiguity reported | Inject a delivery token into every prompt | Native correlation first; no mandatory text rewriting. Tokens may be an opt-in adapter capability, never for shell/slash commands |
| Typed composer recovery | Refuse ambiguous/remainder input; later use receiver-owned buffer token and atomic submit | Infer ownership from rendered text and press Enter or clear/retype | Receiver-owned buffer only. Screenshot/text equality is not ownership or exclusion of a pending copy |

Option B means cooperation **at the point the harness accepts input**, not a
proxy that records “sent” before/after issuing tmux keys. A suitable receiver
provides `accept_once(delivery_key, payload_digest, expected_epoch)` and lookup
by key; same key/digest returns the same durable native message receipt, and a
different digest conflicts. Its dedup reservation, recoverable inbox and
transcript append must share a transaction or an equivalent recoverable
protocol. If an entry is only queued, report pending, not landed. Do not claim
exactly-once execution of arbitrary model tools or shell effects.

No current Claude socket, Codex acceptance marker or OpenCode reader cited above
is evidence of this full contract. Capability flags stay off until crash/replay
acceptance proves each adapter. A sender-side token in prompt text helps
correlation but does not implement receiver deduplication.

## Proposed API and durable model

Canonical mutation, including expected target guards:

```text
agent-deck -p <profile> session redeliver <send-id> \
  --request-id <operation-key> --expect-session <deck-session-id> \
  --expect-revision <revision> --json
agent-deck -p <profile> session redeliver <send-id> --check-only --json
agent-deck -p <profile> session send-status <send-id> --no-resume --json
agent-deck remote <owner-profile-alias> session redeliver <send-id> ... --json
```

The remote alias must resolve to the expected owner/profile; return and validate
both in the receipt. Add the new verb to forwarding rather than introducing an
unscoped local fallback. Mutating calls require `request-id`, `expect-session`
and `expect-revision`; there is no fresh-key-per-HTTP-retry default. `check-only`
reads native proof and returns an eligibility snapshot without saving, starting
workers, extending deadlines or
typing. `send-status --no-resume` is a new read-only snapshot option; preserve
the old default for compatibility. Neither snapshot authorizes a later write:
mutation rechecks everything.

Default response wait: five seconds, optionally `--wait <duration>` up to thirty
seconds. A deadline returns `pending` after durable registration; it does not
cancel the operation. Use a bounded two-minute proof watch initially, then
`blocked_unknown`; a later explicit check can discover a late landing without
authorizing more input. These are proposed policy defaults, not non-delivery
timeouts. Capability discovery uses
`session redeliver --capabilities --json`, with protocol version and per-tool
`proof`, `safe_retry`, `receiver_dedup`, and `owned_buffer_submit` flags.

Internal entry point: `Redeliver(ctx, resolvedOwner, Request) -> Result` in a
delivery coordinator used by the CLI and worker. The request contains only
`send_id`, `request_id`, expected session/revision and optional bounded wait.
It cannot supply replacement text, attachments, target, transport override,
budget bypass, or a caller-provided transcript path. No new HTTP server is
needed; deck-remote keeps its existing POST endpoint.

Three distinct identities:

- Logical delivery key: `(owner_host_id, resolved_profile, send_id)`, immutable
  across attempts and target process restarts. Native conversation/epoch guards
  are stored separately; a changed epoch must not create a fresh dedup key.
- Operation key: bounded opaque `request_id`, unique within owner/profile and
  bound to the send and normalized request fields. Same key with different
  send/options yields `idempotency_conflict`; same key returns its operation.
- Attempt key: monotonic `attempt_no` plus random `attempt_id` and fencing
  generation. Every child result and proof observation names its attempt.

Do not use text hash as the logical key: identical intentional messages are
distinct sends. Freeze payload bytes, existing attribution envelope, attachment
manifest/digests, sender and delivery policy at initial enqueue. Retry reuses
them; missing/changed attachments refuse. A payload digest is internal integrity
metadata, not transcript content returned to clients. The preexisting enqueue
`Record.Key` remains a separate namespace.

Proposed version-2 aggregate fields:

| Field group | Fields / role |
| --- | --- |
| Version/ownership | `schema_version`, `revision`, `owner_host_id`, `profile`, `session_id`, `tool`, `native_session_id`, `target_epoch` |
| Logical send | Existing `send_id`, immutable payload/policy, `payload_digest`, original creation/deadline |
| Attempts | `attempt_id`, `attempt_no`, `fence`, `phase`, `transport`, process identity, immutable result reference, start/end times, per-attempt source fence |
| Operations | Request key binding, `expected_revision`, `decision`, associated attempt, immutable terminal result; alias to an existing active attempt when coalesced |
| Proof | `status`, `strength`, `reason_code`, `native_session_id`, `source_generation`, `row_id`, `observed_at`, adapter/version; internal cursor/anchor |
| Recovery | `redelivery_state`, `active_operation_id`, `retry_after`, structured `nonacceptance_receipt`, original legacy evidence |

Keep compatibility `state`, `verdict`, `settled`, `attempts` and landed fields;
the new orthogonal `redelivery_state` is `idle | pending | blocked | complete`.
Clients must not interpret `complete` as delivered: it means the operation has
finished deciding. `proof.status=landed` is the only new strict delivery claim.
Legacy landed claims remain suppressive even if not upgraded to strict proof.

JSON result includes `schema`, owner/profile, send/session/request IDs,
`revision`, `state`, `redelivery_state`, `outcome`, `reason_code`, `attempt_id`,
`emission`, `retryable`, `proof`, and `operation_final`. `emission` is
`none | possible | confirmed`, so a crash cannot be misreported as no input.
No message, pane dump, raw child error, transcript text or local absolute
transcript path is returned.

| Outcome | Meaning |
| --- | --- |
| `already_landed` | Positive verified proof; no emission |
| `landed` | This operation's attempt now has verified proof |
| `pending` | Durable operation/attempt still executing or watching; repeat key/poll |
| `refused` | This operation emitted nothing; e.g. composer occupied, budget exceeded, target stopped |
| `blocked_unknown` | Prior attempt might still land, proof unreadable/ambiguous, legacy landed claim, or target identity changed |
| `unsupported` | Tool/remote lacks required adapter or protocol capability |
| `cancelled` / `not_found` | Original cancelled, or ID absent/expired; never create a new send |
| `conflict` | Revision, request-key binding, target or payload integrity mismatch |

Exit 0 means a valid decision/result, including refusal; exit 2 is invalid
arguments, exit 1 means the CLI could not obtain a trustworthy result. A transport
disconnect is not a failure-to-send verdict. `emission` describes this operation,
not whether any earlier attempt wrote bytes. Retrying a key returns its stored
decision/attempt, alongside a current send snapshot that may monotonically gain
proof. It does not reevaluate a terminal refusal into a new attempt.

## State transitions and the atomic decision

For a new request, first bind the request key and check expected target/revision
under the record lock. Resolve repeated keys **before** revision checks: the
original request's revision will necessarily be stale after it succeeds.
Different keys aimed at an active attempt coalesce or conflict; they never queue
additional attempts behind it. Coalesced operations finish with that attempt;
they do not later turn into fresh authorizations.

The worker/transport coordinator then executes:

1. Reload record and target identity, acquire the ordered locks below, reconcile
   all prior attempts and native proof. Final persisted landed proof dominates
   any retry request. A possible/legacy match suppresses emission without
   inventing stronger proof. Cancelled/absent/changed-target sends refuse.
2. If another child might still emit, return pending/blocked. PID existence or
   elapsed lease alone is insufficient to decide ownership; use process start
   identity and the transport lock, then durable attempt fencing.
3. Authorize a fresh attempt only when every previous attempt is proven
   `not_sent`, or a verified receiver can deduplicate the same delivery key.
   A receiver's `rejected_final` receipt must fence delayed acceptance of that
   attempt. Missing transcript rows are never this receipt.
4. Immediately before input, recheck proof, target epoch, permissions, cost
   budget, attachment integrity and empty/owned composer under transport
   ownership. Preserve `require_input_prompt` and original attribution. No
   automatic Ctrl+C, draft clearing, interrupt, budget bypass or raw Enter.
5. Commit and fsync the attempt's `emission_reserved` state, source fence and
   operation association before any input can escape. This is the local
   linearization point. An input-capable child must itself validate ownership
   after acquiring transport locks; merely having been spawned is insufficient.
6. Emit once, record the attempt-specific transport result, release transport
   locks, then watch proof with a bounded deadline. A crash after reservation
   and before a definitive result is `unknown`, even if it likely typed nothing.
   A status event follows durable state; an event is not the source of truth.

| Prior record/evidence | Action and resulting state |
| --- | --- |
| Any state with strict landed proof | Persist `landed`, complete operation `already_landed`; no input |
| `queued`, no unresolved earlier emission | Guard and attempt same send; `typing -> typed/submitted -> landed` or blocked |
| `failed`, supported provenance proves all attempts untyped | Reopen same ID under this operation; append attempt; retain old failure/history |
| `typing` or active child | Join existing attempt; never start another |
| `typed/submitted`, late landed proof | Upgrade to `landed` without input, even if formerly settled |
| `typed/submitted`, no row, no final rejection | Preserve state; `redelivery_state=blocked`, `blocked_unknown` |
| Typed bytes in receiver-owned unsubmitted buffer | Only a capable adapter may atomically submit that exact buffer token; no repaste. Crash ambiguity blocks a second submit |
| Definitive rejection/drop, no accepted copy, old attempt fenced | New attempt allowed subject to policy/guards; proof watch still required |
| `cancelled`, missing, expired or different target identity | No reopening by this primitive |

`failed -> queued` is not the implementation by itself. The operation, attempt
history and safety evidence must commit together before the worker can see the
reopened work. The operation has a fresh bounded execution deadline; it never
silently grants the original send another unbounded 30-minute retry budget.
One request authorizes at most one potentially emitting attempt. A pre-input
refusal ends that operation; a deliberate subsequent action uses a new key and
fresh revision. Normal queue scheduling of a still-queued original send remains
possible and is reflected in the current snapshot.

Example adversarial schedule: attempt 1 writes, its result is lost, the harness
delays its transcript append, and requests 2 and 3 arrive. Both must join or
block on attempt 1, even if their scans find nothing. When the row arrives,
reconciliation marks the original send landed. Neither request may have emitted.

## Landed-proof adapters

New adapter return type: `landed | not_sent | pending | unknown | unsupported`,
plus structured evidence and source coverage. Separate a successful scan with
no match from an I/O/parse/identity error, but **both are insufficient by
themselves to authorize re-emission**.

Every attempt captures owner/profile, deck ID, exact native conversation,
process/target epoch, source generation, complete-record cursor and anchor
before emission. Checks cover all historical attempts, including the original
source after a restart. A new file at an old path is not the old source. Partial
lines, rotation, truncation, missing history, budget-exhausted scans and unknown
format versions return unknown coverage. Wall-clock timestamps supplement
identity/position; they do not substitute for them.

For a send with no original native binding and all attempts proven untyped,
first binding to the expected deck session's verified native identity is allowed
under the record/transport locks. Replacing an already-bound native identity is
retargeting and remains refused. This permits recovery of a fresh stopped target
without treating an unknown historical conversation as interchangeable.

| Tool | Positive evidence | Safe retry / limitations |
| --- | --- | --- |
| Claude | Native user UUID in the bound conversation, with exact adapter-decoded payload; or exact queued item conclusively absorbed. Parse documented pasted-content/envelope variants explicitly. A plain enqueue, assistant echo, tool output or pane substring is not landed | Definite pre-input refusal works now. A cache-tax screen marker is only a hint. Broader recovery requires a correlated terminal drop receipt plus receiver fencing, or `accept_once`; socket write success/`msg_id` is insufficient |
| Codex | Exact bound rollout user input row/item, tied to the attempt's accepted native input/turn identity and payload. Support input absorbed into an already-running turn as well as a new turn | Preserve and associate the existing submission marker with the send/attempt. A newer turn alone or disappearance of the marker does not prove this payload landed. No clearing an unresolved marker or composer fallback to force recovery |
| OpenCode | Proposed direct adapter reads the exact session's native user message ID and complete ordered text/attachment parts. Reuse reader parsing where appropriate; bypass the lagging Recall index | Probe actual storage/schema capability. JSON tree support in the pinned reader is not evidence all installed versions use that layout. Unknown layouts/partial parts block; no general dedup guarantee assumed |
| Shell | No native conversation proof in the initial capability set | Return unsupported for strict transcript proof/recovery. No prompt token injection, history grep, echoed command, output or exit-code inference. A future opt-in supervised executor could keep a durable receipt journal, but cannot make arbitrary shell side effects exactly once |

Identity strength is explicit. A correlated native receipt plus payload/source
validation can establish strict `landed`. For legacy text-only records, an exact
full message in the correctly fenced user/absorbed row can establish content
presence, but duplicate identical rows or unowned concurrent input make send
attribution ambiguous. Return `blocked_unknown` with `content_present` evidence
instead of retyping or falsely claiming unique attribution. Do not use the
current whitespace folding to equate semantically different multiline code.
Versioned normalization may undo known transport wrappers/CRLF only; it must
preserve payload meaning and attachment identity. Optionally negotiated
correlation tokens can improve new text sends, with deliberate user-facing
semantics; they are not required for conservative recovery of untyped failures.

Landed proof is monotonic: retain a content-free proof receipt even if the
transcript is later compacted/deleted. An already-proven send must never become
eligible because its source vanished. Conversely, a pending receiver queue
entry remains pending until absorbed/accepted or conclusively rejected.

## Failure cases and recovery decisions

| Case | New primitive's required decision |
| --- | --- |
| Composer occupied by human, picker, permission UI, unreadable pane | No typing/clearing/Enter. `refused` for a provably untouched attempt. Preserve draft bytes. Snapshot readiness is rechecked at the emission boundary |
| Cache-tax drop after input | Do not infer safe retry from a visible drop string or missing row. Correlate to attempt, distinguish a policy rejection from acceptance, require final rejection/fencing. If policy still blocks, return `refused`; never implement tower's pay/clear/succeed policy here |
| Typed but never submitted | Do not append a second copy. Exact receiver-owned buffer + atomic conditional submit can resume; ordinary terminal-text comparison cannot. Legacy typed records with no such ownership stay blocked |
| Transcript writer is slow / Recall index lags | Continue proof reconciliation within bounds; then blocked unknown. Timeout is a UI diagnostic, not a non-delivery certificate |
| Target restarted in same native conversation | Check original source and any certified continuation; fence old process/queue. Rebind the transport epoch only after proving the old attempt cannot emit. Process death alone does not prove no prior landing |
| New native conversation / cross-tool successor | `target_changed`; no automatic replay into another conversation, even if the deck title/ID is reused. Explicit retargeting is a separate future operation |
| Worker dies, orphan child lives | New coordinator cannot reserve another emission. It joins/reconciles the old attempt; fencing rejects stale children before input. Unknown after write remains blocked |
| Caller/SSH response is lost | Retry same owner/profile/send/request tuple. Durable result survives client exit; no second `send` and no control-file cleanup dependency on caller |
| Remote host unavailable | Pending/unknown transport result. Proof and decisions run on owner host. Never fall back to a local matching ID/path or resend locally |
| Local SSH placeholder with no owner-side queue mapping | `unsupported` for strict recovery. Require a verified owner-side logical record; do not fabricate one from local payload/history |
| Send record pruned or attachment missing | `not_found`/`conflict`; no reconstruction by text. Persist compact tombstones for known IDs/operation keys through the supported retry horizon |
| Manual input races agent-deck | Existing cooperative locks cannot exclude it. Recovery requiring owned composer state stays disabled without receiver-side conditional submission |

## Locking, persistence and crash recovery

Use a **record lock shared by every writer**, including enqueue publication,
worker, proof watcher, release/cancel, handoff, prune and redelivery. Each update
reloads current revision under that lock, applies a legal monotonic transition,
and writes one aggregate using unique temporary file, fsync, atomic rename and
directory fsync. Do not use unlocked `Update` for version-2 records. Attempt
artifacts have unique immutable names; late results cannot overwrite a newer
attempt. Persist all safety-critical errors rather than ignoring save failures.

Maintain this ordering when more than one lock is needed:

```text
target queue-worker lock (scheduler only)
  -> native acceptance lock (Codex, when applicable)
  -> target transport/send lock
  -> record mutation lock (short transaction)
```

The request registrar and proof watcher acquire only the record lock, release
it, then notify the scheduler. They never wait for a worker/transport while
holding it. No parent holds the transport lock while waiting for a child that
must acquire that same non-reentrant lock. Either the input-capable child owns
acceptance/transport/record validation, or the coordinator performs input
in-process; recommend extracting the shared emission function for the latter
where practical, with explicit ownership for every entry point.

Resolve the effective profile before constructing any key/path. Record and
worker keys include owner/profile. Preserve the existing host-wide target lock
compatibility with all senders; adding a profile-only lock that stops contending
with direct sends would be unsafe. Acquire an additional native-endpoint lock
when distinct deck aliases can address one pane/conversation. Lifecycle actions
that replace the process/target must participate in epoch fencing; if they
cannot, redelivery rebind fails closed. Profile resolution and no cross-profile
record access are mandatory even when endpoint locking spans profiles.

Request registration needs a profile-scoped operation-key index to detect the
same key used for another send. Reserve that mapping durably first, binding
send plus request digest. Under the record lock write the full operation; only
then dispatch. A crash with an index reservation but no operation is recoverable
by the same request and must not allow a different binding. Order index lock
before record lock; never acquire it while holding the worker/transport locks.
The emission path uses the committed aggregate only. SQLite is the alternative
if maintaining this protocol becomes more complex than migrating storage.

Fences are not time leases. An expired deadline cannot revoke bytes already
in the receiver. A stale worker checks its attempt fence immediately before
input while holding transport ownership. If it might have crossed that point,
recovery is unknown until receiver evidence resolves it. Repeated operation
requests only join; watchdogs must never turn elapsed time into retyping.

Proof watchers may scan without the record lock, then CAS their observations
against immutable attempt/source identity under it. Strict landed evidence may
only advance the record. An observation contradicting a claimed `not_sent`
receipt is an adapter invariant failure: block emission, preserve evidence,
and test that failure mode. Cancellations serialize at the same boundary: before
reservation they can prevent input; after possible emission they cannot promise
revocation. Compatibility release and normal queue delivery must use the same
reservation path so a release cannot bypass an active redelivery operation.

All timers/watchers are stopped and joined by coordinator `Close`; input children
are reaped by their owner. CLI cancellation ends waiting, not a committed
operation. Durable pending work is resumed by owner-host workers on startup or
explicit mutation, without requiring a live phone HTTP request. A read-only
status call never starts them.

## Migration and rollout

1. Introduce capability discovery, read-only status, structured proof results and
   content-free diagnostics first. Advertise `redeliver_v1` only once all
   version-2 writers obey the reservation protocol.
2. Preserve `queue release/cancel` behavior. Upgrade them and normal dequeue to
   shared locking/fencing internally; they must not reset attempt history.
3. Add `schema_version` and lazy migration under the record lock. Preserve legacy
   timestamps, result and state verbatim as historical evidence. Never invent
   an original cursor, native identity, request key or untyped receipt.
4. Legacy `landed` always suppresses emission; a new scan may upgrade its proof.
   Legacy `typed/submitted/typing` is reconciliation-only unless definitive
   attempt evidence is recoverable. Legacy `failed` is retryable only under an
   audited compatible writer contract (the pinned implementation is one), with
   no contradictory child/result evidence; uncertain provenance blocks. Newly
   written failures carry structured `not_sent` provenance.
5. Keep version-2 records in a separate opt-in namespace during rollout. Finish
   or quarantine version-1 pending sends before migrating them. An older binary
   ignores unknown JSON fields and its writers do not participate in new locks
   (`internal/sendqueue/sendqueue.go:250-266`,
   `internal/sendqueue/sendqueue.go:296-304`); merely adding fields is not a safe
   mixed-version plan. Quiesce old writers, workers and watchers; verify that old
   children have exited, without relying on PID absence alone. Under old worker/
   watch locks, first fsync a complete v2 copy marked `migration_pending`, then
   replace v1 with a conservative terminal tombstone (`typed`, `settled=true`,
   `verdict=unknown`, reason code `migrated_to_v2`), then activate v2. The original
   v1 record remains historical evidence in v2. Recovery reconciles incomplete
   migrations before scheduling: a staged copy is never allowed to emit while
   v1 could still emit. Do not use a `failed` tombstone that suggests safe resend.
6. Rollback disables new mutations and keeps v2 records/receipts for read-only
   inspection. Do not project an ambiguous v2 record back to v1 `queued`. Remote
   owner capability is checked independently of the client's binary version.
7. Retain full records/operation results at least through the advertised retry
   horizon (initially seven days, matching existing full-record retention).
   Compact landed/expired IDs and operation bindings to content-free tombstones;
   absent/tombstoned IDs never create records. Purging tombstones ends result
   replay, not the no-recreation rule. Rate-limit keys and bound per-send history;
   reject further requests at the limit rather than evict an active dedup key.

Migration is a release/maintenance procedure, not permission for this design
task to alter this laptop's state. No automatic service restart is proposed as
part of the caller operation.

## What deck-remote calls

Keep `POST /api/rc/ask/redeliver` behind the typed-input authorization gate.
Replace its release helper with one `session redeliver` invocation. Persist an
operation UUID **before** launching the CLI; HTTP retries/reconnects reuse it.
Send expected session/revision and resolved profile/owner. A deliberate new
button action after a terminal refusal creates a new key with the latest
revision. Initial request ID, original send ID and bubble identity remain fixed.

Remove the status-then-release decision from the mutation path. A status read
may populate UI only; agent-deck validates identity, evidence and revision in
the same operation that reserves emission. On timeout, resume/poll the stored
operation instead of minting a new key. Probe owner capability via a read-only
machine-readable response; unsupported deployments show unavailable, with no
fallback to `session send` or broader `queue release` semantics.

For check-only, use the new pure check or `send-status --no-resume`. Display
delivered only for verified landed proof; preserve `landedRowId` and distinguish
submitted/pending from blocked unknown. Merge SSE/status by monotonic revision,
not timestamps alone. Preserve the composer's draft and original bubble.

Keep a durable receipt binding request/profile/owner/session/send after it leaves
the active tracker; restore active tracking when re-delivery becomes pending.
The present in-memory `releasing` flag is UI exclusion, not correctness. Do not
allow an arbitrary completed send to be rebound to an unrelated phone request.
Existing automatic release policy may later call the primitive for provably
untyped queued sends, with its budget preserved. Broad automatic recovery of
“lost” records is a separate operator decision, not enabled by this feature.

Future initial-send idempotency (`session send --idempotency-key`) would close
the lost-initial-receipt gap; it is a separate slice. This primitive requires an
existing known `send_id` and never guesses one from text. Tower can adopt the
same owner-side API once it uses durable send IDs; raw socket `msg_id` is not a
sendqueue ID (different creators:
`internal/send/claudesocket.go:372-379`,
`internal/sendqueue/enqueue.go:121-128`).

## Verification plan for implementation

Write regression tests first and demonstrate failure on the unfixed code in
containers. Use synthetic fixtures; no live transcript samples in reports.
Read-only source inspection is the verification performed for this design.

| Suite | Required assertions |
| --- | --- |
| Decision table | Every legacy/current state crossed with proof status, operation replay, revision mismatch, cancelled/missing IDs; no unsupported state falls through to emission |
| Idempotency | Same key concurrent/sequential/after process restart produces one attempt; key reused for changed send/options conflicts; different keys on active attempt coalesce without later queued emissions |
| Writer races | Release, cancel, normal worker, proof watcher, prune, handoff and redelivery racing: no lost fields, no reverted landed proof, one reservation; profile isolation and endpoint aliases contend correctly |
| Crash injection | Before/after key reservation, operation save, attempt fsync, child start, first byte, Enter, child result, proof save and response; stale child and PID reuse; fsync/disk-full/partial JSON failures never permit an ambiguous retry |
| Late arrival adversary | Pause prior receiver acceptance across a negative transcript scan and a second request; it must block or receiver-dedup, never produce two accepted messages. This is the critical safety test |
| Claude | User row vs echo/tool output; pasted-content and socket envelopes; enqueue-only, absorbed, dequeued, removed; identical payloads; fake cache-tax hint vs correlated final rejection; busy/occupied/ghost composer; no accidental Enter |
| Codex | Exact thread ownership, input in same/new turn, marker unresolved/submitted/ambiguous, native-ID rebinding, fresh rollout, two senders; no marker clearing on miss or fallback bypass |
| OpenCode/shell | Complete/incomplete parts, native ID mismatch, supported/unknown storage layouts, index lag; shell stays unsupported, with zero input side effects |
| Source fidelity | Late-born file, truncated/replaced/same-path source, rotation, clock rollback, malformed and partial records, scan limits, Unicode/multiline distinctions, attachment mutations; uncertainty never becomes `not_sent` |
| Remote | Both SSH-forwarded owner and local SSH-placeholder shapes; connection loss after commit, old server, wrong host/profile, same ID on two hosts; no controller-local proof or fallback send |
| Migration | v1 state matrix, old worker/watch drain, v1 tombstones, downgrade refusal, retained operation replay after prune; absence never reconstructs payload |
| deck-remote | Double tap, two devices, lost HTTP response/SSE, server restart, final tracker eviction, pending tracker resurrection, stale revision, same bubble/composer, unchanged automatic-attempt budget |

Use existing container verification automation after implementation:
`make verify-docker VERIFY_ARGS="--stages test --pkgs './internal/sendqueue/... ./internal/recall/query/... ./internal/session/... ./cmd/agent-deck/...'"`,
then the contributing self-check/reproduction workflow for behavior changes.
Verify Linux and macOS flock/SSH behavior in isolated environments. Before/after
timing must cover queued status, send, same-key replay and large transcript
checks; enforce bounded scan work and measure lock hold time. Do not run
`go test`, `make ci` or pre-push tests on the host.

## Delivery slices and estimates

Engineering days for one implementer, excluding review/CI waits; ranges include
regression tests. No implementation or deployment is authorized by this draft.

| Slice | Scope and exit criterion | Estimate |
| --- | --- | --- |
| 1 | Proof result type, strict identity/source fences, read-only status, capability contract; no emission behavior change | 2–3 days |
| 2 | Versioned aggregate, operation keys/results, record locking/fsync, attempt fences, writer inventory and migration/rollback tests | 3–5 days |
| 3 | Redeliver CLI/coordinator and conservative eligibility; safe failed reopening, late-proof reconciliation, ambiguous blocking; crash/race tests | 3–4 days |
| 4 | Claude/Codex adapter hardening and container acceptance corpus; preserve Codex marker protocol | 3–5 days |
| 5 | Owner-side remote forwarding, capability/version gating, deck-remote persistent retry receipt and UI mapping | 2–3 days |
| 6 | OpenCode direct proof adapter, unsupported shell contract, performance and migration acceptance | 2–3 days |

Option A total: **15–23 engineering days**. A useful Claude/Codex-only milestone
can defer OpenCode's positive proof adapter but must report that limitation.
Option B begins with a **2–3 day feasibility spike per receiver**; implementation
estimate follows verified receipt/dedup support. An extra sender WAL or arbitrary
timeout cannot be substituted if that spike finds no receiver acceptance seam.

## Operator decisions

All nine review recommendations were accepted. The R0 contract at the top is
authoritative; no open operator question or live migration is part of R0.
