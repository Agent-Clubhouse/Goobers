package harness

import (
	"context"
	"strings"
	"testing"
)

func TestConfiguredLauncherSessionArgsBypassContractProbe(t *testing.T) {
	home := t.TempDir()
	adapter := &CopilotAdapter{
		Command:                 []string{"forwarding-launcher", "copilot"},
		RequireLauncherContract: true,
		LauncherSessionArgs:     []string{"--session-file", "{sessionId}.jsonl"},
		Runner: &fakeProcessRunner{act: func(ProcessRequest) error {
			t.Fatal("configured session arguments must not invoke the launcher contract probe")
			return nil
		}},
	}

	argv, _, transcript, cleanup, err := adapter.prepareLauncherSession(
		context.Background(),
		t.TempDir(),
		[]string{"forwarding-launcher", "copilot", "-p", "test"},
		[]string{"COPILOT_HOME=" + home},
	)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) < 2 || argv[len(argv)-2] != "--session-file" || !strings.HasSuffix(argv[len(argv)-1], ".jsonl") {
		t.Fatalf("configured session arguments were not appended: %v", argv)
	}
	sessionID := strings.TrimSuffix(argv[len(argv)-1], ".jsonl")
	if want := copilotSessionLogPath(home, sessionID); transcript != want {
		t.Fatalf("transcript = %q, want %q", transcript, want)
	}
}
