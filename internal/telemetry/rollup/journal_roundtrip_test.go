package rollup

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestJournalEventMirrorFieldSet(t *testing.T) {
	intentionallyUnmirrored := []string{
		"action", "agent", "branchName", "branchStatus", "complete", "completeness",
		"decision", "gaggle", "instructionAddendum", "integrity",
		"minimumIntegrity", "notificationReceipt", "notificationRequest",
		"parallel", "peerMessage", "rationale", "skipCount",
	}
	want := append(jsonFields(reflect.TypeOf(journalEvent{})), intentionallyUnmirrored...)
	sort.Strings(want)
	got := jsonFields(reflect.TypeOf(journal.Event{}))
	if !slices.Equal(got, want) {
		t.Errorf("journal.Event JSON fields = %v, want mirrored fields plus explicit omissions %v", got, want)
	}
}

func jsonFields(typ reflect.Type) []string {
	fields := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		fields = append(fields, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
	}
	sort.Strings(fields)
	return fields
}

// TestAggregateStatusLiteralsMatchRealConstants pins aggregates.go's
// production-shaped status literals (runStatusCompleted/Failed,
// stageStatusSuccess/Failure) against the real wire values internal/journal
// and api/v1alpha1 actually write — issue #129's checklist: this package's
// own hand-written test fixtures previously drifted from these ("succeeded"/
// "failed" instead of "success"/"failure" for a stage; "succeeded" instead of
// "completed" for a run), so aggregation math silently matched neither the
// fixture NOR production, and no test caught it. aggregates.go deliberately
// does not import these packages in production code (same decoupling
// rationale as mirror.go) — this test-only import is the belt-and-suspenders
// check, mirroring TestIngestRunAgainstRealJournalPackage below.
func TestAggregateStatusLiteralsMatchRealConstants(t *testing.T) {
	if runStatusCompleted != string(journal.PhaseCompleted) {
		t.Errorf("runStatusCompleted = %q, want journal.PhaseCompleted %q", runStatusCompleted, journal.PhaseCompleted)
	}
	if runStatusFailed != string(journal.PhaseFailed) {
		t.Errorf("runStatusFailed = %q, want journal.PhaseFailed %q", runStatusFailed, journal.PhaseFailed)
	}
	if stageStatusSuccess != string(apiv1.ResultSuccess) {
		t.Errorf("stageStatusSuccess = %q, want apiv1.ResultSuccess %q", stageStatusSuccess, apiv1.ResultSuccess)
	}
	if stageStatusFailure != string(apiv1.ResultFailure) {
		t.Errorf("stageStatusFailure = %q, want apiv1.ResultFailure %q", stageStatusFailure, apiv1.ResultFailure)
	}
}

// TestIngestRunAgainstRealJournalPackage is the promised fast-follow now that
// #8 (internal/journal, PR #56) has landed on main: it writes a run with the
// REAL journal.Run API (not the hand-written fixtures in fixture_test.go) and
// ingests the real on-disk output. This is belt-and-suspenders on top of the
// hand-written fixtures — it proves the mirror types in mirror.go read
// exactly what the real package writes, closing the drift risk called out in
// PR #59's review notes. Production code (mirror.go/reader.go/ingest.go)
// still does not import internal/journal — only this test does.
func TestIngestRunAgainstRealJournalPackage(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")

	runID, err := telemetry.NewRunID()
	if err != nil {
		t.Fatalf("generate trace id: %v", err)
	}

	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implement",
		WorkflowVersion: 3,
		WorkflowDigest:  "sha256:deadbeefcafef00d",
		GooberDigest:    "sha256:resolvedgoobers",
		Gaggle:          "web",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "issue-42"},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	must(run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1, AttemptClass: journal.AttemptPolicy}))
	must(run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "build", Attempt: 1, Status: "succeeded"}))
	must(run.Append(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "approve", Target: "deploy"}))
	must(run.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "issue", ID: "42", URL: "https://github.com/acme/app/issues/42"},
		Runner:      map[string]any{"operation": "claim"},
	}))
	must(run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "deploy", Attempt: 1, AttemptClass: journal.AttemptPolicy}))
	must(run.Append(journal.Event{Type: journal.EventError, Stage: "deploy", Attempt: 1, Error: &journal.ErrorDetail{Code: "provider.rate_limit", Message: "github secondary rate limit hit"}}))
	must(run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "deploy", Attempt: 1, Status: "failed"}))
	must(run.Append(journal.Event{Type: journal.EventRunFinished, Status: "failed"}))
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := openTestDB(t, tmp)
	if err := db.IngestRun(context.Background(), run.Dir()); err != nil {
		t.Fatalf("IngestRun against real journal output: %v", err)
	}

	runs, err := db.Runs(context.Background())
	if err != nil || len(runs) != 1 {
		t.Fatalf("Runs: %v, %#v", err, runs)
	}
	r := runs[0]
	if r.RunID != runID || r.Workflow != "implement" || r.WorkflowVersion != 3 ||
		r.GooberDigest != "sha256:resolvedgoobers" || r.Gaggle != "web" ||
		r.TriggerKind != "item" || r.TriggerRef != "issue-42" || r.Status != "failed" {
		t.Fatalf("unexpected run row from real journal output: %#v", r)
	}

	stages, err := db.StageAttempts(context.Background(), runID)
	if err != nil || len(stages) != 2 {
		t.Fatalf("StageAttempts: %v, %#v", err, stages)
	}
	if stages[1].Stage != "deploy" || stages[1].ErrorClass != "provider-rate-limit" {
		t.Fatalf("unexpected deploy stage: %#v", stages[1])
	}

	gates, err := db.GateVerdicts(context.Background(), runID)
	if err != nil || len(gates) != 1 || gates[0].Verdict != "approve" {
		t.Fatalf("GateVerdicts: %v, %#v", err, gates)
	}

	muts, err := db.ProviderMutations(context.Background(), runID)
	if err != nil || len(muts) != 1 || muts[0].ExternalID != "42" || muts[0].Operation != "claim" {
		t.Fatalf("ProviderMutations: %v, %#v", err, muts)
	}
}

func TestIngestCommentOnlyRunRecordsPullRequestMutation(t *testing.T) {
	tmp := t.TempDir()
	runID, err := telemetry.NewRunID()
	if err != nil {
		t.Fatalf("generate trace id: %v", err)
	}
	run, err := journal.Create(filepath.Join(tmp, "runs"), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "pr-remediation",
		WorkflowVersion: 1,
		Gaggle:          "web",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "pr-4384"},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	mustAppend := func(event journal.Event) {
		t.Helper()
		if err := run.Append(event); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	mustAppend(journal.Event{Type: journal.EventStageStarted, Stage: "respond-to-findings", Attempt: 1})
	mustAppend(journal.Event{
		Type:        journal.EventRefTouched,
		Stage:       "respond-to-findings",
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "4384", URL: "https://github.com/Agent-Clubhouse/Goobers/pull/4384"},
		Runner:      map[string]any{"operation": "comment"},
	})
	mustAppend(journal.Event{Type: journal.EventStageFinished, Stage: "respond-to-findings", Attempt: 1, Status: string(apiv1.ResultSuccess)})
	mustAppend(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)})
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := openTestDB(t, tmp)
	if err := db.IngestRun(context.Background(), run.Dir()); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}
	mutations, err := db.ProviderMutations(context.Background(), runID)
	if err != nil {
		t.Fatalf("ProviderMutations: %v", err)
	}
	if len(mutations) != 1 {
		t.Fatalf("ProviderMutations = %#v, want one PR comment mutation", mutations)
	}
	mutation := mutations[0]
	if mutation.Provider != "github" || mutation.Kind != "pr" ||
		mutation.ExternalID != "4384" || mutation.Operation != "comment" {
		t.Fatalf("ProviderMutations[0] = %#v, want github PR 4384 comment", mutation)
	}
}
