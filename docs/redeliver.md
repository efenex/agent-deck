# Reconcile or reopen a durable send

```sh
agent-deck session redeliver SEND_ID --request-id TAP_UUID --json
agent-deck session redeliver SEND_ID --request-id TAP_UUID --check-only --json
agent-deck session send-status SEND_ID --no-resume --json
agent-deck remote OWNER session redeliver SEND_ID --request-id TAP_UUID --json
```

Persist one request ID before calling; reuse it after a timeout or lost response.
The owner/profile binds it to the send. Replays return the stored `outcome` and
`reason`, with a current `snapshot`; another send with that key returns `conflict`.
A deliberate new action uses a new key. `--check-only` replays an existing key;
with an unused key it inspects current eligibility. It does not bind the key,
save records, start workers or type. `--no-resume` suppresses status's usual
worker restart. All decisions return exit 0; bad arguments exit 2, operational
errors exit 1. A connection failure is not proof that nothing was sent.

| State | Result/action |
| --- | --- |
| landed | `already_landed`; never sends again |
| queued | Existing guarded queue release; usually `pending` while the worker runs |
| typing | `pending`; joins existing delivery |
| typed/submitted, including settled/lost | Scan only; `landed` on unique proof, otherwise `blocked_unknown`; never sends again |
| failed | Reopen the same ID if eligible; `pending` with reason `reopened` |
| cancelled | `refused`, reason `cancelled`; never reopened |
| absent/pruned | `not_found`; never recreated |
| unsupported tool/SSH placeholder | `unsupported`; no input |

Failed reopening requires a running matching deck session, available cost
budget, readable current native transcript with no matching content in the
covered scan window, no other tracked send of the same text still queued or typed since
creation, and an age of at most one hour by default. Configure
`[send] redeliver_max_age = "1h"`; invalid/nonpositive values disable reopening.
Claude and Codex are supported. A restarted native conversation of the same
harness is allowed. Reopening keeps attempts, increments `redeliveries`, renews
the 30-minute queue deadline and forces the guarded tmux child. It may run ahead
of newer sends because its original ULID retains its sort position.

Proof scans are bounded to 4 MiB and three seconds, the offset of the next send
of the same text when known, and ten minutes after `sent_at`. Already claimed rows and several
matching rows report `blocked_unknown/content_present`. Incomplete or unreadable
coverage also blocks. Check again with a new key for a later landing. Legacy
landed records sharing a row remain suppressive with `shared_landed_row`.

New durable sends capture `transcript_offset` before the delivery child starts:
`path`, `offset` (file size in bytes), `device`/`inode`, and `anchor_bytes`/
`anchor_hash` (SHA-256 of up to 4096 bytes just before the offset). If an inode
is unavailable, `prefix_bytes` and `prefix_hash` identify up to the first 4096
bytes; an empty file without an inode cannot provide an identity. The existing
`transcript_path` and `transcript_from` fields remain for compatibility.
Capture failure leaves the new field absent and keeps the legacy scan rules.

The snapshot only extends coverage; it never narrows it. When the scanned file
is a different path (a restarted native conversation), the snapshot is ignored
and the legacy rules apply. When the legacy window fits the 4 MiB cap, the scan
keeps it. Otherwise reopening and late proof can cover transcripts larger than
4 MiB when the snapshot still matches the file identity, the bytes before the
offset are unchanged, and the bytes from the offset to the scan's end fit the
cap. A row completed before the capture was written before anything was typed
and is not scanned; a row straddling the offset is, from its start, looking
back at most 64 KiB. Failure to find that start, a changed identity or anchor
(replacement, truncation, regrowth, a reused inode), or a window over the cap
returns `scan_limit`. Records without the new field retain the legacy behavior,
including `scan_limit` for oversized presence scans.
Coverage never depends on timestamp ordering. Timestamp filters, the next
same-text send's upper byte boundary, ambiguity and existing row claims still
apply; an offset does not authorize a unique claim when two matching rows exist.
On an oversized file, offset presence coverage starts at the latest attempt's
send time, so a row of an earlier attempt landing between a reopen check and
the next attempt's capture is not seen (the race this check cannot exclude).

The guarantee is per send ID. The presence check reduces duplicates caused by
another send or manual copy; it cannot exclude input racing that check.
OpenCode, shell, SSH placeholders and routed successor sends are unsupported.
No draft clearing, socket retry, raw Enter or Codex marker clearing is performed.

If a crash leaves `pending/request_registered`, the key remains bound and replay
does not rerun the action. Inspect the snapshot. A queued record can resume via
the normal worker or `send-status` without `--no-resume`. A fresh deliberate
action can use a new key. Request bindings survive record pruning; R0 does not
garbage-collect them. Responses contain IDs and state, not message/transcript
content. Remote calls decide entirely on the owner; an older owner reports
`unsupported` without a controller-local fallback.
