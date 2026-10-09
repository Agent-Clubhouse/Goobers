package decisiongate

import (
	"context"

	"github.com/goobers/goobers/internal/decider"
)

// PublicationLeakQuestion is the threshold key for outgoing-text review.
const PublicationLeakQuestion = "contains_non_public_details"

// DefaultPublicationLeakThreshold keeps ambiguous text on the existing path.
var DefaultPublicationLeakThreshold = Threshold{Accept: 0.9, Reject: 0.1}

var publicationLeakQuestion = decider.Noul(
	"Does this proposed public pull request or issue text disclose non-public operational, personal, credential, customer, or internal infrastructure details that should receive human review before publication?",
	&decider.NoulCriteria{
		True:  "The text appears to disclose details that are not appropriate for a public repository.",
		False: "The text contains only ordinary public project information.",
	},
)

type publicationText struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// EvaluatePublication judges semantics only. Deterministic validation and
// secret-pattern checks remain the caller's authoritative publication gates.
func (g *Gate) EvaluatePublication(ctx context.Context, kind, title, body string) (Outcome, error) {
	return g.JudgeNoul(ctx, PublicationLeakQuestion, publicationText{
		Kind: kind, Title: title, Body: body,
	}, publicationLeakQuestion)
}
