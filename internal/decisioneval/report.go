package decisioneval

import (
	"context"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
)

// Options tune a run.
type Options struct {
	// MinSamples is the smallest denominator for which a percentage is
	// printed. Zero means DefaultMinSamples.
	MinSamples int
	// Repeats is how many times each item is asked, for run-to-run agreement.
	Repeats int
	// MaxFalseBlockRate is the graduation ceiling for System One false
	// blocks over report negatives. Zero means 0.02.
	MaxFalseBlockRate float64
}

// DefaultMinSamples mirrors the rule of never printing percentages from tiny
// samples.
const DefaultMinSamples = 20

// Report is the comparison on the report split.
type Report struct {
	Question  string
	Threshold decisiongate.Threshold
	Tune      int
	SystemOne Score
	Judge     Score
	Agreement Agreement
	Opts      Options
}

// Run chooses thresholds on the tune split, then scores System One (through
// the real gate over d) and the baseline judge j on the report split.
func Run(ctx context.Context, f Fixture, d decider.Decider, j Judge, o Options) (Report, error) {
	if o.MinSamples == 0 {
		o.MinSamples = DefaultMinSamples
	}
	if o.MaxFalseBlockRate == 0 {
		o.MaxFalseBlockRate = 0.02
	}
	th := ChooseThreshold(f.ItemsIn(SplitTune))
	s1, err := NewSystemOneArm(d, f, th)
	if err != nil {
		return Report{}, err
	}
	items := f.ItemsIn(SplitReport)
	r := Report{Question: f.Question, Threshold: th, Tune: len(f.ItemsIn(SplitTune)), Opts: o}
	r.SystemOne = Evaluate(ctx, s1, items, o.Repeats)
	r.Judge = Evaluate(ctx, &JudgeArm{J: j, Instructions: f.Instructions}, items, o.Repeats)
	r.Agreement = Agree(r.SystemOne, r.Judge)
	return r, nil
}

// pct prints num/den as a percentage, or a refusal below the minimum sample.
func (r Report) pct(num, den int) string {
	if den < r.Opts.MinSamples {
		return fmt.Sprintf("n/a (n=%d<%d)", den, r.Opts.MinSamples)
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", 100*float64(num)/float64(den), num, den)
}

// Graduation says whether the evidence supports shadow to enforce.
func (r Report) Graduation() (bool, string) {
	s := r.SystemOne
	switch {
	case s.Positives < r.Opts.MinSamples || s.Negative < r.Opts.MinSamples:
		return false, fmt.Sprintf("insufficient samples: %d positives, %d negatives, need %d each", s.Positives, s.Negative, r.Opts.MinSamples)
	case float64(s.FP) > r.Opts.MaxFalseBlockRate*float64(s.Negative):
		return false, fmt.Sprintf("false-block rate above %.0f%%", 100*r.Opts.MaxFalseBlockRate)
	case s.TP*r.Judge.Positives < r.Judge.TP*s.Positives:
		return false, "recall below the LLM-judge baseline"
	}
	return true, "meets sample, false-block and baseline-recall criteria"
}

// Render formats the report as deterministic plain text.
func (r Report) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "question: %s\n", r.Question)
	fmt.Fprintf(&b, "threshold (chosen on %d tune items): accept=%.2f reject=%.2f\n", r.Tune, r.Threshold.Accept, r.Threshold.Reject)
	for _, s := range []Score{r.SystemOne, r.Judge} {
		fmt.Fprintf(&b, "\n[%s] report-split n=%d positives=%d negatives=%d\n", s.Arm, s.N, s.Positives, s.Negative)
		fmt.Fprintf(&b, "  false-pass=%d false-block=%d unsure=%d errors=%d\n", s.FalsePass(), s.FalseBlock(), s.Unsure, s.Errors)
		fmt.Fprintf(&b, "  recall=%s\n", r.pct(s.TP, s.Positives))
		fmt.Fprintf(&b, "  precision=%s\n", r.pct(s.TP, s.TP+s.FP))
		fmt.Fprintf(&b, "  false-alarm=%s\n", r.pct(s.FP, s.Negative))
		fmt.Fprintf(&b, "  unsure=%s\n", r.pct(s.Unsure, s.N))
		fmt.Fprintf(&b, "  determinism=%s over %d runs\n", r.pct(s.Stable, s.N), s.Repeats)
		if s.BrierN > 0 {
			fmt.Fprintf(&b, "  brier=%.4f (n=%d)\n", s.Brier, s.BrierN)
		}
	}
	fmt.Fprintf(&b, "\nagreement(system-one, llm-judge)=%s\n", r.pct(r.Agreement.Same, r.Agreement.N))
	ok, why := r.Graduation()
	fmt.Fprintf(&b, "graduation(shadow->enforce): ready=%t: %s\n", ok, why)
	return b.String()
}
