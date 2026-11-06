package workflowcluster

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/telemetry"
)

const (
	TrendAxisModel   = "model"
	TrendAxisHarness = "harness"
	// TrendAxisAuthoringSkill groups by the authoring goober's skill and
	// instruction version. Journals do not yet record a digest scoped to that
	// goober: RunIdentity.GooberDigest covers every participating goober plus
	// model and harness, so it would misattribute model, harness, or unrelated
	// goober edits as skill changes. Scanned observations therefore leave this
	// axis empty and report every sample under MissingProvenance.
	TrendAxisAuthoringSkill = "authoring-skill"
	trendNoiseCluster       = "noise"
	// DefaultTrendMaxRuns caps the run journals a trend scan reads.
	DefaultTrendMaxRuns = DefaultBehaviorMaxRuns
)

type TrendObservation struct {
	ID       string
	At       time.Time
	Point    Point
	Fitness  float64
	Versions map[string]string
}

type ClusterOccupancy struct {
	Cluster     string  `json:"cluster"`
	Samples     int     `json:"samples"`
	Share       float64 `json:"share"`
	MeanFitness float64 `json:"meanFitness"`
}

type TrendDrift struct {
	From                  string  `json:"from"`
	OccupancyDistance     float64 `json:"occupancyDistance"`
	EntropyDelta          float64 `json:"entropyDelta"`
	PairwiseDistanceDelta float64 `json:"pairwiseDistanceDelta"`
	FitnessDelta          float64 `json:"fitnessDelta"`
	Signal                string  `json:"signal"`
}

type TrendCohort struct {
	Version              string             `json:"version"`
	FirstObserved        time.Time          `json:"firstObserved"`
	Samples              int                `json:"samples"`
	Occupancy            []ClusterOccupancy `json:"occupancy"`
	Entropy              float64            `json:"entropy"`
	MeanPairwiseDistance float64            `json:"meanPairwiseDistance"`
	MeanFitness          float64            `json:"meanFitness"`
	Drift                *TrendDrift        `json:"drift,omitempty"`
}

type TrendAxis struct {
	Axis    string        `json:"axis"`
	Cohorts []TrendCohort `json:"cohorts"`
	// MissingProvenance counts samples with no single recorded version on this
	// axis; they are in no cohort.
	MissingProvenance int `json:"missingProvenance"`
}

type TrendReport struct {
	ModelID        string `json:"modelId"`
	FeatureVersion string `json:"featureVersion"`
	Samples        int    `json:"samples"`
	Excluded       int    `json:"excluded"`
	// Candidates is the number of run journals found; OverLimit of them were
	// not read because the scan's run cap was reached.
	Candidates int         `json:"candidates"`
	OverLimit  int         `json:"overLimit"`
	Axes       []TrendAxis `json:"axes"`
}

// AnalyzeTrends assigns child-workflow shapes to one frozen model and compares
// chronological model, harness, and authoring-skill cohorts. Signals are
// observational changes, not selection or null-baseline claims.
func AnalyzeTrends(model Model, observations []TrendObservation) (TrendReport, error) {
	observations = append([]TrendObservation(nil), observations...)
	sort.Slice(observations, func(i, j int) bool { return observations[i].ID < observations[j].ID })
	report := TrendReport{ModelID: model.ID, FeatureVersion: model.FeatureVersion, Samples: len(observations)}
	assigned := make(map[string]string, len(observations))
	for _, observation := range observations {
		if observation.ID == "" {
			return TrendReport{}, fmt.Errorf("workflowcluster: trend observation id is required")
		}
		if _, exists := assigned[observation.ID]; exists {
			return TrendReport{}, fmt.Errorf("workflowcluster: duplicate trend observation id %q", observation.ID)
		}
		if math.IsNaN(observation.Fitness) || math.IsInf(observation.Fitness, 0) || observation.Fitness < 0 || observation.Fitness > 1 {
			return TrendReport{}, fmt.Errorf("workflowcluster: trend observation %q fitness must be in [0, 1]", observation.ID)
		}
		assignment, err := model.Assign(StructuralFeatureVersion, observation.Point)
		if err != nil {
			return TrendReport{}, err
		}
		cluster := assignment.ClusterID
		if cluster == "" {
			cluster = trendNoiseCluster
		}
		assigned[observation.ID] = cluster
	}
	for _, axis := range []string{TrendAxisModel, TrendAxisHarness, TrendAxisAuthoringSkill} {
		groups := map[string][]TrendObservation{}
		missing := 0
		for _, observation := range observations {
			if version := observation.Versions[axis]; version != "" {
				groups[version] = append(groups[version], observation)
			} else {
				missing++
			}
		}
		cohorts := make([]TrendCohort, 0, len(groups))
		for version, members := range groups {
			cohorts = append(cohorts, summarizeTrendCohort(model.Schema, version, members, assigned))
		}
		sort.Slice(cohorts, func(i, j int) bool {
			if !cohorts[i].FirstObserved.Equal(cohorts[j].FirstObserved) {
				return cohorts[i].FirstObserved.Before(cohorts[j].FirstObserved)
			}
			return cohorts[i].Version < cohorts[j].Version
		})
		for i := 1; i < len(cohorts); i++ {
			cohorts[i].Drift = compareTrendCohorts(cohorts[i-1], cohorts[i])
		}
		report.Axes = append(report.Axes, TrendAxis{Axis: axis, Cohorts: cohorts, MissingProvenance: missing})
	}
	return report, nil
}

func summarizeTrendCohort(schema Schema, version string, members []TrendObservation, assigned map[string]string) TrendCohort {
	cohort := TrendCohort{Version: version, FirstObserved: members[0].At, Samples: len(members)}
	type aggregate struct {
		count   int
		fitness float64
	}
	occupancy := map[string]aggregate{}
	for _, member := range members {
		if member.At.Before(cohort.FirstObserved) {
			cohort.FirstObserved = member.At
		}
		cohort.MeanFitness += member.Fitness
		value := occupancy[assigned[member.ID]]
		value.count++
		value.fitness += member.Fitness
		occupancy[assigned[member.ID]] = value
	}
	cohort.MeanFitness /= float64(len(members))
	for cluster, value := range occupancy {
		share := float64(value.count) / float64(len(members))
		cohort.Entropy -= share * math.Log2(share)
		cohort.Occupancy = append(cohort.Occupancy, ClusterOccupancy{
			Cluster: cluster, Samples: value.count, Share: share, MeanFitness: value.fitness / float64(value.count),
		})
	}
	sort.Slice(cohort.Occupancy, func(i, j int) bool { return cohort.Occupancy[i].Cluster < cohort.Occupancy[j].Cluster })
	var pairs int
	for i := range members {
		for j := i + 1; j < len(members); j++ {
			cohort.MeanPairwiseDistance += schema.Distance(members[i].Point, members[j].Point)
			pairs++
		}
	}
	if pairs > 0 {
		cohort.MeanPairwiseDistance /= float64(pairs)
	}
	return cohort
}

func compareTrendCohorts(previous, current TrendCohort) *TrendDrift {
	drift := &TrendDrift{
		From: previous.Version, EntropyDelta: current.Entropy - previous.Entropy,
		PairwiseDistanceDelta: current.MeanPairwiseDistance - previous.MeanPairwiseDistance,
		FitnessDelta:          current.MeanFitness - previous.MeanFitness, Signal: "mixed",
	}
	shares := func(cohort TrendCohort) map[string]float64 {
		result := map[string]float64{}
		for _, cluster := range cohort.Occupancy {
			result[cluster.Cluster] = cluster.Share
		}
		return result
	}
	before, after := shares(previous), shares(current)
	keys := map[string]bool{}
	for key := range before {
		keys[key] = true
	}
	for key := range after {
		keys[key] = true
	}
	for key := range keys {
		drift.OccupancyDistance += math.Abs(before[key] - after[key])
	}
	drift.OccupancyDistance /= 2
	const epsilon = 1e-9
	switch {
	case drift.EntropyDelta < -epsilon && drift.PairwiseDistanceDelta < -epsilon && drift.FitnessDelta <= epsilon:
		drift.Signal = "mode-collapse"
	case drift.EntropyDelta < -epsilon && drift.PairwiseDistanceDelta < -epsilon:
		drift.Signal = "converging"
	case drift.EntropyDelta > epsilon && drift.PairwiseDistanceDelta > epsilon:
		drift.Signal = "diverging"
	case math.Abs(drift.EntropyDelta) <= epsilon && math.Abs(drift.PairwiseDistanceDelta) <= epsilon:
		drift.Signal = "stable"
	}
	return drift
}

// TrendScanOptions bounds a child-trend scan.
type TrendScanOptions struct {
	// MaxRuns caps the run journals read, newest first. Parents of the read
	// children are loaded at most once each in addition. Default 2000.
	MaxRuns int
}

// ScanChildTrends reconstructs agent-authored child shapes and their authoring
// provenance from retained journals. Individual unreadable or incomplete runs
// are counted as excluded rather than weakening a cohort with partial data.
// The scan reads at most opts.MaxRuns journals, newest first, and each parent
// journal once.
func ScanChildTrends(ctx context.Context, root string, model Model, opts TrendScanOptions) (TrendReport, error) {
	if opts.MaxRuns <= 0 {
		opts.MaxRuns = DefaultTrendMaxRuns
	}
	candidates, err := listRunCandidates(ctx, root)
	if err != nil {
		return TrendReport{}, fmt.Errorf("list trend journals: %w", err)
	}
	dirsByName := map[string][]string{}
	for _, candidate := range candidates {
		name := filepath.Base(candidate.dir)
		dirsByName[name] = append(dirsByName[name], candidate.dir)
	}
	scan := trendScan{dirsByName: dirsByName, parents: map[string]*trendParent{}, limits: BehaviorScanOptions{}.withDefaults()}
	var observations []TrendObservation
	seen := map[string]bool{}
	excluded, overLimit := 0, 0
	for i, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return TrendReport{}, err
		}
		if i >= opts.MaxRuns {
			overLimit = len(candidates) - i
			break
		}
		observation, child, err := scan.observe(candidate.dir)
		switch {
		case !child || seen[observation.ID]:
		case err != nil:
			excluded++
		default:
			observations = append(observations, observation)
		}
		if child && observation.ID != "" {
			seen[observation.ID] = true
		}
	}
	report, err := AnalyzeTrends(model, observations)
	if err != nil {
		return TrendReport{}, err
	}
	report.Excluded, report.Candidates, report.OverLimit = excluded, len(candidates), overLimit
	return report, nil
}

type trendParent struct {
	input BehaviorInput
	err   error
}

type trendScan struct {
	dirsByName map[string][]string
	parents    map[string]*trendParent
	limits     BehaviorScanOptions
}

// observe reads one candidate journal. child reports whether it is an
// agent-authored child run; err excludes a child whose shape or provenance
// cannot be reconstructed.
func (s *trendScan) observe(dir string) (TrendObservation, bool, error) {
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return TrendObservation{}, false, err
	}
	identity, err := reader.Identity()
	if err != nil || identity.Child == nil {
		return TrendObservation{}, false, err
	}
	observation := TrendObservation{ID: identity.RunID}
	if info, err := os.Stat(filepath.Join(dir, "events.jsonl")); err == nil && info.Size() > s.limits.MaxEventBytes {
		return observation, true, errBehaviorOversized
	}
	events, err := reader.Events()
	if err != nil {
		return observation, true, err
	}
	phase := journal.PhaseFromEvents(events)
	if !phase.IsTerminal() {
		return observation, true, ErrRunNotTerminal
	}
	machine, err := runsemantics.PinnedWorkflowMachine(reader, identity)
	if err != nil {
		return observation, true, err
	}
	features, err := ExtractDefinition(machine.Def)
	if err != nil {
		return observation, true, err
	}
	parent, err := s.parent(identity.Child)
	if err != nil {
		return observation, true, err
	}
	modelVersion, harnessVersion := authoringVersions(parent, identity.Child.StageOccurrence)
	observation.At = identity.StartedAt
	if observation.At.IsZero() {
		observation.At, _ = runClock(events)
	}
	observation.Point = StructuralPoint(identity.RunID, features)
	if phase == journal.PhaseCompleted {
		observation.Fitness = 1
	}
	// The authoring-skill axis is deliberately unset; see TrendAxisAuthoringSkill.
	observation.Versions = map[string]string{TrendAxisModel: modelVersion, TrendAxisHarness: harnessVersion}
	return observation, true, nil
}

// parent loads the authoring parent's journal once per scan.
func (s *trendScan) parent(lineage *journal.ChildLineage) (BehaviorInput, error) {
	if cached, ok := s.parents[lineage.ParentRunID]; ok {
		return cached.input, cached.err
	}
	loaded := &trendParent{err: fmt.Errorf("workflowcluster: parent run %q is not retained", lineage.ParentRunID)}
	for _, dir := range s.dirsByName[lineage.ParentRunID] {
		input, err := readBehaviorInput(dir, s.limits)
		if err != nil {
			loaded.err = err
			continue
		}
		if input.Identity.RunID == lineage.ParentRunID && input.Identity.Gaggle == lineage.Gaggle {
			loaded = &trendParent{input: input}
			break
		}
	}
	s.parents[lineage.ParentRunID] = loaded
	return loaded.input, loaded.err
}

// authoringVersions returns the model and harness of the parent task spans
// recorded for the stage occurrence that authored the child, or "" when that
// occurrence recorded none or more than one. Spans carry stage, branch, and
// attempt but not the occurrence, so a span whose key repeats across loop
// visits is attributed to the start nearest its own start time.
func authoringVersions(parent BehaviorInput, occurrence string) (string, string) {
	type startKey struct {
		stage           string
		branch, attempt int
	}
	type stageStart struct {
		at        time.Time
		authoring bool
	}
	type branchStage struct {
		stage  string
		branch int
	}
	starts := map[startKey][]stageStart{}
	prior := map[branchStage]string{}
	for _, event := range parent.Events {
		if event.Type != journal.EventStageStarted {
			continue
		}
		key := branchStage{event.Stage, event.Branch}
		prior[key] = journal.StageOccurrence(parent.Identity.RunID, event, prior[key])
		start := startKey{event.Stage, event.Branch, max(event.Attempt, 1)}
		starts[start] = append(starts[start], stageStart{at: event.Time, authoring: prior[key] == occurrence})
	}
	models, harnesses := map[string]bool{}, map[string]bool{}
	for _, span := range parent.Spans {
		if span.Kind != telemetry.SpanKindTask {
			continue
		}
		branch, _ := strconv.Atoi(span.Attributes[telemetry.AttrBranch])
		attempt, err := strconv.Atoi(span.Attributes[telemetry.AttrAttemptNumber])
		if err != nil || attempt < 1 {
			attempt = 1
		}
		candidates := starts[startKey{span.Attributes[telemetry.AttrStage], branch, attempt}]
		if len(candidates) == 0 {
			continue
		}
		nearest := candidates[0]
		for _, candidate := range candidates[1:] {
			if absDuration(span.StartTime.Sub(candidate.at)) < absDuration(span.StartTime.Sub(nearest.at)) {
				nearest = candidate
			}
		}
		if !nearest.authoring {
			continue
		}
		if value := span.Attributes[telemetry.AttrModel]; value != "" {
			models[value] = true
		}
		if value := span.Attributes[telemetry.AttrHarnessVersion]; value != "" {
			harnesses[value] = true
		}
	}
	single := func(values map[string]bool) string {
		if len(values) != 1 {
			return ""
		}
		for value := range values {
			return value
		}
		return ""
	}
	return single(models), single(harnesses)
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
