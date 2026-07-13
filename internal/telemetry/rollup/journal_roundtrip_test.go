package rollup

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

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
	if err := db.IngestRun(run.Dir()); err != nil {
		t.Fatalf("IngestRun against real journal output: %v", err)
	}

	runs, err := db.Runs()
	if err != nil || len(runs) != 1 {
		t.Fatalf("Runs: %v, %#v", err, runs)
	}
	r := runs[0]
	if r.RunID != runID || r.Workflow != "implement" || r.WorkflowVersion != 3 ||
		r.Gaggle != "web" || r.TriggerKind != "item" || r.TriggerRef != "issue-42" || r.Status != "failed" {
		t.Fatalf("unexpected run row from real journal output: %#v", r)
	}

	stages, err := db.StageAttempts(runID)
	if err != nil || len(stages) != 2 {
		t.Fatalf("StageAttempts: %v, %#v", err, stages)
	}
	if stages[1].Stage != "deploy" || stages[1].ErrorClass != "provider-rate-limit" {
		t.Fatalf("unexpected deploy stage: %#v", stages[1])
	}

	gates, err := db.GateVerdicts(runID)
	if err != nil || len(gates) != 1 || gates[0].Verdict != "approve" {
		t.Fatalf("GateVerdicts: %v, %#v", err, gates)
	}

	muts, err := db.ProviderMutations(runID)
	if err != nil || len(muts) != 1 || muts[0].ExternalID != "42" || muts[0].Operation != "claim" {
		t.Fatalf("ProviderMutations: %v, %#v", err, muts)
	}
}
