package workflowcluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// BehaviorFeatureVersion identifies the run-behavior (phenotype) descriptor
// schema. It is distinct from StructuralFeatureVersion so behavior and
// structure are never mixed in one fitted model. Bump it whenever a feature is
// added, removed, or computed differently.
const BehaviorFeatureVersion = "workflowcluster.behavior/v1"

// Errors that exclude a run from behavior descriptors.
var (
	// ErrRunNotTerminal reports a run that has not finished.
	ErrRunNotTerminal = errors.New("workflowcluster: run is not terminal")
	// ErrUndefinedProvenance reports a run without a defined EffectiveVersion:
	// no workflow digest, or agentic spans that disagree on model or harness.
	// Such runs are excluded from EffectiveVersion cohorts and so here too.
	ErrUndefinedProvenance = errors.New("workflowcluster: run has undefined EffectiveVersion provenance")
)

// nearMissFraction is the share of a budget at which a run counts as a near
// miss of it.
const nearMissFraction = 0.8

// visitTokens classify each stage start relative to the branch's earlier
// starts, independent of stage names: a first visit, an immediate repeat
// (retry), or a revisit of an earlier stage (loop).
var visitTokens = []string{"new", "retry", "loop"}

// childActions are the parent-side dispositions of a child workflow result.
var childActions = []string{"merge", "replace", "discard"}

// BehaviorInput is the journal data one descriptor is derived from. Spans are
// the decoded telemetry.SpanSchema records referenced by Events.
type BehaviorInput struct {
	Identity journal.RunIdentity
	Events   []journal.Event
	Spans    []telemetry.SpanRecord
}

// BehaviorLineage links an agent-authored child run to its parent run and
// retained proposal through the run's existing child provenance.
type BehaviorLineage struct {
	ParentRunID    string `json:"parentRunId"`
	ParentWorkflow string `json:"parentWorkflow"`
	SourceDigest   string `json:"sourceDigest"`
}

// BehaviorDescriptor is the phenotype of one terminal run. It holds only
// identifiers, digests, and numeric features: stage names, messages, goals,
// and other free text never contribute.
type BehaviorDescriptor struct {
	Version          string           `json:"version"`
	RunID            string           `json:"runId"`
	Gaggle           string           `json:"gaggle"`
	Workflow         string           `json:"workflow"`
	WorkflowDigest   string           `json:"workflowDigest"`
	EffectiveVersion string           `json:"effectiveVersion"`
	Outcome          journal.RunPhase `json:"outcome"`
	StartedAt        time.Time        `json:"startedAt"`
	Lineage          *BehaviorLineage `json:"lineage,omitempty"`
	// Provenance contains only axis-scoped identifiers recorded by the run.
	// Skills and tools remain absent until journals pin those scoped digests.
	Provenance map[string]string `json:"provenance,omitempty"`
	// Numeric holds named features. Every name is present for every run so the
	// schema is fixed within Version.
	Numeric map[string]float64 `json:"numeric"`
}

// ExtractBehavior derives a run's behavior descriptor. It is a pure function
// of in: the same journal data always yields a byte-identical descriptor.
func ExtractBehavior(in BehaviorInput) (BehaviorDescriptor, error) {
	id := in.Identity
	outcome := journal.PhaseFromEvents(in.Events)
	if !outcome.IsTerminal() {
		return BehaviorDescriptor{}, ErrRunNotTerminal
	}
	ev, err := behaviorEffectiveVersion(id, in.Spans)
	if err != nil {
		return BehaviorDescriptor{}, err
	}
	numeric := map[string]float64{}
	for _, phase := range []journal.RunPhase{journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated} {
		numeric["outcome."+string(phase)] = boolFeature(outcome == phase)
	}
	started, finished := runClock(in.Events)
	visitFeatures(numeric, in.Events)
	controlFeatures(numeric, in.Events)
	timeFeatures(numeric, id, in.Events, started, finished)
	usageFeatures(numeric, in.Spans)
	childFeatures(numeric, id, in.Events)

	d := BehaviorDescriptor{
		Version: BehaviorFeatureVersion, RunID: id.RunID, Gaggle: id.Gaggle, Workflow: id.Workflow,
		WorkflowDigest: id.WorkflowDigest, EffectiveVersion: ev, Outcome: outcome,
		StartedAt: started.UTC(), Numeric: numeric, Provenance: behaviorProvenance(in.Spans),
	}

	if c := id.Child; c != nil {
		d.Lineage = &BehaviorLineage{ParentRunID: c.ParentRunID, ParentWorkflow: c.ParentWorkflow, SourceDigest: c.SourceDigest}
	}
	return d, nil
}

func behaviorProvenance(spans []telemetry.SpanRecord) map[string]string {
	models, harnesses := map[string]bool{}, map[string]bool{}
	for _, span := range spans {
		if model := span.Attributes[telemetry.AttrModel]; model != "" {
			models[model] = true
		}
		if harness := span.Attributes[telemetry.AttrHarnessVersion]; harness != "" {
			harnesses[harness] = true
		}
	}
	out := map[string]string{}
	if len(models) == 1 {
		out[StratifyModel] = setLabel(models, false)
	}
	if len(harnesses) == 1 {
		out[StratifyHarness] = setLabel(harnesses, false)
	}
	return out
}

// behaviorEffectiveVersion mirrors the rollup cohort rule: a run needs a
// workflow digest, and its agentic spans must agree on model and harness.
func behaviorEffectiveVersion(id journal.RunIdentity, spans []telemetry.SpanRecord) (string, error) {
	if id.WorkflowDigest == "" {
		return "", ErrUndefinedProvenance
	}
	models, harnesses := map[string]bool{}, map[string]bool{}
	for _, span := range spans {
		if model := span.Attributes[telemetry.AttrModel]; model != "" {
			models[model] = true
		}
		if harness := span.Attributes[telemetry.AttrHarnessVersion]; harness != "" {
			harnesses[harness] = true
		}
	}
	if len(models) > 1 || len(harnesses) > 1 {
		return "", ErrUndefinedProvenance
	}
	v := rollup.EffectiveVersion{WorkflowDigest: id.WorkflowDigest, GooberDigest: id.GooberDigest}
	for model := range models {
		v.Model = model
	}
	for harness := range harnesses {
		v.HarnessVersion = harness
	}
	return v.Hash(), nil
}

// runClock returns the first run.started and last run.finished times.
func runClock(events []journal.Event) (time.Time, time.Time) {
	var started, finished time.Time
	for _, e := range events {
		switch e.Type {
		case journal.EventRunStarted:
			if started.IsZero() {
				started = e.Time
			}
		case journal.EventRunFinished:
			finished = e.Time
		}
	}
	if started.IsZero() && len(events) > 0 {
		started = events[0].Time
	}
	return started, finished
}

// visitFeatures counts stage visits and name-free visit bigrams per branch.
// Seq interleaving across branches is a scheduling artefact, so sequences are
// built within each branch only.
func visitFeatures(numeric map[string]float64, events []journal.Event) {
	prev := append([]string{"start"}, visitTokens...)
	for _, token := range visitTokens {
		numeric["visits."+token] = 0
	}
	for _, a := range prev {
		for _, b := range visitTokens {
			numeric["visits.ngram."+a+">"+b] = 0
		}
	}
	type branchState struct {
		last string
		seen map[string]bool
		prev string
	}
	branches := map[int]*branchState{}
	distinct := map[string]bool{}
	var visits, maxAttempt float64
	retries := map[journal.AttemptClass]float64{}
	for _, e := range events {
		if e.Type != journal.EventStageStarted {
			continue
		}
		visits++
		distinct[e.Stage] = true
		maxAttempt = max(maxAttempt, float64(e.Attempt))
		if e.Attempt > 1 {
			retries[e.AttemptClass]++
		}
		b := branches[e.Branch]
		if b == nil {
			b = &branchState{seen: map[string]bool{}, prev: "start"}
			branches[e.Branch] = b
		}
		token := "new"
		switch {
		case b.last == e.Stage:
			token = "retry"
		case b.seen[e.Stage]:
			token = "loop"
		}
		numeric["visits.ngram."+b.prev+">"+token]++
		numeric["visits."+token]++
		b.prev, b.last, b.seen[e.Stage] = token, e.Stage, true
	}
	numeric["visits.total"] = visits
	numeric["stages.distinct"] = float64(len(distinct))
	numeric["attempts.max"] = maxAttempt
	numeric["retries.policy"] = retries[journal.AttemptPolicy] + retries[""]
	numeric["retries.infra"] = retries[journal.AttemptInfra]
	numeric["retries.human"] = retries[journal.AttemptHuman]
}

// controlFeatures counts gates, human interventions, escalations, parallel
// fan-out, and errors.
func controlFeatures(numeric map[string]float64, events []journal.Event) {
	for _, name := range []string{
		"gates.evaluated", "gates.escalated", "gates.humanPaused", "gates.overridden",
		"interventions", "reruns", "parallel.visits", "parallel.maxFanOut", "branches.unsuccessful",
		"errors", "repass.max",
	} {
		numeric[name] = 0
	}
	for _, e := range events {
		numeric["repass.max"] = max(numeric["repass.max"], float64(e.RepassAttempt()))
		switch e.Type {
		case journal.EventGateEvaluated:
			numeric["gates.evaluated"]++
			if e.Escalated {
				numeric["gates.escalated"]++
			}
		case journal.EventGatePaused:
			numeric["gates.humanPaused"]++
		case journal.EventGateOverridden:
			numeric["gates.overridden"]++
		case journal.EventRunResumed:
			numeric["interventions"]++
		case journal.EventStageRerunRequested:
			numeric["reruns"]++
		case journal.EventParallelStarted:
			numeric["parallel.visits"]++
			numeric["parallel.maxFanOut"] = max(numeric["parallel.maxFanOut"], float64(len(e.Completeness)))
		case journal.EventMapStarted:
			numeric["parallel.visits"]++
			if e.MapRoster != nil {
				numeric["parallel.maxFanOut"] = max(numeric["parallel.maxFanOut"], float64(len(e.MapRoster.Items)))
			}
		case journal.EventBranchFinished:
			if e.BranchStatus != journal.BranchSucceeded && e.BranchStatus != journal.BranchSkipped {
				numeric["branches.unsuccessful"]++
			}
		case journal.EventError:
			numeric["errors"]++
		}
	}
}

// timeFeatures records log-scaled wall, child-wait, and active durations plus
// budget near misses against the run's pinned controls.
func timeFeatures(numeric map[string]float64, id journal.RunIdentity, events []journal.Event, started, finished time.Time) {
	var wall, wait time.Duration
	if !started.IsZero() && finished.After(started) {
		wall = finished.Sub(started)
		if intervals, err := journal.ChildWaitIntervals(events, started, finished, nil); err == nil {
			for _, interval := range intervals {
				wait += interval.To.Sub(interval.From)
			}
		}
	}
	active := wall - wait
	numeric["duration.log1pSeconds"] = math.Log1p(wall.Seconds())
	numeric["duration.activeLog1pSeconds"] = math.Log1p(active.Seconds())
	numeric["childWait.log1pSeconds"] = math.Log1p(wait.Seconds())

	numeric["nearMiss.timeout"], numeric["nearMiss.repass"], numeric["budget.exhausted"] = 0, 0, 0
	if controls := id.RunControls; controls != nil {
		if limit, err := time.ParseDuration(controls.MaxRunDuration); err == nil && limit > 0 {
			numeric["nearMiss.timeout"] = boolFeature(active.Seconds() >= nearMissFraction*limit.Seconds())
		}
		if controls.MaxRepasses > 0 {
			numeric["nearMiss.repass"] = boolFeature(numeric["repass.max"] >= nearMissFraction*float64(controls.MaxRepasses))
		}
	}
	if cause, err := journal.TerminalCauseFromEvents(events); err == nil && cause != nil {
		switch cause.Classification {
		case journal.TerminalRetryExhaustion, journal.TerminalPolicyExhaustion:
			numeric["budget.exhausted"] = 1
		}
	}
}

// usageFeatures sums token and cost usage over task and gate spans. Run spans
// are skipped so totals are not double counted.
func usageFeatures(numeric map[string]float64, spans []telemetry.SpanRecord) {
	var input, output, cost float64
	for _, span := range spans {
		if span.Kind != telemetry.SpanKindTask && span.Kind != telemetry.SpanKindGate {
			continue
		}
		input += spanNumber(span, telemetry.AttrGenAIUsageInputTokens)
		output += spanNumber(span, telemetry.AttrGenAIUsageOutputTokens)
		cost += spanNumber(span, telemetry.AttrUsageCostUSD)
	}
	numeric["usage.log1pInputTokens"] = math.Log1p(input)
	numeric["usage.log1pOutputTokens"] = math.Log1p(output)
	numeric["usage.log1pCostCents"] = math.Log1p(cost * 100)
}

func spanNumber(span telemetry.SpanRecord, attr string) float64 {
	value, err := strconv.ParseFloat(span.Attributes[attr], 64)
	if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}

// childFeatures marks agent-authored child runs and counts the children a
// parent spawned and how it disposed of their results.
func childFeatures(numeric map[string]float64, id journal.RunIdentity, events []journal.Event) {
	numeric["child.generated"] = boolFeature(id.Child != nil)
	children := map[string]bool{}
	for _, action := range childActions {
		numeric["children."+action] = 0
	}
	for _, e := range events {
		if e.Type != journal.EventRunnerAnnotation || e.Runner["kind"] != journal.ChildWaitKind {
			continue
		}
		request, ok := childWaitRequest(e)
		if !ok {
			continue
		}
		children[request.ChildRunID] = true
		if _, known := numeric["children."+request.Action]; known {
			numeric["children."+request.Action]++
		}
	}
	numeric["children.spawned"] = float64(len(children))
}

func childWaitRequest(e journal.Event) (journal.ChildHandoffRequest, bool) {
	data, err := json.Marshal(e.Runner["childWait"])
	if err != nil || len(data) > journal.MaxChildWaitBytes {
		return journal.ChildHandoffRequest{}, false
	}
	var header journal.ChildWaitHeader
	if json.Unmarshal(data, &header) != nil || header.Request.ChildRunID == "" {
		return journal.ChildHandoffRequest{}, false
	}
	return header.Request, true
}

func boolFeature(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Distribution summarizes one feature over a cohort's runs.
type Distribution struct {
	Mean     float64 `json:"mean"`
	P50      float64 `json:"p50"`
	P90      float64 `json:"p90"`
	Variance float64 `json:"variance"`
}

// BehaviorSummary aggregates the descriptors of one EffectiveVersion cohort
// of one workflow.
type BehaviorSummary struct {
	ID               string                  `json:"id"`
	Version          string                  `json:"version"`
	Gaggle           string                  `json:"gaggle"`
	Workflow         string                  `json:"workflow"`
	EffectiveVersion string                  `json:"effectiveVersion"`
	SampleCount      int                     `json:"sampleCount"`
	ChildRuns        int                     `json:"childRuns"`
	Provenance       map[string]string       `json:"provenance,omitempty"`
	Features         map[string]Distribution `json:"features"`
}

// SummarizeBehavior groups descriptors by gaggle, workflow, and
// EffectiveVersion and summarizes each feature's distribution. Cohorts with
// fewer than minSamples runs are dropped. The result is sorted by ID and
// independent of input order.
func SummarizeBehavior(descriptors []BehaviorDescriptor, minSamples int) []BehaviorSummary {
	groups := map[string][]BehaviorDescriptor{}
	for _, d := range descriptors {
		id := behaviorCohortID(d)
		groups[id] = append(groups[id], d)
	}
	summaries := make([]BehaviorSummary, 0, len(groups))
	for _, id := range sortedKeys(groups) {
		members := groups[id]
		if len(members) < max(1, minSamples) {
			continue
		}
		first := members[0]
		s := BehaviorSummary{
			ID: id, Version: BehaviorFeatureVersion, Gaggle: first.Gaggle, Workflow: first.Workflow,
			EffectiveVersion: first.EffectiveVersion, SampleCount: len(members), Features: map[string]Distribution{},
			Provenance: commonProvenance(members),
		}

		names := map[string]bool{}
		for _, d := range members {
			if d.Lineage != nil {
				s.ChildRuns++
			}
			for name := range d.Numeric {
				names[name] = true
			}
		}
		for _, name := range sortedKeys(names) {
			values := make([]float64, len(members))
			for i, d := range members {
				values[i] = d.Numeric[name]
			}
			s.Features[name] = distribution(values)
		}
		summaries = append(summaries, s)
	}
	return summaries
}

func commonProvenance(members []BehaviorDescriptor) map[string]string {
	out := map[string]string{}
	for _, axis := range provenanceAxes {
		value := members[0].Provenance[axis]
		if value == "" {
			continue
		}
		same := true
		for _, member := range members[1:] {
			same = same && member.Provenance[axis] == value
		}
		if same {
			out[axis] = value
		}
	}
	return out
}

func behaviorCohortID(d BehaviorDescriptor) string {
	ev := strings.TrimPrefix(d.EffectiveVersion, "sha256:")
	if len(ev) > 16 {
		ev = ev[:16]
	}
	return fmt.Sprintf("%s/%s@%s", d.Gaggle, d.Workflow, ev)
}

func distribution(values []float64) Distribution {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	mean := sum / float64(len(sorted))
	var squares float64
	for _, v := range sorted {
		squares += (v - mean) * (v - mean)
	}
	return Distribution{
		Mean: mean, P50: nearestRank(sorted, 0.5), P90: nearestRank(sorted, 0.9),
		Variance: squares / float64(len(sorted)),
	}
}

func nearestRank(sorted []float64, q float64) float64 {
	rank := int(math.Ceil(q * float64(len(sorted))))
	return sorted[min(max(rank, 1), len(sorted))-1]
}

// BehaviorPoint flattens a cohort summary into a Point keyed by its ID. Each
// feature contributes its mean and p90, so both typical behavior and tail
// behavior separate cohorts.
func BehaviorPoint(s BehaviorSummary) Point {
	numeric := make(map[string]float64, 2*len(s.Features))
	for name, dist := range s.Features {
		numeric[name+".mean"] = dist.Mean
		numeric[name+".p90"] = dist.P90
	}
	return Point{ID: s.ID, Numeric: numeric}
}
