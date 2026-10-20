package harness

import (
	"context"
	"errors"
	"github.com/goobers/goobers/internal/invoke"
	"slices"
	"testing"
)

type interactiveCaptureRunner struct {
	request     ProcessRequest
	leaveWriter bool
}

func (r *interactiveCaptureRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	r.request = req
	if r.leaveWriter {
		_ = invoke.RegisterWorkspaceWriter(ctx)
	}
	return ProcessResult{ExitCode: 1}, errors.New("fixture process exit")
}
func TestInteractiveCopilotOwnedLaunch(t *testing.T) {
	for _, pending := range []bool{false, true} {
		r := &interactiveCaptureRunner{leaveWriter: pending}
		c := &CopilotAdapter{Command: []string{"copilot"}, Runner: r}
		_, err := c.OpenInteractive(context.Background(), RunRequest{Mode: ModeInvoke, Workspace: t.TempDir(), Tools: []string{"view"}})
		if err == nil {
			t.Fatal("expected fixture startup failure")
		}
		if errors.Is(err, invoke.ErrWorkspaceNotQuiescent) != pending {
			t.Fatal("custody mismatch", err)
		}
		if slices.Contains(r.request.Command, "--no-auto-update") || !slices.Contains(r.request.Command, "--headless") || !slices.Contains(r.request.Command, "--port") {
			t.Fatal(r.request.Command)
		}
		if len(r.request.Env) == 0 {
			t.Fatal("missing scoped environment")
		}
	}
}
