package runner

import (
	"encoding/json"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestRecoveryVerdictResolverReadsOwnDurableArtifact(t *testing.T) {
	run := newRunnerTestJournal(t, "recovery-evidence")
	prior := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Rationale: "  Original rationale.\n\nAll paragraphs.  ", Findings: []apiv1.Finding{{Severity: apiv1.SeverityError, Message: "Keep the original finding."}}}
	data, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := run.RecordBranchArtifact(1, "verdict/review.json", data)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Branch: 1, Verdict: "needs-changes", Ref: &ref}); err != nil {
		t.Fatal(err)
	}
	// A newer sibling event with the same gate name must not shadow our review.
	if err := run.Append(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Branch: 2, Verdict: "pass"}); err != nil {
		t.Fatal(err)
	}
	got, err := recoveryVerdictResolver(&branchJournal{run: run, branch: 1})("review")
	if err != nil || got == nil || !reflect.DeepEqual(*got, prior) {
		t.Fatalf("durable recovery=%+v err=%v", got, err)
	}
}

func TestRecoveryVerdictIsScopedToBranchAndParallelGeneration(t *testing.T) {
	prior := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Rationale: "Original branch review."}
	data, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	ref := journal.Ref{Digest: "expected"}
	for _, tc := range []struct {
		name   string
		events []journal.Event
		branch int
		want   bool
	}{
		{"root", []journal.Event{{Type: journal.EventGateEvaluated, Gate: "review", Ref: &ref}, {Type: journal.EventGateEvaluated, Gate: "review", Branch: 2}}, 0, true},
		{"own branch", []journal.Event{{Type: journal.EventParallelStarted}, {Type: journal.EventGateEvaluated, Gate: "review", Branch: 1, Ref: &ref}, {Type: journal.EventGateEvaluated, Gate: "review", Branch: 2}}, 1, true},
		{"sibling only", []journal.Event{{Type: journal.EventGateEvaluated, Gate: "review", Branch: 2, Ref: &ref}}, 1, false},
		{"previous parallel block", []journal.Event{{Type: journal.EventGateEvaluated, Gate: "review", Branch: 1, Ref: &ref}, {Type: journal.EventParallelStarted}}, 1, false},
		{"latest missing evidence", []journal.Event{{Type: journal.EventGateEvaluated, Gate: "review", Ref: &ref}, {Type: journal.EventGateEvaluated, Gate: "review"}}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := latestRecoveryVerdict(tc.events, "review", tc.branch, func(got journal.Ref) ([]byte, error) {
				if got.Digest != ref.Digest {
					t.Fatalf("unexpected artifact: %+v", got)
				}
				return data, nil
			})
			if err != nil || (got != nil) != tc.want {
				t.Fatalf("verdict=%+v want=%v err=%v", got, tc.want, err)
			}
			if got != nil && got.Rationale != prior.Rationale {
				t.Fatalf("changed rationale: %+v", got)
			}
		})
	}
}
