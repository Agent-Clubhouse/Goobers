package vnext

import (
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// Calibration is an immutable, caller-supplied snapshot of observed runtime
// facts. It is intentionally not a journal or read-model handle: simulations
// must remain read-only and reproducible from the snapshot they record.
type Calibration struct {
	WindowStart time.Time
	WindowEnd   time.Time
	Runs        int
	MinSamples  int
	Gates       map[string]map[string]int
	Outcomes    map[string]int
	Nodes       map[string]NodeCalibration
}

// NodeCalibration contains observations for one executable graph node.
type NodeCalibration struct {
	Samples    int
	Successes  int
	Durations  []time.Duration
	RetryWaste []float64
	Costs      []float64
}

// ChangedNode is an explicit assumption about a changed agentic node. Its
// values are never inferred from historical observations.
type ChangedNode struct {
	SuccessProbability float64
	Duration           time.Duration
	Cost               float64
	RetryWaste         float64
}

// SimulationOptions controls a deterministic Monte-Carlo projection.
type SimulationOptions struct {
	Samples      int
	Seed         int64
	ChangedNodes map[string]ChangedNode
}

// SimulationResult is a forecast and its provenance.
type SimulationResult struct {
	CalibrationWindow  Window
	SampleCount        int
	Confidence         Confidence
	FallbackNodes      []string
	DistributionShift  []string
	Outcomes           map[string]int
	ExpectedCost       float64
	ExpectedCycleTime  time.Duration
	ExpectedRetryWaste float64
	NodeContributions  map[string]NodeContribution
	BranchMass         map[string]float64
	FallbackMode       string
}

// Window identifies the observation interval used for calibration.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Confidence describes how much empirical support backs a simulation.
type Confidence string

const (
	// ConfidenceLow indicates sparse or fallback-heavy evidence.
	ConfidenceLow Confidence = "low"
	// ConfidenceMedium indicates partial empirical support.
	ConfidenceMedium Confidence = "medium"
	// ConfidenceHigh indicates broad empirical support.
	ConfidenceHigh Confidence = "high"
)

// NodeContribution summarizes a node's simulated resource contribution.
type NodeContribution struct {
	Visits       int
	ExpectedTime time.Duration
	ExpectedCost float64
}

// Simulate compiles and simulates a workflow using the supplied snapshot.
func Simulate(def Definition, calibration Calibration, options SimulationOptions) (SimulationResult, error) {
	machine, problems := newMachineForCheck(def)
	if len(problems) != 0 {
		return SimulationResult{}, fmt.Errorf("simulate workflow: %s", problems[0])
	}
	if problems := structuralProblems(machine); len(problems) != 0 {
		return SimulationResult{}, fmt.Errorf("simulate workflow: %s", problems[0])
	}
	return SimulateGraph(machine.Graph(), calibration, options)
}

// SimulateGraph evaluates a canonical graph. It is useful for what-if callers
// that have already applied a topology edit to a compiled definition.
func SimulateGraph(graph Graph, calibration Calibration, options SimulationOptions) (SimulationResult, error) {
	if graph.Start == "" {
		return SimulationResult{}, fmt.Errorf("simulate workflow: graph start is required")
	}
	if options.Samples <= 0 {
		options.Samples = 1000
	}
	if calibration.MinSamples <= 0 {
		calibration.MinSamples = 10
	}
	result := SimulationResult{
		CalibrationWindow: Window{Start: calibration.WindowStart.UTC(), End: calibration.WindowEnd.UTC()},
		SampleCount:       options.Samples,
		Confidence:        confidence(calibration.Runs, calibration.MinSamples),
		Outcomes:          make(map[string]int),
		NodeContributions: make(map[string]NodeContribution),
		BranchMass:        make(map[string]float64),
	}
	fallback := map[string]bool{}
	shift := map[string]bool{}
	rng := rand.New(rand.NewSource(options.Seed))
	edges := make(map[string][]GraphEdge)
	nodes := make(map[string]GraphNode)
	for _, edge := range graph.Edges {
		edges[edge.Source] = append(edges[edge.Source], edge)
	}
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	for name := range options.ChangedNodes {
		changed := options.ChangedNodes[name]
		if changed.SuccessProbability < 0 || changed.SuccessProbability > 1 {
			return SimulationResult{}, fmt.Errorf("simulate workflow: changed node %q success probability must be between 0 and 1", name)
		}
		shift[name] = true
	}

	for sample := 0; sample < options.Samples; sample++ {
		state := graph.Start
		var cycle time.Duration
		var retryWaste float64
		visited := 0
		for visited < 10000 {
			if outcome := terminalOutcome(state); outcome != "" {
				result.Outcomes[outcome]++
				result.BranchMass[outcome]++
				break
			}
			outgoing := edges[state]
			if len(outgoing) == 0 {
				return SimulationResult{}, fmt.Errorf("simulate workflow: state %q has no outgoing edge", state)
			}
			node := nodes[state]
			observation, observed := calibration.Nodes[state]
			var nodeDuration time.Duration
			var nodeCost float64
			if changed, ok := options.ChangedNodes[state]; ok {
				if changed.Duration > 0 {
					cycle += changed.Duration
				}
				nodeDuration = changed.Duration
				nodeCost = changed.Cost
				result.ExpectedCost += changed.Cost
				retryWaste += changed.RetryWaste
				if rng.Float64() >= changed.SuccessProbability {
					result.Outcomes["abort"]++
					break
				}
			} else if observed && observation.Samples >= calibration.MinSamples {
				d := sampledDuration(rng, observation.Durations)
				cycle += d
				nodeDuration = d
				retryWaste += sampledFloat(rng, observation.RetryWaste)
				nodeCost = sampledFloat(rng, observation.Costs)
				result.ExpectedCost += nodeCost
				if observation.Successes > 0 && observation.Successes < observation.Samples &&
					rng.Float64() >= float64(observation.Successes)/float64(observation.Samples) {
					result.Outcomes["abort"]++
					result.BranchMass["abort"]++
					break
				}
			} else {
				if node.Kind != GraphNodeGate {
					fallback[state] = true
				}
			}
			contribution := result.NodeContributions[state]
			contribution.Visits++
			contribution.ExpectedTime += nodeDuration
			contribution.ExpectedCost += nodeCost
			result.NodeContributions[state] = contribution

			if node.Kind == GraphNodeParallel {
				branchEdges := make([]GraphEdge, 0, len(outgoing))
				var failureEdge *GraphEdge
				for i := range outgoing {
					if outgoing[i].Outcome == "branch-failed" {
						edge := outgoing[i]
						failureEdge = &edge
					} else {
						branchEdges = append(branchEdges, outgoing[i])
					}
				}
				branchFailed := false
				for _, branch := range branchEdges {
					result.BranchMass[branch.Branch]++
					failed, err := simulateBranch(graph, branch.Target, branchEdges, calibration, options,
						rng, &result, fallback, shift, &cycle, &retryWaste)
					if err != nil {
						return SimulationResult{}, err
					}
					branchFailed = branchFailed || failed
				}
				if branchFailed {
					if failureEdge != nil {
						state = failureEdge.Target
					} else {
						state = TargetAbort
					}
					visited++
					continue
				}
				if len(branchEdges) > 0 {
					state = parallelJoin(graph, branchEdges)
					visited++
					continue
				}
				if failureEdge != nil {
					state = failureEdge.Target
					visited++
					continue
				}
			}
			edge := chooseEdge(rng, state, node, outgoing, calibration.Gates, calibration.MinSamples, fallback)
			state = edge.Target
			result.BranchMass[edgeLabel(edge)]++
			visited++
		}
		if visited == 10000 {
			result.Outcomes["escalate"]++
		}
		result.ExpectedCycleTime += cycle
		result.ExpectedRetryWaste += retryWaste
	}
	result.ExpectedCycleTime /= time.Duration(options.Samples)
	result.ExpectedCost /= float64(options.Samples)
	result.ExpectedRetryWaste /= float64(options.Samples)
	for name, contribution := range result.NodeContributions {
		contribution.ExpectedTime /= time.Duration(options.Samples)
		result.NodeContributions[name] = contribution
	}
	for name := range fallback {
		if len(name) < 8 || name[:8] != "@static:" {
			result.FallbackNodes = append(result.FallbackNodes, name)
		}
	}
	if len(fallback) > 0 {
		result.FallbackMode = "static-all-possible"
	}
	for name := range shift {
		result.DistributionShift = append(result.DistributionShift, name)
	}
	for branch, mass := range result.BranchMass {
		result.BranchMass[branch] = mass / float64(options.Samples)
	}
	sort.Strings(result.FallbackNodes)
	sort.Strings(result.DistributionShift)
	return result, nil
}

// EvaluateWhatIf compares two read-only graph projections using the same
// calibration and seed, making deltas auditable.
func EvaluateWhatIf(baseline, candidate Graph, calibration Calibration, options SimulationOptions) (WhatIfResult, error) {
	base, err := SimulateGraph(baseline, calibration, options)
	if err != nil {
		return WhatIfResult{}, err
	}
	after, err := SimulateGraph(candidate, calibration, options)
	if err != nil {
		return WhatIfResult{}, err
	}
	return WhatIfResult{
		Baseline: base, Candidate: after,
		SuccessRateDelta: rate(after, "complete") - rate(base, "complete"),
		CostDelta:        after.ExpectedCost - base.ExpectedCost,
		CycleTimeDelta:   after.ExpectedCycleTime - base.ExpectedCycleTime,
		RetryWasteDelta:  after.ExpectedRetryWaste - base.ExpectedRetryWaste,
		BranchMassDelta:  subtractMass(after.BranchMass, base.BranchMass),
	}, nil
}

// WhatIfResult compares a candidate graph against its baseline.
type WhatIfResult struct {
	Baseline, Candidate SimulationResult
	SuccessRateDelta    float64
	CostDelta           float64
	CycleTimeDelta      time.Duration
	RetryWasteDelta     float64
	BranchMassDelta     map[string]float64
}

func chooseEdge(r *rand.Rand, state string, node GraphNode, edges []GraphEdge, gates map[string]map[string]int, minimum int, fallback map[string]bool) GraphEdge {
	if node.Kind != GraphNodeGate {
		return edges[0]
	}
	counts := gates[state]
	total := 0
	for _, edge := range edges {
		total += counts[edge.Outcome]
	}
	if total < minimum {
		fallback["@static:"+state] = true
		return edges[r.Intn(len(edges))]
	}
	pick := r.Intn(total)
	for _, edge := range edges {
		pick -= counts[edge.Outcome]
		if pick < 0 {
			return edge
		}
	}
	return edges[len(edges)-1]
}

func edgeLabel(edge GraphEdge) string {
	if edge.Outcome != "" {
		return edge.Source + ":" + edge.Outcome
	}
	if edge.Branch != "" {
		return edge.Source + ":" + edge.Branch
	}
	return edge.Source + "->" + edge.Target
}

func subtractMass(candidate, baseline map[string]float64) map[string]float64 {
	delta := make(map[string]float64, len(candidate)+len(baseline))
	for key, value := range baseline {
		delta[key] = -value
	}
	for key, value := range candidate {
		delta[key] += value
	}
	return delta
}

// simulateBranch walks one static fan-out branch until it reaches the common
// join. Branches are independent projections; their node observations are
// accumulated into the same sample result.
func simulateBranch(graph Graph, start string, branches []GraphEdge, calibration Calibration,
	options SimulationOptions, rng *rand.Rand, result *SimulationResult, fallback, shift map[string]bool,
	cycle *time.Duration, retryWaste *float64) (bool, error) {
	join := parallelJoin(graph, branches)
	state := start
	for steps := 0; steps < 10000 && state != join; steps++ {
		if terminalOutcome(state) != "" {
			return terminalOutcome(state) != "complete", nil
		}
		var node GraphNode
		found := false
		for _, candidate := range graph.Nodes {
			if candidate.ID == state {
				node, found = candidate, true
				break
			}
		}
		if !found {
			return false, fmt.Errorf("simulate workflow: branch state %q is undefined", state)
		}
		outgoing := make([]GraphEdge, 0)
		for _, edge := range graph.Edges {
			if edge.Source == state {
				outgoing = append(outgoing, edge)
			}
		}
		if len(outgoing) == 0 {
			return false, fmt.Errorf("simulate workflow: branch state %q has no outgoing edge", state)
		}
		observation, observed := calibration.Nodes[state]
		var duration time.Duration
		var cost float64
		if changed, ok := options.ChangedNodes[state]; ok {
			duration, cost = changed.Duration, changed.Cost
			shift[state] = true
			*cycle += duration
			result.ExpectedCost += cost
			if rng.Float64() >= changed.SuccessProbability {
				return true, nil
			}
		} else if observed && observation.Samples >= calibration.MinSamples {
			duration = sampledDuration(rng, observation.Durations)
			cost = sampledFloat(rng, observation.Costs)
			*cycle += duration
			result.ExpectedCost += cost
			if observation.Successes > 0 && observation.Successes < observation.Samples &&
				rng.Float64() >= float64(observation.Successes)/float64(observation.Samples) {
				return true, nil
			}
			*retryWaste += sampledFloat(rng, observation.RetryWaste)
		} else {
			if node.Kind != GraphNodeGate {
				fallback[state] = true
			}
		}
		result.NodeContributions[state] = addContribution(result.NodeContributions[state], duration, cost)
		edge := chooseEdge(rng, state, node, outgoing, calibration.Gates, calibration.MinSamples, fallback)
		state = edge.Target
	}
	return false, nil
}

func addContribution(contribution NodeContribution, duration time.Duration, cost float64) NodeContribution {
	contribution.Visits++
	contribution.ExpectedTime += duration
	contribution.ExpectedCost += cost
	return contribution
}

func parallelJoin(graph Graph, branches []GraphEdge) string {
	if len(branches) == 0 {
		return ""
	}
	reachable := func(start string) map[string]bool {
		seen := map[string]bool{}
		queue := []string{start}
		for len(queue) > 0 {
			state := queue[0]
			queue = queue[1:]
			if seen[state] {
				continue
			}
			seen[state] = true
			for _, edge := range graph.Edges {
				if edge.Source == state && edge.Terminal == "" {
					queue = append(queue, edge.Target)
				}
			}
		}
		return seen
	}
	common := reachable(branches[0].Target)
	for _, branch := range branches[1:] {
		next := reachable(branch.Target)
		for state := range common {
			if !next[state] {
				delete(common, state)
			}
		}
	}
	for _, node := range graph.Nodes {
		if common[node.ID] {
			return node.ID
		}
	}
	return ""
}
func sampledDuration(r *rand.Rand, values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	return values[r.Intn(len(values))]
}

func sampledFloat(r *rand.Rand, values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	return values[r.Intn(len(values))]
}

func terminalOutcome(state string) string {
	switch state {
	case TerminalComplete:
		return "complete"
	case TargetAbort:
		return "abort"
	case TargetEscalate:
		return "escalate"
	default:
		return ""
	}
}

func confidence(runs, minimum int) Confidence {
	if runs < minimum {
		return ConfidenceLow
	}
	if runs < minimum*10 {
		return ConfidenceMedium
	}
	return ConfidenceHigh
}

func rate(result SimulationResult, outcome string) float64 {
	return float64(result.Outcomes[outcome]) / float64(result.SampleCount)
}
