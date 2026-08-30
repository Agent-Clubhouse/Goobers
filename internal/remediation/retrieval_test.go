package remediation

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/api/integrity"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

func TestLoadIndexProjectsReadModelRemediationExamplesAndAugmentsAgent(t *testing.T) {
	instanceRoot := t.TempDir()
	runsDir := filepath.Join(instanceRoot, "runs")
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "historical", Workflow: "repair", Gaggle: "test",
		StartedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventError, Stage: "implement",
		Error: &journal.ErrorDetail{Code: "compile", Message: "undefined symbol"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
		Status: "success", Outputs: map[string]any{"fix": "add the missing import", "didItHelp": true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Target: journal.TargetComplete,
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := projectRunIntoReadModel(instanceRoot, filepath.Join(runsDir, "historical")); err != nil {
		t.Fatal(err)
	}

	failedRun, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "unsuccessful", Workflow: "repair", Gaggle: "test",
		StartedAt: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := failedRun.Append(journal.Event{
		Type: journal.EventError, Stage: "implement",
		Error: &journal.ErrorDetail{Code: "compile", Message: "undefined symbol"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := failedRun.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
		Status: "success", Outputs: map[string]any{"fix": "risky workaround", "didItHelp": false},
	}); err != nil {
		t.Fatal(err)
	}
	if err := failedRun.Append(journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseFailed),
	}); err != nil {
		t.Fatal(err)
	}
	if err := failedRun.Close(); err != nil {
		t.Fatal(err)
	}
	if err := projectRunIntoReadModel(instanceRoot, filepath.Join(runsDir, "unsuccessful")); err != nil {
		t.Fatal(err)
	}

	index, err := LoadIndex(runsDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := AugmentInvocation(apiv1.InvocationEnvelope{}, index, Query{
		Stage: "implement", ErrorClass: "compile", FailureExcerpt: "undefined symbol",
	}, Options{K: 2})
	if !strings.Contains(env.InstructionAddendum, "add the missing import") ||
		!strings.Contains(env.InstructionAddendum, "did-it-help: true") ||
		!strings.Contains(env.InstructionAddendum, "did-it-help: false") {
		t.Fatalf("instruction addendum = %q", env.InstructionAddendum)
	}
}

func projectRunIntoReadModel(instanceRoot, runDir string) error {
	store, err := readmodel.Open(filepath.Join(instanceRoot, readmodel.FileName))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	reader, err := journal.OpenReadOnly(runDir)
	if err != nil {
		return err
	}
	identity, err := reader.Identity()
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	projection, err := readmodel.ProjectRunFromJournal(reader, identity, events)
	if err != nil {
		return err
	}
	return store.UpsertRun(context.Background(), projection)
}

func TestLoadIndexReturnsNotExistWithoutReadModel(t *testing.T) {
	runsDir := t.TempDir()
	if _, err := LoadIndex(runsDir, nil); err == nil {
		t.Fatal("LoadIndex succeeded without read.db, want not-exist error")
	}
}

func TestRetrieveRanksVerifiedFreshMatchingFix(t *testing.T) {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	index := NewIndex([]Record{
		{ID: "stale", Stage: "review", ErrorClass: "timeout", FailureExcerpt: "review timeout",
			FixExcerpt: "increase polling timeout", DidItHelp: true, OutcomeKnown: true, ObservedAt: now.Add(-180 * 24 * time.Hour),
			ConfigDigest: "old", Integrity: integrity.Maintainer},
		{ID: "match", Stage: "review", ErrorClass: "timeout", FailureExcerpt: "review timeout",
			FixExcerpt: "retry the bounded poll", DidItHelp: true, OutcomeKnown: true, ObservedAt: now,
			ConfigDigest: "current", Integrity: integrity.Maintainer},
		{ID: "bad", Stage: "review", ErrorClass: "timeout", FailureExcerpt: "review timeout",
			FixExcerpt: "known bad fix", DidItHelp: false, OutcomeKnown: true, Integrity: integrity.Maintainer},
	}, nil)

	got := index.Retrieve(Query{Stage: "review", ErrorClass: "timeout", FailureExcerpt: "review timeout", ConfigDigest: "current"},
		Options{K: 2, Now: now})
	if len(got) != 2 || got[0].ID != "match" || !got[0].DidItHelp {
		t.Fatalf("results = %+v, want current verified fix first", got)
	}
	if !strings.Contains(got[1].FewShot, "did-it-help: false") {
		t.Fatalf("second result few-shot = %q, want did-it-help outcome label", got[1].FewShot)
	}
}

func TestIndexScrubsAndBoundsContent(t *testing.T) {
	secret := "ghp_" + strings.Repeat("x", 40)
	index := NewIndex([]Record{{
		ID: "safe", Stage: "build", ErrorClass: "compile", FailureExcerpt: secret,
		FixExcerpt: strings.Repeat("fix ", excerptLimit), DidItHelp: true, OutcomeKnown: true, Integrity: integrity.Trusted,
	}}, journal.NewPatternScrubber())
	got := index.Retrieve(Query{Stage: "build", ErrorClass: "compile", FailureExcerpt: secret},
		Options{Now: time.Now()})
	if len(got) != 1 || strings.Contains(got[0].FewShot, secret) {
		t.Fatalf("result = %+v, expected redacted result", got)
	}
	if len([]rune(got[0].FailureExcerpt)) > excerptLimit || len([]rune(got[0].FixExcerpt)) > excerptLimit {
		t.Fatal("excerpt exceeded bound")
	}
}

func TestIndexRejectsUntrustedAndUnknownOutcomeRecords(t *testing.T) {
	index := NewIndex([]Record{
		{ID: "unapproved", Stage: "x", FailureExcerpt: "failure", FixExcerpt: "fix", DidItHelp: true, OutcomeKnown: true, Integrity: integrity.Unapproved},
		{ID: "unknown-outcome", Stage: "x", FailureExcerpt: "failure", FixExcerpt: "fix", Integrity: integrity.Trusted},
	}, nil)
	if got := index.Retrieve(Query{Stage: "x", FailureExcerpt: "failure"}, Options{}); len(got) != 0 {
		t.Fatalf("results = %+v, want none", got)
	}
}

func TestLoadIndexIgnoresRunningRunsWithoutOutcome(t *testing.T) {
	instanceRoot := t.TempDir()
	runsDir := filepath.Join(instanceRoot, "runs")
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "running", Workflow: "repair", Gaggle: "test", StartedAt: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventError, Stage: "implement",
		Error: &journal.ErrorDetail{Code: "compile", Message: "undefined symbol"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
		Status: "success", Outputs: map[string]any{"fix": "draft"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := projectRunIntoReadModel(instanceRoot, filepath.Join(runsDir, "running")); err != nil {
		t.Fatal(err)
	}
	index, err := LoadIndex(runsDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := index.Retrieve(Query{
		Stage: "implement", ErrorClass: "compile", FailureExcerpt: "undefined symbol",
	}, Options{}); len(got) != 0 {
		t.Fatalf("results = %+v, want no running-run examples", got)
	}
}
