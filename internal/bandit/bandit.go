// Package bandit implements safe, deterministic routing of declared node
// variants. It does not mutate configuration; promotion is an output for a
// separately gated workflow.
package bandit

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/journal"
)

// Schema identifies the durable observation event format.
const Schema = "goobers.dev/bandit/observation/v1"

// Arm is a declarative variant and its required gate strength.
type Arm struct {
	Name      string            `json:"name" yaml:"name"`
	Variant   map[string]string `json:"variant,omitempty" yaml:"variant,omitempty"`
	GateLevel int               `json:"gateLevel" yaml:"gateLevel"`
}

// Config controls deterministic assignment, retirement, and promotion.
type Config struct {
	Stage             string  `json:"stage" yaml:"stage"`
	Seed              uint64  `json:"seed" yaml:"seed"`
	Arms              []Arm   `json:"arms" yaml:"arms"`
	ExplorationBudget int     `json:"explorationBudget" yaml:"explorationBudget"`
	MinSamples        int     `json:"minSamples" yaml:"minSamples"`
	MaxFailureRate    float64 `json:"maxFailureRate" yaml:"maxFailureRate"`
	MinLift           float64 `json:"minLift" yaml:"minLift"`
	Confidence        float64 `json:"confidence" yaml:"confidence"`
	TrainWindow       int     `json:"trainWindow" yaml:"trainWindow"`
	EvalWindow        int     `json:"evalWindow" yaml:"evalWindow"`
	DefaultGateLevel  int     `json:"defaultGateLevel" yaml:"defaultGateLevel"`
}

// Observation records the outcome of one assigned arm.
type Observation struct {
	Schema    string  `json:"schema"`
	Stage     string  `json:"stage"`
	RunID     string  `json:"runId"`
	Arm       string  `json:"arm"`
	Reward    float64 `json:"reward"`
	RewardSet bool    `json:"rewardSet,omitempty"`
	Success   bool    `json:"success"`
	Window    string  `json:"window"` // train or eval
	Assigned  uint64  `json:"assigned"`
}

// Assignment is the deterministic arm selected for a run.
type Assignment struct {
	Arm     string
	Seed    uint64
	Variant map[string]string
}

// Apply overlays the selected declarative variant on a node's base settings.
// The returned map is independent of both inputs and is safe for a launcher to
// modify while constructing its invocation.
func (a Assignment) Apply(base map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(a.Variant))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range a.Variant {
		out[key] = value
	}
	return out
}

// Decision describes whether an arm is eligible for promotion.
type Decision struct {
	Arm          string
	Eligible     bool
	Retired      bool
	Reason       string
	Confidence   float64
	Lift         float64
	TrainSamples int
	EvalSamples  int
}

// Proposal is a promotion nomination. Approval remains external to this
// package; the router never changes the declared default.
type Proposal struct {
	Stage            string
	Arm              string
	Variant          map[string]string
	Lift             float64
	Confidence       float64
	RequiresApproval bool
}

// Journal is the small append-only seam needed by the router. journal.Run is
// the production implementation; the interface also permits read-model
// projections and deterministic tests.
type Journal interface {
	Append(journal.Event) error
}

// Validate verifies the experiment's safety and sampling constraints.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Stage) == "" {
		return fmt.Errorf("bandit stage must not be empty")
	}
	if len(c.Arms) < 2 || len(c.Arms) > 3 {
		return fmt.Errorf("bandit requires 2 or 3 arms")
	}
	seen := make(map[string]bool, len(c.Arms))
	for _, arm := range c.Arms {
		if strings.TrimSpace(arm.Name) == "" || seen[arm.Name] {
			return fmt.Errorf("bandit arm names must be unique and non-empty")
		}
		seen[arm.Name] = true
	}
	defaultArm := c.defaultArm()
	for _, arm := range c.Arms {
		if arm.GateLevel < defaultArm.GateLevel {
			return fmt.Errorf("bandit arm %q weakens the default gate arm %q", arm.Name, defaultArm.Name)
		}
	}
	if c.ExplorationBudget <= 0 || c.MinSamples <= 0 {
		return fmt.Errorf("bandit explorationBudget and minSamples must be positive")
	}
	if c.ExplorationBudget < len(c.Arms)*c.MinSamples {
		return fmt.Errorf("bandit explorationBudget must cover the sample floor for every arm")
	}
	if c.MaxFailureRate <= 0 || c.MaxFailureRate >= 1 {
		return fmt.Errorf("bandit maxFailureRate must be between 0 and 1")
	}
	if c.MinLift < 0 || c.Confidence <= 0 || c.Confidence >= 1 {
		return fmt.Errorf("bandit minLift must be non-negative and confidence must be between 0 and 1")
	}
	if c.TrainWindow < c.MinSamples || c.EvalWindow < c.MinSamples {
		return fmt.Errorf("bandit windows must contain at least minSamples observations")
	}
	return nil
}

func (c Config) defaultArm() Arm {
	defaultArm := c.Arms[0]
	for _, arm := range c.Arms {
		if arm.Name == "control" {
			defaultArm = arm
			break
		}
	}
	return defaultArm
}

// Assign deterministically selects an active arm for a run.
func (c Config) Assign(runID string, observations []Observation) (Assignment, error) {
	if err := c.Validate(); err != nil {
		return Assignment{}, err
	}
	if strings.TrimSpace(runID) == "" {
		return Assignment{}, fmt.Errorf("bandit run ID must not be empty")
	}
	count := 0
	for _, observation := range observations {
		if observation.Stage == c.Stage {
			count++
		}
	}
	if count >= c.ExplorationBudget {
		return Assignment{}, fmt.Errorf("bandit exploration budget exhausted")
	}

	counts := make(map[string]int, len(c.Arms))
	for _, observation := range observations {
		if observation.Stage == c.Stage && observation.Window == "train" {
			counts[observation.Arm]++
		}
	}
	active := make([]Arm, 0, len(c.Arms))
	for _, arm := range c.Arms {
		if !c.Retired(arm.Name, observations) {
			active = append(active, arm)
		}
	}
	if len(active) == 0 {
		return Assignment{}, fmt.Errorf("all bandit arms are retired")
	}
	// Fill the sample floor before sampling posteriors. Ties are resolved by
	// declaration order, making replay independent of map iteration.
	for _, arm := range active {
		if counts[arm.Name] < c.MinSamples {
			return c.assignment(arm, runID), nil
		}
	}

	best := active[0]
	bestScore := -1.0
	for _, arm := range active {
		alpha, beta := posterior(arm.Name, filterWindow(observations, c.Stage, "train"))
		score := betaSample(alpha, beta, assignmentSeed(c.Seed, runID+"|"+arm.Name))
		if score > bestScore {
			best, bestScore = arm, score
		}
	}
	return c.assignment(best, runID), nil
}

// Evaluate assesses held-out observations against the promotion criteria.
func (c Config) Evaluate(observations []Observation) (Decision, error) {
	if err := c.Validate(); err != nil {
		return Decision{}, err
	}
	train := filterWindow(observations, c.Stage, "train")
	eval := filterWindow(observations, c.Stage, "eval")
	if len(train) < c.TrainWindow || len(eval) < c.EvalWindow {
		return Decision{Reason: "insufficient-held-out-data", TrainSamples: len(train), EvalSamples: len(eval)}, nil
	}
	for _, arm := range c.Arms {
		if armSamples(train, arm.Name) < c.MinSamples || armSamples(eval, arm.Name) < c.MinSamples {
			return Decision{Reason: "insufficient-arm-data", TrainSamples: len(train), EvalSamples: len(eval)}, nil
		}
	}
	defaultArm := c.Arms[0]
	for _, arm := range c.Arms {
		if arm.Name == "control" {
			defaultArm = arm
			break
		}

	}
	control := rate(eval, defaultArm.Name)
	best := Decision{Reason: "no-arm-cleared-bar", TrainSamples: len(train), EvalSamples: len(eval)}
	for _, arm := range c.Arms {
		if arm.Name == defaultArm.Name {
			continue
		}
		failures := failureRate(eval, arm.Name)
		if failures > c.MaxFailureRate {
			continue
		}
		lift := rate(eval, arm.Name) - control
		confidence := posteriorConfidence(arm.Name, defaultArm.Name, eval)
		if lift >= c.MinLift && confidence >= c.Confidence && (!best.Eligible || lift > best.Lift) {
			best = Decision{Arm: arm.Name, Eligible: true, Lift: lift, Confidence: confidence, TrainSamples: len(train), EvalSamples: len(eval)}
		}
	}
	return best, nil
}

// AssignAndRecord chooses an arm and durably records the assignment before
// returning it. A failed append is returned so execution cannot proceed with
// an unaudited experiment.
func (c Config) AssignAndRecord(runID string, observations []Observation, out Journal) (Assignment, error) {
	if out == nil {
		return Assignment{}, fmt.Errorf("bandit assignment journal is required")
	}
	assignment, err := c.Assign(runID, observations)
	if err != nil {
		return Assignment{}, err
	}
	if err := out.Append(journal.Event{
		Type:  journal.EventBanditAssignment,
		Stage: c.Stage,
		Outputs: map[string]any{
			"arm": assignment.Arm, "seed": assignment.Seed, "variant": assignment.Variant,
		},
	}); err != nil {
		return Assignment{}, fmt.Errorf("record bandit assignment: %w", err)
	}
	return assignment, nil
}

// RecordObservation appends one validated outcome to the run journal.
func (c Config) RecordObservation(observation Observation, out Journal) error {
	if out == nil {
		return fmt.Errorf("bandit observation journal is required")
	}
	data, err := observation.MarshalJSON()
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("encode bandit observation: %w", err)
	}
	if err := out.Append(journal.Event{Type: journal.EventBanditObservation, Stage: c.Stage, Outputs: payload}); err != nil {
		return fmt.Errorf("record bandit observation: %w", err)
	}
	return nil
}

// EvaluateAndRecord emits retirement observations and, when eligible, a
// gated promotion proposal. It never emits a proposal for an ineligible arm.
func (c Config) EvaluateAndRecord(observations []Observation, out Journal) (Decision, *Proposal, error) {
	if out == nil {
		return Decision{}, nil, fmt.Errorf("bandit decision journal is required")
	}
	decision, err := c.Evaluate(observations)
	if err != nil {
		return Decision{}, nil, err
	}
	for _, arm := range c.Arms {
		if c.Retired(arm.Name, observations) {
			if err := out.Append(journal.Event{Type: journal.EventBanditRetired, Stage: c.Stage, Outputs: map[string]any{"arm": arm.Name, "reason": "failure-rate"}}); err != nil {
				return Decision{}, nil, fmt.Errorf("record bandit retirement: %w", err)
			}
			decision.Retired = true
		}
	}
	if !decision.Eligible {
		return decision, nil, nil
	}
	arm := c.arm(decision.Arm)
	proposal := &Proposal{
		Stage: c.Stage, Arm: decision.Arm, Variant: cloneVariant(arm.Variant),
		Lift: decision.Lift, Confidence: decision.Confidence, RequiresApproval: true,
	}
	if err := out.Append(journal.Event{Type: journal.EventBanditPromotionProposed, Stage: c.Stage, Outputs: map[string]any{
		"arm": proposal.Arm, "lift": proposal.Lift, "confidence": proposal.Confidence,
		"requiresApproval": proposal.RequiresApproval, "variant": proposal.Variant,
	}}); err != nil {
		return Decision{}, nil, fmt.Errorf("record bandit promotion proposal: %w", err)
	}
	return decision, proposal, nil
}

// Retired reports whether an arm exceeds the configured failure-rate limit.
func (c Config) Retired(arm string, observations []Observation) bool {
	if c.Validate() != nil {
		return false
	}
	selected := filterWindow(observations, c.Stage, "")
	for _, observation := range selected {
		if observation.Arm == arm {
			return failureRate(selected, arm) > c.MaxFailureRate
		}
	}
	return false
}

// MarshalJSON validates and serializes an observation.
func (o Observation) MarshalJSON() ([]byte, error) {
	type alias Observation
	if o.Schema == "" {
		o.Schema = Schema
	}
	if o.Stage == "" || o.RunID == "" || o.Arm == "" || (o.Window != "train" && o.Window != "eval") ||
		math.IsNaN(o.Reward) || math.IsInf(o.Reward, 0) || o.Reward < 0 || o.Reward > 1 {
		return nil, fmt.Errorf("invalid bandit observation")
	}
	return json.Marshal(alias(o))
}

func assignmentSeed(seed uint64, runID string) uint64 {
	var input [8]byte
	binary.BigEndian.PutUint64(input[:], seed)
	sum := sha256.Sum256(append(input[:], []byte(runID)...))
	return binary.BigEndian.Uint64(sum[:8])
}

func (c Config) assignment(arm Arm, runID string) Assignment {
	return Assignment{Arm: arm.Name, Seed: assignmentSeed(c.Seed, runID), Variant: cloneVariant(arm.Variant)}
}

func (c Config) arm(name string) Arm {
	for _, arm := range c.Arms {
		if arm.Name == name {
			return arm
		}
	}
	return Arm{}
}

func cloneVariant(variant map[string]string) map[string]string {
	if variant == nil {
		return nil
	}
	out := make(map[string]string, len(variant))
	for key, value := range variant {
		out[key] = value
	}
	return out
}

func armSamples(observations []Observation, arm string) int {
	count := 0
	for _, observation := range observations {
		if observation.Arm == arm {
			count++
		}
	}
	return count
}

func posterior(arm string, observations []Observation) (float64, float64) {
	successes := 0.0
	failures := 0.0
	for _, observation := range observations {
		if observation.Arm != arm {
			continue
		}
		if observation.Success {
			successes++
		} else {
			failures++
		}
	}
	return successes + 1, failures + 1
}

func betaSample(alpha, beta float64, seed uint64) float64 {
	x := uniform(seed)
	y := uniform(seed ^ 0x9e3779b97f4a7c15)
	// A deterministic normal approximation is sufficient for routing and
	// avoids a mutable RNG in the scheduler.
	mean := alpha / (alpha + beta)
	total := alpha + beta
	stddev := math.Sqrt(alpha * beta / (total * total * (total + 1)))
	z := math.Sqrt(-2*math.Log(math.Max(x, 1e-12))) * math.Cos(2*math.Pi*y)
	return math.Max(0, math.Min(1, mean+stddev*z))
}

func uniform(seed uint64) float64 {
	return float64(seed>>11) / float64(uint64(1)<<53)
}

func filterWindow(observations []Observation, stage, window string) []Observation {
	out := make([]Observation, 0, len(observations))
	for _, observation := range observations {
		if observation.Stage == stage && (window == "" || observation.Window == window) {
			out = append(out, observation)
		}
	}
	return out
}

func rate(observations []Observation, arm string) float64 {
	var reward, total float64
	for _, observation := range observations {
		if observation.Arm == arm {
			total++
			reward += observationReward(observation)
		}
	}
	if total == 0 {
		return 0
	}
	return reward / total
}

func failureRate(observations []Observation, arm string) float64 {
	var failures, total float64
	for _, observation := range observations {
		if observation.Arm != arm {
			continue
		}
		total++
		if !observation.Success {
			failures++
		}
	}
	if total == 0 {
		return 0
	}
	return failures / total
}

func observationReward(observation Observation) float64 {
	if observation.RewardSet {
		return observation.Reward
	}
	if observation.Success {
		return 1
	}
	return 0
}

func posteriorConfidence(arm, control string, observations []Observation) float64 {
	const draws = 512
	wins := 0
	for i := 0; i < draws; i++ {
		a, b := posterior(arm, observations)
		c, d := posterior(control, observations)
		if betaSample(a, b, uint64(i)*2+1) > betaSample(c, d, uint64(i)*2+2) {
			wins++
		}
	}
	return float64(wins) / draws
}

// SortObservations orders observations by assignment sequence.
func SortObservations(observations []Observation) {
	sort.SliceStable(observations, func(i, j int) bool {
		return observations[i].Assigned < observations[j].Assigned
	})
}
