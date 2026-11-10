package decisioneval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/knowledgerank"
)

// Graded relevance labels for RankCandidate.Label.
const (
	LabelIrrelevant = 0
	LabelRelated    = 1
	LabelNeeded     = 2
)

// RankCandidate is one labelled knowledge item. Score is the recorded
// System One answer on the 0..2 scale, used by the replay decider.
type RankCandidate struct {
	ID     string   `json:"id"`
	Source string   `json:"source,omitempty"`
	Text   string   `json:"text"`
	Label  int      `json:"label"`
	Score  *float64 `json:"score,omitempty"`
}

// RankTask is one task with its existing candidate order.
type RankTask struct {
	ID         string          `json:"id"`
	Split      Split           `json:"split"`
	Task       string          `json:"task"`
	Candidates []RankCandidate `json:"candidates"`
}

// RankProvenance says where recorded scores came from. A synthetic fixture
// illustrates the format and is not evidence.
type RankProvenance struct {
	Kind       string `json:"kind"`
	Model      string `json:"model"`
	Prompt     string `json:"prompt"`
	RecordedAt string `json:"recordedAt"`
}

// RankFixture is a labelled knowledge-reranking benchmark.
type RankFixture struct {
	Name       string         `json:"name"`
	Provenance RankProvenance `json:"provenance"`
	Tasks      []RankTask     `json:"tasks"`
}

// LoadRankFixture reads and validates a rank fixture.
func LoadRankFixture(path string) (RankFixture, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return RankFixture{}, err
	}
	var f RankFixture
	if err := json.Unmarshal(b, &f); err != nil {
		return RankFixture{}, fmt.Errorf("decode rank fixture %s: %w", path, err)
	}
	return f, f.Validate()
}

// Validate reports a fixture that cannot be scored.
func (f RankFixture) Validate() error {
	if f.Name == "" || f.Provenance.Kind == "" || f.Provenance.Model == "" || f.Provenance.Prompt == "" || f.Provenance.RecordedAt == "" {
		return errors.New("rank fixture needs name and complete provenance")
	}
	seen := map[string]bool{}
	for _, t := range f.Tasks {
		if t.ID == "" || seen[t.ID] {
			return fmt.Errorf("task id %q is empty or duplicated", t.ID)
		}
		seen[t.ID] = true
		if t.Split != SplitTune && t.Split != SplitReport {
			return fmt.Errorf("task %q: split must be tune or report", t.ID)
		}
		ids := map[string]bool{}
		for _, c := range t.Candidates {
			if c.ID == "" || ids[c.ID] || c.Label < LabelIrrelevant || c.Label > LabelNeeded {
				return fmt.Errorf("task %q: candidate %q has a bad id or label", t.ID, c.ID)
			}
			ids[c.ID] = true
			if c.Score != nil && (*c.Score < 0 || *c.Score > 2) {
				return fmt.Errorf("task %q: candidate %q score out of range", t.ID, c.ID)
			}
		}
	}
	return nil
}

// TasksIn returns the tasks of one split.
func (f RankFixture) TasksIn(s Split) []RankTask {
	var out []RankTask
	for _, t := range f.Tasks {
		if t.Split == s {
			out = append(out, t)
		}
	}
	return out
}

// ReplayScoreDecider returns the recorded score for each task and candidate
// text. Unknown pairs error, which the reranker treats as keep.
type ReplayScoreDecider struct{ byKey map[string]float64 }

// NewReplayScoreDecider indexes the recorded scores of f.
func NewReplayScoreDecider(f RankFixture) *ReplayScoreDecider {
	m := map[string]float64{}
	for _, t := range f.Tasks {
		for _, c := range t.Candidates {
			if c.Score != nil {
				m[t.Task+"\x00"+c.Text] = *c.Score
			}
		}
	}
	return &ReplayScoreDecider{byKey: m}
}

// Decide implements decider.Decider.
func (r *ReplayScoreDecider) Decide(_ context.Context, req decider.Request) (decider.Response, error) {
	st, ok := req.State.(knowledgerank.State)
	if !ok {
		return decider.Response{}, errors.New("replay: unexpected state type")
	}
	s, ok := r.byKey[st.Task+"\x00"+st.Candidate]
	if !ok {
		return decider.Response{}, errors.New("no recorded score for state")
	}
	out := decider.Response{Model: "replay", Answers: map[string]decider.Answer{}}
	for id := range req.Questions {
		v := s
		out.Answers[id] = decider.Answer{Type: decider.KindScore, Score: &v}
	}
	return out, nil
}

// PrecisionAt is hits over slots for the top k of each ranking.
type PrecisionAt struct{ K, Hits, Slots int }

// RankScore is one arm's result on the report split.
type RankScore struct {
	Arm string
	P   []PrecisionAt
	// Dropped is how many candidates the arm would remove; FalseDrops is how
	// many of those were directly needed.
	Dropped, FalseDrops int
}

// RankReport compares the existing order with the model ranking.
type RankReport struct {
	Name                    string
	Thresholds              knowledgerank.Thresholds
	Tune, Tasks, Candidates int
	Needed                  int
	Existing, Model         RankScore
	Provenance              RankProvenance
	Opts                    RankOptions
	errors                  int
}

// RankOptions tune a rank run.
type RankOptions struct {
	// MinTasks is the smallest task count for which a percentage prints.
	// Zero means DefaultMinSamples.
	MinTasks int
	// Ks are the cutoffs for precision at k. Empty means 1, 3 and 5.
	Ks []int
	// MaxFalseDropRate is the graduation ceiling for needed items dropped,
	// over needed items. Zero means 0.02.
	MaxFalseDropRate float64
}

// RunRank chooses the drop threshold on the tune tasks, then compares the
// existing order with the real knowledgerank ranking on the report tasks, all
// through the real gate over d.
func RunRank(ctx context.Context, f RankFixture, d decider.Decider, o RankOptions) (RankReport, error) {
	if o.MinTasks == 0 {
		o.MinTasks = DefaultMinSamples
	}
	if len(o.Ks) == 0 {
		o.Ks = []int{1, 3, 5}
	}
	if o.MaxFalseDropRate == 0 {
		o.MaxFalseDropRate = 0.02
	}
	set := knowledgerank.Settings{Enabled: true, DropAtOrBelow: ChooseDropThreshold(f.TasksIn(SplitTune))}
	rk, err := newRanker(d, set)
	if err != nil {
		return RankReport{}, err
	}
	_, th := set.Effective()
	r := RankReport{Name: f.Name, Thresholds: th, Tune: len(f.TasksIn(SplitTune)), Provenance: f.Provenance, Opts: o,
		Existing: RankScore{Arm: "existing-order"}, Model: RankScore{Arm: "model-rank"}}
	for _, k := range o.Ks {
		r.Existing.P = append(r.Existing.P, PrecisionAt{K: k})
		r.Model.P = append(r.Model.P, PrecisionAt{K: k})
	}
	for _, t := range f.TasksIn(SplitReport) {
		r.addTask(ctx, rk, t)
	}
	return r, nil
}

func (r *RankReport) addTask(ctx context.Context, rk *knowledgerank.Ranker, t RankTask) {
	r.Tasks++
	label := map[string]int{}
	cands := make([]knowledgerank.Candidate, len(t.Candidates))
	var existing []string
	for i, c := range t.Candidates {
		label[c.ID] = c.Label
		cands[i] = knowledgerank.Candidate{ID: c.ID, Source: c.Source, Text: c.Text}
		existing = append(existing, c.ID)
		r.Candidates++
		if c.Label == LabelNeeded {
			r.Needed++
		}
	}
	rec, _ := rk.Shadow(ctx, t.Task, cands)
	for _, s := range rec.Scored {
		if s.Error != "" {
			r.errors++
		}
	}
	for _, id := range rec.Dropped {
		r.Model.Dropped++
		if label[id] == LabelNeeded {
			r.Model.FalseDrops++
		}
	}
	tally(&r.Existing, existing, label)
	tally(&r.Model, rec.Hypothetical, label)
}

func newRanker(d decider.Decider, s knowledgerank.Settings) (*knowledgerank.Ranker, error) {
	g, err := decisiongate.New(d, decisiongate.Config{}, nil)
	if err != nil {
		return nil, fmt.Errorf("rank eval: %w", err)
	}
	return knowledgerank.New(g, s)
}

func tally(s *RankScore, ranking []string, label map[string]int) {
	for i := range s.P {
		k := min(s.P[i].K, len(ranking))
		s.P[i].Slots += k
		for _, id := range ranking[:k] {
			if label[id] == LabelNeeded {
				s.P[i].Hits++
			}
		}
	}
}

// ChooseDropThreshold picks the highest drop cut point (in 0.05 steps, never
// above 0.95 so a "related" score is not dropped by tuning alone) that drops
// no needed item on the tune tasks, using recorded scores. It falls back to
// the conservative 0.5 default when there is no tune evidence.
func ChooseDropThreshold(tune []RankTask) float64 {
	best := 0.5
	have := false
	for c := 5; c <= 95; c += 5 {
		cut := float64(c) / 100
		bad := false
		for _, t := range tune {
			for _, cd := range t.Candidates {
				if cd.Score != nil {
					have = true
					if cd.Label == LabelNeeded && *cd.Score <= cut {
						bad = true
					}
				}
			}
		}
		if !bad && have {
			best = cut
		}
	}
	return best
}

func (r RankReport) pct(num, den int) string {
	if r.Tasks < r.Opts.MinTasks || den == 0 {
		return fmt.Sprintf("n/a (tasks=%d<%d)", r.Tasks, r.Opts.MinTasks)
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", 100*float64(num)/float64(den), num, den)
}

// Graduation says whether the evidence supports shadow to enforce.
func (r RankReport) Graduation() (bool, string) {
	k3 := 0
	for i, p := range r.Model.P {
		if p.K == 3 {
			k3 = i
		}
	}
	switch {
	case r.Tasks < r.Opts.MinTasks || r.Needed < r.Opts.MinTasks:
		return false, fmt.Sprintf("insufficient samples: %d tasks, %d needed items, need %d each", r.Tasks, r.Needed, r.Opts.MinTasks)
	case float64(r.Model.FalseDrops) > r.Opts.MaxFalseDropRate*float64(r.Needed):
		return false, fmt.Sprintf("needed items dropped above %.0f%%", 100*r.Opts.MaxFalseDropRate)
	case r.Model.P[k3].Hits*r.Existing.P[k3].Slots < r.Existing.P[k3].Hits*r.Model.P[k3].Slots:
		return false, "precision below the existing order"
	}
	return true, "meets sample, false-drop and precision criteria"
}

// Render formats the report as deterministic plain text.
func (r RankReport) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "fixture: %s (%s, model %s, prompt %s, recorded %s)\n", r.Name, r.Provenance.Kind, r.Provenance.Model, r.Provenance.Prompt, r.Provenance.RecordedAt)
	fmt.Fprintf(&b, "thresholds (drop chosen on %d tune tasks): dropAtOrBelow=%.2f neededAtOrAbove=%.2f\n", r.Tune, r.Thresholds.DropAtOrBelow, r.Thresholds.NeededAtOrAbove)
	fmt.Fprintf(&b, "report-split tasks=%d candidates=%d needed=%d score-errors=%d\n", r.Tasks, r.Candidates, r.Needed, r.errors)
	for _, s := range []RankScore{r.Existing, r.Model} {
		fmt.Fprintf(&b, "\n[%s]\n", s.Arm)
		for _, p := range s.P {
			fmt.Fprintf(&b, "  precision@%d=%s\n", p.K, r.pct(p.Hits, p.Slots))
		}
		if s.Arm == r.Model.Arm {
			fmt.Fprintf(&b, "  would-drop=%d needed-dropped=%d\n", s.Dropped, s.FalseDrops)
		}
	}
	ok, why := r.Graduation()
	fmt.Fprintf(&b, "\ngraduation(shadow->enforce): ready=%t: %s\n", ok, why)
	return b.String()
}
