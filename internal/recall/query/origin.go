package query

import (
	"encoding/json"
	"html"
	"strings"
)

// Row origins: who put a user-side row into the conversation. Set on the
// rows that come from user records, queue operations and hook output; absent
// on assistant and tool rows.
//
//	human              typed at the terminal (or a slash command, or a `!` shell line)
//	peer:<from-id>     another agent-deck session ([agent-deck from:<id>]) or a
//	                   Claude Code peer or sub-agent (<agent-message from="<id>">)
//	hook               agent-deck/Claude hook output injected into the turn
//	task-notification  a background task or monitor reporting back
//	system             other injected context (skills, caveats, reminders, summaries)
const (
	OriginHuman            = "human"
	OriginHook             = "hook"
	OriginTaskNotification = "task-notification"
	OriginSystem           = "system"
	originPeerPrefix       = "peer:"
	originPeerUnknown      = originPeerPrefix + "unknown"
)

// nativeOrigin is the `origin` object Claude Code writes on user records and
// queued_command attachments since 2.1.x ({"kind":"human"}, {"kind":
// "task-notification"}, {"kind":"peer","from":"<id>"}).
type nativeOrigin struct {
	Kind string
	From string
}

func parseNativeOrigin(raw json.RawMessage) nativeOrigin {
	o := timelineObject(raw)
	return nativeOrigin{Kind: timelineString(o["kind"]), From: timelineString(o["from"])}
}

// peerFromText returns the sender id when text opens with agent-deck's
// `[agent-deck from:<id>]` tag (also inside the <pasted_content> wrapper
// Claude Code puts around long pastes) or Claude Code's
// `<agent-message from="<id>">` envelope.
func peerFromText(text string) string {
	t := strings.TrimSpace(text)
	// Claude Code introduces a peer's message with a sentence of its own.
	t = strings.TrimSpace(strings.TrimPrefix(t, "Another Claude session sent a message:"))
	if strings.HasPrefix(t, "<pasted_content") {
		if i := strings.Index(t, ">"); i >= 0 {
			t = strings.TrimSpace(t[i+1:])
		}
	}
	if rest, ok := strings.CutPrefix(t, "[agent-deck from:"); ok {
		if i := strings.IndexByte(rest, ']'); i > 0 {
			return strings.TrimSpace(rest[:i])
		}
	}
	if rest, ok := strings.CutPrefix(t, `<agent-message from="`); ok {
		if i := strings.IndexByte(rest, '"'); i > 0 {
			return rest[:i]
		}
	}
	return ""
}

// deriveOrigin classifies a user-side row. Order: an agent-deck/peer tag in
// the text (a peer's send arrives as a human-typed message, so Claude's own
// origin cannot tell), then Claude's native origin, then the text and flags
// for transcripts written before Claude recorded one. kind is the row kind
// textRow chose.
func deriveOrigin(kind, text string, meta bool, no nativeOrigin) string {
	if id := peerFromText(text); id != "" {
		return originPeerPrefix + id
	}
	switch no.Kind {
	case "":
	case "human":
		return OriginHuman
	case "task-notification":
		return OriginTaskNotification
	case "peer":
		if no.From != "" {
			return originPeerPrefix + no.From
		}
		return originPeerUnknown
	default:
		return OriginSystem
	}
	t := strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(t, "<task-notification>"):
		return OriginTaskNotification
	case strings.HasPrefix(t, "Stop hook feedback:"), strings.HasPrefix(t, "[INBOX]"):
		return OriginHook
	case kind == "command", kind == "bash":
		return OriginHuman
	case meta, kind == "system":
		return OriginSystem
	}
	return OriginHuman
}

// Shell lines the user ran with `!` are recorded as a <bash-input> user
// record followed by a <bash-stdout>/<bash-stderr> one.
const (
	bashInputTag  = "<bash-input>"
	bashStdoutTag = "<bash-stdout>"
)

func isBashInput(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), bashInputTag)
}

func isBashOutput(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, bashStdoutTag) || strings.HasPrefix(t, "<bash-stderr>")
}

// bashShellRow is the row for a `!` shell line: the same bash kind the Bash
// tool uses, marked meta.user_shell, finished when its output record lands.
func bashShellRow(id, ts, text string) Row {
	cmd := html.UnescapeString(xmlTag(text, "bash-input"))
	return Row{
		ID: id, Kind: "bash", TS: ts, Title: firstLine(cmd, 120), Detail: cmd,
		Finished: boolPtr(false), Origin: OriginHuman, RawType: "user",
		Meta: map[string]any{"user_shell": true},
	}
}

// bashShellOutput folds a <bash-stdout>/<bash-stderr> record into the text
// and error flag resultRow takes. The terminal XML-escapes both streams.
func bashShellOutput(text string) (string, bool) {
	stdout := html.UnescapeString(xmlTag(text, "bash-stdout"))
	stderr := html.UnescapeString(xmlTag(text, "bash-stderr"))
	if stderr == "" {
		return stdout, false
	}
	if stdout == "" {
		return stderr, true
	}
	return strings.TrimRight(stdout, "\n") + "\n" + stderr, false
}
