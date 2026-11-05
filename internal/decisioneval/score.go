package decisioneval

import (
	"context"

	"github.com/goobers/goobers/internal/decisiongate"
)

// Score is one arm's confusion counts on one split. A "yes" decision is a
// flag; Unsure and errored answers are not flags, so an unsure positive is a
// false pass (the gate would not have blocked it).
type Score struct {
	Arm                    string
	N, Positives, Negative int
	TP, FP, FN, TN         int
	Unsure, Errors         int
	// Brier is the mean squared error of the yes-probability against the
	// label over BrierN items that carried a probability.
	Brier  float64
	BrierN int
	// Stable counts items whose decision was identical across all repeats.
	Stable, Repeats int
	flags           []bool
}

// FalsePass is the number of true positives the arm failed to flag.
func (s Score) FalsePass() int { return s.FN }

// FalseBlock is the number of negatives the arm flagged.
func (s Score) FalseBlock() int { return s.FP }

// Evaluate runs arm over items repeats times (at least once) and scores it.
func Evaluate(ctx context.Context, arm Arm, items []Item, repeats int) Score {
	if repeats < 1 {
		repeats = 1
	}
	s := Score{Arm: arm.Name(), Repeats: repeats}
	var sq float64
	for _, it := range items {
		first := arm.Answer(ctx, it)
		stable := true
		for r := 1; r < repeats; r++ {
			if arm.Answer(ctx, it).Decision != first.Decision {
				stable = false
			}
		}
		s.add(it, first, stable)
		if first.Prob != nil {
			d := *first.Prob - label01(it.Label)
			sq += d * d
			s.BrierN++
		}
	}
	if s.BrierN > 0 {
		s.Brier = sq / float64(s.BrierN)
	}
	return s
}

func label01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (s *Score) add(it Item, v Verdict, stable bool) {
	s.N++
	if stable {
		s.Stable++
	}
	if v.Err != nil {
		s.Errors++
	}
	if v.Decision == decisiongate.Uncertain {
		s.Unsure++
	}
	flagged := v.Decision == decisiongate.Yes
	s.flags = append(s.flags, flagged)
	switch {
	case it.Label && flagged:
		s.Positives++
		s.TP++
	case it.Label:
		s.Positives++
		s.FN++
	case flagged:
		s.Negative++
		s.FP++
	default:
		s.Negative++
		s.TN++
	}
}

// Agreement is how often two arms made the same flag/no-flag call on the same
// items, with the sample size.
type Agreement struct{ Same, N int }

// Agree compares two scores computed over the same items in the same order.
func Agree(a, b Score) Agreement {
	n := min(len(a.flags), len(b.flags))
	ag := Agreement{N: n}
	for i := 0; i < n; i++ {
		if a.flags[i] == b.flags[i] {
			ag.Same++
		}
	}
	return ag
}

// ChooseThreshold picks the accept threshold on tune items that minimises
// false passes plus false blocks, preferring the higher (more conservative)
// threshold on ties, using the recorded systemOne probabilities. Reject is
// set to 1-accept so the uncertain band is symmetric. Items without a
// probability are skipped. The reported numbers must use a different split.
func ChooseThreshold(tune []Item) decisiongate.Threshold {
	best, bestErr := decisiongate.DefaultClaimThreshold, -1
	for c := 55; c <= 99; c++ {
		accept := float64(c) / 100
		errs := 0
		for _, it := range tune {
			if it.SystemOne == nil {
				continue
			}
			if (*it.SystemOne >= accept) != it.Label {
				errs++
			}
		}
		if bestErr < 0 || errs <= bestErr {
			best, bestErr = decisiongate.Threshold{Accept: accept, Reject: 1 - accept}, errs
		}
	}
	return best
}
