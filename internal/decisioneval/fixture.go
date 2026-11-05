// Package decisioneval is an offline, deterministic A/B harness that scores
// the System One decider and a free-form LLM-judge baseline against the same
// labelled fixtures. It exists to justify (or block) graduating a decision
// gate from shadow to enforce mode with counts and sample sizes.
//
// Replay arms read recorded answers from the fixture, so the default run
// needs no network, credentials or endpoints. Live arms are opt-in: callers
// supply their own decider.Decider and Judge and must first pass LiveEnabled,
// which looks only at an environment variable name.
package decisioneval

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Split names which part of a fixture an item belongs to. Thresholds are
// chosen on SplitTune; reported numbers use SplitReport only.
type Split string

// Fixture splits.
const (
	SplitTune   Split = "tune"
	SplitReport Split = "report"
)

// Item is one labelled input. Label is the ground truth for the question
// (true means "yes"). SystemOne and Judge are recorded answers used by the
// replay arms; they are ignored by live arms.
type Item struct {
	ID        string   `json:"id"`
	Split     Split    `json:"split"`
	Text      string   `json:"text"`
	Label     bool     `json:"label"`
	SystemOne *float64 `json:"systemOne,omitempty"`
	Judge     string   `json:"judge,omitempty"`
	// Source is the label provenance, for example "handoffcheck" or "human".
	Source string `json:"source,omitempty"`
}

// Fixture is a labelled benchmark for one yes/no question.
type Fixture struct {
	Question     string `json:"question"`
	Instructions string `json:"instructions"`
	Items        []Item `json:"items"`
}

// LoadFixture reads and validates a fixture file.
func LoadFixture(path string) (Fixture, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, err
	}
	var f Fixture
	if err := json.Unmarshal(b, &f); err != nil {
		return Fixture{}, fmt.Errorf("decode fixture %s: %w", path, err)
	}
	return f, f.Validate()
}

// Validate reports a fixture that cannot be scored.
func (f Fixture) Validate() error {
	if f.Question == "" || f.Instructions == "" {
		return errors.New("fixture needs question and instructions")
	}
	seen := map[string]bool{}
	for _, it := range f.Items {
		if it.ID == "" || seen[it.ID] {
			return fmt.Errorf("item id %q is empty or duplicated", it.ID)
		}
		seen[it.ID] = true
		if it.Split != SplitTune && it.Split != SplitReport {
			return fmt.Errorf("item %q: split must be tune or report", it.ID)
		}
		if it.SystemOne != nil && (*it.SystemOne < 0 || *it.SystemOne > 1) {
			return fmt.Errorf("item %q: systemOne probability out of range", it.ID)
		}
	}
	return nil
}

// ItemsIn returns the items of one split.
func (f Fixture) ItemsIn(s Split) []Item {
	var out []Item
	for _, it := range f.Items {
		if it.Split == s {
			out = append(out, it)
		}
	}
	return out
}

// LiveEnv is the name of the environment variable that opts into live model
// calls. Only the name lives in the repository.
const LiveEnv = "GOOBERS_DECIDE_EVAL_LIVE"

// LiveEnabled reports whether live arms may be used. It is off unless the
// variable is exactly "1".
func LiveEnabled(getenv func(string) string) bool { return getenv(LiveEnv) == "1" }
