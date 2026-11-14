# Transport-aware send readiness

`session send` with `send_transport = "auto"` validates Claude's socket identity
before waiting for a terminal composer. A validated socket text send can proceed
without a composer, including when the pane still reports background activity.
The socket is revalidated under the existing send lock after any foreground
deferral. Socket acceptance still
means `queued_socket`, not proof that Claude processed the message.

`--defer-if-busy` waits for a fresh foreground turn signal. Each hook observation
is reloaded from disk and goes through the existing session ownership checks.
Missing, stale (older than the hook freshness window, 2 minutes for Claude),
implausibly future-dated or rejected hooks do not become idle evidence; the
fallback reads a fresh pane and holds on foreground-busy evidence (spinner,
working title, running-turn line), startup, a terminal state or an unreadable
pane. Background work alone does not hold. This stage never requires a
composer: keyboard delivery applies its own composer, draft and menu guards,
and a validated socket send needs none. The signal is checked again at
delivery. Background work remains visible in global session status.

A socket refusal before any write restores full terminal readiness, draft and
menu guards before fallback bytes, including after an early socket validation
or a send requested with `--no-wait`.
Bare slash commands, `--draft` and `--require-input-prompt` keep their terminal
semantics. An error after socket writing starts never falls back to keystrokes.

Keyboard readiness uses a monotonic deadline that includes pane probes and
settling time. Tmux identity and capture subprocesses share the remaining budget
(at most three seconds per probe). Startup still needs three consecutive prompt
sightings. Capture failures, terminal errors and dead panes refuse input. A
historical prompt glyph does not establish a current composer: Claude needs its
framed input region or the older prompt-plus-known-footer layout, without an
operator draft, live foreground spinner or menu.
An operator draft returns the existing retryable `composer_blocked` refusal
promptly, so a queued attempt does not occupy the whole readiness budget.

JSON readiness rejections carry a `readiness` object:

| Field | Meaning |
| --- | --- |
| `stage` | `defer_foreground` or `tmux_readiness` |
| `last_status`, `last_status_reason` | Last observed state and its classifier |
| `last_reason` | Reason input was refused, or the last probe error |
| `status_counts`, `status_errors`, `capture_errors` | Observations during this wait |
| `composer_present` | Whether current composer evidence was observed; `null` when unreadable or unprobed |
| `elapsed_ms` | Actual elapsed wait time |
| `target_id`, `pane_id` | Deck target and captured tmux pane; empty pane ID means no pane was observed |

No pane contents or draft text are included. These diagnostics supplement the
existing error code and delivery verdict.

## Reproduction and tests

`tests/repro/transport_readiness.py` is container-only. It creates a temporary
HOME, XDG directories, database, private tmux server and dummy Unix socket peer.
The dummy peer renders an active pane with no composer and records every socket
message and keyboard byte. No agent CLI is started. The script returns 0 for all
healthy controls, 1 for the specific readiness-before-socket defect, and 2 for a
harness error. Run the identical script against base and fixed binaries:

```sh
python3 tests/repro/transport_readiness.py /absolute/path/to/agent-deck
```

Acceptance coverage lives in `internal/send/ready_*test.go`,
`internal/tmux/readiness_test.go`, `internal/session/foreground_hook_test.go` and
`cmd/agent-deck/session_send_readiness_test.go`. The four saved pane fixtures under
`internal/send/testdata/readiness/` are synthetic structural fixtures, not a
reconstruction of the unavailable incident pane. Existing captured background
work fixtures also exercise the production classifier.

All Go tests must run in containers, as required by `AGENTS.md`. On a small Docker
VM, use `GOFLAGS='-buildvcs=false -p=1'`, `GOMAXPROCS=1`, `GOGC=20` and
`GOMEMLIMIT=400MiB` for both the top-level command and nested builds.
