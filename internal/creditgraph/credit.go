package creditgraph

import (
	"math"
	"strconv"
	"strings"
)

// AttributionSchemaVersion identifies the credit-assignment contract's shape.
const AttributionSchemaVersion = "goobers.dev/creditgraph/attribution/v1"

// FailureClass names the kind of failure a cause finding attributes. The
// taxonomy is the contract: a consumer switches on it.
type FailureClass string

// The failure-cause taxonomy.
const (
	// ClassBadToolChoice is a tool call that named the wrong tool for the work.
	ClassBadToolChoice FailureClass = "bad-tool-choice"
	// ClassBadToolResult is a tool that ran and reported an unsuccessful result.
	ClassBadToolResult FailureClass = "bad-tool-result"
	// ClassBadInterpretation is a failure that follows only successful tool
	// results: the inputs were good and what was made of them was not.
	ClassBadInterpretation FailureClass = "bad-interpretation"
	// ClassWeakInstructions is a failure with no recorded work at all: the
	// prompt or instructions produced no tool calls and no evidence.
	ClassWeakInstructions FailureClass = "weak-instructions"
	// ClassRouting is a failure where the model that ran is not the model the
	// subagent requested.
	ClassRouting FailureClass = "routing"
	// ClassModel is a failure where the model produced no output.
	ClassModel FailureClass = "model"
	// ClassTopology is a failure caused by how agents were wired: a declared
	// dependency on a subagent that never ran.
	ClassTopology FailureClass = "topology"
	// ClassEnvironment is a failure the harness or environment raised rather
	// than one an agent decision produced.
	ClassEnvironment FailureClass = "environment"
	// ClassUnknown is an explicit refusal to attribute a cause. It is what
	// missing provenance and contradictory evidence produce, never blame.
	ClassUnknown FailureClass = "unknown"
)

// Contribution is one node's signed, uncertain share of the run's outcome.
// Share is the node's responsibility mass in [0,1]; Score carries the sign of
// what the node is estimated to have contributed, so a tool that succeeded
// inside a failing run scores positive and the failing stage scores negative.
type Contribution struct {
	NodeID      string     `json:"nodeId"`
	Kind        NodeKind   `json:"kind"`
	Label       string     `json:"label,omitempty"`
	Stage       string     `json:"stage,omitempty"`
	Attempt     int        `json:"attempt,omitempty"`
	Share       float64    `json:"share"`
	Score       float64    `json:"score"`
	Uncertainty float64    `json:"uncertainty"`
	Confidence  float64    `json:"confidence"`
	Provenance  Provenance `json:"provenance"`
}

// CauseFinding is one classified failure cause, with the assumptions it rests
// on and the evidence behind it. Evidence entries prefixed "intervention:"
// name a natural experiment — a repeated stage attempt whose outcome differed
// — and are the only entries that are more than correlational.
type CauseFinding struct {
	Class       FailureClass `json:"class"`
	NodeID      string       `json:"nodeId"`
	Stage       string       `json:"stage,omitempty"`
	Confidence  float64      `json:"confidence"`
	Summary     string       `json:"summary"`
	Assumptions []string     `json:"assumptions,omitempty"`
	Evidence    []string     `json:"evidence,omitempty"`
}

// Attribution is the credit assignment computed over one run's graph. Its
// contributions and causes are in deterministic graph order, so two
// attributions of the same graph compare equal.
type Attribution struct {
	Schema        string         `json:"schema"`
	RunID         string         `json:"runId,omitempty"`
	RootID        string         `json:"rootId"`
	Outcome       string         `json:"outcome,omitempty"`
	OutcomeSign   float64        `json:"outcomeSign"`
	Contributions []Contribution `json:"contributions"`
	Causes        []CauseFinding `json:"causes,omitempty"`
	Assumptions   []string       `json:"assumptions,omitempty"`
}

// Contribution returns the contribution computed for one node.
func (a Attribution) Contribution(nodeID string) (Contribution, bool) {
	for _, contribution := range a.Contributions {
		if contribution.NodeID == nodeID {
			return contribution, true
		}
	}
	return Contribution{}, false
}

// CausesOfClass returns the findings of one failure class, in graph order.
func (a Attribution) CausesOfClass(class FailureClass) []CauseFinding {
	var result []CauseFinding
	for _, cause := range a.Causes {
		if cause.Class == class {
			result = append(result, cause)
		}
	}
	return result
}

// The propagation constants. They are deliberately coarse: the estimate is a
// responsibility share, not a probability, and a finer-looking number would
// only dress correlation up as measurement.
const (
	shareEpsilon           = 1e-9
	recordedUncertainty    = 0.1
	unknownUncertainty     = 0.5
	gapUncertainty         = 0.1
	maxGapUncertainty      = 0.4
	unknownEdgeUncertainty = 0.3
	maxSubtreeUncertainty  = 0.4
	agreeingWeight         = 2.0
	neutralWeight          = 1.0
	opposingWeight         = 0.5
)

// The base confidence of each classification rule before the stage's own
// provenance, contradictions, and intervention evidence adjust it.
const (
	directConfidence      = 0.8
	inferredConfidence    = 0.6
	structuralConfidence  = 0.7
	contradictionPenalty  = 0.6
	interventionRatio     = 0.25
	unknownCauseCeiling   = 0.5
	unknownMajorityFactor = 2
)

var baseAssumptions = []string{
	"contribution shares are responsibility estimates derived from graph structure and recorded status signals, not measured causal effects",
	"a share is correlational unless the finding names intervention evidence",
	"missing provenance lowers confidence and is never attributed as blame",
}

type attributor struct {
	graph               *Graph
	outcomeSign         float64
	gapCount            map[string]int
	share               map[string]float64
	weightedUncertainty map[string]float64
	attempts            map[string][]Node
}

// Attribute computes signed contribution estimates with uncertainty over a
// run's credit graph and classifies the causes of its failures. The result is
// reproducible for a fixed input graph.
func Attribute(graph *Graph) Attribution {
	if graph == nil {
		return Attribution{Schema: AttributionSchemaVersion}
	}
	a := &attributor{
		graph:               graph,
		gapCount:            map[string]int{},
		share:               map[string]float64{},
		weightedUncertainty: map[string]float64{},
		attempts:            map[string][]Node{},
	}
	for _, gap := range graph.Gaps {
		a.gapCount[gap.NodeID]++
	}
	for _, node := range graph.Nodes {
		if node.Kind == KindStage {
			a.attempts[node.Label] = append(a.attempts[node.Label], node)
		}
	}
	root, _ := graph.Root()
	a.outcomeSign = statusSign(root.Attributes["status"])
	a.propagate(graph.RootID, 1, 0, map[string]bool{})

	attribution := Attribution{
		Schema:      AttributionSchemaVersion,
		RunID:       graph.RunID,
		RootID:      graph.RootID,
		Outcome:     root.Attributes["status"],
		OutcomeSign: a.outcomeSign,
		Assumptions: append([]string(nil), baseAssumptions...),
	}
	for _, node := range graph.Nodes {
		share, reached := a.share[node.ID]
		if !reached {
			continue
		}
		uncertainty := 1.0
		if share > shareEpsilon {
			uncertainty = a.weightedUncertainty[node.ID] / share
		}
		uncertainty = combine(uncertainty, a.subtreeUncertainty(node.ID))
		attribution.Contributions = append(attribution.Contributions, Contribution{
			NodeID: node.ID, Kind: node.Kind, Label: node.Label, Stage: node.Stage,
			Attempt: node.Attempt, Share: round(share), Score: round(share * a.signOf(node)),
			Uncertainty: round(uncertainty), Confidence: round(1 - uncertainty),
			Provenance: node.Provenance,
		})
	}
	attribution.Causes = a.classify(attribution)
	return attribution
}

// propagate splits a node's responsibility mass across its out-edges,
// preferring edges toward nodes whose own recorded signal agrees with the
// outcome. The path set makes a declared dependency cycle terminate rather
// than recurse forever.
func (a *attributor) propagate(id string, share, inherited float64, path map[string]bool) {
	if share < shareEpsilon || path[id] {
		return
	}
	node, ok := a.graph.Node(id)
	if !ok {
		return
	}
	uncertainty := combine(inherited, a.localUncertainty(node))
	a.share[id] += share
	a.weightedUncertainty[id] += share * uncertainty
	path[id] = true
	defer delete(path, id)

	edges := a.graph.OutEdges(id)
	weights := make([]float64, len(edges))
	total := 0.0
	for i, edge := range edges {
		weights[i] = a.edgeWeight(edge)
		total += weights[i]
	}
	if total == 0 {
		return
	}
	for i, edge := range edges {
		childUncertainty := uncertainty
		if edge.Provenance != ProvenanceRecorded {
			childUncertainty = combine(uncertainty, unknownEdgeUncertainty)
		}
		a.propagate(edge.To, share*weights[i]/total, childUncertainty, path)
	}
}

// subtreeUncertainty is how unsure the estimate of a node's own contribution
// is because of what the journal does not say about what happened below it: a
// node whose descendants are unrecorded or gap-bearing cannot have its share
// split confidently, so the uncertainty rises rather than the missing element
// being blamed.
func (a *attributor) subtreeUncertainty(id string) float64 {
	total, affected := 0, 0
	a.graph.Walk(id, func(node Node, depth int) bool {
		if depth == 0 {
			return true
		}
		total++
		if node.Provenance != ProvenanceRecorded || a.gapCount[node.ID] > 0 {
			affected++
		}
		return true
	})
	if total == 0 {
		return 0
	}
	return maxSubtreeUncertainty * float64(affected) / float64(total)
}

func (a *attributor) edgeWeight(edge Edge) float64 {
	child, ok := a.graph.Node(edge.To)
	if !ok {
		return neutralWeight
	}
	sign := localSign(child)
	switch {
	case sign == 0 || a.outcomeSign == 0:
		return neutralWeight
	case sign == a.outcomeSign:
		return agreeingWeight
	default:
		return opposingWeight
	}
}

func (a *attributor) localUncertainty(node Node) float64 {
	base := recordedUncertainty
	if node.Provenance != ProvenanceRecorded {
		base = unknownUncertainty
	}
	gaps := math.Min(float64(a.gapCount[node.ID])*gapUncertainty, maxGapUncertainty)
	return combine(base, gaps)
}

// signOf is the direction a node is estimated to have pushed the outcome: its
// own recorded signal when it has one, and otherwise the outcome's direction.
func (a *attributor) signOf(node Node) float64 {
	if sign := localSign(node); sign != 0 {
		return sign
	}
	return a.outcomeSign
}

func localSign(node Node) float64 {
	switch node.Kind {
	case KindOutcome, KindStage:
		return statusSign(node.Attributes["status"])
	case KindEvaluator:
		return verdictSign(node.Attributes["verdict"])
	case KindToolResult:
		switch node.Attributes["success"] {
		case "true":
			return 1
		case "false":
			return -1
		}
	}
	return 0
}

func statusSign(status string) float64 {
	switch status {
	case "completed", "success", "succeeded":
		return 1
	case "failed", "failure", "error", "aborted":
		return -1
	default:
		return 0
	}
}

func verdictSign(verdict string) float64 {
	switch verdict {
	case "pass", "approve", "approved", "success":
		return 1
	case "fail", "failure", "needs-changes", "reject", "rejected":
		return -1
	default:
		return 0
	}
}

func combine(first, second float64) float64 {
	return clamp(1 - (1-clamp(first))*(1-clamp(second)))
}

func clamp(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

func round(value float64) float64 {
	return math.Round(value*1e6) / 1e6
}

func roundConfidence(value float64) float64 {
	return math.Round(clamp(value)*1e3) / 1e3
}

// stageFacts is everything the classifier reads out of one failing stage's
// subtree. It is collected in graph order so classification is reproducible.
type stageFacts struct {
	node            Node
	failedResults   []string
	successResults  []string
	unresolvedCalls []string
	toolOfCall      map[string]string
	callOrder       []string
	callOfResult    map[string]string
	routingNodes    []string
	silentModels    []string
	toolCalls       int
	evidence        int
	subagents       int
	dependencyGaps  []string
	unknownNodes    int
	totalNodes      int
	gaps            int
	passingVerdicts []string
}

func (a *attributor) classify(attribution Attribution) []CauseFinding {
	var causes []CauseFinding
	failing := 0
	for _, node := range a.graph.Nodes {
		if node.Kind != KindStage || localSign(node) >= 0 {
			continue
		}
		failing++
		causes = append(causes, a.classifyStage(a.collect(node), attribution)...)
	}
	if failing == 0 && a.outcomeSign < 0 {
		root, _ := a.graph.Root()
		causes = append(causes, a.finding(ClassUnknown, root.ID, "", unknownCauseCeiling, attribution,
			"the run recorded a failing outcome but no stage recorded a failing status, so no cause is attributed",
			[]string{"a cause is attributed only to a stage the journal recorded as failing"}, nil))
	}
	return causes
}

func (a *attributor) collect(stage Node) stageFacts {
	facts := stageFacts{
		node: stage, toolOfCall: map[string]string{}, callOfResult: map[string]string{},
	}
	a.graph.Walk(stage.ID, func(node Node, depth int) bool {
		facts.gaps += a.gapCount[node.ID]
		if depth > 0 {
			facts.totalNodes++
			if node.Provenance != ProvenanceRecorded {
				facts.unknownNodes++
			}
		}
		switch node.Kind {
		case KindSubagent:
			facts.subagents++
			for _, edge := range a.graph.OutEdges(node.ID) {
				if edge.Kind == EdgeDependsOn {
					if target, ok := a.graph.Node(edge.To); ok && target.Provenance != ProvenanceRecorded {
						facts.dependencyGaps = append(facts.dependencyGaps, target.ID)
					}
				}
			}
			for _, gap := range a.graph.Gaps {
				if gap.NodeID == node.ID && strings.HasPrefix(gap.Reason, "declared dependency on subagent ") {
					facts.dependencyGaps = append(facts.dependencyGaps, node.ID)
				}
			}
		case KindModelInvocation:
			requested := node.Attributes["requestedModel"]
			resolved := node.Attributes["model"]
			if requested != "" && resolved != "" && requested != resolved {
				facts.routingNodes = append(facts.routingNodes, node.ID)
			}
			if node.Attributes["outputTokens"] == "0" {
				facts.silentModels = append(facts.silentModels, node.ID)
			}
		case KindToolCall:
			facts.toolCalls++
			facts.callOrder = append(facts.callOrder, node.ID)
			facts.toolOfCall[node.ID] = node.Attributes["tool"]
			produced := false
			for _, edge := range a.graph.OutEdges(node.ID) {
				if edge.Kind == EdgeProduces {
					produced = true
					facts.callOfResult[edge.To] = node.ID
				}
			}
			if !produced {
				facts.unresolvedCalls = append(facts.unresolvedCalls, node.ID)
			}
		case KindToolResult:
			switch node.Attributes["success"] {
			case "true":
				facts.successResults = append(facts.successResults, node.ID)
			case "false":
				facts.failedResults = append(facts.failedResults, node.ID)
			}
		case KindEvidence:
			facts.evidence++
		}
		return true
	})
	for _, node := range a.graph.NodesOfKind(KindEvaluator) {
		if verdictSign(node.Attributes["verdict"]) <= 0 {
			continue
		}
		for _, edge := range a.graph.OutEdges(node.ID) {
			if edge.Kind == EdgeEvaluates && edge.To == stage.ID {
				facts.passingVerdicts = append(facts.passingVerdicts, node.ID)
			}
		}
	}
	return facts
}

func (a *attributor) classifyStage(facts stageFacts, attribution Attribution) []CauseFinding {
	stage := facts.node.Label
	if facts.provenanceMissing() {
		return []CauseFinding{a.finding(ClassUnknown, facts.node.ID, stage, unknownCauseCeiling, attribution,
			"provenance is missing ("+strconv.Itoa(facts.unknownNodes)+" of "+strconv.Itoa(facts.totalNodes)+
				" nodes unrecorded, "+strconv.Itoa(facts.gaps)+" gap(s)), so no cause is attributed",
			[]string{"a stage whose recorded provenance is mostly absent gets lower confidence rather than a blamed component"},
			nil)}
	}

	var causes []CauseFinding
	if facts.node.Attributes["status"] == "error" {
		causes = append(causes, a.finding(ClassEnvironment, facts.node.ID, stage, directConfidence, attribution,
			"the stage recorded an error status, which the harness or environment raises rather than an agent decision",
			[]string{"an \"error\" status is treated as environment or harness failure, not as an agent decision"}, nil))
	}
	if len(facts.failedResults) > 0 {
		causes = append(causes, a.finding(ClassBadToolResult, facts.failedResults[0], stage, directConfidence, attribution,
			strconv.Itoa(len(facts.failedResults))+" tool result(s) in this stage recorded an unsuccessful outcome",
			nil, facts.failedResults))
	}
	if call, replacement, ok := facts.badChoice(); ok {
		causes = append(causes, a.finding(ClassBadToolChoice, call, stage, inferredConfidence, attribution,
			"tool "+facts.toolOfCall[call]+" failed and a later call to "+facts.toolOfCall[replacement]+
				" succeeded in the same stage",
			[]string{"a later successful call to a different tool is read as evidence the earlier choice was wrong; it is correlation, not a proven counterfactual"},
			[]string{call, replacement}))
	}
	if len(facts.failedResults) == 0 && len(facts.successResults) > 0 {
		causes = append(causes, a.finding(ClassBadInterpretation, facts.node.ID, stage, inferredConfidence, attribution,
			"every recorded tool result in this failing stage succeeded, so the failure follows what was made of them",
			[]string{"tool results that report success are assumed to have carried usable content"},
			facts.successResults))
	}
	if facts.subagents > 0 && facts.toolCalls == 0 && facts.evidence == 0 {
		causes = append(causes, a.finding(ClassWeakInstructions, facts.node.ID, stage, inferredConfidence, attribution,
			"the stage's subagents recorded no tool call and produced no evidence",
			[]string{"a stage that records no work at all is attributed to its prompt or instructions rather than to a component that never ran"},
			nil))
	}
	if len(facts.routingNodes) > 0 {
		causes = append(causes, a.finding(ClassRouting, facts.routingNodes[0], stage, structuralConfidence, attribution,
			"the model that ran is not the model the subagent requested",
			nil, facts.routingNodes))
	}
	if len(facts.silentModels) > 0 {
		causes = append(causes, a.finding(ClassModel, facts.silentModels[0], stage, structuralConfidence, attribution,
			"a model invocation in this stage recorded no output tokens",
			nil, facts.silentModels))
	}
	if len(facts.dependencyGaps) > 0 {
		causes = append(causes, a.finding(ClassTopology, facts.dependencyGaps[0], stage, structuralConfidence, attribution,
			"a subagent declared a dependency on a subagent that recorded no lifecycle of its own",
			[]string{"a declared dependency that never ran is read as a wiring problem, not as a fault of the depending agent"},
			facts.dependencyGaps))
	}
	if len(causes) == 0 {
		causes = append(causes, a.finding(ClassUnknown, facts.node.ID, stage, unknownCauseCeiling, attribution,
			"no recorded signal in this stage distinguishes a cause",
			[]string{"an unattributed failure stays unknown rather than being assigned to the nearest recorded component"},
			nil))
	}
	return a.adjust(causes, facts)
}

// provenanceMissing reports whether the stage's own record is absent, whether
// unrecorded nodes are the majority of its subtree, or whether the journal
// carries gaps where its work should be. Each is a reason to lower confidence
// and attribute nothing.
func (facts stageFacts) provenanceMissing() bool {
	switch {
	case facts.node.Provenance != ProvenanceRecorded, facts.totalNodes == 0:
		return true
	case facts.unknownNodes*unknownMajorityFactor > facts.totalNodes:
		return true
	default:
		return facts.gaps > 0 && facts.toolCalls == 0 && facts.evidence == 0
	}
}

// badChoice finds a failing tool call followed by a successful call to a
// different tool in the same stage.
func (facts stageFacts) badChoice() (string, string, bool) {
	failed := map[string]bool{}
	succeeded := map[string]bool{}
	for _, result := range facts.failedResults {
		failed[facts.callOfResult[result]] = true
	}
	for _, result := range facts.successResults {
		succeeded[facts.callOfResult[result]] = true
	}
	for position, call := range facts.callOrder {
		if !failed[call] {
			continue
		}
		for _, later := range facts.callOrder[position+1:] {
			if succeeded[later] && facts.toolOfCall[later] != facts.toolOfCall[call] {
				return call, later, true
			}
		}
	}
	return "", "", false
}

// adjust applies the evidence that is about the whole stage rather than about
// one rule: contradictory signals lower every finding's confidence, and a
// repeated attempt whose outcome differed raises it, because that repeat is a
// real intervention rather than a correlation.
func (a *attributor) adjust(causes []CauseFinding, facts stageFacts) []CauseFinding {
	var contradictions []string
	if len(facts.failedResults) > 0 && len(facts.successResults) > 0 {
		contradictions = append(contradictions, "contradictory evidence: "+strconv.Itoa(len(facts.successResults))+
			" tool result(s) in the same stage succeeded")
	}
	for _, evaluator := range facts.passingVerdicts {
		contradictions = append(contradictions, "contradictory evidence: evaluator "+evaluator+
			" returned a passing verdict for this failing stage")
	}
	intervention := a.intervention(facts.node)
	for index := range causes {
		if len(contradictions) > 0 {
			causes[index].Assumptions = append(causes[index].Assumptions, contradictions...)
			causes[index].Confidence = roundConfidence(causes[index].Confidence * contradictionPenalty)
		}
		if intervention != "" {
			causes[index].Evidence = append(causes[index].Evidence, intervention)
			causes[index].Confidence = roundConfidence(causes[index].Confidence +
				(1-causes[index].Confidence)*interventionRatio)
		}
	}
	return causes
}

// intervention reports a natural experiment: another attempt of the same stage
// whose recorded outcome differed from this one's.
func (a *attributor) intervention(stage Node) string {
	for _, other := range a.attempts[stage.Label] {
		if other.ID == stage.ID || localSign(other) <= 0 {
			continue
		}
		return "intervention: stage " + stage.Label + " attempt " + strconv.Itoa(stage.Attempt) +
			" failed and attempt " + strconv.Itoa(other.Attempt) + " succeeded"
	}
	return ""
}

func (a *attributor) finding(class FailureClass, nodeID, stage string, base float64, attribution Attribution, summary string, assumptions, evidence []string) CauseFinding {
	confidence := base
	if contribution, ok := attribution.Contribution(nodeID); ok {
		confidence *= contribution.Confidence
	}
	return CauseFinding{
		Class: class, NodeID: nodeID, Stage: stage, Confidence: roundConfidence(confidence),
		Summary: summary, Assumptions: append([]string(nil), assumptions...),
		Evidence: append([]string(nil), evidence...),
	}
}
