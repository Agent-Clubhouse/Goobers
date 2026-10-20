//go:build integration && !windows

package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/harness"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationCopilotParentAuthorsChildThroughRealWorkers(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	t.Setenv("GOOBERS_PARENT_QUALIFICATION_HARNESS", "copilot")
	qualifyContainedParentJourney(t, "merge")
}

func TestIntegrationCopilotParentCancellationStopsAuthoredChild(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	t.Setenv("GOOBERS_PARENT_QUALIFICATION_HARNESS", "copilot")
	qualifyContainedParentJourney(t, "cancel")
}

func TestIntegrationContainedParentSurvivesDaemonProcessLossCopilot(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	t.Setenv("GOOBERS_PARENT_QUALIFICATION_HARNESS", "copilot")
	qualifyParentDaemonProcessLoss(t, false)
}

// The host probe must accept the real adapter's complete preflight argv while
// refusing arbitrary prompt execution outside the worker.
func TestIntegrationCopilotQualificationHostPreflight(t *testing.T) {
	testdep.Require(t, "sh")
	t.Setenv("GOOBERS_PARENT_QUALIFICATION_HARNESS", "copilot")
	bin := t.TempDir()
	installQualificationParentProbe(t, bin)
	command := filepath.Join(bin, "copilot")
	adapter := &harness.CopilotAdapter{
		Command: []string{command}, AuthCheckArgs: copilotAuthCheckArgs,
		AuthCheckSuccessLine: copilotAuthCheckSuccessLine,
		ModelCredential:      func(context.Context) (string, error) { return "qualification-copilot-model-token", nil },
	}
	if _, err := adapter.Preflight(t.Context()); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), command, "-p", "author a child").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "parent execution reached the host") {
		t.Fatalf("host inference was not refused: %v %s", err, output)
	}
}
