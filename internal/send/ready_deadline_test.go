package send

import (
	"errors"
	"testing"
	"time"
)

type failingCaptureReadyChecker struct{ mockReadyChecker }

func (*failingCaptureReadyChecker) CapturePaneFresh() (string, error) {
	return "", errors.New("capture failed")
}

func TestReadinessCaptureErrorFailsClosed(t *testing.T) {
	for _, tool := range []string{"claude", "codex", "cursor"} {
		t.Run(tool, func(t *testing.T) {
			checker := &failingCaptureReadyChecker{mockReadyChecker{statuses: []string{"active", "waiting"}}}
			err := WaitForAgentReady(checker, tool, time.Second, PromptGates{
				ClaudeComposer: tool == "claude", CodexPrompt: tool == "codex",
			})
			if err == nil {
				t.Fatal("capture failure authorized keyboard input")
			}
		})
	}
}

type slowReadyChecker struct{ mockReadyChecker }

func (*slowReadyChecker) GetStatus() (string, error) {
	time.Sleep(150 * time.Millisecond)
	return "active", nil
}

func TestReadinessDeadlineIncludesProbeTime(t *testing.T) {
	start := time.Now()
	err := WaitForAgentReady(&slowReadyChecker{}, "shell", time.Second, PromptGates{})
	if err == nil {
		t.Fatal("busy pane authorized keyboard input")
	}
	if elapsed := time.Since(start); elapsed > 1200*time.Millisecond {
		t.Fatalf("probe time escaped deadline: %s", elapsed)
	}
}
