package readservice

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

func TestTerminalSummaryParityAcrossReadSurfaces(t *testing.T) {
	service, layout, machine := fixtureService(t)
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	telemetryStore := &fakeTelemetryStore{}
	want := make(map[string]*readmodel.TerminalSummary)
	fixtures := []struct {
		name           string
		phase          journal.RunPhase
		stageStatus    string
		verdict        string
		operation      string
		code           string
		classification journal.TerminalClassification
	}{
		{name: "opened-pr", phase: journal.PhaseCompleted, stageStatus: "success", operation: "open"},
		{name: "merged", phase: journal.PhaseCompleted, stageStatus: "success", verdict: "merged", operation: "merge"},
		{name: "refused", phase: journal.PhaseCompleted, verdict: "refused"},
		{name: "no-work", phase: journal.PhaseCompleted, stageStatus: "no-work"},
		{name: "failed", phase: journal.PhaseFailed, stageStatus: "failure", code: "stage_failed", classification: journal.TerminalStageFailure},
		{name: "aborted", phase: journal.PhaseAborted, code: "run_canceled", classification: journal.TerminalOperatorAbort},
		{name: "escalated", phase: journal.PhaseEscalated, code: "review_limit", classification: journal.TerminalPolicyExhaustion},
		{name: "legacy", phase: journal.PhaseCompleted},
	}
	for _, fixture := range fixtures {
		runID := "terminal-" + fixture.name
		run, clock := createFixtureRun(t, layout, machine, runID, machine.Def.Name, "goobers", fixedTime, journal.Trigger{Kind: journal.TriggerItem, Ref: "1"}, false)
		appendEvent := func(event journal.Event) {
			t.Helper()
			clock.advance(time.Millisecond)
			if err := run.Append(event); err != nil {
				t.Fatal(err)
			}
		}
		appendEvent(journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "issue", ID: "1"}, Runner: map[string]any{"operation": "claim"}})
		if fixture.stageStatus != "" {
			appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
			event := journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: fixture.stageStatus}
			if fixture.code != "" {
				event.Error = &journal.ErrorDetail{Code: fixture.code}
			}
			appendEvent(event)
		}
		if fixture.verdict != "" {
			appendEvent(journal.Event{Type: journal.EventGateEvaluated, Gate: "merge-review", Verdict: fixture.verdict, Target: "@complete"})
		}
		if fixture.operation != "" {
			var confirmation *providers.MergeConfirmation
			if fixture.operation == "merge" {
				confirmation = &providers.MergeConfirmation{RepositoryAPIURL: "https://api.github.com/repos/acme/web", PullID: "2", MergeSHA: "abc"}
			}
			appendEvent(journal.Event{Type: journal.EventRefTouched, Stage: "open-pr", ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "2", URL: "https://github.com/acme/web/pull/2"}, Runner: providers.MutationRunnerFields(fixture.operation, confirmation, nil, nil)})
		}
		finish := journal.Event{Type: journal.EventRunFinished, Status: string(fixture.phase), Disposition: readmodel.DispositionProduced}
		if fixture.stageStatus == "no-work" {
			finish.Disposition = readmodel.DispositionNoWork
		}
		if fixture.name == "legacy" {
			finish.Disposition = ""
		}
		if fixture.code != "" {
			finish.TerminalCause = &journal.TerminalCause{Schema: journal.TerminalCauseSchema, Phase: fixture.phase, Classification: fixture.classification, SelectorKind: "condition", Code: fixture.code, CausalEventSeq: 2}
		}
		appendEvent(finish)
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		detail, err := service.GetRun(t.Context(), runID)
		if err != nil {
			t.Fatal(err)
		}
		want[runID] = detail.TerminalSummary
		if detail.TerminalSummary == nil || detail.TerminalSummary.Phase != fixture.phase {
			t.Fatalf("missing terminal summary: %+v", detail)
		}
		if !reflect.DeepEqual(detail.Outcome, detail.TerminalSummary.Outcome) {
			t.Fatalf("canonical and existing outcome differ: %+v / %+v", detail.Outcome, detail.TerminalSummary.Outcome)
		}
		reader, err := journal.OpenRead(filepath.Join(layout.RunsDir(), runID))
		if err != nil {
			t.Fatal(err)
		}
		identity, err := reader.Identity()
		if err != nil {
			t.Fatal(err)
		}
		events, err := reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertRun(t.Context(), readmodel.ProjectRun(identity, readmodel.Projection{}, events)); err != nil {
			t.Fatal(err)
		}
		telemetryStore.outcomes = append(telemetryStore.outcomes, rollup.ImplementationOutcome{RunID: runID, ItemID: "1", Status: string(fixture.phase), StartedAt: detail.StartedAt, FinishedAt: *detail.FinishedAt})
	}
	page, err := service.ListRuns(t.Context(), RunListOptions{ShowNoWork: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Runs {
		if !reflect.DeepEqual(item.TerminalSummary, want[item.ID]) {
			t.Fatalf("journal list differs for %s", item.ID)
		}
	}
	service.sources.ReadModel = store
	service.EnableReadModelReads()
	service.telemetry = &Telemetry{store: telemetryStore}
	opens := 0
	openRunObserver = func(string) { opens++ }
	t.Cleanup(func() { openRunObserver = nil })
	page, err = service.ListRuns(t.Context(), RunListOptions{ShowNoWork: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != len(want) {
		t.Fatalf("list size = %d, want %d", len(page.Runs), len(want))
	}
	for _, item := range page.Runs {
		if !reflect.DeepEqual(item.TerminalSummary, want[item.ID]) {
			t.Fatalf("projected list differs for %s: %+v / %+v", item.ID, item.TerminalSummary, want[item.ID])
		}
	}
	metrics, err := service.TelemetryImplementationOutcomes(t.Context(), TelemetryImplementationOutcomesRequest{Gaggle: "goobers", Since: fixedTime.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics.Items) != len(want) {
		t.Fatalf("metrics size = %d, want %d", len(metrics.Items), len(want))
	}
	for _, item := range metrics.Items {
		if !reflect.DeepEqual(item.TerminalSummary, want[item.RunID]) {
			t.Fatalf("telemetry differs for %s: %+v / %+v", item.RunID, item.TerminalSummary, want[item.RunID])
		}
	}
	if opens != 0 {
		t.Fatalf("bounded list/telemetry opened %d journals", opens)
	}
}

func TestTerminalSummaryTelemetryUnavailableAndLifecycleMismatch(t *testing.T) {
	store := &fakeTelemetryStore{outcomes: []rollup.ImplementationOutcome{{RunID: "run", Status: "completed", FinishedAt: fixedTime}}}
	service := &Local{telemetry: &Telemetry{store: store}}
	for _, row := range []*readmodel.RunRow{nil, {RunID: "run", Phase: journal.PhaseRunning}, {RunID: "run", Phase: journal.PhaseCompleted, FinishedAt: &[]time.Time{fixedTime.Add(time.Second)}[0]}} {
		if row != nil {
			service.sources.ReadModel = terminalSummaryReader{row: *row}
			service.EnableReadModelReads()
		}
		got, err := service.TelemetryImplementationOutcomes(t.Context(), TelemetryImplementationOutcomesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		summary := got.Items[0].TerminalSummary
		if summary == nil || summary.Status != "unavailable" || summary.WorkDisposition != readmodel.DispositionUnknown || !summary.Legacy {
			t.Fatalf("unavailable telemetry fabricated facts: %+v", summary)
		}
	}
	sentinel := errors.New("read projection failed")
	service.sources.ReadModel = terminalSummaryReader{err: sentinel}
	if _, err := service.TelemetryImplementationOutcomes(t.Context(), TelemetryImplementationOutcomesRequest{}); !errors.Is(err, sentinel) {
		t.Fatalf("read error = %v", err)
	}
}

type terminalSummaryReader struct {
	readmodel.Reader
	row readmodel.RunRow
	err error
}

func (r terminalSummaryReader) GetRun(_ context.Context, _ string) (readmodel.RunRow, bool, error) {
	return r.row, r.row.RunID != "", r.err
}
