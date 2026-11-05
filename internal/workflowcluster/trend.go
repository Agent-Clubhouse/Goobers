package workflowcluster

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/telemetry"
)

const (
	TrendAxisModel          = "model"
	TrendAxisHarness        = "harness"
	TrendAxisAuthoringSkill = "authoring-skill"
	trendNoiseCluster       = "noise"
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
}

type TrendReport struct {
	ModelID        string      `json:"modelId"`
	FeatureVersion string      `json:"featureVersion"`
	Samples        int         `json:"samples"`
	Excluded       int         `json:"excluded"`
	Axes           []TrendAxis `json:"axes"`
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
		for _, observation := range observations {
			if version := observation.Versions[axis]; version != "" {
				groups[version] = append(groups[version], observation)
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
		report.Axes = append(report.Axes, TrendAxis{Axis: axis, Cohorts: cohorts})
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

// ScanChildTrends reconstructs agent-authored child shapes and their authoring
// provenance from retained journals. Individual unreadable or incomplete runs
// are counted as excluded rather than weakening a cohort with partial data.
func ScanChildTrends(root string, model Model) (TrendReport, error) {
	runDirs, err := instance.NewLayout(root).RunDirs()
	if err != nil {
		return TrendReport{}, fmt.Errorf("list trend journals: %w", err)
	}
	dirs := map[string]string{}
	identities := map[string]journal.RunIdentity{}
	for _, runsDir := range runDirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			return TrendReport{}, fmt.Errorf("read trend journals: %w", err)
		}
		for _, entry := range entries {
			dir := filepath.Join(runsDir, entry.Name())
			if !entry.IsDir() || !journal.Recorded(dir) {
				continue
			}
			reader, err := journal.OpenReadOnly(dir)
			if err != nil {
				continue
			}
			identity, err := reader.Identity()
			if err != nil {
				continue
			}
			dirs[identity.RunID], identities[identity.RunID] = dir, identity
		}
	}
	var observations []TrendObservation
	excluded := 0
	for runID, identity := range identities {
		if identity.Child == nil {
			continue
		}
		reader, err := journal.OpenReadOnly(dirs[runID])
		if err != nil {
			excluded++
			continue
		}
		events, err := reader.Events()
		if err != nil || !journal.PhaseFromEvents(events).IsTerminal() {
			excluded++
			continue
		}
		machine, err := runsemantics.PinnedWorkflowMachine(reader, identity)
		if err != nil {
			excluded++
			continue
		}
		features, err := ExtractDefinition(machine.Def)
		if err != nil {
			excluded++
			continue
		}
		parent, ok := identities[identity.Child.ParentRunID]
		if !ok {
			excluded++
			continue
		}
		parentInput, err := readBehaviorInput(dirs[parent.RunID], BehaviorScanOptions{}.withDefaults())
		if err != nil {
			excluded++
			continue
		}
		modelVersion, harnessVersion := authoringVersions(parentInput.Spans)
		at := identity.StartedAt
		if at.IsZero() {
			at, _ = runClock(events)
		}
		fitness := 0.0
		if journal.PhaseFromEvents(events) == journal.PhaseCompleted {
			fitness = 1
		}
		observations = append(observations, TrendObservation{
			ID: runID, At: at, Point: StructuralPoint(runID, features), Fitness: fitness,
			Versions: map[string]string{
				TrendAxisModel: modelVersion, TrendAxisHarness: harnessVersion, TrendAxisAuthoringSkill: parent.GooberDigest,
			},
		})
	}
	report, err := AnalyzeTrends(model, observations)
	report.Excluded = excluded
	return report, err
}

func authoringVersions(spans []telemetry.SpanRecord) (string, string) {
	models, harnesses := map[string]bool{}, map[string]bool{}
	for _, span := range spans {
		if span.Kind != telemetry.SpanKindTask {
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
