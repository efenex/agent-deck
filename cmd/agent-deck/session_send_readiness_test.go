package main

import (
	"errors"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/send"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestSocketReadinessSelection(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		keyboardOnly  bool
		unavailable   bool
		want          bool
	}{
		{"text", "hello", false, false, true},
		{"bare slash", "/clear", false, false, false},
		{"explicit input prompt or draft", "hello", true, false, false},
		{"unavailable", "hello", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolve := instResolveOK
			if tc.unavailable {
				resolve = func(*session.Instance) (send.ClaudeSocketTarget, error) {
					return send.ClaudeSocketTarget{}, &send.Unavailable{Reason: send.ReasonDeadPid}
				}
			}
			got, fallback := planSendReadiness(claudeInst("sid"), tc.message, "auto", tc.keyboardOnly, resolve)
			if got != tc.want || fallback != tc.unavailable {
				t.Fatalf("skip=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestValidatedSocketBypassesTerminalReadiness(t *testing.T) {
	target := &mockSendRetryTarget{statuses: []string{"active"}, panes: []string{"no composer"}}
	tun := defaultSendTuning()
	tun.beforeTmux = func() error { t.Fatal("socket invoked terminal readiness"); return nil }
	tun.beforeDelivery = func() error { return nil } // fresh waiting hook
	writes := 0
	res, err := performSend(claudeInst("sid"), target, "hello", false, tun, "auto", false, nil, instResolveOK,
		func(send.ClaudeSocketTarget, string) (string, error) { writes++; return "msg", nil })
	if err != nil || writes != 1 || res.delivery != deliveryQueuedSocket {
		t.Fatalf("writes=%d result=%+v err=%v", writes, res, err)
	}
	assertNoReadinessKeys(t, target)
}

func TestSocketFallbackRestoresReadinessBeforeBytes(t *testing.T) {
	for _, atWrite := range []bool{false, true} {
		target := &mockSendRetryTarget{statuses: []string{"active"}, panes: []string{"no composer"}}
		tun := defaultSendTuning()
		readyCalls := 0
		tun.beforeTmux = func() error {
			readyCalls++
			return &send.ReadinessError{Stage: "tmux_readiness", LastReason: "no_prompt"}
		}
		unavailable := &send.Unavailable{Reason: send.ReasonDeadPid}
		resolve := instResolveOK
		if !atWrite {
			resolve = func(*session.Instance) (send.ClaudeSocketTarget, error) {
				return send.ClaudeSocketTarget{}, unavailable
			}
		}
		res, err := performSend(claudeInst("sid"), target, "hello", true, tun, "auto", false, nil, resolve,
			func(send.ClaudeSocketTarget, string) (string, error) { return "", unavailable })
		var diagnostic *send.ReadinessError
		if readyCalls != 1 || !errors.As(err, &diagnostic) || !res.untyped {
			t.Fatalf("atWrite=%v calls=%d result=%+v err=%v", atWrite, readyCalls, res, err)
		}
		assertNoReadinessKeys(t, target)
	}
}

func TestForegroundRecheckPreventsSocketWrite(t *testing.T) {
	target := &mockSendRetryTarget{}
	tun := defaultSendTuning()
	tun.beforeDelivery = func() error {
		return &send.ReadinessError{Stage: "defer_foreground", LastStatus: "running"}
	}
	res, err := performSend(claudeInst("sid"), target, "hello", false, tun, "auto", false, nil, instResolveOK,
		func(send.ClaudeSocketTarget, string) (string, error) {
			t.Fatal("wrote while foreground running")
			return "", nil
		})
	if err == nil || !res.untyped {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	assertNoReadinessKeys(t, target)
}

func TestDeferredSocketWaitUsesFreshForegroundVerdict(t *testing.T) {
	target := &mockSendRetryTarget{statuses: []string{"active"}, panes: []string{"no composer"}}
	tun := defaultSendTuning()
	tun.beforeDelivery = func() error { return nil } // fresh waiting, background remains
	res, err := performSend(claudeInst("sid"), target, "hello", false, tun, "auto", true,
		func() (string, error) {
			t.Fatal("aggregate status replaced fresh foreground verdict")
			return "running", nil
		}, instResolveOK,
		func(send.ClaudeSocketTarget, string) (string, error) { return "msg", nil })
	if err != nil || res.targetBusyAtSend || res.busyProbeFailed {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	assertNoReadinessKeys(t, target)
}

func TestFallbackReadinessStillRunsDraftAndMenuGuards(t *testing.T) {
	for _, pane := range []string{
		"────────────────────\n❯ operator draft\n────────────────────",
		"❯ 1. Yes\nEnter to confirm",
	} {
		target := &mockSendRetryTarget{panes: []string{pane}, statuses: []string{"waiting"}}
		tun := defaultSendTuning()
		tun.beforeTmux = func() error { return nil } // frame changes after wait
		tun.guardHold, tun.guardPoll = time.Millisecond, time.Millisecond
		_, err := runTmuxSend(target, claudeInst("sid"), "hello", false, tun, send.ReasonDeadPid)
		if err == nil {
			t.Fatal("unsafe fallback accepted")
		}
		assertNoReadinessKeys(t, target)
	}
}

func assertNoReadinessKeys(t *testing.T, target *mockSendRetryTarget) {
	t.Helper()
	if target.sendKeysCalls+target.sendChunkedCalls+target.sendEnterCalls+target.sendCtrlCCalls != 0 {
		t.Fatal("readiness refusal emitted keyboard bytes")
	}
}

func TestReadinessDraftKeepsRetryableDelivery(t *testing.T) {
	data := readinessErrorData(&send.ReadinessError{Stage: "tmux_readiness", LastReason: send.ComposerCodeDraft}, "target")
	if outcome, _ := classifyChild(data, 1); outcome != childNotSent {
		t.Fatalf("draft readiness refusal lost its existing retryable delivery: %#v", data)
	}
}

func TestForegroundReadinessPrecedesFinalSocketValidation(t *testing.T) {
	target := &mockSendRetryTarget{}
	tun := defaultSendTuning()
	foregroundChecked := false
	tun.beforeDelivery = func() error { foregroundChecked = true; return nil }
	_, err := performSend(claudeInst("sid"), target, "hello", false, tun, "auto", false, nil,
		func(inst *session.Instance) (send.ClaudeSocketTarget, error) {
			if !foregroundChecked {
				t.Fatal("socket identity validated before the potentially long foreground wait")
			}
			return instResolveOK(inst)
		}, func(send.ClaudeSocketTarget, string) (string, error) { return "msg", nil })
	if err != nil {
		t.Fatal(err)
	}
	assertNoReadinessKeys(t, target)
}
