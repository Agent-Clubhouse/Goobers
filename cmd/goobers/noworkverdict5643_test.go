package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// seedVerdictRun adds a run to l whose implement stage finished with status
// and outputs, taking over the item's claim from priorRunID.
func seedVerdictRun(t *testing.T, l instance.Layout, runID, priorRunID string, status apiv1.ResultStatus, outputs map[string]any) {
	t.Helper()
	seedItemRepositoryForTest(t, l, runID, "5629", noWorkStreakRepo)
	reclaimForRun(t, l, runID, "5629", priorRunID)
	seedRunWithEvents(t, l, runID, []journal.Event{{
		Type: journal.EventStageFinished, Stage: "implement", Status: string(status), Outputs: outputs,
	}})
}

// newVerdictLayout returns a layout whose item 5629 is claimed by "run-seed",
// a run that is never settled — later runs take the claim over from it.
func newVerdictLayout(t *testing.T) instance.Layout {
	t.Helper()
	return seedNoWorkStreakRun(t, "run-seed", "5629", string(apiv1.ResultSuccess), "")
}

func commentCalls(calls []providers.UpdateWorkItemRequest, marker string) []providers.UpdateWorkItemRequest {
	var out []providers.UpdateWorkItemRequest
	for _, call := range calls {
		if strings.HasPrefix(call.Comment, marker) {
			out = append(out, call)
		}
	}
	return out
}

// TestNoWorkVerdictIsRecordedOnTheIssue is #5643's first half: a no-work
// verdict below the park threshold is written to the issue with its
// classification, reason and evidence, instead of existing only in the run.
func TestNoWorkVerdictIsRecordedOnTheIssue(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	l := newVerdictLayout(t)
	seedVerdictRun(t, l, "run-a", "run-seed", apiv1.ResultNoWork, map[string]any{
		"status": "already-fixed", "reason": "the flake fix is already on main", "existingCommit": "02642a86a",
	})
	if err := settleNoWorkStreak(context.Background(), fake, l, "run-a", "implement", "https://runs.example/run-a"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	verdicts := commentCalls(fake.calls, noWorkVerdictMarker)
	if len(verdicts) != 1 {
		t.Fatalf("verdict comments = %d (calls %+v), want 1", len(verdicts), fake.calls)
	}
	call := verdicts[0]
	if len(call.AddLabels) != 0 || len(call.RemoveLabels) != 0 {
		t.Fatalf("a recorded verdict below threshold must not change labels: %+v", call)
	}
	for _, want := range []string{"`already-fixed`", "the flake fix is already on main", "02642a86a", "https://runs.example/run-a", "1 of 3"} {
		if !strings.Contains(call.Comment, want) {
			t.Fatalf("verdict comment missing %q:\n%s", want, call.Comment)
		}
	}
	record, err := loadNoWorkStreakRecord(context.Background(), l, noWorkStreakRepo, "5629")
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if record.Verdict != "already-fixed" || record.Evidence != "02642a86a" || record.RunID != "run-a" {
		t.Fatalf("record = %+v, want the verdict persisted", record)
	}
}

// TestContradictingNoWorkVerdictIsFlagged covers two no-work runs that
// classified the same item differently.
func TestContradictingNoWorkVerdictIsFlagged(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	l := newVerdictLayout(t)
	seedVerdictRun(t, l, "run-a", "run-seed", apiv1.ResultNoWork, map[string]any{"status": "already-fixed"})
	if err := settleNoWorkStreak(context.Background(), fake, l, "run-a", "implement", ""); err != nil {
		t.Fatalf("settle a: %v", err)
	}
	seedVerdictRun(t, l, "run-b", "run-a", apiv1.ResultNoWork, map[string]any{"status": "not-actionable"})
	if err := settleNoWorkStreak(context.Background(), fake, l, "run-b", "implement", ""); err != nil {
		t.Fatalf("settle b: %v", err)
	}
	verdicts := commentCalls(fake.calls, noWorkVerdictMarker)
	if len(verdicts) != 2 {
		t.Fatalf("verdict comments = %d, want 2", len(verdicts))
	}
	if strings.Contains(verdicts[0].Comment, "contradicts") {
		t.Fatalf("the first verdict has nothing to contradict:\n%s", verdicts[0].Comment)
	}
	if !strings.Contains(verdicts[1].Comment, "contradicts the previous one") || !strings.Contains(verdicts[1].Comment, "run-a") {
		t.Fatalf("second verdict comment does not flag the contradiction:\n%s", verdicts[1].Comment)
	}
}

// TestProductiveRunAfterNoWorkVerdictIsFlagged is #5629's incident: an
// already-fixed verdict followed twenty minutes later by a run that produced
// the fix. The disagreement is written to the issue, once.
func TestProductiveRunAfterNoWorkVerdictIsFlagged(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	l := newVerdictLayout(t)
	seedVerdictRun(t, l, "run-a", "run-seed", apiv1.ResultNoWork, map[string]any{
		"status": "already-fixed", "reason": "already fixed on main",
	})
	if err := settleNoWorkStreak(context.Background(), fake, l, "run-a", "implement", ""); err != nil {
		t.Fatalf("settle no-work: %v", err)
	}
	seedVerdictRun(t, l, "run-b", "run-a", apiv1.ResultSuccess, nil)
	for i := 0; i < 2; i++ { // a retried terminal notification must not flag twice
		if err := settleNoWorkStreak(context.Background(), fake, l, "run-b", "implement", "https://runs.example/run-b"); err != nil {
			t.Fatalf("settle productive %d: %v", i, err)
		}
	}
	flags := commentCalls(fake.calls, noWorkContradictionMarker)
	if len(flags) != 1 {
		t.Fatalf("contradiction comments = %d, want exactly 1", len(flags))
	}
	for _, want := range []string{"run-a", "`already-fixed`", "already fixed on main", "https://runs.example/run-b"} {
		if !strings.Contains(flags[0].Comment, want) {
			t.Fatalf("contradiction comment missing %q:\n%s", want, flags[0].Comment)
		}
	}
	record, err := loadNoWorkStreakRecord(context.Background(), l, noWorkStreakRepo, "5629")
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if record.Count != 0 {
		t.Fatalf("count = %d, want the streak reset", record.Count)
	}
}

// TestContradictionCommentFailureStillResetsStreak keeps #5379's rule: the
// reset depends only on the state plane, even when the flag cannot be posted.
func TestContradictionCommentFailureStillResetsStreak(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	l := newVerdictLayout(t)
	seedVerdictRun(t, l, "run-a", "run-seed", apiv1.ResultNoWork, map[string]any{"status": "already-fixed"})
	if err := settleNoWorkStreak(context.Background(), fake, l, "run-a", "implement", ""); err != nil {
		t.Fatalf("settle no-work: %v", err)
	}
	seedVerdictRun(t, l, "run-b", "run-a", apiv1.ResultSuccess, nil)
	fake.updateErr = io.ErrUnexpectedEOF
	if err := settleNoWorkStreak(context.Background(), fake, l, "run-b", "implement", ""); err == nil {
		t.Fatal("want the failed contradiction comment reported")
	}
	record, err := loadNoWorkStreakRecord(context.Background(), l, noWorkStreakRepo, "5629")
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if record.Count != 0 {
		t.Fatalf("count = %d, want the reset to land despite the provider failure", record.Count)
	}
}

// TestClaimedItemCarriesPriorNoWorkVerdict is #5643's read side: query-backlog
// hands the recorded verdict to the run that claims the item next.
func TestClaimedItemCarriesPriorNoWorkVerdict(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	l := newVerdictLayout(t)
	item, err := json.Marshal(providers.WorkItem{ID: "5629", Title: "flake"})
	if err != nil {
		t.Fatal(err)
	}
	if got := withPriorNoWorkVerdict(context.Background(), l, noWorkStreakRepo, "5629", item, io.Discard); string(got) != string(item) {
		t.Fatalf("an item with no recorded verdict changed:\n%s", got)
	}

	seedVerdictRun(t, l, "run-a", "run-seed", apiv1.ResultNoWork, map[string]any{
		"status": "already-fixed", "noWorkReason": "fixed by an earlier commit", "existingCommit": "02642a86a",
	})
	if err := settleNoWorkStreak(context.Background(), fake, l, "run-a", "implement", ""); err != nil {
		t.Fatalf("settle: %v", err)
	}
	enriched := withPriorNoWorkVerdict(context.Background(), l, noWorkStreakRepo, "5629", item, io.Discard)
	var decoded struct {
		providers.WorkItem
		Prior *priorNoWorkVerdict `json:"priorNoWorkVerdict"`
	}
	if err := json.Unmarshal(enriched, &decoded); err != nil {
		t.Fatalf("decode %s: %v", enriched, err)
	}
	if decoded.ID != "5629" || decoded.Title != "flake" {
		t.Fatalf("item fields lost: %+v", decoded.WorkItem)
	}
	want := priorNoWorkVerdict{Count: 1, Stage: "implement", Verdict: "already-fixed", Reason: "fixed by an earlier commit", Evidence: "02642a86a", RunID: "run-a"}
	if decoded.Prior == nil {
		t.Fatalf("claimed item = %s, want %s", enriched, priorNoWorkVerdictField)
	}
	got := *decoded.Prior
	got.RecordedAt = want.RecordedAt
	if got != want || decoded.Prior.RecordedAt.IsZero() {
		t.Fatalf("prior verdict = %+v, want %+v with a timestamp", *decoded.Prior, want)
	}
}
