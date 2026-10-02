package decisiongate

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/decider"
)

// ClaimQuestion is the threshold key for the spurious-claim question.
const ClaimQuestion = "claims_bad_input"

// DefaultClaimThreshold applies when settings declare none for ClaimQuestion.
// Probe values on the live scorer were 0.002 for a clean success and 0.037 for
// a genuine block, against 0.96 or higher for bad-input claims.
var DefaultClaimThreshold = Threshold{Accept: 0.9, Reject: 0.1}

// ClaimVerdict says what to do with an agent reply that may claim its input
// was corrupted, incomplete or unreadable.
type ClaimVerdict string

// Claim verdicts.
const (
	// ClaimNone: nothing to act on.
	ClaimNone ClaimVerdict = "none"
	// ClaimGenuine: the input really was invalid, so the agent may be right.
	ClaimGenuine ClaimVerdict = "genuine"
	// ClaimSpurious: the input passed deterministic checks but the reply says
	// otherwise; the producer should be re-run.
	ClaimSpurious ClaimVerdict = "spurious"
	// ClaimUnsure: the model was not confident; use the configured fallback.
	ClaimUnsure ClaimVerdict = "unsure"
)

var claimQuestion = decider.Noul(
	"Does this reply claim that its input was corrupted, incomplete, truncated, malformed or unreadable, instead of producing the requested result?",
	&decider.NoulCriteria{
		True:  "The reply says it could not read or use its input.",
		False: "The reply is a normal result that uses its input.",
	},
)

// EvaluateClaim decides whether reply is a spurious bad-input claim. inputValid
// must come from a deterministic check (handoffcheck); the model is consulted
// only when the input is valid, because a claim about invalid input may be
// true and needs no model.
func (g *Gate) EvaluateClaim(ctx context.Context, inputValid bool, reply string) (ClaimVerdict, Outcome, error) {
	if !inputValid {
		return ClaimGenuine, Outcome{Name: ClaimQuestion}, nil
	}
	o, err := g.JudgeNoul(ctx, ClaimQuestion, reply, claimQuestion)
	if err != nil {
		return ClaimUnsure, o, err
	}
	switch o.Decision {
	case Yes:
		return ClaimSpurious, o, nil
	case No:
		return ClaimNone, o, nil
	}
	return ClaimUnsure, o, nil
}

// RetryBudget bounds how many times a producer is re-run after a spurious
// claim.
type RetryBudget struct {
	Max int
}

// Run calls produce up to Max+1 times. produce receives the attempt number
// (0-based) and a note to feed back to the producer. It stops as soon as the
// reply is not a spurious claim. If the budget is exhausted, the last reply
// and ErrBudgetExhausted are returned so the caller can escalate.
func (b RetryBudget) Run(ctx context.Context, g *Gate, inputValid bool,
	produce func(ctx context.Context, attempt int, note string) (string, error)) (string, ClaimVerdict, error) {
	note := ""
	var reply string
	var verdict ClaimVerdict
	for attempt := 0; attempt <= b.Max; attempt++ {
		var err error
		reply, err = produce(ctx, attempt, note)
		if err != nil {
			return reply, verdict, err
		}
		verdict, _, _ = g.EvaluateClaim(ctx, inputValid, reply)
		if verdict != ClaimSpurious {
			return reply, verdict, nil
		}
		note = "Your previous reply said the input was unreadable, but it passed validation. Re-read the input and answer again."
	}
	return reply, verdict, ErrBudgetExhausted
}

// ErrBudgetExhausted means every retry still produced a spurious claim.
var ErrBudgetExhausted = errors.New("decisiongate: retry budget exhausted")
