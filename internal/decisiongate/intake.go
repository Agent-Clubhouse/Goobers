package decisiongate

import (
	"context"
	"strings"

	"github.com/goobers/goobers/internal/decider"
)

// IntakeRiskQuestion is the threshold key for prose-only duplicate or blocker
// detection at backlog intake.
const IntakeRiskQuestion = "backlog_intake_risk"

// DefaultIntakeRiskThreshold starts conservatively because intake results are
// shadow-only until measured sample counts justify enforcement.
var DefaultIntakeRiskThreshold = Threshold{Accept: 0.9, Reject: 0.1}

// IntakeItem is the provider-neutral prose the intake decision may inspect.
type IntakeItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
}

type intakeState struct {
	Candidate  IntakeItem   `json:"candidate"`
	OpenIssues []IntakeItem `json:"openIssues"`
}

var intakeRiskQuestion = decider.Noul(
	"Does the candidate duplicate any listed open issue, or does its prose say that work in a listed open issue must finish first?",
	&decider.NoulCriteria{
		True:  "The candidate is semantically the same work as a listed issue, or explicitly depends on a listed issue.",
		False: "The candidate is distinct and its prose does not depend on any listed issue.",
	},
)

const (
	intakeCandidateBodyLimit = 8000
	intakeTitleLimit         = 500
	intakePeerLimit          = 100
)

// EvaluateIntakeRisk judges prose only. Callers must run deterministic native
// dependency checks first and must treat Uncertain or errors as no policy change.
func (g *Gate) EvaluateIntakeRisk(ctx context.Context, candidate IntakeItem, open []IntakeItem) (Outcome, error) {
	candidate.Title = truncateIntakeText(candidate.Title, intakeTitleLimit)
	candidate.Body = truncateIntakeText(candidate.Body, intakeCandidateBodyLimit)
	peers := make([]IntakeItem, 0, min(len(open), intakePeerLimit))
	for _, item := range open {
		if item.ID != candidate.ID {
			peers = append(peers, IntakeItem{ID: item.ID, Title: truncateIntakeText(item.Title, intakeTitleLimit)})
			if len(peers) == intakePeerLimit {
				break
			}
		}
	}
	return g.JudgeNoul(ctx, IntakeRiskQuestion, intakeState{Candidate: candidate, OpenIssues: peers}, intakeRiskQuestion)
}

func truncateIntakeText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	var b strings.Builder
	b.Grow(limit)
	for _, r := range value {
		if b.Len()+len(string(r)) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
