package runner

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestRerunUsesDurableGenerationAfterRunnerReconstruction(t *testing.T) {
	machine := rerunTaskMachine(t)
	implementer := &rerunTaskGoober{}
	finisher := &capturingSuccessGoober{}
	runner, runsDir := newRerunTestRunner(t, func(name string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
		if name == "implementer" {
			return implementer, nil
		}
		return finisher, nil
	}, nil)
	admitted := "sha256:" + strings.Repeat("a", 64)
	runner.cfg.ConfigGeneration = admitted
	runner.cfg.InstanceID = strings.Repeat("a", 32)
	repository := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	const runID = "generation-rerun"
	result, err := runner.Start(t.Context(), StartInput{RunID: runID, Machine: machine, Gaggle: "acme-web", RepoRef: repository, Trigger: journal.Trigger{Kind: journal.TriggerManual}})
	if err != nil || result.Phase != journal.PhaseEscalated {
		t.Fatalf("initial attempt=%+v err=%v", result, err)
	}
	initialAttempts := len(implementer.invocations)
	// Reconstructed runner configuration is not authority for an existing run's
	// provenance. The rerun must recover the admitted identity from its journal.
	runner.cfg.ConfigGeneration = "sha256:" + strings.Repeat("b", 64)
	runner.cfg.InstanceID = strings.Repeat("b", 32)
	result, err = runner.RerunStage(t.Context(), RerunStageInput{RunID: runID, Machine: machine, RepoRef: repository, Stage: "implement", Actor: "maintainer", InstructionAddendum: "Complete the requested change.", ExpectedTerminalSeq: terminalRunSequence(t, runsDir, runID)})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatalf("rerun=%+v err=%v", result, err)
	}
	if len(implementer.invocations) != initialAttempts+1 {
		t.Fatalf("attempts=%d", len(implementer.invocations))
	}
	for _, attempt := range append(implementer.invocations, finisher.invocations...) {
		if attempt.ConfigGeneration != admitted || attempt.InstanceID != strings.Repeat("a", 32) {
			t.Fatalf("rerun replaced admitted provenance: generation=%s instance=%s", attempt.ConfigGeneration, attempt.InstanceID)
		}
	}
}
