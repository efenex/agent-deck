# Synthetic peer fixtures (R1)

Created 2026-11-14 UTC from writers, without reading conversations. Every payload
is `[REDACTED]`; IDs and times are synthetic.

## Native socket user rows

`claude-socket-{user,tagged-user,string-user}.jsonl` contains native `type:user`,
`isMeta:true`, `origin:{kind:peer,from:unknown}`, text-block or string content, UUID and timestamp.

Writer provenance:

- `internal/send/claudesocket.go:SendOverClaudeSocket` at `723f4af6` writes the
  user message content unchanged. `cmd/agent-deck/session_send_envelope.go`
  applies the optional `[agent-deck from:...]` tag before enqueueing.
- Static inspection of Claude Code 2.1.325, SHA-256
  `84590fefe6338be9087f5814b61f9a54c5f21524163a4c127cb5a808e31009f2`:
  the socket receiver at byte offset 310937056 builds `origin.kind=peer`, then
  queues `mode:prompt`, `value`, `isMeta:true` (310937600 onwards). The sender
  frame has no `from`, so the native origin uses `unknown`; the fork derives
  its peer ID from the content tag instead.
  `Q4t` constructs `preamble + LF + input + LF LF + footer + optional suffix`.
  Its preambles/footer constants start at byte 282368420. Idle uses `Zh`/`hr`;
  mid-turn uses `Qh`/`hr` plus `Jc` when SendMessage is unavailable, or `Ea` otherwise.
  The native user-row mutator at byte 294432494 applies this writer to string
  content or text blocks in place; the fixtures exercise both forms.
  These are embedded writer source bytes, not strings inferred from transcripts.
- The checked-in native fixtures use the exact idle `hr` envelope from that writer.
  They cover untagged and tagged text blocks, plus tagged string content. A matcher
  unit test separately exercises both mid-turn suffixes from `Q4t`. Matching preserves the
  sender tag because the queue record already contains it.

The parser accepts only complete supported envelopes; prefix-only, partial,
unknown-footer, assistant/tool/hook, compaction and different-sender matches
remain unproven. This is content proof, not authenticated sender identity.

## Fork recall-origin row

`claude-fork-origin-row.json` is a **serialized recall Row**, not native JSONL.
It is synthesized from the same tagged native fixture using these writers:

- `upstream/recall-origin-v2` at `c375f83a9803dae12ef40b37c5a697fa0315ca60`:
  `internal/recall/query/rows.go` (`Row`, `textRow`, `claudeUserText`) and
  `origin.go` (`peerFromText`, `deriveOrigin`).
- `upstream/recall-chain-v3` at `606b09191de71d5a3e7fcee3dab8a10d9fb61b51`
  retains those writers. The CLI encodes Row/RowFrame using `encoding/json`.

The fork retains `kind:system` for meta user text, its normal first-line title,
full body, `raw_type:user`, and adds `origin:peer:synthetic-sender` derived from
the embedded tag. The matcher test decodes that JSON into the current Row,
which ignores the fork-only origin field, then requires complete envelope and
full-message equality. No fork branch was merged, and no origin field or
serialized-recall input adapter was added to the native transcript parser.

## Transport gate

An envelope row proves only a send whose delivering child reported
`transport: socket` and that was not guarded (`--require-input-prompt` is
always typed into the pane). `LandedWindow.PeerEnvelope` carries that choice;
the failed-reopen presence check keeps envelopes on, where any copy blocks.
`cmd/agent-deck/session_redeliver_transport_test.go` builds the evidence from
`sendSuccessData`, the payload the queue child writes.
