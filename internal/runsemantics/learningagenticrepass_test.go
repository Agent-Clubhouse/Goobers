package runsemantics

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/workflow"
)

// TestLearningEpisodeAppliesToBranchIsTheCanonicalPredicate is the table form
// of the ruling as #3942 extends it. Each row names the branch shape and the
// reason, because the value of this predicate is entirely in what it EXCLUDES.
func TestLearningEpisodeAppliesToBranchIsTheCanonicalPredicate(t *testing.T) {
	for _, tt := range []struct {
		name   string
		branch LearningEpisodeBranch
		want   bool
		why    string
	}{
		{
			name:   "agentic reviewer needs-changes re-entering its implementer",
			branch: LearningEpisodeBranch{Outcome: string(apiv1.VerdictNeedsChanges), Target: "implement", Attempt: 1},
			want:   true,
			why: "#3942: the canonical true repass. The retry classifier declines it (not an automated " +
				"status-equals gate, not infra), and before this the classifier's veto is what withheld " +
				"the correction from the flagship implementation lane",
		},
		{
			name:   "automated policy failure re-entering its implementer",
			branch: LearningEpisodeBranch{Outcome: gate.OutcomeFail, Target: "implement", Attempt: 1},
			want:   true,
			why:    "the deterministic repass #3913 ported; unchanged",
		},
		{
			name:   "infrastructure outcome re-entering its stage",
			branch: LearningEpisodeBranch{Outcome: gate.OutcomeInfra, Target: "local-ci", Attempt: 2},
			want:   true,
			why:    "an infra repass is still a repass; the ruling is about re-entry, not about failure class",
		},
		{
			name:   "a passing gate",
			branch: LearningEpisodeBranch{Outcome: gate.OutcomePass, Target: "push-branch", Attempt: 1},
			want:   false,
			why:    "an advancing gate has nothing to teach; the subject was accepted",
		},
		{
			name:   "an escalated needs-changes",
			branch: LearningEpisodeBranch{Outcome: string(apiv1.VerdictNeedsChanges), Target: "park-escalated", Escalated: true, Attempt: 3},
			want:   false,
			why: "budget exhaustion and the #316 duplicate-diff guard both resolve Escalated: the branch " +
				"is a disposition, and the stage it names is not being asked to try again",
		},
		{
			name:   "a forward branch to a stage that has not run",
			branch: LearningEpisodeBranch{Outcome: string(apiv1.VerdictNeedsChanges), Target: "park-needs-human", Attempt: 0},
			want:   false,
			why:    "#3929: repassAttempt 0 means the target has never run and has produced nothing to correct",
		},
		{
			name:   "a terminal @abort branch",
			branch: LearningEpisodeBranch{Outcome: gate.OutcomeFail, Target: workflow.TargetAbort, Attempt: 1},
			want:   false,
			why:    "a reserved terminal is not a stage; there is no dispatch to inject into",
		},
		{
			name:   "a terminal @escalate branch",
			branch: LearningEpisodeBranch{Outcome: gate.OutcomeFail, Target: workflow.TargetEscalate, Attempt: 1},
			want:   false,
			why:    "same, and the run is ending rather than repassing",
		},
		{
			name:   "a terminal complete branch",
			branch: LearningEpisodeBranch{Outcome: string(apiv1.VerdictNeedsChanges), Target: workflow.TerminalComplete, Attempt: 1},
			want:   false,
			why:    "same; retained verbatim from routeRetryDecision's guard set",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := LearningEpisodeAppliesToBranch(tt.branch); got != tt.want {
				t.Fatalf("LearningEpisodeAppliesToBranch(%+v) = %v, want %v — %s", tt.branch, got, tt.want, tt.why)
			}
		})
	}
}

// LearningEpisodeAppliesToBranch must remain a strict refinement of the #3929
// attempt predicate: everything it admits, LearningEpisodeAppliesToRepass
// admits too. If that ever stopped holding, #3929's ruling and #3942's
// extension would have drifted apart and the forward-branch regressions next
// door would be pinning a rule nothing enforces.
func TestLearningEpisodeBranchPredicateRefinesTheRepassPredicate(t *testing.T) {
	outcomes := []string{
		gate.OutcomePass, gate.OutcomeFail, gate.OutcomeInfra,
		string(apiv1.VerdictNeedsChanges), string(apiv1.VerdictPass),
	}
	targets := []string{
		"implement", "guard-before-implement", "park-needs-human",
		workflow.TargetAbort, workflow.TargetEscalate, workflow.TerminalComplete,
	}
	for _, outcome := range outcomes {
		for _, target := range targets {
			for _, escalated := range []bool{false, true} {
				for attempt := -1; attempt <= 3; attempt++ {
					b := LearningEpisodeBranch{
						Outcome: outcome, Target: target, Escalated: escalated, Attempt: attempt,
					}
					if LearningEpisodeAppliesToBranch(b) && !LearningEpisodeAppliesToRepass(attempt) {
						t.Fatalf("branch %+v is admitted by LearningEpisodeAppliesToBranch but not by "+
							"LearningEpisodeAppliesToRepass; #3942 widens WHICH failure classes reach the "+
							"question, never the #3929 answer to it", b)
					}
				}
			}
		}
	}
}
