package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

type fakeBacklogIntakeJudge struct {
	outcome decisiongate.Outcome
	err     error
	calls   int
}

func (f *fakeBacklogIntakeJudge) EvaluateIntakeRisk(context.Context, decisiongate.IntakeItem, []decisiongate.IntakeItem) (decisiongate.Outcome, error) {
	f.calls++
	return f.outcome, f.err
}

type recordingBacklogIntakeAnnotator struct {
	events []journal.Event
}

func (a *recordingBacklogIntakeAnnotator) Append(event journal.Event) error {
	a.events = append(a.events, event)
	return nil
}

func (*recordingBacklogIntakeAnnotator) Close() error { return nil }

func TestObserveBacklogIntakeShadowRecordsFlagWithoutChangingCandidates(t *testing.T) {
	judge := &fakeBacklogIntakeJudge{outcome: decisiongate.Outcome{
		Name: decisiongate.IntakeRiskQuestion, Decision: decisiongate.Yes, Probability: 0.97,
	}}
	annotations := &recordingBacklogIntakeAnnotator{}
	originalResolve, originalOpen := resolveBacklogIntakeJudge, openStageAnnotator
	resolveBacklogIntakeJudge = func(instance.Layout) (backlogIntakeJudge, float64, bool, error) {
		return judge, 1, true, nil
	}
	openStageAnnotator = func(instance.Layout) (stageAnnotator, error) { return annotations, nil }
	t.Cleanup(func() {
		resolveBacklogIntakeJudge = originalResolve
		openStageAnnotator = originalOpen
	})

	items := []providers.WorkItem{{ID: "42", Title: "candidate"}, {ID: "43", Title: "peer"}}
	observeBacklogIntakeShadow(context.Background(), backlogQueryEnv{
		repo:   providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"},
		stderr: io.Discard,
	}, "run-1", "implementation", "", items[:1], items)

	if judge.calls != 1 || len(annotations.events) != 1 {
		t.Fatalf("calls = %d, events = %d, want one of each", judge.calls, len(annotations.events))
	}
	fields := annotations.events[0].Runner
	if fields["annotation"] != backlogIntakeShadowAnnotation || fields["itemId"] != "42" || fields["flagged"] != true || fields["peerCount"] != 1 {
		t.Fatalf("shadow annotation = %#v", fields)
	}
	if len(items) != 2 || items[0].ID != "42" {
		t.Fatalf("shadow observation changed candidates: %+v", items)
	}
}

func TestObserveBacklogIntakeShadowRecordsUncertainErrorAdvisory(t *testing.T) {
	judge := &fakeBacklogIntakeJudge{
		outcome: decisiongate.Outcome{Decision: decisiongate.Uncertain},
		err:     errors.New("unavailable"),
	}
	annotations := &recordingBacklogIntakeAnnotator{}
	originalResolve, originalOpen := resolveBacklogIntakeJudge, openStageAnnotator
	resolveBacklogIntakeJudge = func(instance.Layout) (backlogIntakeJudge, float64, bool, error) {
		return judge, 1, true, nil
	}
	openStageAnnotator = func(instance.Layout) (stageAnnotator, error) { return annotations, nil }
	t.Cleanup(func() {
		resolveBacklogIntakeJudge = originalResolve
		openStageAnnotator = originalOpen
	})

	observeBacklogIntakeShadow(context.Background(), backlogQueryEnv{stderr: io.Discard}, "run-1", "implementation", "",
		[]providers.WorkItem{{ID: "42"}}, []providers.WorkItem{{ID: "42"}})

	fields := annotations.events[0].Runner
	if fields["verdict"] != string(decisiongate.Uncertain) || fields["flagged"] != false || fields["error"] != true {
		t.Fatalf("uncertain shadow annotation = %#v", fields)
	}
}
