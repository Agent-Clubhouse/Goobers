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
	items   []string
}

func (f *fakeBacklogIntakeJudge) EvaluateIntakeRisk(_ context.Context, item decisiongate.IntakeItem, _ []decisiongate.IntakeItem) (decisiongate.Outcome, error) {
	f.calls++
	f.items = append(f.items, item.ID)
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
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}
	if fields["annotation"] != backlogIntakeShadowAnnotation || fields["itemId"] != "42" ||
		fields["provider"] != string(providers.ProviderGitHub) || fields["repositoryKey"] != repo.CanonicalKey() ||
		fields["flagged"] != true || fields["peerCount"] != 1 {
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

func installBacklogIntakeShadowRecorder(t *testing.T) (*fakeBacklogIntakeJudge, *recordingBacklogIntakeAnnotator) {
	t.Helper()
	judge := &fakeBacklogIntakeJudge{outcome: decisiongate.Outcome{
		Name: decisiongate.IntakeRiskQuestion, Decision: decisiongate.No, Probability: 0.01,
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
	return judge, annotations
}

func TestBacklogIntakeShadowObservesResweepSuppliedCandidate(t *testing.T) {
	judge, annotations := installBacklogIntakeShadowRecorder(t)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Ready re-sweep item", providers.LabelApproved, providers.LabelReady)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "resweep-shadow")
	configureCurationResweep(t, "1", "1")
	t.Setenv("GOOBERS_INPUT_RECONCILEMETADATA", "false")
	t.Chdir(t.TempDir())

	if code, _, stderr := runArgs(t, "backlog-query", "--claim", "--resweep", root); code != 0 {
		t.Fatalf("backlog-query: code=%d stderr=%q", code, stderr)
	}
	if judge.calls != 1 || judge.items[0] != "7" || intakeShadowAnnotationCount(annotations.events) != 1 {
		t.Fatalf("shadow calls=%d items=%v annotations=%d, want only re-sweep item 7",
			judge.calls, judge.items, intakeShadowAnnotationCount(annotations.events))
	}
}

func TestBacklogIntakeShadowResweepModeExcludesForwardCandidates(t *testing.T) {
	judge, annotations := installBacklogIntakeShadowRecorder(t)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(1, "Forward item", providers.LabelApproved)
	server.addIssue(7, "Ready re-sweep item", providers.LabelApproved, providers.LabelReady)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "resweep-only-shadow")
	configureCurationResweep(t, "2", "2")
	t.Setenv("GOOBERS_INPUT_RECONCILEMETADATA", "false")
	t.Chdir(t.TempDir())

	if code, _, stderr := runArgs(t, "backlog-query", "--claim", "--resweep", root); code != 0 {
		t.Fatalf("backlog-query: code=%d stderr=%q", code, stderr)
	}
	if judge.calls != 1 || judge.items[0] != "7" || intakeShadowAnnotationCount(annotations.events) != 1 {
		t.Fatalf("shadow calls=%d items=%v annotations=%d, want only re-sweep item 7",
			judge.calls, judge.items, intakeShadowAnnotationCount(annotations.events))
	}
}

func intakeShadowAnnotationCount(events []journal.Event) int {
	count := 0
	for _, event := range events {
		if event.Runner["annotation"] == backlogIntakeShadowAnnotation {
			count++
		}
	}
	return count
}
