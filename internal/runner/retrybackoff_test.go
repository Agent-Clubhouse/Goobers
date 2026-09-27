package runner

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

func TestRunnerRetryBackoffRecordsScheduledTimer(t *testing.T) {
	flaky := &flakyDeterministic{failUntil: 1}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return flaky, nil }, gate.NewAutomatedEvaluator())
	result, err := r.Start(context.Background(), StartInput{RunID: "observed-retry", Machine: retryFixtureMachineWithBackoff(t, 2, time.Second), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}})
	if err != nil || result.Phase != journal.PhaseCompleted || flaky.calls != 2 {
		t.Fatalf("result=%+v calls=%d err=%v", result, flaky.calls, err)
	}
	rd, err := journal.OpenRead(filepath.Join(runsDir, "observed-retry"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var state readmodel.RetryBackoffState
	var deadline time.Time
	count := 0
	for _, event := range events {
		state = state.After(event)
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == journal.RetryBackoffKind {
			count++
			if len(state.Waits) != 1 {
				t.Fatalf("timer not projected: %+v", event)
			}
			wait := state.Waits[0]
			if wait.Driver != "local" || wait.Class != journal.AttemptPolicy || wait.Attempt != 1 || wait.Deadline.Sub(wait.ObservedAt) != time.Second {
				t.Fatalf("wrong actual timer: %+v", wait)
			}
			deadline = wait.Deadline
		}
		if event.Type == journal.EventStageStarted && event.Stage == "implement" && event.Attempt == 2 {
			if deadline.IsZero() || event.Time.Before(deadline) || len(state.Waits) != 0 {
				t.Fatalf("next attempt did not follow/clear scheduled timer: %+v %+v", event, state)
			}
		}
	}
	if count != 1 || len(state.Waits) != 0 {
		t.Fatalf("observations=%d terminal=%+v", count, state)
	}
}

func TestRetryBackoffCrashResumeDurablyInvalidatesTimer(t *testing.T) {
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: "resume-wait", Workflow: "work", Gaggle: "alpha", Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jr.Close() }()
	now := time.Now().UTC()
	if err := jr.Append(journal.RetryBackoffEvent("work", 1, "local", journal.AttemptInfra, now, now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, "resume-wait"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if err := resetRetryBackoffOnResume(jr, events); err != nil {
		t.Fatal(err)
	}
	events, err = reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var state readmodel.RetryBackoffState
	for _, event := range events {
		state = state.After(event)
	}
	if len(state.Waits) != 0 || events[len(events)-1].Runner["kind"] != journal.RetryBackoffResetKind {
		t.Fatalf("old process timer survived: %+v", events)
	}
	count := len(events)
	if err := resetRetryBackoffOnResume(jr, events); err != nil {
		t.Fatal(err)
	}
	events, err = reader.Events()
	if err != nil || len(events) != count {
		t.Fatalf("reset duplicated: %d/%d %v", len(events), count, err)
	}
}

func TestRetryBackoffUsesParallelBranchJournal(t *testing.T) {
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: "branch-wait", Workflow: "work", Gaggle: "alpha", Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jr.Close() }()
	// The actual parallel runner wrapper must receive the annotation, rather
	// than unwrapping to the run and accidentally assigning root branch zero.
	branch := &branchJournal{run: jr, branch: 2}
	if err := waitForRetry(context.Background(), context.Background(), branch, "branch-work", 3, journal.AttemptInfra, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, "branch-wait"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	event := events[len(events)-1]
	if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != journal.RetryBackoffKind || event.Branch != 2 || event.Attempt != 3 {
		t.Fatalf("branch retry lost scope: %+v", event)
	}
}
