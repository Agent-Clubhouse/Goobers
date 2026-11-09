// Package knowledgerank shadow-ranks already-eligible knowledge candidates
// with the decision gate's score question. Code decides eligibility first
// (source allowlist, internal-knowledge opt-in) and the model only judges
// relevance. It is advisory: Shadow never changes the caller's selection, and
// an error or borderline score keeps the item, so the existing path is always
// the fallback.
package knowledgerank

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
)

// QuestionName names the score question in gate telemetry.
const QuestionName = "knowledge_relevance"

const (
	defaultTopN     = 10
	maxTopN         = 50
	maxTextBytes    = 4000
	defaultDrop     = 0.5
	defaultKeep     = 1.5
	levelIrrelevant = 0
	levelNeeded     = 2
)

var question = decider.Score(
	"How useful is this knowledge item for completing the task?",
	[]any{
		"irrelevant: unrelated to the task",
		"related: touches the topic but is not needed to do the task",
		"directly needed: the task cannot be done well without it",
	},
)

// Settings is the opt-in. Everything is off by default, and the zero value
// sends nothing to any model.
type Settings struct {
	Enabled bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// AllowInternalKnowledge must be true before any candidate marked
	// Internal is sent to the model. It exists because the decider may be a
	// third-party endpoint.
	AllowInternalKnowledge bool `json:"allowInternalKnowledge,omitempty" yaml:"allowInternalKnowledge,omitempty"`
	// Sources limits which candidate sources are scored. Empty means all
	// eligible sources.
	Sources []string `json:"sources,omitempty" yaml:"sources,omitempty"`
	// TopN is how many candidates per task are scored. Zero means 10.
	TopN int `json:"topN,omitempty" yaml:"topN,omitempty"`
	// DropAtOrBelow marks a score as would-drop. Zero means 0.5 on the 0..2
	// scale.
	DropAtOrBelow float64 `json:"dropAtOrBelow,omitempty" yaml:"dropAtOrBelow,omitempty"`
	// NeededAtOrAbove marks a score as directly needed. Zero means 1.5.
	NeededAtOrAbove float64 `json:"neededAtOrAbove,omitempty" yaml:"neededAtOrAbove,omitempty"`
}

// Thresholds are the resolved score cut points, logged with every record.
type Thresholds struct {
	DropAtOrBelow   float64 `json:"dropAtOrBelow"`
	NeededAtOrAbove float64 `json:"neededAtOrAbove"`
}

// Effective returns the settings with defaults applied.
func (s Settings) Effective() (topN int, th Thresholds) {
	topN, th = s.TopN, Thresholds{DropAtOrBelow: s.DropAtOrBelow, NeededAtOrAbove: s.NeededAtOrAbove}
	if topN == 0 {
		topN = defaultTopN
	}
	if th.DropAtOrBelow == 0 {
		th.DropAtOrBelow = defaultDrop
	}
	if th.NeededAtOrAbove == 0 {
		th.NeededAtOrAbove = defaultKeep
	}
	return topN, th
}

// Validate rejects settings that cannot be used.
func (s Settings) Validate() error {
	topN, th := s.Effective()
	switch {
	case topN < 1 || topN > maxTopN:
		return fmt.Errorf("knowledgerank: topN must be within 1..%d", maxTopN)
	case th.DropAtOrBelow < levelIrrelevant || th.NeededAtOrAbove > levelNeeded || th.DropAtOrBelow >= th.NeededAtOrAbove:
		return errors.New("knowledgerank: need 0 <= dropAtOrBelow < neededAtOrAbove <= 2")
	}
	return nil
}

// Candidate is one already-eligible knowledge item. Code has applied access
// and freshness before it gets here.
type Candidate struct {
	ID     string
	Source string
	// Internal marks knowledge that must not reach a model without
	// AllowInternalKnowledge.
	Internal bool
	Text     string
}

// State is what the model sees for one candidate.
type State struct {
	Task      string `json:"task"`
	Source    string `json:"source,omitempty"`
	Candidate string `json:"candidate"`
}

// Skip reasons for candidates that were not scored.
const (
	SkipInternal = "internal-not-opted-in"
	SkipSource   = "source-not-allowed"
	SkipBeyondN  = "beyond-top-n"
)

// Class is the graded reading of a score.
type Class string

// Classes. Related is the borderline band and is always kept.
const (
	ClassIrrelevant Class = "irrelevant"
	ClassRelated    Class = "related"
	ClassNeeded     Class = "needed"
)

// Scored is the shadow result for one candidate. It carries no text.
type Scored struct {
	ID         string  `json:"id"`
	Source     string  `json:"source,omitempty"`
	Skipped    string  `json:"skipped,omitempty"`
	Score      float64 `json:"score,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Class      Class   `json:"class,omitempty"`
	Cached     bool    `json:"cached,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// Record is the reproducible shadow record for one task. Existing is the
// selection the caller keeps using; Hypothetical is what a threshold-enforced
// ranking would have selected.
type Record struct {
	TaskDigest   string     `json:"taskDigest"`
	Model        string     `json:"model,omitempty"`
	TopN         int        `json:"topN"`
	Thresholds   Thresholds `json:"thresholds"`
	Existing     []string   `json:"existing"`
	Hypothetical []string   `json:"hypothetical"`
	Dropped      []string   `json:"dropped,omitempty"`
	Scored       []Scored   `json:"scored"`
}

// Ranker scores candidates through a gate. A nil Ranker is the off state.
type Ranker struct {
	gate *decisiongate.Gate
	s    Settings
}

// New returns a Ranker, or nil when the feature is off or no gate is
// available. Nothing is ever scored without Enabled.
func New(g *decisiongate.Gate, s Settings) (*Ranker, error) {
	if !s.Enabled {
		return nil, nil
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if g == nil {
		return nil, nil
	}
	return &Ranker{gate: g, s: s}, nil
}

// Shadow scores the top N eligible candidates and records the hypothetical
// ranking. It reports false on a nil Ranker. It never errors and never
// reorders or removes anything from existing.
func (r *Ranker) Shadow(ctx context.Context, task string, existing []Candidate) (Record, bool) {
	if r == nil {
		return Record{}, false
	}
	topN, th := r.s.Effective()
	rec := Record{TaskDigest: digest(task), TopN: topN, Thresholds: th}
	scored := make([]Scored, len(existing))
	var pick []int
	for i, c := range existing {
		rec.Existing = append(rec.Existing, c.ID)
		scored[i] = Scored{ID: c.ID, Source: c.Source, Skipped: r.skip(c, len(pick) >= topN)}
		if scored[i].Skipped == "" {
			pick = append(pick, i)
		}
	}
	models := make([]string, len(existing))
	var wg sync.WaitGroup
	for _, i := range pick {
		wg.Add(1)
		go func() {
			defer wg.Done()
			models[i] = r.score(ctx, task, existing[i], th, &scored[i])
		}()
	}
	wg.Wait()
	for _, m := range models {
		if m != "" {
			rec.Model = m
			break
		}
	}
	rec.Scored = scored
	rec.Hypothetical, rec.Dropped = hypothetical(scored)
	return rec, true
}

func (r *Ranker) skip(c Candidate, full bool) string {
	switch {
	case c.Internal && !r.s.AllowInternalKnowledge:
		return SkipInternal
	case len(r.s.Sources) > 0 && !slices.Contains(r.s.Sources, c.Source):
		return SkipSource
	case full:
		return SkipBeyondN
	}
	return ""
}

func (r *Ranker) score(ctx context.Context, task string, c Candidate, th Thresholds, out *Scored) string {
	o, err := r.gate.JudgeScore(ctx, QuestionName, State{Task: clip(task), Source: c.Source, Candidate: clip(c.Text)}, question)
	if err != nil {
		out.Error = err.Error()
		return ""
	}
	out.Score, out.Confidence, out.Cached = o.Score, o.Confidence, o.Cached
	switch {
	case o.Score <= th.DropAtOrBelow:
		out.Class = ClassIrrelevant
	case o.Score >= th.NeededAtOrAbove:
		out.Class = ClassNeeded
	default:
		out.Class = ClassRelated
	}
	return o.Model
}

// hypothetical orders scored survivors by score, then everything else in the
// existing order. Only a scored irrelevant item is dropped.
func hypothetical(scored []Scored) (kept, dropped []string) {
	var ranked []Scored
	var rest []string
	for _, s := range scored {
		switch {
		case s.Class == ClassIrrelevant:
			dropped = append(dropped, s.ID)
		case s.Class != "":
			ranked = append(ranked, s)
		default:
			rest = append(rest, s.ID)
		}
	}
	sort.SliceStable(ranked, func(a, b int) bool { return ranked[a].Score > ranked[b].Score })
	for _, s := range ranked {
		kept = append(kept, s.ID)
	}
	return append(kept, rest...), dropped
}

func clip(s string) string {
	if len(s) > maxTextBytes {
		return s[:maxTextBytes]
	}
	return s
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
