package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

type rerunTaskGoober struct {
	invocations []apiv1.InvocationEnvelope
}

func (g *rerunTaskGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.invocations = append(g.invocations, env)
	if env.InstructionAddendum == "" {
		return apiv1.ResultEnvelope{
			Status:  apiv1.ResultBlocked,
			Summary: "operator guidance required",
			Error:   &apiv1.ErrorInfo{Code: "guidance_required", Message: "operator guidance required"},
		}, nil
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "followed the addendum"}, nil
}

func (*rerunTaskGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

type capturingSuccessGoober struct {
	invocations []apiv1.InvocationEnvelope
}

func (g *capturingSuccessGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.invocations = append(g.invocations, env)
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (*capturingSuccessGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

type rerunGateReviewer struct {
	addenda []string
}

type rerunInfrastructureGoober struct {
	invocations []apiv1.InvocationEnvelope
}

func (g *rerunInfrastructureGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.invocations = append(g.invocations, env)
	if env.InstructionAddendum == "" {
		return apiv1.ResultEnvelope{Status: apiv1.ResultBlocked}, nil
	}
	return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(errors.New("provider unavailable"))
}

func (*rerunInfrastructureGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func (*rerunGateReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (r *rerunGateReviewer) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	r.addenda = append(r.addenda, env.InstructionAddendum)
	if env.InstructionAddendum == "" {
		return apiv1.Verdict{Decision: apiv1.VerdictFail, Rationale: "needs operator direction"}, nil
	}
	return apiv1.Verdict{Decision: apiv1.VerdictPass, Rationale: "operator direction resolved the concern"}, nil
}

func TestRunnerRerunStageAppliesAddendumToOneAgenticTaskAttempt(t *testing.T) {
	const (
		runID    = "run-rerun-task"
		actor    = "maintainer@example.com"
		addendum = "Reuse the existing parser instead of adding a dependency."
	)
	machine := rerunTaskMachine(t)
	before, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}

	implementer := &rerunTaskGoober{}
	finisher := &capturingSuccessGoober{}
	r, runsDir := newRerunTestRunner(t, func(name string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
		if name == "implementer" {
			return implementer, nil
		}
		return finisher, nil
	}, nil)
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}

	started, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repo,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.Phase != journal.PhaseEscalated {
		t.Fatalf("initial phase = %s, want escalated", started.Phase)
	}

	result, err := r.RerunStage(context.Background(), RerunStageInput{
		RunID: runID, Machine: machine, RepoRef: repo, Stage: "implement",
		Actor: actor, InstructionAddendum: addendum,
	})
	if err != nil {
		t.Fatalf("RerunStage: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("rerun phase = %s, want completed", result.Phase)
	}
	if len(implementer.invocations) != 2 {
		t.Fatalf("implementer invocations = %d, want 2", len(implementer.invocations))
	}
	if got := implementer.invocations[1].InstructionAddendum; got != addendum {
		t.Fatalf("rerun addendum = %q, want %q", got, addendum)
	}
	if len(finisher.invocations) != 1 || finisher.invocations[0].InstructionAddendum != "" {
		t.Fatalf("downstream invocation inherited one-off addendum: %+v", finisher.invocations)
	}

	after, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || bytes.Contains(after, []byte(addendum)) {
		t.Fatalf("workflow definition changed during one-off rerun:\nbefore=%s\nafter=%s", before, after)
	}

	events := readRerunEvents(t, runsDir, runID)
	request := findRerunRequest(t, events, "implement")
	if request.Actor != actor || request.InstructionAddendum != addendum ||
		request.Attempt != 2 || request.AttemptClass != journal.AttemptHuman || request.Time.IsZero() {
		t.Fatalf("rerun request event = %+v", request)
	}
	if !hasStageAttempt(events, "implement", 2, journal.AttemptHuman, apiv1.ResultSuccess) {
		t.Fatalf("journal does not contain the requested human attempt: %+v", events)
	}
}

func TestRunnerRerunStageAppliesAddendumToAgenticReviewerGate(t *testing.T) {
	const addendum = "Do not block on the generated fixture."
	machine := rerunGateMachine(t)
	reviewer := &rerunGateReviewer{}
	runID := "run-rerun-gate"
	byTask := map[string]stubTaskResult{runID + ":implement": {status: apiv1.ResultSuccess}}
	r, runsDir := newRerunTestRunner(t, func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
		return reviewer, nil
	}, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &committingStubDeterministic{t: t, rec: rec, byTask: byTask}, nil
	})
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}

	started, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repo,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.Phase != journal.PhaseEscalated {
		t.Fatalf("initial phase = %s, want escalated", started.Phase)
	}

	result, err := r.RerunStage(context.Background(), RerunStageInput{
		RunID: runID, Machine: machine, RepoRef: repo, Stage: "review",
		Actor: "release-manager", InstructionAddendum: addendum,
	})
	if err != nil {
		t.Fatalf("RerunStage: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("rerun phase = %s, want completed", result.Phase)
	}
	if len(reviewer.addenda) != 2 || reviewer.addenda[0] != "" || reviewer.addenda[1] != addendum {
		t.Fatalf("reviewer addenda = %q, want [empty, %q]", reviewer.addenda, addendum)
	}

	request := findRerunRequest(t, readRerunEvents(t, runsDir, runID), "review")
	if request.Attempt != 2 || request.Actor != "release-manager" || request.InstructionAddendum != addendum {
		t.Fatalf("review rerun request = %+v", request)
	}
}

func TestPendingRerunRestoresUnfinishedAddendum(t *testing.T) {
	machine := rerunTaskMachine(t)
	events := []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultBlocked)},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
		{
			Type: journal.EventStageRerunRequested, Stage: "implement", Attempt: 2,
			AttemptClass: journal.AttemptHuman, Actor: "maintainer",
			InstructionAddendum: "Try the parser seam.",
		},
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptHuman},
	}

	rerun, seed, err := pendingRerun(events, machine)
	if err != nil {
		t.Fatal(err)
	}
	if rerun == nil || rerun.stage != "implement" || rerun.attempt != 2 || rerun.requestAttempt != 2 ||
		rerun.policyAttempts != 1 || rerun.infrastructureFailures != 0 ||
		rerun.instructionAddendum != "Try the parser seam." {
		t.Fatalf("pending rerun = %+v", rerun)
	}
	if len(seed) != 0 {
		t.Fatalf("pending rerun seed length = %d, want 0 before the original first stage", len(seed))
	}

	events = append(events, journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptHuman, Status: string(apiv1.ResultSuccess),
	})
	if rerun, _, err := pendingRerun(events, machine); err != nil {
		t.Fatal(err)
	} else if rerun != nil {
		t.Fatalf("completed rerun remained pending: %+v", rerun)
	}
}

func TestStageResultWithInterruptedErrorCodeIsNotRecoveryMarker(t *testing.T) {
	machine := rerunTaskMachine(t)
	events := []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultBlocked)},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
		{
			Type: journal.EventStageRerunRequested, Stage: "implement", Attempt: 2,
			AttemptClass: journal.AttemptHuman, Actor: "maintainer",
			InstructionAddendum: "Retry with guidance.",
		},
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptHuman},
		{
			Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
			AttemptClass: journal.AttemptHuman, Status: string(apiv1.ResultFailure),
			Error: &journal.ErrorDetail{Code: interruptedAttemptErrorCode, Message: "stage-owned result"},
			Artifacts: []journal.Ref{{
				Path: "artifacts/stage-owned", Digest: "sha256:stage-owned", Size: 11,
			}},
		},
	}

	if isInterruptedAttemptMarker(events[len(events)-1]) {
		t.Fatal("stage-controlled error code was classified as a runner recovery marker")
	}
	if rerun, _, err := pendingRerun(events, machine); err != nil {
		t.Fatal(err)
	} else if rerun != nil {
		t.Fatalf("completed stage result remained pending: %+v", rerun)
	}
	stage, result, ok := lastFinishedSubject(events)
	if !ok || stage != "implement" || result.Error == nil || result.Error.Code != interruptedAttemptErrorCode {
		t.Fatalf("last finished subject = (%q, %+v, %t)", stage, result, ok)
	}
	pointers := reconstructPointers(events)
	if len(pointers) != 1 || pointers[0].Artifact == nil || pointers[0].Artifact.Digest != "sha256:stage-owned" {
		t.Fatalf("reconstructed pointers = %+v", pointers)
	}
}

func TestResumeContinuesInterruptedHumanRerunWithRecordedAddendum(t *testing.T) {
	const (
		runID    = "run-rerun-resume"
		addendum = "Use the parser seam after recovery."
	)
	machine := rerunTaskMachine(t)
	implementer := &rerunTaskGoober{}
	finisher := &capturingSuccessGoober{}
	r, runsDir := newRerunTestRunner(t, func(name string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
		if name == "implementer" {
			return implementer, nil
		}
		return finisher, nil
	}, nil)
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseEscalated {
		t.Fatalf("initial phase = %s, want escalated", result.Phase)
	}

	runDir := filepath.Join(runsDir, runID)
	recovered, _, err := journal.Recover(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Append(journal.Event{
		Type: journal.EventStageRerunRequested, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptHuman, Actor: "maintainer",
		InstructionAddendum: addendum,
	}); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Append(journal.Event{
		Type: journal.EventStageStarted, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptHuman,
	}); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}

	result, err = r.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine, RepoRef: repo})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("resumed rerun phase = %s, want completed", result.Phase)
	}
	if len(implementer.invocations) != 2 || implementer.invocations[1].InstructionAddendum != addendum {
		t.Fatalf("resumed rerun invocations = %+v", implementer.invocations)
	}

	events := readRerunEvents(t, runsDir, runID)
	if !hasStageAttempt(events, "implement", 3, journal.AttemptHuman, apiv1.ResultSuccess) {
		t.Fatalf("journal does not contain recovered human attempt: %+v", events)
	}
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Stage == "implement" && event.Attempt == 2 {
			if event.AttemptClass != journal.AttemptHuman || event.Error == nil ||
				event.Error.Code != interruptedAttemptErrorCode || !isInterruptedAttemptMarker(event) {
				t.Fatalf("interrupted human attempt marker = %+v", event)
			}
			return
		}
	}
	t.Fatal("journal does not close interrupted human attempt")
}

func TestResumeAdvancesHumanRerunAfterInterruptedAttemptWasClosed(t *testing.T) {
	const (
		runID    = "run-rerun-resume-after-close"
		addendum = "Keep the recovery addendum."
	)
	machine := rerunTaskMachine(t)
	implementer := &rerunTaskGoober{}
	finisher := &capturingSuccessGoober{}
	r, runsDir := newRerunTestRunner(t, func(name string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
		if name == "implementer" {
			return implementer, nil
		}
		return finisher, nil
	}, nil)
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseEscalated {
		t.Fatalf("initial phase = %s, want escalated", result.Phase)
	}

	runDir := filepath.Join(runsDir, runID)
	recovered, _, err := journal.Recover(runDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{
			Type: journal.EventStageRerunRequested, Stage: "implement", Attempt: 2,
			AttemptClass: journal.AttemptHuman, Actor: "maintainer",
			InstructionAddendum: addendum,
		},
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptHuman},
		{
			Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
			AttemptClass: journal.AttemptHuman, Status: string(apiv1.ResultFailure),
			Error: &journal.ErrorDetail{
				Code:    interruptedAttemptErrorCode,
				Message: "attempt was in flight when the runner was interrupted",
			},
			Runner: map[string]any{interruptedAttemptMarkerKey: true},
		},
	} {
		if err := recovered.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}

	result, err = r.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine, RepoRef: repo})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("resumed rerun phase = %s, want completed", result.Phase)
	}
	if len(implementer.invocations) != 2 || implementer.invocations[1].InstructionAddendum != addendum {
		t.Fatalf("resumed rerun invocations = %+v", implementer.invocations)
	}

	events := readRerunEvents(t, runsDir, runID)
	if !hasStageAttempt(events, "implement", 3, journal.AttemptHuman, apiv1.ResultSuccess) {
		t.Fatalf("journal does not contain advanced human attempt: %+v", events)
	}
	startsAtTwo := 0
	for _, event := range events {
		if event.Type == journal.EventStageStarted && event.Stage == "implement" && event.Attempt == 2 {
			startsAtTwo++
		}
	}
	if startsAtTwo != 1 {
		t.Fatalf("stage attempt 2 started %d times, want 1", startsAtTwo)
	}
}

func TestResumeDoesNotResetHumanRerunPolicyBudgetAfterRepeatedCrashes(t *testing.T) {
	const runID = "run-rerun-repeated-crashes"
	machine := rerunTaskMachine(t)
	implementer := &rerunTaskGoober{}
	r, runsDir := newRerunTestRunner(t, func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
		return implementer, nil
	}, nil)
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseEscalated {
		t.Fatalf("initial phase = %s, want escalated", result.Phase)
	}

	recovered, _, err := journal.Recover(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{
			Type: journal.EventStageRerunRequested, Stage: "implement", Attempt: 2,
			AttemptClass: journal.AttemptHuman, Actor: "maintainer",
			InstructionAddendum: "Preserve this guidance.",
		},
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptHuman},
		{
			Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
			AttemptClass: journal.AttemptHuman, Status: string(apiv1.ResultFailure),
			Error:  &journal.ErrorDetail{Code: interruptedAttemptErrorCode},
			Runner: map[string]any{interruptedAttemptMarkerKey: true},
		},
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 3, AttemptClass: journal.AttemptHuman},
	} {
		if err := recovered.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}

	result, err = r.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine, RepoRef: repo})
	if err == nil {
		t.Fatal("Resume: want an error after the rerun policy budget is exhausted")
	}
	if result.Phase != journal.PhaseFailed {
		t.Fatalf("resumed phase = %s, want failed", result.Phase)
	}
	if len(implementer.invocations) != 1 {
		t.Fatalf("implementer invocations = %d, want only the original attempt", len(implementer.invocations))
	}
}

func TestResumeDoesNotResetHumanGateRerunBudgetAfterRepeatedCrashes(t *testing.T) {
	const (
		runID    = "run-rerun-gate-repeated-crashes"
		addendum = "Keep the reviewer guidance."
	)
	machine := rerunGateMachine(t)
	reviewer := &rerunGateReviewer{}
	byTask := map[string]stubTaskResult{runID + ":implement": {status: apiv1.ResultSuccess}}
	r, runsDir := newRerunTestRunner(t, func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
		return reviewer, nil
	}, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &committingStubDeterministic{t: t, rec: rec, byTask: byTask}, nil
	})
	r.cfg.MaxRepasses = 1
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseEscalated {
		t.Fatalf("initial phase = %s, want escalated", result.Phase)
	}

	recovered, _, err := journal.Recover(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{
			Type: journal.EventStageRerunRequested, Stage: "review", Attempt: 2,
			AttemptClass: journal.AttemptHuman, Actor: "maintainer",
			InstructionAddendum: addendum,
		},
		{Type: journal.EventGateStarted, Gate: "review", Runner: map[string]any{"repassAttempt": 1}},
		{Type: journal.EventGateStarted, Gate: "review", Runner: map[string]any{"repassAttempt": 2}},
	} {
		if err := recovered.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}

	result, err = r.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine, RepoRef: repo})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseEscalated {
		t.Fatalf("resumed phase = %s, want escalated", result.Phase)
	}
	if len(reviewer.addenda) != 1 {
		t.Fatalf("reviewer invocations = %d, want only the original attempt", len(reviewer.addenda))
	}

	events := readRerunEvents(t, runsDir, runID)
	last := events[len(events)-2]
	if last.Type != journal.EventGateEvaluated || last.Gate != "review" ||
		last.Runner["interrupted"] != true || last.Runner["repassAttempt"] != float64(2) {
		t.Fatalf("recovered gate verdict = %+v", last)
	}
}

func TestResumePreservesHumanRerunInfrastructureBudget(t *testing.T) {
	const runID = "run-rerun-infrastructure-budget"
	machine := rerunTaskMachineWithMaxAttempts(t, 3)
	implementer := &rerunInfrastructureGoober{}
	r, runsDir := newRerunTestRunner(t, func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
		return implementer, nil
	}, nil)
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseEscalated {
		t.Fatalf("initial phase = %s, want escalated", result.Phase)
	}

	recovered, _, err := journal.Recover(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{
			Type: journal.EventStageRerunRequested, Stage: "implement", Attempt: 2,
			AttemptClass: journal.AttemptHuman, Actor: "maintainer",
			InstructionAddendum: "Use the provider fallback.",
		},
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptHuman},
		{
			Type: journal.EventError, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptHuman,
			Error:  &journal.ErrorDetail{Code: "executor_error"},
			Runner: map[string]any{retryFailureClassKey: string(journal.AttemptInfra)},
		},
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 3, AttemptClass: journal.AttemptInfra},
	} {
		if err := recovered.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}

	result, err = r.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine, RepoRef: repo})
	if err == nil {
		t.Fatal("Resume: want an error when the final infrastructure allowance fails")
	}
	if result.Phase != journal.PhaseFailed {
		t.Fatalf("resumed phase = %s, want failed", result.Phase)
	}
	if len(implementer.invocations) != 2 {
		t.Fatalf("implementer invocations = %d, want one original and one resumed invocation", len(implementer.invocations))
	}
}

func rerunTaskMachine(t *testing.T) *workflow.Machine {
	return rerunTaskMachineWithMaxAttempts(t, 2)
}

func rerunTaskMachineWithMaxAttempts(t *testing.T, maxAttempts int32) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "rerun-task", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web", Start: "implement",
			Tasks: []apiv1.Task{
				{
					Name: "implement", Type: apiv1.TaskAgentic, Goober: "implementer", Goal: "implement the change", Next: "finish",
					Retry: &apiv1.RetryPolicy{MaxAttempts: maxAttempts},
				},
				{Name: "finish", Type: apiv1.TaskAgentic, Goober: "finisher", Goal: "finish the run", Next: workflow.TerminalComplete},
			},
		},
	})
	if err != nil {
		t.Fatalf("compile task rerun machine: %v", err)
	}
	return machine
}

func rerunGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "rerun-gate", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web", Start: "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review",
			}},
			Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					string(apiv1.VerdictPass):         workflow.TerminalComplete,
					string(apiv1.VerdictFail):         workflow.TargetEscalate,
					string(apiv1.VerdictNeedsChanges): workflow.TargetEscalate,
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("compile gate rerun machine: %v", err)
	}
	return machine
}

func newRerunTestRunner(t *testing.T, newAgentic NewAgenticFunc, newDeterministic NewDeterministicFunc) (*Runner, string) {
	t.Helper()
	root := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	runsDir := filepath.Join(root, "runs")
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewAgentic:       newAgentic,
		NewDeterministic: newDeterministic,
		Worktrees:        manager,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, runsDir
}

func readRerunEvents(t *testing.T, runsDir, runID string) []journal.Event {
	t.Helper()
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func findRerunRequest(t *testing.T, events []journal.Event, stage string) journal.Event {
	t.Helper()
	for _, event := range events {
		if event.Type == journal.EventStageRerunRequested && event.Stage == stage {
			return event
		}
	}
	t.Fatalf("journal has no rerun request for %q", stage)
	return journal.Event{}
}

func hasStageAttempt(events []journal.Event, stage string, attempt int, class journal.AttemptClass, status apiv1.ResultStatus) bool {
	started, finished := false, false
	for _, event := range events {
		if event.Stage != stage || event.Attempt != attempt || event.AttemptClass != class {
			continue
		}
		switch event.Type {
		case journal.EventStageStarted:
			started = true
		case journal.EventStageFinished:
			finished = event.Status == string(status)
		}
	}
	return started && finished
}
