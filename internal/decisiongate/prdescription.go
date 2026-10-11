package decisiongate

import (
	"context"

	"github.com/goobers/goobers/internal/decider"
)

// PRDescriptionAgreementQuestion is the threshold key for whether pull request
// metadata agrees with the proposed change.
const PRDescriptionAgreementQuestion = "pr_description_agreement"

// DefaultPRDescriptionAgreementThreshold leaves ambiguous comparisons unsure.
var DefaultPRDescriptionAgreementThreshold = Threshold{Accept: 0.8, Reject: 0.2}

// PRDescriptionState contains deterministic diff facts and the prose whose
// semantic agreement the model judges.
type PRDescriptionState struct {
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	ChangedFiles   []string `json:"changedFiles"`
	AddedLines     int      `json:"addedLines"`
	DeletedLines   int      `json:"deletedLines"`
	BinaryFiles    int      `json:"binaryFiles"`
	SizeBucket     string   `json:"sizeBucket"`
	Patch          string   `json:"patch"`
	PatchTruncated bool     `json:"patchTruncated"`
}

var prDescriptionAgreementQuestion = decider.Noul(
	"Do the pull request title and description accurately describe the intent and material scope of the proposed diff?",
	&decider.NoulCriteria{
		True:  "The title and description match the changed files and patch without omitting or contradicting a material change.",
		False: "The title or description is unrelated to, contradicts, or omits a material part of the changed files and patch.",
	},
)

// EvaluatePRDescription scores semantic agreement. Uncertain and errors remain
// advisory; callers retain their existing publication path.
func (g *Gate) EvaluatePRDescription(ctx context.Context, state PRDescriptionState) (Outcome, error) {
	return g.JudgeNoul(ctx, PRDescriptionAgreementQuestion, state, prDescriptionAgreementQuestion)
}
