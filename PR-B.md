**Stacked on the `recall: origin on user-side rows` PR (needs `Row.Seg` / `MergeRow`); review that one first.**

## What problem does this solve?

`/clear` and `/new` start a new native conversation under the same deck session. `session_links` keeps every id, but nothing exposes them, so `recall timeline <session>` shows only the live conversation and the history before a `/clear` is unreachable for a client.

## Why this change

- New read-only verb `recall chain <session> --json`: conversations oldest first (`native_id`, `first_seen`/`last_seen` unix + RFC3339, `current`, `authoritative`, `path` when on this host). The live one is last even after a resume to an older id. It needs only state.db, so it works with `[recall] enabled=false`; `<session>` may be an old conversation id.
- `recall timeline --chain` reads rows across the chain with the existing paging: `--tail N` fills from the newest backwards, `--before C --limit N` continues, each row carries `seg`, and a `boundary:<native id>` row (kind `system`, raw_type `chain/boundary`) precedes each conversation after the first. `--since`, `--agent`, `--v1` and `--transcript` are rejected with `--chain`; follow is unchanged; missing transcripts are skipped; both are allowlisted for `remote <host> recall`.

## User impact

New verb and flag; no change without `--chain`. Help text and docs (`docs/recall-timeline.md`, `docs/remote-recall.md`, cli-reference) updated.

## Evidence

Copy of the tower state.db (7 conversations): `chain` lists all 7; `--tail 1500` then `--before` paging walks 20,644 rows in 14 calls (15 s), 6 boundary rows, last `before_cursor` empty.

Tests: `internal/recall/query/chain_test.go` (tail fill, before walk, spill, empty newest conversation, missing transcript, rejects, resync reasons), `cmd/agent-deck/recall_chain_cmd_test.go` (replayed session_links history, recall off, old id, unknown session, paging without duplicates, remote allowlist), `internal/statedb/recall_hints_test.go` (old id lookup). Help goldens updated.

Run in the cloud sandbox (no docker), on top of upstream main: `go build ./...` OK; `go vet` clean; `go test ./internal/recall/... ./internal/statedb/...` pass; `go test ./cmd/agent-deck -run 'Chain|Golden|Help|RecallTimeline|RecallRows|RecallRemote'` passes. A wider `-run Remote` also matched the unrelated remote-sweep tests, which failed here (they depend on hosts/tmux); not investigated. The container `FULL_TESTS=1` run is **still owed** (operator, laptop).

**Diff size:** about +1060/-45 on top of the stacked PR (self-check against main shows +1682/-48 because it includes that one), over the ~400-line guideline; roughly half is tests, docs and goldens. It is one feature (the verb and the flag share the chain reader).

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
