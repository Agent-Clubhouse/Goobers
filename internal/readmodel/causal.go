package readmodel

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/workflow"
)

// CausalObservation is the journal-derived outcome for one node in one run.
// Outcome is a bad-outcome measure, where a positive effect means the
// intervention increased the bad outcome.
type CausalObservation struct {
	Node       string
	Outcome    float64
	Treated    bool
	Post       bool
	Randomized bool
	Arm        string
	Covariates map[string]string
}

// CausalOptions controls the identification guardrails.
type CausalOptions struct {
	MinCohortSize int
	Z             float64
	Gaggle        string
	Workflow      string
	Since         time.Time
	Until         time.Time
	WorkflowGraph *workflow.Graph
}

// CausalIdentification explains how an effect was identified.
type CausalIdentification string

// Supported causal identification strategies.
const (
	CausalRandomized              CausalIdentification = "randomized"
	CausalDifferenceInDifferences CausalIdentification = "observational-difference-in-differences"
	CausalUnidentifiable          CausalIdentification = "unidentifiable"
)

// CausalNodeCredit is a causal estimate for one node. An unidentifiable
// result is returned rather than omitted so callers cannot mistake missing
// evidence for a zero effect. Effect may retain a correlational pre/post
// contrast when PromotionSource is correlational-fallback, but that value is
// never eligible for automated promotion.
type CausalNodeCredit struct {
	Node              string               `json:"node"`
	Effect            float64              `json:"effect"`
	Lower             float64              `json:"lower"`
	Upper             float64              `json:"upper"`
	Identification    CausalIdentification `json:"identification"`
	Caveat            string               `json:"caveat"`
	TreatedBefore     int                  `json:"treatedBefore"`
	TreatedAfter      int                  `json:"treatedAfter"`
	ControlBefore     int                  `json:"controlBefore"`
	ControlAfter      int                  `json:"controlAfter"`
	IntervalAvailable bool                 `json:"intervalAvailable"`
	PromotionEligible bool                 `json:"promotionEligible"`
	PromotionSource   string               `json:"promotionSource"`
}

type causalRunFact struct {
	id, gaggle, workflow, target, verdict, workflowDigest string
	workflowVersion                                       int
	started                                               time.Time
	nodes                                                 map[string]causalNodeFact
	triggerKind, triggerRef                               string
	gooberDigest                                          string
}

type causalNodeFact struct {
	identity   string
	randomized bool
	arm        string
	parents    []string
	causalDAG  map[string]struct{} // DAG descendants of this node
}

// CausalCredit derives observations from the projected journal facts. A node
// identity change is the intervention. Runs routed through the node form the
// treated cohort before and after the change; contemporaneous runs that did
// not route through it form the control cohort. When a workflow always routes
// through the node, the pre/post contrast is retained only as an explicitly
// ineligible correlational fallback. The projection is read-only.
func (s *Store) CausalCredit(ctx context.Context, options CausalOptions) ([]CausalNodeCredit, error) {
	predicates := []string{"r.terminal = 1"}
	var args []any
	if options.Gaggle != "" {
		predicates = append(predicates, "r.gaggle = ?")
		args = append(args, options.Gaggle)
	}
	if options.Workflow != "" {
		predicates = append(predicates, "r.workflow = ?")
		args = append(args, options.Workflow)
	}
	if !options.Since.IsZero() {
		predicates = append(predicates, "r.started_at >= ?")
		args = append(args, formatTime(options.Since))
	}
	if !options.Until.IsZero() {
		predicates = append(predicates, "r.started_at <= ?")
		args = append(args, formatTime(options.Until))
	}
	query := `SELECT r.run_id, r.started_at, COALESCE(r.outcome_target, ''),
		COALESCE(r.outcome_verdict, ''), r.workflow_version,
		COALESCE(r.workflow_digest, ''), COALESCE(r.gaggle, ''),
		COALESCE(r.workflow, ''), COALESCE(r.trigger_kind, ''),
		COALESCE(r.trigger_ref, ''), COALESCE(r.goober_digest, ''),
		COALESCE(rn.kind, ''), COALESCE(rn.name, ''), COALESCE(rn.identity, ''),
		COALESCE(rn.randomized, 0), COALESCE(rn.arm, '')
		FROM run r LEFT JOIN run_node rn ON rn.run_id = r.run_id
		WHERE ` + strings.Join(predicates, " AND ") + `
		ORDER BY r.started_at ASC, r.run_id ASC`
	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("readmodel: causal projection: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var runs []*causalRunFact
	byRun := map[string]*causalRunFact{}
	for rows.Next() {
		var id, started, target, verdict, workflowDigest, gaggle, workflow, triggerKind, triggerRef, gooberDigest, kind, name, identity, arm string
		var randomized int
		var workflowVersion int
		if err := rows.Scan(&id, &started, &target, &verdict, &workflowVersion,
			&workflowDigest, &gaggle, &workflow, &triggerKind, &triggerRef, &gooberDigest, &kind, &name, &identity, &randomized, &arm); err != nil {
			return nil, fmt.Errorf("readmodel: scan causal projection: %w", err)
		}
		at, err := time.Parse(timeFormat, started)
		if err != nil {
			return nil, fmt.Errorf("readmodel: causal run time %q: %w", started, err)
		}
		run := byRun[id]
		if run == nil {
			run = &causalRunFact{
				id: id, gaggle: gaggle, workflow: workflow,
				started: at, target: target, verdict: verdict,
				workflowVersion: workflowVersion, workflowDigest: workflowDigest,
				triggerKind: triggerKind, triggerRef: triggerRef,
				gooberDigest: gooberDigest,
				nodes:        map[string]causalNodeFact{},
			}
			byRun[id] = run
			runs = append(runs, run)
		}
		if kind != "" && name != "" {
			node := kind + ":" + name
			run.nodes[node] = causalNodeFact{
				identity: identity, randomized: randomized != 0,
				arm: strings.ToLower(strings.TrimSpace(arm)),
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: causal projection rows: %w", err)
	}
	if err := s.loadNodeParents(ctx, runs, predicates, args); err != nil {
		return nil, err
	}

	// Construct the workflow DAG from parent-child relationships and use it
	// to reason about confounding. For each node, compute descendants and
	// confounders to guide identification strategy. Use the declared workflow
	// graph as the primary causal structure if available.
	if err := buildCausalDAG(runs, options.WorkflowGraph); err != nil {
		return nil, err
	}

	// A node's intervention signature includes both its own versioned identity
	// and the workflow definition that routed it. This captures prompt/tool
	// changes carried by the node as well as config/workflow changepoints.
	identities := map[string]map[string]struct{}{}
	for _, run := range runs {
		for node, fact := range run.nodes {
			signature := interventionSignature(run, fact.identity)
			if signature != "" {
				if identities[node] == nil {
					identities[node] = map[string]struct{}{}
				}
				identities[node][signature] = struct{}{}
			}
		}
	}
	var observations []CausalObservation
	var unavailable []CausalNodeCredit
	allNodes := map[string]struct{}{}
	for _, run := range runs {
		for node := range run.nodes {
			allNodes[node] = struct{}{}
		}
	}
	for node, versions := range identities {
		nodeHasRandomized := false
		for _, run := range runs {
			if fact, ok := run.nodes[node]; ok && fact.randomized {
				nodeHasRandomized = true
				break
			}
		}
		if len(versions) < 2 {
			if nodeHasRandomized {
				for _, run := range runs {
					fact, routed := run.nodes[node]
					if !routed || !fact.randomized {
						continue
					}
					bad := run.target == "@abort" || strings.EqualFold(run.verdict, "fail") ||
						strings.EqualFold(run.verdict, "failure") || strings.EqualFold(run.verdict, "reject")
					observations = append(observations, CausalObservation{
						Node:       node,
						Outcome:    boolFloat(bad),
						Treated:    strings.EqualFold(fact.arm, "treatment"),
						Post:       true,
						Randomized: true,
						Arm:        fact.arm,
						Covariates: observationCovariates(run, node),
					})
				}
				continue
			}
			unavailable = append(unavailable, cannotIdentify(node,
				"cannot identify: no versioned node changepoint is recorded"))
			continue
		}
		var cutoff time.Time
		var previous, next string
		for _, run := range runs {
			fact, routed := run.nodes[node]
			if !routed {
				continue
			}
			signature := interventionSignature(run, fact.identity)
			if signature != "" && previous != "" && signature != previous {
				cutoff = run.started
				next = signature
				break
			}
			if signature != "" {
				previous = signature
			}
		}
		if cutoff.IsZero() || previous == "" || next == "" {
			unavailable = append(unavailable, cannotIdentify(node,
				"cannot identify: no ordered versioned node changepoint is recorded"))
			continue
		}
		for _, run := range runs {
			fact, routed := run.nodes[node]
			post := !run.started.Before(cutoff)
			if routed {
				signature := interventionSignature(run, fact.identity)
				// Exclude mixed-rollout observations from the wrong side of
				// the selected changepoint. Routed runs are the treated
				// cohort; runs that did not route through the node are the
				// contemporaneous control cohort.
				if (!post && signature != previous) || (post && signature != next) {
					continue
				}
			}
			bad := run.target == "@abort" || strings.EqualFold(run.verdict, "fail") ||
				strings.EqualFold(run.verdict, "failure") || strings.EqualFold(run.verdict, "reject")
			observations = append(observations, CausalObservation{
				Node: node, Outcome: boolFloat(bad), Treated: routed,
				Post:       post,
				Randomized: routed && fact.randomized,
				Arm:        fact.arm,
				Covariates: observationCovariates(run, node),
			})
		}
	}

	for node := range allNodes {
		if _, ok := identities[node]; !ok {
			unavailable = append(unavailable, cannotIdentify(node,
				"cannot identify: node has no versioned identity"))
		}
	}
	estimated, err := EstimateCausalCredit(observations, options)
	if err != nil {
		return nil, err
	}
	result := append(unavailable, estimated...)
	sort.Slice(result, func(i, j int) bool { return result[i].Node < result[j].Node })
	return result, nil
}

func (s *Store) loadNodeParents(ctx context.Context, runs []*causalRunFact, predicates []string, args []any) error {
	if len(runs) == 0 {
		return nil
	}
	byRun := make(map[string]*causalRunFact, len(runs))
	for _, run := range runs {
		byRun[run.id] = run
	}
	query := `SELECT p.run_id, p.kind, p.name, p.identity, p.parent_kind, p.parent_name
		FROM run_node_parent p JOIN run r ON r.run_id = p.run_id
		WHERE ` + strings.Join(predicates, " AND ")
	db, release, err := s.readHandle()
	if err != nil {
		return err
	}
	defer release()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("readmodel: causal parent projection: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var runID, kind, name, identity, parentKind, parentName string
		if err := rows.Scan(&runID, &kind, &name, &identity, &parentKind, &parentName); err != nil {
			return fmt.Errorf("readmodel: scan causal parent projection: %w", err)
		}
		run := byRun[runID]
		if run == nil {
			continue
		}
		nodeKey := kind + ":" + name
		fact, ok := run.nodes[nodeKey]
		if !ok || fact.identity != identity {
			continue
		}
		parent := parentKind + ":" + parentName
		if !contains(fact.parents, parent) {
			fact.parents = append(fact.parents, parent)
			sort.Strings(fact.parents)
			run.nodes[nodeKey] = fact
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("readmodel: causal parent projection rows: %w", err)
	}
	return nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// buildCausalDAG constructs the workflow DAG using the declared workflow graph
// as the primary causal structure, and falls back to parent-child relationships
// from the journal where the graph is unavailable. For each node, we compute
// its descendants in the DAG to understand what nodes it can confound.
func buildCausalDAG(runs []*causalRunFact, graph *workflow.Graph) error {
	// Build a reverse index from each run: child -> parents
	// Start with the declared workflow graph if available
	declaredParents := make(map[string]map[string]struct{})
	if graph != nil {
		// Map graph node IDs to their types as defined in the workflow
		kindByID := map[string]string{}
		for _, node := range graph.Nodes {
			switch node.Kind {
			case workflow.GraphNodeAgentic, workflow.GraphNodeDeterministic:
				kindByID[node.ID] = "stage"
			case workflow.GraphNodeGate:
				kindByID[node.ID] = "gate"
			}
		}

		// Extract edges from the declared workflow graph
		for _, edge := range graph.Edges {
			if edge.Terminal != "" {
				continue
			}
			childKind, childOK := kindByID[edge.Target]
			parentKind, parentOK := kindByID[edge.Source]
			if !childOK || !parentOK {
				continue
			}
			childKey := childKind + ":" + edge.Target
			parentKey := parentKind + ":" + edge.Source

			if declaredParents[childKey] == nil {
				declaredParents[childKey] = make(map[string]struct{})
			}
			declaredParents[childKey][parentKey] = struct{}{}
		}
	}

	// Build a reverse index from each run: child -> parents
	// Then use it to compute descendants (nodes that can be confounded by parents)
	for _, run := range runs {
		// Start with declared parents and add run-observed parents
		parents := make(map[string]map[string]struct{})
		for key, parentSet := range declaredParents {
			if parents[key] == nil {
				parents[key] = make(map[string]struct{})
			}
			for p := range parentSet {
				parents[key][p] = struct{}{}
			}
		}

		// Also include parents from run_node_parent table for this run
		// (in case they differ from the declared graph)
		for node, fact := range run.nodes {
			if parents[node] == nil {
				parents[node] = make(map[string]struct{})
			}
			for _, parent := range fact.parents {
				parents[node][parent] = struct{}{}
			}
		}

		// For each node, compute all nodes that are its descendants
		// (nodes it can confound through data/control flow)
		descendants := make(map[string]map[string]struct{})
		for node := range run.nodes {
			descendants[node] = make(map[string]struct{})
			// A node's immediate descendants are any nodes whose parents include this node
			for otherNode := range run.nodes {
				for parentKey := range parents[otherNode] {
					if parentKey == node {
						descendants[node][otherNode] = struct{}{}
						break
					}
				}
			}
		}
		// Store the DAG structure in each node
		for node := range run.nodes {
			fact := run.nodes[node]
			fact.causalDAG = descendants[node]
			run.nodes[node] = fact
		}
	}
	return nil
}

func observationCovariates(run *causalRunFact, node string) map[string]string {
	covariates := map[string]string{
		"gaggle":   run.gaggle,
		"workflow": run.workflow,
	}
	// Include trigger information as confounders for workload/repo
	if run.triggerKind != "" {
		covariates["trigger_kind"] = run.triggerKind
	}
	if run.triggerRef != "" {
		covariates["trigger_ref"] = run.triggerRef
	}
	// Include goober digest as a covariate to capture goober version/configuration changes
	if run.gooberDigest != "" {
		covariates["goober_digest"] = run.gooberDigest
	}
	fact, ok := run.nodes[node]
	if !ok {
		return covariates
	}
	for _, parent := range fact.parents {
		parentFact, parentOK := run.nodes[parent]
		signature := "<missing>"
		if parentOK {
			signature = interventionSignature(run, parentFact.identity)
		}
		covariates["parent:"+parent] = signature
	}
	return covariates
}

func interventionSignature(run *causalRunFact, identity string) string {
	if identity == "" && run.workflowVersion == 0 && run.workflowDigest == "" {
		return ""
	}
	return fmt.Sprintf("%s|workflow:%d:%s", identity, run.workflowVersion, run.workflowDigest)
}

func cannotIdentify(node, caveat string) CausalNodeCredit {
	return CausalNodeCredit{
		Node: node, Identification: CausalUnidentifiable, Caveat: caveat,
		PromotionSource: "correlational-fallback",
	}
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

const (
	defaultCausalMinCohort = 10
	defaultCausalZ         = 1.96
)

// EstimateCausalCredit projects causal effects from journal observations.
//
// Randomized observations use a direct treatment/control arm comparison.
// Other observations use difference-in-differences, provided every cell has
// enough observations and the treated and control cohorts overlap on all
// supplied covariates. No observational estimate is emitted as causal when
// those identification conditions are not met.
func EstimateCausalCredit(observations []CausalObservation, options CausalOptions) ([]CausalNodeCredit, error) {
	minCohort := options.MinCohortSize
	if minCohort <= 0 {
		minCohort = defaultCausalMinCohort
	}
	z := options.Z
	if z <= 0 {
		z = defaultCausalZ
	}
	if math.IsNaN(z) || math.IsInf(z, 0) {
		return nil, fmt.Errorf("readmodel: causal confidence multiplier must be finite")
	}

	grouped := make(map[string][]CausalObservation)
	for _, observation := range observations {
		if observation.Node == "" {
			return nil, fmt.Errorf("readmodel: causal observation has no node")
		}
		if math.IsNaN(observation.Outcome) || math.IsInf(observation.Outcome, 0) {
			return nil, fmt.Errorf("readmodel: causal observation for %q has non-finite outcome", observation.Node)
		}
		grouped[observation.Node] = append(grouped[observation.Node], observation)
	}
	nodes := make([]string, 0, len(grouped))
	for node := range grouped {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	result := make([]CausalNodeCredit, 0, len(nodes))
	for _, node := range nodes {
		estimate := estimateNode(grouped[node], minCohort, z)
		result = append(result, estimate)
	}
	return result, nil
}

func estimateNode(observations []CausalObservation, minCohort int, z float64) CausalNodeCredit {
	result := CausalNodeCredit{
		Node: observations[0].Node, PromotionSource: "correlational-fallback",
	}
	for _, observation := range observations {
		if observation.Treated {
			if observation.Post {
				result.TreatedAfter++
			} else {
				result.TreatedBefore++
			}
		} else if observation.Post {
			result.ControlAfter++
		} else {
			result.ControlBefore++
		}
	}

	for _, observation := range observations {
		if observation.Randomized {
			return randomizedEstimate(observations, minCohort, z, result)
		}
	}
	if result.TreatedBefore >= minCohort && result.TreatedAfter >= minCohort &&
		result.ControlBefore == 0 && result.ControlAfter == 0 {
		before := cell(observations, true, false)
		after := cell(observations, true, true)
		effect := mean(after) - mean(before)
		se := standardError(before, after)
		result.Effect, result.Lower, result.Upper = effect, effect-z*se, effect+z*se
		result.Identification = CausalUnidentifiable
		result.Caveat = "cannot identify: changepoint has no contemporaneous control cohort; correlational pre/post contrast retained"
		return result
	}
	if !hasCovariates(observations) {
		result.Identification = CausalUnidentifiable
		result.Caveat = "cannot identify: observational estimates require recorded covariates"
		return result
	}
	if !covariatesOverlap(observations) {
		result.Identification = CausalUnidentifiable
		result.Caveat = "cannot identify: treated and control cohorts do not overlap on recorded covariates"
		return result
	}
	if result.TreatedBefore < minCohort || result.TreatedAfter < minCohort ||
		result.ControlBefore < minCohort || result.ControlAfter < minCohort {
		result.Identification = CausalUnidentifiable
		result.Caveat = "cannot identify: each pre/post treated and control cohort must meet the minimum size"
		return result
	}

	treatmentPre, treatmentPost := cell(observations, true, false), cell(observations, true, true)
	controlPre, controlPost := cell(observations, false, false), cell(observations, false, true)
	effect := mean(treatmentPost) - mean(treatmentPre) - mean(controlPost) + mean(controlPre)
	se := standardError(treatmentPre, treatmentPost, controlPre, controlPost)
	result.Effect, result.Lower, result.Upper = effect, effect-z*se, effect+z*se
	result.IntervalAvailable = true
	result.Identification = CausalDifferenceInDifferences
	result.PromotionEligible = true
	result.PromotionSource = string(result.Identification)
	result.Caveat = "observational difference-in-differences; assumes parallel trends and no unrecorded confounding"
	return result
}

func hasCovariates(observations []CausalObservation) bool {
	for _, observation := range observations {
		if len(observation.Covariates) > 0 {
			return true
		}
	}
	return false
}

func randomizedEstimate(observations []CausalObservation, minCohort int, z float64, result CausalNodeCredit) CausalNodeCredit {
	var treatment, control []float64
	for _, observation := range observations {
		if !observation.Randomized || !observation.Post {
			continue
		}
		if observation.Arm == "treatment" || observation.Treated {
			treatment = append(treatment, observation.Outcome)
		} else if observation.Arm == "control" {
			control = append(control, observation.Outcome)
		}
	}
	if len(treatment) < minCohort || len(control) < minCohort {
		result.Identification = CausalUnidentifiable
		result.Caveat = "cannot identify: randomized treatment and control arms must meet the minimum size"
		return result
	}
	effect := mean(treatment) - mean(control)
	se := standardError(treatment, control)
	result.Effect, result.Lower, result.Upper = effect, effect-z*se, effect+z*se
	result.IntervalAvailable = true
	result.Identification = CausalRandomized
	result.PromotionEligible = true
	result.PromotionSource = string(result.Identification)
	result.Caveat = "randomized arm comparison; estimate is conditional on the recorded intervention and outcome"
	return result
}

func cell(observations []CausalObservation, treated, post bool) []float64 {
	values := make([]float64, 0)
	for _, observation := range observations {
		if observation.Treated == treated && observation.Post == post {
			values = append(values, observation.Outcome)
		}
	}
	return values
}

func mean(values []float64) float64 {
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func standardError(groups ...[]float64) float64 {
	var se float64
	for _, group := range groups {
		if len(group) >= 2 {
			average := mean(group)
			var groupVariance float64
			for _, value := range group {
				delta := value - average
				groupVariance += delta * delta
			}
			se += groupVariance / float64(len(group)*(len(group)-1))
		}
	}
	return math.Sqrt(se)
}

func covariatesOverlap(observations []CausalObservation) bool {
	for key := range allCovariates(observations) {
		treated, control := map[string]bool{}, map[string]bool{}
		for _, observation := range observations {
			value, ok := observation.Covariates[key]
			if !ok {
				// A missing value is not evidence of overlap. Treating it as
				// absent would allow a covariate observed in only one cohort
				// to pass the guard and turn confounding into a causal claim.
				return false
			}
			if observation.Treated {
				treated[value] = true
			} else {
				control[value] = true
			}
		}
		for value := range treated {
			if !control[value] {
				return false
			}
		}
		for value := range control {
			if !treated[value] {
				return false
			}
		}
	}
	return true
}

func allCovariates(observations []CausalObservation) map[string]struct{} {
	keys := map[string]struct{}{}
	for _, observation := range observations {
		for key := range observation.Covariates {
			keys[key] = struct{}{}
		}
	}
	return keys
}
