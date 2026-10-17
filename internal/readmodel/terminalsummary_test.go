package readmodel

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func terminalEvents(events ...journal.Event) []journal.Event {
	for i := range events {
		events[i].Schema = journal.EventSchema
		events[i].Seq = uint64(i + 1)
		events[i].Time = projectBase.Add(time.Duration(i) * time.Millisecond)
	}
	return events
}

func terminalRef(kind, id, operation string) journal.Event {
	return journal.Event{Type: journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: kind, ID: id, URL: "https://example.test/" + kind + "/" + id},
		Runner:      map[string]any{"operation": operation}}
}

func projectedTerminal(events []journal.Event) *TerminalSummary {
	row := ProjectRun(testIdentity(), Projection{}, events).Run
	return row.Operator.Terminal.Summary(row.Phase, row.Operator.Reliability.TerminalCause)
}

func TestTerminalSummaryEvidenceAxes(t *testing.T) {
	for _, tt := range []struct {
		name        string
		phase       journal.RunPhase
		events      []journal.Event
		disposition string
		wantWork    string
		wantStatus  string
		wantRef     string
		wantVerdict string
		cause       *journal.TerminalCause
	}{
		{name: "opened PR", phase: journal.PhaseCompleted, events: []journal.Event{terminalRef("issue", "1", "claim"), terminalRef("pr", "2", "open"), terminalRef("branch", "head", "")}, disposition: DispositionProduced, wantWork: DispositionProduced, wantStatus: "complete", wantRef: "2"},
		{name: "unfavorable business outcome", phase: journal.PhaseCompleted, events: []journal.Event{{Type: journal.EventGateEvaluated, Gate: "merge-review", Verdict: "refused", Target: "@complete"}}, disposition: DispositionProduced, wantWork: DispositionUnknown, wantStatus: "partial", wantVerdict: "refused"},
		{name: "semantic no work", phase: journal.PhaseCompleted, events: []journal.Event{{Type: journal.EventStageStarted, Stage: "query"}, {Type: journal.EventStageFinished, Stage: "query", Status: "no-work"}}, disposition: DispositionNoWork, wantWork: DispositionNoWork, wantStatus: "complete"},
		{name: "legacy semantic no work", phase: journal.PhaseCompleted, events: []journal.Event{{Type: journal.EventStageStarted, Stage: "query"}, {Type: journal.EventStageFinished, Stage: "query", Status: "no-work"}}, wantWork: DispositionNoWork, wantStatus: "partial"},
		{name: "short successful real work", phase: journal.PhaseCompleted, events: []journal.Event{{Type: journal.EventStageStarted, Stage: "implement"}, {Type: journal.EventStageFinished, Stage: "implement", Status: "success"}}, disposition: DispositionProduced, wantWork: DispositionProduced, wantStatus: "complete"},
		{name: "legacy accounting is unknown", phase: journal.PhaseCompleted, wantWork: DispositionUnknown, wantStatus: "partial"},
		{name: "modern accounting is unknown", phase: journal.PhaseCompleted, disposition: DispositionProduced, wantWork: DispositionUnknown, wantStatus: "partial"},
		{name: "failed", phase: journal.PhaseFailed, disposition: DispositionProduced, wantWork: DispositionUnknown, wantStatus: "partial", cause: &journal.TerminalCause{Schema: journal.TerminalCauseSchema, Phase: journal.PhaseFailed, Classification: journal.TerminalStageFailure, Code: "build_failed", CausalEventSeq: 1}},
		{name: "aborted", phase: journal.PhaseAborted, disposition: DispositionProduced, wantWork: DispositionUnknown, wantStatus: "partial", cause: &journal.TerminalCause{Schema: journal.TerminalCauseSchema, Phase: journal.PhaseAborted, Classification: journal.TerminalOperatorAbort, Code: "run_canceled", CausalEventSeq: 1}},
		{name: "escalated", phase: journal.PhaseEscalated, disposition: DispositionProduced, wantWork: DispositionUnknown, wantStatus: "partial", cause: &journal.TerminalCause{Schema: journal.TerminalCauseSchema, Phase: journal.PhaseEscalated, Classification: journal.TerminalPolicyExhaustion, Code: "review_limit", CausalEventSeq: 1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			events := append(tt.events, journal.Event{Type: journal.EventRunFinished, Status: string(tt.phase), Disposition: tt.disposition, TerminalCause: tt.cause})
			got := projectedTerminal(terminalEvents(events...))
			if got == nil || got.Phase != tt.phase || got.WorkDisposition != tt.wantWork || got.Status != tt.wantStatus {
				t.Fatalf("summary = %+v", got)
			}
			if tt.wantRef != "" && (got.PrimaryExternalResult == nil || got.PrimaryExternalResult.Ref.ID != tt.wantRef) {
				t.Fatalf("primary result = %+v", got.PrimaryExternalResult)
			}
			if tt.wantVerdict != "" && (got.Outcome == nil || got.Outcome.Verdict != tt.wantVerdict || got.Reason != nil) {
				t.Fatalf("workflow outcome became an execution failure: %+v", got)
			}
			if tt.cause != nil && (got.Reason == nil || got.Reason.Source != "terminal-cause" || got.Reason.Code != tt.cause.Code || got.CausalEventSeq != tt.cause.CausalEventSeq) {
				t.Fatalf("typed reason = %+v", got)
			}
		})
	}
}

func TestTerminalSummaryPrimaryResultsRequireConfirmation(t *testing.T) {
	merged := terminalRef("pr", "123", "merge")
	merged.Runner = providers.MutationRunnerFields("merge", &providers.MergeConfirmation{RepositoryAPIURL: "https://api.example.test/repos/acme/web", PullID: "123"}, nil, nil)
	observed := terminalRef("pr", "observed", "merge")
	failed := terminalRef("pr", "failed", "open")
	failed.Runner["outcome"] = "failure"
	events := terminalEvents(merged, terminalRef("pr", "opened", "open"), observed, failed, terminalRef("issue", "claimed", "claim"), terminalRef("commit", "sha", "push"),
		journal.Event{Type: journal.EventRunFinished, Status: "completed", Disposition: DispositionProduced})
	got := projectedTerminal(events)
	if got.PrimaryExternalResult.Ref.ID != "123" || !got.PrimaryExternalResult.ConfirmedMerge || got.WorkDisposition != DispositionProduced {
		t.Fatalf("confirmed result = %+v", got)
	}
	for _, ref := range got.RelatedExternalRefs {
		if ref.Ref.ID == "failed" || ref.ConfirmedMerge {
			t.Fatalf("attempted/observed mutation promoted: %+v", ref)
		}
	}
	got = projectedTerminal(terminalEvents(observed, journal.Event{Type: journal.EventRunFinished, Status: "completed", Disposition: DispositionProduced}))
	if got.PrimaryExternalResult.ConfirmedMerge || got.WorkDisposition != DispositionUnknown {
		t.Fatalf("observed merge became this run's work: %+v", got)
	}
	for _, address := range []string{"bogus", "https://user:password@example.test/repo", "https://example.test/repo?token=secret"} {
		merged.Runner = providers.MutationRunnerFields("merge", &providers.MergeConfirmation{RepositoryAPIURL: address, PullID: "123"}, nil, nil)
		got = projectedTerminal(terminalEvents(merged, journal.Event{Type: journal.EventRunFinished, Status: "completed"}))
		if got.PrimaryExternalResult.ConfirmedMerge || got.WorkDisposition != DispositionUnknown {
			t.Fatalf("invalid merge confirmation accepted: %+v", got)
		}
	}
	got = projectedTerminal(terminalEvents(terminalRef("pr", "1", "observe"), terminalRef("commit", "sha", "observe"), journal.Event{Type: journal.EventRunFinished, Status: "completed"}))
	if got.PrimaryExternalResult.Ref.Kind != "pr" || got.WorkDisposition != DispositionUnknown {
		t.Fatalf("observed commit outranked PR: %+v", got)
	}
}

func TestTerminalSummaryReasonPrecedenceAndRecovery(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventStageStarted, Stage: "build"},
		{Type: journal.EventStageFinished, Stage: "build", Status: "failure", Error: &journal.ErrorDetail{Code: "build_failed"}},
		{Type: journal.EventError, Error: &journal.ErrorDetail{Code: "cleanup_failed"}},
		{Type: journal.EventRunFinished, Status: "failed"},
		{Type: journal.EventError, Error: &journal.ErrorDetail{Code: "notify_failed"}},
	}
	got := projectedTerminal(terminalEvents(events...))
	if got.Reason.Code != "build_failed" || got.CausalEventSeq != 2 || !got.Legacy {
		t.Fatalf("legacy stage error = %+v", got)
	}
	events = append(events[:2], journal.Event{Type: journal.EventStageStarted, Stage: "build"}, journal.Event{Type: journal.EventStageFinished, Stage: "build", Status: "success"},
		journal.Event{Type: journal.EventError, Error: &journal.ErrorDetail{Code: "run_canceled"}}, journal.Event{Type: journal.EventRunFinished, Status: "aborted"})
	got = projectedTerminal(terminalEvents(events...))
	if got.Reason.Code != "run_canceled" || got.CausalEventSeq != 5 {
		t.Fatalf("recovered stage error hid later error: %+v", got)
	}
	events[5].TerminalCause = &journal.TerminalCause{Schema: journal.TerminalCauseSchema, Phase: journal.PhaseAborted, Classification: journal.TerminalOperatorAbort, Code: "operator_abort", CausalEventSeq: 5}
	got = projectedTerminal(terminalEvents(events...))
	if got.Reason.Code != "operator_abort" || got.Reason.Source != "terminal-cause" {
		t.Fatalf("typed cause lost precedence: %+v", got)
	}
	got = projectedTerminal(terminalEvents(
		journal.Event{Type: journal.EventStageFinished, Stage: "build", Branch: 1, Status: "failure", Error: &journal.ErrorDetail{Code: "branch_failed"}},
		journal.Event{Type: journal.EventStageStarted, Stage: "build", Branch: 2},
		journal.Event{Type: journal.EventStageFinished, Stage: "build", Branch: 2, Status: "success"},
		journal.Event{Type: journal.EventRunFinished, Status: "failed"}))
	if got.Reason == nil || got.Reason.Code != "branch_failed" {
		t.Fatalf("sibling success erased failure: %+v", got)
	}
	got = projectedTerminal(terminalEvents(
		journal.Event{Type: journal.EventError, Error: &journal.ErrorDetail{Code: "review_limit"}},
		journal.Event{Type: journal.EventRunFinished, Status: "escalated"},
		journal.Event{Type: journal.EventError, Error: &journal.ErrorDetail{Code: "context_canceled"}},
		journal.Event{Type: journal.EventRunFinished, Status: "aborted"},
		journal.Event{Type: journal.EventError, Error: &journal.ErrorDetail{Code: "cleanup_failed"}}))
	if got.Phase != journal.PhaseAborted || got.Reason.Code != "context_canceled" || got.CausalEventSeq != 3 {
		t.Fatalf("superseding terminal decision = %+v", got)
	}
	got = projectedTerminal(terminalEvents(
		journal.Event{Type: journal.EventStageFinished, Stage: "build", Status: "failure", Error: &journal.ErrorDetail{Code: "build_failed"}},
		journal.Event{Type: journal.EventRunFinished, Status: "failed"},
		journal.Event{Type: journal.EventError, Error: &journal.ErrorDetail{Code: "notify_failed"}},
		journal.Event{Type: journal.EventRunFinished, Status: "failed"}))
	if got.Reason == nil || got.Reason.Code != "build_failed" || got.CausalEventSeq != 1 {
		t.Fatalf("duplicate finish promoted cleanup error: %+v", got)
	}
}

func TestTerminalSummaryNoWorkDoesNotOverrideProducedAccounting(t *testing.T) {
	for _, disposition := range []string{"", DispositionProduced} {
		got := projectedTerminal(terminalEvents(
			journal.Event{Type: journal.EventError, Stage: "query", Error: &journal.ErrorDetail{Code: "executor_error"}},
			journal.Event{Type: journal.EventStageStarted, Stage: "query"},
			journal.Event{Type: journal.EventStageFinished, Stage: "query", Status: "no-work"},
			journal.Event{Type: journal.EventRunFinished, Status: "completed", Disposition: disposition}))
		if got.WorkDisposition != DispositionUnknown {
			t.Fatalf("failed prior attempt became no-work: %+v", got)
		}
	}
}

func TestTerminalSummaryBoundAndIncrementalReplay(t *testing.T) {
	events := []journal.Event{terminalRef("pr", "primary", "open")}
	for i := range 5 * TerminalExternalRefLimit {
		events = append(events, terminalRef("issue", fmt.Sprint(i), "claim"))
	}
	events = terminalEvents(append(events, journal.Event{Type: journal.EventRunFinished, Status: "completed", Disposition: DispositionProduced})...)
	whole := ProjectRun(testIdentity(), Projection{}, events)
	want := whole.Run.Operator.Terminal
	if len(want.Refs) != TerminalExternalRefLimit || !want.RefsTruncated || want.Refs[0].Ref.ID != "primary" {
		t.Fatalf("unbounded or wrong primary: %+v", want)
	}
	for split := range len(events) + 1 {
		prefix := ProjectRun(testIdentity(), Projection{}, events[:split])
		// Match persistence between batches, including zero/empty slices.
		data, err := json.Marshal(prefix.Run.Operator)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &prefix.Run.Operator); err != nil {
			t.Fatal(err)
		}
		got := ProjectRun(testIdentity(), prefix, events[split:]).Run.Operator.Terminal
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d differs from replay", split)
		}
	}
	store := openTestStore(t)
	if err := store.UpsertRun(t.Context(), whole); err != nil {
		t.Fatal(err)
	}
	row, found, err := store.GetRun(t.Context(), testIdentity().RunID)
	if err != nil || !found || !reflect.DeepEqual(row.Operator.Terminal, want) {
		t.Fatalf("persisted facts: found=%v err=%v", found, err)
	}
}

func TestTerminalSummaryLifecycleAndUnknown(t *testing.T) {
	for _, reopen := range []journal.EventType{journal.EventRunResumed, journal.EventGateOverridden, journal.EventStageRerunRequested} {
		events := terminalEvents(terminalRef("pr", "old", "open"), journal.Event{Type: journal.EventRunFinished, Status: "completed", Disposition: DispositionProduced}, journal.Event{Type: reopen, Gate: "review", Verdict: "pass", Target: "implement"})
		if got := projectedTerminal(events); got != nil {
			t.Fatalf("%s active summary = %+v", reopen, got)
		}
		events = append(events, terminalEvents(journal.Event{Type: journal.EventRunFinished, Status: "completed"})[0])
		events[len(events)-1].Seq = 4
		got := projectedTerminal(events)
		if got.PrimaryExternalResult != nil || got.WorkDisposition != DispositionUnknown || !got.Legacy {
			t.Fatalf("%s retained stale terminal facts: %+v", reopen, got)
		}
	}
	got := (TerminalFacts{}).Summary(journal.PhaseCompleted, nil)
	if got.Status != "unavailable" || got.WorkDisposition != DispositionUnknown || !got.Legacy {
		t.Fatalf("missing facts = %+v", got)
	}
}
