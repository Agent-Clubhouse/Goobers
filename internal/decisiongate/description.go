package decisiongate

import (
	"context"
	"strings"

	"github.com/goobers/goobers/internal/decider"
)

// DescriptionQuestion is the threshold key for the description-vs-diff
// question.
const DescriptionQuestion = "description_matches_diff"

// DefaultDescriptionThreshold keeps ambiguous results on the existing path.
var DefaultDescriptionThreshold = Threshold{Accept: 0.9, Reject: 0.1}

// DescriptionVerdict is advisory: it can add a review note, never block.
type DescriptionVerdict string

// Description verdicts.
const (
	DescriptionMatch    DescriptionVerdict = "match"
	DescriptionMismatch DescriptionVerdict = "mismatch"
	DescriptionUnsure   DescriptionVerdict = "unsure"
)

// maxDescriptionDiffBytes bounds the diff text sent to the model.
const maxDescriptionDiffBytes = 24 << 10

var descriptionQuestion = decider.Noul(
	"Does the pull request title and description accurately describe what the diff changes, without claiming changes the diff does not make or omitting a major change it does make?",
	&decider.NoulCriteria{
		True:  "The title and description agree with the diff.",
		False: "The description claims something the diff does not do, or leaves out a major part of the diff.",
	},
)

// DiffStats are facts computed in code from a unified diff.
type DiffStats struct {
	Files     []string `json:"files"`
	Additions int      `json:"additions"`
	Deletions int      `json:"deletions"`
}

// SizeBucket groups a diff by changed lines so shadow scores can be compared
// across sizes.
func (s DiffStats) SizeBucket() string {
	switch n := s.Additions + s.Deletions; {
	case n == 0:
		return "empty"
	case n <= 20:
		return "xs"
	case n <= 100:
		return "s"
	case n <= 500:
		return "m"
	default:
		return "l"
	}
}

// ParseDiffStats reads file names and line counts from a unified diff.
func ParseDiffStats(diff string) DiffStats {
	var s DiffStats
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			if name := strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/"); name != "/dev/null" {
				s.Files = append(s.Files, name)
			}
		case strings.HasPrefix(line, "--- "):
		case strings.HasPrefix(line, "+"):
			s.Additions++
		case strings.HasPrefix(line, "-"):
			s.Deletions++
		}
	}
	return s
}

type descriptionState struct {
	Title string    `json:"title"`
	Body  string    `json:"body"`
	Stats DiffStats `json:"stats"`
	Diff  string    `json:"diff"`
}

// DescriptionResult is one advisory check. It holds no text.
type DescriptionResult struct {
	Verdict     DescriptionVerdict
	SizeBucket  string
	Probability float64
	Cached      bool
	Err         error
}

// EvaluateDescription scores whether a PR title and body match its diff. An
// empty diff or empty title is decided in code and never reaches the model;
// uncertain answers and errors return DescriptionUnsure so publishing is
// unaffected. The result is advisory only.
func (g *Gate) EvaluateDescription(ctx context.Context, title, body, diff string) DescriptionResult {
	stats := ParseDiffStats(diff)
	res := DescriptionResult{Verdict: DescriptionUnsure, SizeBucket: stats.SizeBucket()}
	if strings.TrimSpace(title) == "" || strings.TrimSpace(diff) == "" {
		return res
	}
	if len(diff) > maxDescriptionDiffBytes {
		diff = diff[:maxDescriptionDiffBytes]
	}
	o, err := g.JudgeNoul(ctx, DescriptionQuestion, descriptionState{Title: title, Body: body, Stats: stats, Diff: diff}, descriptionQuestion)
	res.Probability, res.Cached, res.Err = o.Probability, o.Cached, err
	if err != nil {
		return res
	}
	switch o.Decision {
	case Yes:
		res.Verdict = DescriptionMatch
	case No:
		res.Verdict = DescriptionMismatch
	}
	return res
}
