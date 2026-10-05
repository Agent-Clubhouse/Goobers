package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestContainedChildHumanEpochResumesCommonGuidanceSnapshot(t *testing.T) {
	machine := restartMachine(t)
	implementer, finisher := &rerunTaskGoober{}, &capturingSuccessGoober{}
	agentic := func(name string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
		if name == "implementer" {
			return implementer, nil
		}
		return finisher, nil
	}
	base, runsDir := newRerunTestRunner(t, agentic, nil)
	base.cfg.ConfigGeneration = journal.Digest([]byte("child archive"))
	base.cfg.ScratchDir = t.TempDir()
	in := childWorkspaceStart(machine)
	in.Gaggle, in.Child.Gaggle = "acme-web", "acme-web"
	source, err := base.Start(t.Context(), in)
	if err != nil || source.Phase != journal.PhaseEscalated {
		t.Fatal(source, err)
	}
	saveRestartGuidance(t, runsDir, in.RunID, "selected", "Use the retained child work")
	reader, err := journal.OpenReadOnly(filepath.Join(runsDir, in.RunID))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(reader.Dir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	request := StageRestartRequest{EpochID: strings.Repeat("a", 32), Stage: "implement", PrincipalRef: "issuer:human", ExpectedTerminalSeq: terminalRunSequence(t, runsDir, in.RunID), GuidanceIDs: []string{"selected"}, Rationale: "Human reviewed child failure"}
	_, scrubber := journal.DefaultScrubber()
	if _, err := PrepareStageRestart(reader, machine, request, scrubber); err == nil {
		t.Fatal("ordinary adapter acquired child custody")
	}
	plan, err := PrepareChildStageRestart(reader, machine, request, scrubber)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalStageRestartPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = ParseStageRestartPlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	lineage := *plan.Source.Child
	lineage.ExecutionEpoch, lineage.PriorResultRef, lineage.RestartDigest = 1, journal.Digest([]byte("sealed result")), journal.Digest([]byte("admitted restart"))
	plan.Continuation.ChildContinuation = &lineage
	continued, err := journal.CreateContinuation(runsDir, plan.Continuation)
	if err != nil {
		t.Fatal(err)
	}
	epochReader, err := journal.OpenReadOnly(continued.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := epochReader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := continued.Close(); err != nil {
		t.Fatal(err)
	}
	deterministic := func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return nil, errors.New("unexpected deterministic stage")
	}
	var contexts int
	factories := StageRestartExecutionFactories{NewAgentic: agentic, NewDeterministic: deterministic, Context: func(ctx context.Context, actual journal.RunIdentity, _ SecretRegistrar) (context.Context, func(), error) {
		if actual.RunID != id.RunID || actual.Child == nil || *actual.Child != lineage {
			t.Fatal("context received foreign child identity")
		}
		contexts++
		return ctx, func() {}, nil
	}}
	if _, err := base.ForStageRestartExecution(id, factories); err == nil {
		t.Fatal("native driver admitted child restart")
	}
	contained, err := base.ForChildExecution(id, ChildExecutionFactories{NewAgentic: agentic, NewDeterministic: deterministic})
	if err != nil {
		t.Fatal(err)
	}
	human, err := contained.ForStageRestartExecution(id, factories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := human.Resume(t.Context(), ResumeInput{RunID: id.RunID, Machine: machine, GooberDigest: in.GooberDigest, RepoRef: in.RepoRef})
	if err != nil || result.Phase != journal.PhaseCompleted || contexts != 1 {
		t.Fatal(result, contexts, err)
	}
	if len(implementer.invocations) != 2 || implementer.invocations[1].Attempt != 2 || !strings.Contains(implementer.invocations[1].InstructionAddendum, "Use the retained child work") || len(finisher.invocations) != 1 || finisher.invocations[0].InstructionAddendum != "" {
		t.Fatal("common retry/guidance semantics changed", implementer.invocations, finisher.invocations)
	}
	after, err := os.ReadFile(filepath.Join(reader.Dir(), "events.jsonl"))
	if err != nil || string(before) != string(after) {
		t.Fatal("source child journal changed", err)
	}
	if _, err := human.Start(t.Context(), in); err == nil {
		t.Fatal("human epoch driver started original child")
	}
}
