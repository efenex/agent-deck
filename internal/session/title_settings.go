package session

import "github.com/asheshgoplani/agent-deck/internal/tmux"

// The [display] title settings live in the user config, which internal/tmux
// cannot read: internal/session imports internal/tmux, so the dependency only
// runs one way. Registering a callback here closes the loop.
//
// Before this, both settings were applied from exactly one place — the TUI's
// config load — so every one-shot process that writes set-titles-string
// (`agent-deck rename`, a move, `session start`, the web daemon) rewrote the
// title back to the built-in "[<project>] <name>" template and left it there
// until the TUI next touched the session. A CLI rename silently undid the
// operator's configured title.
//
// internal/session is linked into every entry point that can construct or
// rename a session, and its init runs before any of them do work, so the
// fallback is in place everywhere without each call site opting in.
func init() {
	tmux.SetTitleSettingsProvider(func() (string, bool) {
		cfg, err := LoadUserConfig()
		if err != nil || cfg == nil {
			return "", false
		}
		return cfg.Display.GetTitleFormat(), !cfg.Display.GetIncludeCwdPrefix()
	})
}
