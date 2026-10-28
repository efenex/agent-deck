## What problem does this solve?

A client reading `agent-deck recall timeline` rows cannot tell who put a user-side row into the conversation. A peer's `[agent-deck from:<id>]` message arrives as a human-typed `user` row; queued_command peer messages, sub-agent hand-backs and Stop-hook injections arrive as `system` rows; and a `!` shell line is two raw user rows (`<bash-input>` / `<bash-stdout>`). One 108 MB transcript had 54 raw `<bash-input>` rows and 18 untagged peer messages.

## Why this change

Rows get an additive, `omitempty` `origin`: `human`, `peer:<from-id>`, `hook`, `task-notification`, `system`. Order of evidence: the `[agent-deck from:<id>]` tag (also inside `<pasted_content>` and after Claude's "Another Claude session sent a message:" line) or an `<agent-message from="<id>">` envelope; then Claude Code's own `origin` object; then text/flags for older transcripts (`Stop hook feedback:`, `[INBOX]`, `<task-notification>`, `isMeta`). Row `kind` is unchanged, except that a `!` line becomes one `bash` row (`meta.user_shell`, origin human) that an `update` completes when `<bash-stdout>` lands. The pending row id rides in the follow cursor; an output without its input in view is a finished bash row. `MergeRow` merges `origin` and `seg`.

## User impact

`recall timeline` rows gain an `origin` field and `!` shell lines show as one bash row. Existing fields and kinds are unchanged; goldens are pure additions.

## Evidence

Tower transcripts (7 /clear segments, 20.6k rows): user/human 1251, user/peer ~100, system/hook 1108, system/task-notification 775, system/peer:unknown 226, bash user_shell 76.

Tests: `internal/recall/query/origin_test.go` (derivation table, all record shapes, Codex and v1 turns, MergeRow, shell pairing including across a follow-cursor resume, `--since` after the output record, shell output stream folding, origin precedence). Goldens regenerated.

Run in the cloud sandbox (no docker): `go build ./...` OK; `go test ./internal/recall/...` passes (sandboxed HOME). The container run (`FULL_TESTS=1`) is **still owed** (operator, laptop).

**Diff size:** +619/-10, over the ~400-line guideline: about 390 lines are tests and goldens, and the production change is one new file (`origin.go`, 143 lines) plus a small edit to `rows.go`.

**Self-check (cloud sandbox, base upstream/main):** gofmt, go vet, go build, test-added, no-changelog and the body gates pass; the sandboxed-tests and revert-check steps did not run (no container).

## AI disclosure

- [ ] Human-written
- [ ] AI-assisted (I directed and reviewed it)
- [x] AI-authored (a model wrote most of it)

**Model(s), if AI helped:** claude-sonnet-5-5 (wrote it); an advisor model reviewed.

**Prompt / session log (optional):**

## What actually bothered you

<operator>

## Checklist

- [x] Targeted diff: one problem, no unrelated changes
- [x] Tests added or updated for new behavior
- [ ] Test suite passes sandboxed, inside a container: container run NOT done (owed, operator)
- [x] If this touches a hot path: not a hot path
- [x] CHANGELOG.md untouched (entries are added at landing)
- [x] AI-assisted? Disclosed above, with validation evidence, and I can answer questions about the code
- [ ] "Allow edits from maintainers" is enabled (set at filing)

<!-- gate:ai=authored model=claude-sonnet-5-5 intent=yes -->
