package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestRerunStageRejectsGooberDigestMismatch(t *testing.T) {
	const (
		runID       = "run-rerun-goober-mismatch"
		pinned      = "sha256:pinned-goober"
		replacement = "sha256:replacement-goober"
	)
	machine := rerunTaskMachine(t)
	implementer := &rerunTaskGoober{}
	r, runsDir := newRerunTestRunner(t, func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
		return implementer, nil
	}, nil)
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}

	started, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, GooberDigest: pinned, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repo,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.Phase != journal.PhaseEscalated {
		t.Fatalf("initial phase = %s, want escalated", started.Phase)
	}

	_, err = r.RerunStage(context.Background(), RerunStageInput{
		RunID: runID, Machine: machine, GooberDigest: replacement, RepoRef: repo,
		Stage: "implement", Actor: "maintainer", InstructionAddendum: "Try again.",
	})
	if err == nil || !strings.Contains(err.Error(), "goober digest") || !strings.Contains(err.Error(), "WF-016") {
		t.Fatalf("RerunStage error = %v, want goober identity refusal", err)
	}
	if len(implementer.invocations) != 1 {
		t.Fatalf("implementer invocations = %d, want only the original attempt", len(implementer.invocations))
	}
	for _, event := range readRerunEvents(t, runsDir, runID) {
		if event.Type == journal.EventStageRerunRequested {
			t.Fatalf("refused rerun journaled a request: %+v", event)
		}
	}
}

func TestResumeFromTerminalRejectsGooberDigestMismatch(t *testing.T) {
	const (
		runID       = "run-terminal-goober-mismatch"
		pinned      = "sha256:pinned-goober"
		replacement = "sha256:replacement-goober"
	)
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), GooberDigest: pinned, Gaggle: machine.Def.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatalf("append run.finished: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	det := &countingDeterministic{}
	r := terminalResumeRunner(t, runsDir, fixtureRepo, wtMgr, det)
	_, err = r.ResumeFromTerminal(context.Background(), ResumeFromTerminalInput{
		RunID: runID, Machine: machine, GooberDigest: replacement,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Target:  "implement", Actor: "maintainer",
	})
	if err == nil || !strings.Contains(err.Error(), "goober digest") || !strings.Contains(err.Error(), "WF-016") {
		t.Fatalf("ResumeFromTerminal error = %v, want goober identity refusal", err)
	}
	if det.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", det.calls)
	}
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, event := range events {
		if event.Type == journal.EventRunResumed {
			t.Fatalf("refused terminal resume journaled run.resumed: %+v", event)
		}
	}
}
