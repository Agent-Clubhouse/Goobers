package runsemantics

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
)

func finishedWith(stage string, paths ...string) journal.Event {
	e := journal.Event{Type: journal.EventStageFinished, Stage: stage}
	for _, p := range paths {
		e.Artifacts = append(e.Artifacts, journal.Ref{Path: p})
	}
	return e
}

func gateVerdict(gateID, verdict string) journal.Event {
	return journal.Event{Type: journal.EventGateEvaluated, Gate: gateID, Verdict: verdict}
}

func artifactContext(name, path string) apiv1.ContextPointer {
	return apiv1.ContextPointer{Name: name, Artifact: &apiv1.ArtifactPointer{Path: path}}
}

func contextNames(pointers []apiv1.ContextPointer) []string {
	names := make([]string, 0, len(pointers))
	for _, p := range pointers {
		names = append(names, p.Name)
	}
	return names
}

// TestReviewerContextPointersWithholdsSupersededValidation pins #5901: a
// local-ci failure that sent the run back to implement must not reach the
// reviewer of the remediated diff, while every other pointer still does.
func TestReviewerContextPointersWithholdsSupersededValidation(t *testing.T) {
	deterministic := func(stage string) bool {
		return stage == "local-ci" || stage == "query-backlog" || stage == "push-branch"
	}
	failedCI := func(path string) []journal.Event {
		return []journal.Event{finishedWith("push-branch"), finishedWith("local-ci", path), gateVerdict("local-gate", "fail")}
	}
	history := func(groups ...[]journal.Event) []journal.Event {
		var out []journal.Event
		for _, g := range groups {
			out = append(out, g...)
		}
		return out
	}
	prelude := []journal.Event{
		finishedWith("query-backlog", "artifacts/backlog"),
		gateVerdict("backlog-gate", gate.OutcomePass),
		finishedWith("implement", "artifacts/impl-1"),
		gateVerdict("review", gate.OutcomePass),
	}
	remediated := []journal.Event{finishedWith("implement", "artifacts/impl-2")}
	backlog := artifactContext("query-backlog.artifact[0]", "artifacts/backlog")
	implement := artifactContext("implement.artifact[0]", "artifacts/impl-2")
	verdict := apiv1.ContextPointer{Name: "review.verdict", External: &apiv1.ExternalRef{}}
	crossRun := apiv1.ContextPointer{Name: "other.artifact[0]", RunID: "other-run", Artifact: &apiv1.ArtifactPointer{Path: "artifacts/ci-old"}}
	withCI := func(path string) []apiv1.ContextPointer {
		return []apiv1.ContextPointer{backlog, artifactContext("local-ci.artifact[0]", path), implement, verdict, crossRun}
	}
	all := []string{"query-backlog.artifact[0]", "local-ci.artifact[0]", "implement.artifact[0]", "review.verdict", "other.artifact[0]"}
	withoutCI := []string{"query-backlog.artifact[0]", "implement.artifact[0]", "review.verdict", "other.artifact[0]"}

	cases := []struct {
		name            string
		events          []journal.Event
		isDeterministic func(string) bool
		pointers        []apiv1.ContextPointer
		want            []string
	}{
		{"remediated after failing validation", history(prelude, failedCI("artifacts/ci-old"), remediated), deterministic, withCI("artifacts/ci-old"), withoutCI},
		{"subject not re-run since the failing verdict", history(prelude, failedCI("artifacts/ci-old")), deterministic, withCI("artifacts/ci-old"), all},
		{"validation passed", history(prelude, []journal.Event{finishedWith("local-ci", "artifacts/ci-old"), gateVerdict("local-gate", gate.OutcomePass)}, remediated), deterministic, withCI("artifacts/ci-old"), all},
		{"evidence produced after the latest subject finish", history(prelude, failedCI("artifacts/ci-old"), remediated, failedCI("artifacts/ci-new")), deterministic, withCI("artifacts/ci-new"), all},
		{"agentic stage judged non-pass", history(prelude, failedCI("artifacts/ci-old"), remediated), func(string) bool { return false }, withCI("artifacts/ci-old"), all},
		{"cross-run pointer to a superseded path", history(prelude, failedCI("artifacts/ci-old"), remediated), deterministic, []apiv1.ContextPointer{crossRun}, []string{"other.artifact[0]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contextNames(ReviewerContextPointers(tc.events, "implement", tc.isDeterministic, tc.pointers))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("pointers = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReviewerContextPointersKeepsSubjectArtifacts guards the subject's own
// output: a review gate failing implement must never hide implement's work.
func TestReviewerContextPointersKeepsSubjectArtifacts(t *testing.T) {
	events := []journal.Event{
		finishedWith("implement", "artifacts/impl-1"),
		gateVerdict("review", "fail"),
		finishedWith("implement", "artifacts/impl-1"),
	}
	pointers := []apiv1.ContextPointer{artifactContext("implement.artifact[0]", "artifacts/impl-1")}
	got := ReviewerContextPointers(events, "implement", func(string) bool { return true }, pointers)
	if len(got) != 1 {
		t.Fatalf("pointers = %v, want subject artifact kept", contextNames(got))
	}
}

// TestReviewerContextPointersKeepsRemediationBrief is the boundary of the
// #5901 rule: a CI failure routed to a dedicated remediation stage never
// tested that stage's output, so its first reviewer must still see it. Only
// a later failure of remediate-ci's own revision is superseded.
func TestReviewerContextPointersKeepsRemediationBrief(t *testing.T) {
	deterministic := func(stage string) bool { return stage == "ci-poll" }
	firstFailure := []journal.Event{
		finishedWith("implement", "artifacts/impl"),
		gateVerdict("review", gate.OutcomePass),
		finishedWith("ci-poll", "artifacts/ci-1"),
		gateVerdict("ci-gate", "fail"),
		finishedWith("remediate-ci", "artifacts/fix-1"),
	}
	brief := []apiv1.ContextPointer{artifactContext("ci-poll.artifact[0]", "artifacts/ci-1")}
	if got := ReviewerContextPointers(firstFailure, "remediate-ci", deterministic, brief); len(got) != 1 {
		t.Fatalf("first remediation review context = %v, want the CI failure kept", contextNames(got))
	}

	secondFailure := append(append([]journal.Event(nil), firstFailure...),
		gateVerdict("review", gate.OutcomePass),
		finishedWith("ci-poll", "artifacts/ci-2"),
		gateVerdict("ci-gate", "fail"),
		finishedWith("remediate-ci", "artifacts/fix-2"),
	)
	stale := []apiv1.ContextPointer{artifactContext("ci-poll.artifact[0]", "artifacts/ci-2")}
	if got := ReviewerContextPointers(secondFailure, "remediate-ci", deterministic, stale); len(got) != 0 {
		t.Fatalf("repeat remediation review context = %v, want the failure of remediate-ci's own revision withheld", contextNames(got))
	}
}
