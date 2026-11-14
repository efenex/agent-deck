package main

import (
	"errors"

	"github.com/asheshgoplani/agent-deck/internal/send"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

func planSendReadiness(inst *session.Instance, message, config string, keyboardOnly bool,
	resolve func(*session.Instance) (send.ClaudeSocketTarget, error),
) (socketReady, tmuxFallback bool) {
	if keyboardOnly {
		return false, false
	}
	if resolve == nil {
		resolve = resolveClaudeSocketTargetForInstance
	}
	transport, reason, _ := chooseSendTransport(transportInputs{
		tool: inst.Tool, configValue: config, message: message,
		claudeSessionID: inst.ClaudeSessionID, isSSH: inst.IsSSH(),
		resolve: func() (send.ClaudeSocketTarget, error) { return resolve(inst) },
	})
	return transport == transportSocket, transport == transportTmux && !selectorLevelReasons[reason]
}

func identifyReadinessError(err error, targetID string) error {
	var diagnostic *send.ReadinessError
	if errors.As(err, &diagnostic) {
		diagnostic.TargetID = targetID
	}
	return err
}

func readinessErrorData(err error, targetID string) map[string]interface{} {
	var diagnostic *send.ReadinessError
	if !errors.As(identifyReadinessError(err, targetID), &diagnostic) {
		return nil
	}
	data := map[string]interface{}{"readiness": diagnostic}
	if diagnostic.Stage == "tmux_readiness" && diagnostic.LastReason == send.ComposerCodeDraft {
		data["delivery"] = deliveryComposerBlocked
	}
	return data
}
