package decisiongate

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/goobers/goobers/internal/decider"
)

// IntakeThresholdKey is the threshold key for the duplicate/dependency
// relation question.
const IntakeThresholdKey = "intake_relation"

// DefaultIntakeThreshold keeps ambiguous relations unflagged.
var DefaultIntakeThreshold = Threshold{Accept: 0.85, Reject: 0.15}

// Intake detection bounds. Retrieval is code; the model only sees a few
// bounded candidates.
const (
	// MaxIntakePool bounds how many open items are considered per intake.
	MaxIntakePool = 200
	// MaxIntakeCandidates bounds how many candidates the model is shown.
	MaxIntakeCandidates = 5
	// maxIntakeBodyBytes bounds each body shown to the model.
	maxIntakeBodyBytes = 600
	// minIntakeSimilarity is the title-token Jaccard below which an open
	// item is not a candidate.
	minIntakeSimilarity = 0.3
)

// IntakeItem is the identity and text of one work item.
type IntakeItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
}

// IntakeKind is the advisory relation between an intake item and an open one.
type IntakeKind string

// Kinds. IntakeUnsure means the caller changes nothing.
const (
	IntakeNone       IntakeKind = "none"
	IntakeDuplicate  IntakeKind = "duplicate"
	IntakeDependency IntakeKind = "dependency"
	IntakeUnsure     IntakeKind = "unsure"
)

// IntakeBasis says who decided an intake verdict.
type IntakeBasis string

// Bases.
const (
	IntakeBasisCode  IntakeBasis = "code"
	IntakeBasisModel IntakeBasis = "model"
)

// IntakeVerdict is the advisory reading of one intake item. Related is the
// open item's ID for duplicate and dependency verdicts.
type IntakeVerdict struct {
	Kind    IntakeKind
	Related string
	Basis   IntakeBasis
	// Candidates is how many open items retrieval produced.
	Candidates int
}

var (
	intakeDependencyLink = regexp.MustCompile(`(?i)\b(?:blocked by|depends on|requires)\s+#(\d+)`)
	intakeToken          = regexp.MustCompile(`[a-z0-9]{3,}`)
)

var intakeStop = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "into": true,
	"not": true, "when": true, "that": true, "this": true, "add": true, "fix": true,
}

// ExplicitDependencies returns the issue numbers a body declares it is blocked
// by or depends on, in order and without duplicates. It is code, not a model.
func ExplicitDependencies(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range intakeDependencyLink.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

func intakeTokens(title string) map[string]bool {
	set := map[string]bool{}
	for _, t := range intakeToken.FindAllString(strings.ToLower(title), -1) {
		if !intakeStop[t] {
			set[t] = true
		}
	}
	return set
}

func normalizeIntakeTitle(title string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

// RankIntakeCandidates picks up to MaxIntakeCandidates open items whose titles
// resemble the intake item's, best first. Only the first MaxIntakePool open
// items are considered, and the item itself is never a candidate.
func RankIntakeCandidates(item IntakeItem, open []IntakeItem) []IntakeItem {
	if len(open) > MaxIntakePool {
		open = open[:MaxIntakePool]
	}
	want := intakeTokens(item.Title)
	if len(want) == 0 {
		return nil
	}
	type scored struct {
		item  IntakeItem
		score float64
	}
	var hits []scored
	for _, o := range open {
		if o.ID == item.ID {
			continue
		}
		have := intakeTokens(o.Title)
		shared := 0
		for t := range want {
			if have[t] {
				shared++
			}
		}
		if union := len(want) + len(have) - shared; union > 0 {
			if s := float64(shared) / float64(union); s >= minIntakeSimilarity {
				hits = append(hits, scored{o, s})
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > MaxIntakeCandidates {
		hits = hits[:MaxIntakeCandidates]
	}
	out := make([]IntakeItem, len(hits))
	for i, h := range hits {
		out[i] = h.item
		if len(out[i].Body) > maxIntakeBodyBytes {
			out[i].Body = strings.ToValidUTF8(out[i].Body[:maxIntakeBodyBytes], "")
		}
	}
	return out
}

// DecideIntakeByCode returns a verdict when code alone settles the relation:
// an explicit dependency link to an open item, or an identical normalized
// title. ok is false when the semantics need a model.
func DecideIntakeByCode(item IntakeItem, open []IntakeItem) (IntakeVerdict, bool) {
	openIDs := map[string]bool{}
	for _, o := range open {
		openIDs[o.ID] = true
	}
	for _, dep := range ExplicitDependencies(item.Body) {
		if openIDs[dep] && dep != item.ID {
			return IntakeVerdict{Kind: IntakeDependency, Related: dep, Basis: IntakeBasisCode}, true
		}
	}
	title := normalizeIntakeTitle(item.Title)
	if title == "" {
		return IntakeVerdict{}, false
	}
	for _, o := range open {
		if o.ID != item.ID && normalizeIntakeTitle(o.Title) == title {
			return IntakeVerdict{Kind: IntakeDuplicate, Related: o.ID, Basis: IntakeBasisCode}, true
		}
	}
	return IntakeVerdict{}, false
}

const (
	intakeOptionNone = "none"
	intakeDupPrefix  = "dup-"
	intakeDepPrefix  = "dep-"
)

func intakeQuestion(cands []IntakeItem) decider.Question {
	options := map[string]any{
		intakeOptionNone: "The new item is unrelated to every candidate.",
	}
	for _, c := range cands {
		options[intakeDupPrefix+c.ID] = "The new item asks for the same outcome as candidate " + c.ID + ", so one of them is redundant."
		options[intakeDepPrefix+c.ID] = "The new item cannot be done until candidate " + c.ID + " is finished."
	}
	return decider.Choice("Is the new item a duplicate of, or blocked by, one of the open candidate items?", options)
}

type intakeState struct {
	Item       IntakeItem   `json:"item"`
	Candidates []IntakeItem `json:"candidates"`
}

// EvaluateIntake reads an intake item against the open backlog. Code settles
// explicit dependency links and identical titles first; otherwise retrieval
// bounds the candidates and one choice question judges the prose. Any model
// failure or low-confidence answer is IntakeUnsure, never a guess.
func (g *Gate) EvaluateIntake(ctx context.Context, item IntakeItem, open []IntakeItem) (IntakeVerdict, error) {
	if v, ok := DecideIntakeByCode(item, open); ok {
		return v, nil
	}
	cands := RankIntakeCandidates(item, open)
	if len(cands) == 0 {
		return IntakeVerdict{Kind: IntakeNone, Basis: IntakeBasisCode}, nil
	}
	if len(item.Body) > 2*maxIntakeBodyBytes {
		item.Body = strings.ToValidUTF8(item.Body[:2*maxIntakeBodyBytes], "")
	}
	out, err := g.JudgeChoice(ctx, IntakeThresholdKey, intakeState{Item: item, Candidates: cands}, intakeQuestion(cands))
	v := IntakeVerdict{Kind: IntakeUnsure, Basis: IntakeBasisModel, Candidates: len(cands)}
	if err != nil || out.Decision != Yes {
		return v, err
	}
	switch {
	case out.Choice == intakeOptionNone:
		v.Kind = IntakeNone
	case strings.HasPrefix(out.Choice, intakeDupPrefix):
		v.Kind, v.Related = IntakeDuplicate, strings.TrimPrefix(out.Choice, intakeDupPrefix)
	case strings.HasPrefix(out.Choice, intakeDepPrefix):
		v.Kind, v.Related = IntakeDependency, strings.TrimPrefix(out.Choice, intakeDepPrefix)
	}
	return v, nil
}

// IntakeTally joins shadow flags against later escalation: of the items
// flagged, how many were escalated, and the base rate among unflagged items.
type IntakeTally struct {
	Flagged, FlaggedEscalated     int
	Unflagged, UnflaggedEscalated int
}

// Add records one item's shadow flag and whether it was later escalated.
func (t *IntakeTally) Add(flagged, escalated bool) {
	switch {
	case flagged:
		t.Flagged++
		if escalated {
			t.FlaggedEscalated++
		}
	default:
		t.Unflagged++
		if escalated {
			t.UnflaggedEscalated++
		}
	}
}

// Rates returns the escalation rate of flagged and unflagged items. ok is
// false until both groups hold at least minSample items, so a thin sample is
// never reported as a rate.
func (t IntakeTally) Rates(minSample int) (flagged, unflagged float64, ok bool) {
	if t.Flagged < minSample || t.Unflagged < minSample || t.Flagged == 0 || t.Unflagged == 0 {
		return 0, 0, false
	}
	return float64(t.FlaggedEscalated) / float64(t.Flagged), float64(t.UnflaggedEscalated) / float64(t.Unflagged), true
}
