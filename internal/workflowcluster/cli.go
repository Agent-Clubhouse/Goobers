package workflowcluster

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// CLIHelp documents `goobers workflow clusters`.
const CLIHelp = "Usage: goobers workflow clusters [--features structural|behavior] [--cohort <label>] [--since <duration>] [--max-runs <n>] [--min-samples <n>] [--min-cluster-size <n>] [--seed <n>] [--stratify gaggle|cohort|model|harness|skills|tools] [--ablation] [--json] [path]\n" +
	"       goobers workflow clusters --snapshot [--features structural|behavior] [--instance-id <id>] [--salt <s>] [path]\n" +
	"       goobers workflow clusters --fleet <dir> [--min-cluster-size <n>] [--seed <n>] [--json]\n" +
	"       goobers workflow clusters --templates <evidence.json> --cluster-model <model.json> [--json]\n\n" +
	"Labs (experimental, advisory). Cluster the instance's workflows with HDBSCAN\n" +
	"and report each cluster's medoid, members, stability, and distinguishing\n" +
	"features. Points that fit no dense group are reported as noise.\n\n" +
	"--features structural (default) clusters configured workflows by structural\n" +
	"shape (node kinds, gates, retries, parallelism, graph topology).\n\n" +
	"--features behavior clusters what runs actually did: it reads terminal run\n" +
	"journals (agent-authored child runs included), derives a behavior\n" +
	"descriptor per run (stage-visit patterns, attempts and retries, gate,\n" +
	"escalation and human hits, fan-out, child spawns and dispositions, usage,\n" +
	"duration, outcome, near misses), and clusters one point per workflow\n" +
	"EffectiveVersion cohort from each feature's mean and p90. Runs without a\n" +
	"defined EffectiveVersion are excluded. --since keeps runs started within\n" +
	"that Go duration, --max-runs bounds the journals read (newest first), and\n" +
	"cohorts with fewer than --min-samples runs are dropped.\n\n" +
	"Node names, stage names, and goal text never contribute. Structural and\n" +
	"behavior features are never mixed in one model. --cohort limits structural\n" +
	"clustering to one privacy-safe task cohort. --stratify tests whether\n" +
	"clusters merely track gaggle, task-cohort, model, harness, skill-set, or\n" +
	"tool-set boundaries (a confound). Undefined provenance is excluded and\n" +
	"counted; that check is observational, not gym-controlled. Nothing is\n" +
	"modified (default path \".\").\n\n" +
	"Task cohorts use metadata.labels[\"goobers.dev/task-class\"] when explicitly\n" +
	"set. Otherwise an auto:<digest> label is derived only from trigger types,\n" +
	"work-item kinds, the declared policy-action side-effect set, and repo:*\n" +
	"capabilities. Workflow and stage names, goals, selectors, schedules, signals,\n" +
	"and webhook payload text are excluded, making derivation deterministic and\n" +
	"rename-invariant. --stratify cohort requires structural features and cannot\n" +
	"be combined with --cohort.\n\n" +
	"--ablation refits after dropping each feature (the WL histogram is one\n" +
	"group), reports stability and assignment change, and flags dominant\n" +
	"features. Populations through 2,000 points are exact; larger populations\n" +
	"use a deterministic 1,000-point HDBSCAN sample and radius assignment.\n\n" +
	"Fleet convergence: --snapshot prints this instance's feature vectors as a\n" +
	"fleet snapshot (JSON) with no workflow, gaggle, goober, or goal text;\n" +
	"workflows become salted keys and goober names salted hashes. The instance\n" +
	"ID and salt default to the fleet association (salt = fleet ID), so every\n" +
	"instance in a fleet hashes alike. --fleet <dir> pools the *.json snapshots\n" +
	"in dir and clusters distinct workflow shapes across instances: a shape\n" +
	"cloned onto many instances counts once, only the most common feature\n" +
	"version is clustered (others are listed as excluded), each instance is\n" +
	"scored aligned/private/outlier, and clusters are tested for an instance\n" +
	"confound.\n\n" +
	"Reusable templates: --templates <evidence.json> reads a JSON array of\n" +
	"joined child-proposal evidence (proposalId, proposalDigest, runId,\n" +
	"parentRunId, repository, taskCohort, succeeded, findings, parameters) and\n" +
	"--cluster-model a frozen structural model whose point IDs are those\n" +
	"proposal IDs. It reports each stable cluster that clears the evidence bar\n" +
	"(stability, parent-run and repository support, no policy or safety\n" +
	"findings, outcomes no worse than comparable alternatives per task cohort,\n" +
	"a parameterized medoid) as a review-required config change request for\n" +
	"the child-authoring catalog, and lists why every other cluster was\n" +
	"suppressed. Advisory only: nothing is written or registered.\n"

// cliMinClusterSize is smaller than the library default because a single
// instance usually configures tens of workflows, not hundreds.
const cliMinClusterSize = 3

// Feature sets selectable with --features.
const (
	featuresStructural = "structural"
	featuresBehavior   = "behavior"
)

// cliMinSamples is the default smallest EffectiveVersion cohort clustered.
const cliMinSamples = 3

// RunCLI implements `goobers workflow clusters`. fs is the caller's flag set.
func RunCLI(args []string, fs *flag.FlagSet, stdout, stderr io.Writer) int {
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, CLIHelp) }
	features := fs.String("features", featuresStructural, "feature set to cluster on (structural or behavior)")
	cohort := fs.String("cohort", "", "structural: cluster only workflows in this task cohort")
	since := fs.Duration("since", 0, "behavior: only runs started within this duration")
	maxRuns := fs.Int("max-runs", DefaultBehaviorMaxRuns, "behavior: most run journals read, newest first")
	minSamples := fs.Int("min-samples", cliMinSamples, "behavior: smallest EffectiveVersion cohort clustered")
	minSize := fs.Int("min-cluster-size", cliMinClusterSize, "smallest group reported as a cluster")
	seed := fs.Uint64("seed", 0, "seed for stability resampling")
	stratify := fs.String("stratify", "", "confound axis (gaggle, cohort, model, harness, skills, or tools)")
	asJSON := fs.Bool("json", false, "emit the fitted model as JSON")
	ablation := fs.Bool("ablation", false, "report leave-one-feature-out cluster stability and assignment change")
	snapshot := fs.Bool("snapshot", false, "emit a privacy-safe fleet snapshot of this instance's features")
	instanceID := fs.String("instance-id", "", "snapshot: instance ID (default: fleet association)")
	salt := fs.String("salt", "", "snapshot: salt for name hashes (default: fleet ID)")
	fleetDir := fs.String("fleet", "", "cluster the *.json fleet snapshots in this directory")
	templates := fs.String("templates", "", "propose reusable templates from this joined evidence JSON")
	clusterModel := fs.String("cluster-model", "", "templates: frozen structural cluster model JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *templates != "" || *clusterModel != "" {
		if templatesArgsInvalid(fs, *templates, *clusterModel) {
			fs.Usage()
			return 2
		}
		return runTemplatesCLI(*templates, *clusterModel, *asJSON, stdout, stderr)
	}
	behaviorOnly, snapshotOnly, saltSet := false, false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "since", "max-runs", "min-samples":
			behaviorOnly = true
		case "salt":
			saltSet = true
			snapshotOnly = true
		case "instance-id":
			snapshotOnly = true
		}
	})
	parsed := cliArgs{
		features: *features, cohort: *cohort, stratify: *stratify, fleetDir: *fleetDir, since: *since,
		maxRuns: *maxRuns, minSamples: *minSamples, minSize: *minSize, nargs: fs.NArg(),
		asJSON: *asJSON, snapshot: *snapshot, ablation: *ablation, behaviorOnly: behaviorOnly, snapshotOnly: snapshotOnly,
	}
	if parsed.invalid() {
		fs.Usage()
		return 2
	}
	if *fleetDir != "" {
		return runFleetCLI(*fleetDir, Options{MinClusterSize: *minSize, Seed: *seed}, *asJSON, stdout, stderr)
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	featureVersion := StructuralFeatureVersion
	var behavior *behaviorReport
	var points []Point
	var strata pointStrata
	var err error
	if *features == featuresBehavior {
		featureVersion = BehaviorFeatureVersion
		scanOpts := BehaviorScanOptions{MaxRuns: *maxRuns}
		if *since > 0 {
			scanOpts.Since = time.Now().Add(-*since)
		}
		behavior, points, err = behaviorPoints(root, scanOpts, *minSamples)
		if behavior != nil {
			strata = pointStrata{StratifyGaggle: gaggleStrata(points)}
			for _, axis := range provenanceAxes {
				strata[axis] = behaviorStrata(behavior.Cohorts, axis)
			}
		}
	} else {
		points, strata, err = configuredPoints(root)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if *cohort != "" {
		points = pointsInCohort(points, *cohort)
		if len(points) == 0 {
			_, _ = fmt.Fprintf(stderr, "error: no workflows in cohort %q\n", *cohort)
			return 1
		}
	}
	if *snapshot {
		return runSnapshotCLI(root, featureVersion, *instanceID, *salt, saltSet, points, stdout, stderr)
	}
	model, err := Fit(featureVersion, points, Options{MinClusterSize: *minSize, Seed: *seed})
	if err != nil {
		if behavior != nil {
			writeBehaviorScan(stderr, *behavior)
		}
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	var confounds *ConfoundReport
	if *stratify != "" {
		report := ConfoundCheck(model, strata[*stratify], ConfoundOptions{Seed: *seed})
		confounds = &report
	}
	var ablations *AblationReport
	if *ablation {
		report, err := Ablate(featureVersion, points, Options{MinClusterSize: *minSize, Seed: *seed}, model)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		ablations = &report
	}
	if *asJSON {
		var out any = model
		if behavior != nil || confounds != nil || ablations != nil {
			out = struct {
				Model     Model           `json:"model"`
				Confounds *ConfoundReport `json:"confounds,omitempty"`
				Behavior  *behaviorReport `json:"behavior,omitempty"`
				Ablation  *AblationReport `json:"ablation,omitempty"`
			}{model, confounds, behavior, ablations}
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(out); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		return 0
	}
	writeReport(stdout, model)
	if behavior != nil {
		writeBehaviorScan(stdout, *behavior)
	}
	if confounds != nil {
		writeConfounds(stdout, *stratify, *confounds)
	}
	if ablations != nil {
		writeAblation(stdout, *ablations)
	}
	return 0
}

// cliArgs holds the parsed flags RunCLI validates together.
type cliArgs struct {
	features, cohort, stratify, fleetDir                   string
	since                                                  time.Duration
	maxRuns, minSamples, minSize, nargs                    int
	asJSON, snapshot, ablation, behaviorOnly, snapshotOnly bool
}

// invalid reports flag combinations RunCLI rejects with usage.
func (a cliArgs) invalid() bool {
	return a.nargs > 1 || a.minSize < 2 || (a.stratify != "" && !validStratify(a.stratify)) ||
		(a.features != featuresStructural && a.features != featuresBehavior) ||
		((a.cohort != "" || a.stratify == StratifyCohort) && a.features != featuresStructural) ||
		(a.cohort != "" && a.stratify == StratifyCohort) ||
		(a.behaviorOnly && a.features != featuresBehavior) || a.since < 0 || a.maxRuns < 1 || a.minSamples < 1 ||
		(a.snapshotOnly && !a.snapshot) || (a.snapshot && (a.fleetDir != "" || a.asJSON || a.stratify != "" || a.cohort != "" || a.ablation)) ||
		(a.fleetDir != "" && (a.nargs > 0 || a.stratify != "" || a.cohort != "" || a.ablation || a.behaviorOnly))
}

// behaviorReport is the cohort and scan accounting behind a behavior model.
type behaviorReport struct {
	Scan       BehaviorScan      `json:"scan"`
	MinSamples int               `json:"minSamples"`
	Cohorts    []BehaviorSummary `json:"cohorts"`
	// Undersampled counts cohorts dropped for having fewer than MinSamples runs.
	Undersampled int `json:"undersampled"`
}

// behaviorPoints scans run journals under root and returns one point per
// sufficiently sampled EffectiveVersion cohort.
func behaviorPoints(root string, opts BehaviorScanOptions, minSamples int) (*behaviorReport, []Point, error) {
	scan, err := ScanBehavior(root, opts)
	if err != nil {
		return nil, nil, err
	}
	cohorts := SummarizeBehavior(scan.Descriptors, minSamples)
	report := &behaviorReport{
		MinSamples: minSamples, Cohorts: cohorts,
		Undersampled: len(SummarizeBehavior(scan.Descriptors, 1)) - len(cohorts),
	}
	scan.Descriptors = nil
	report.Scan = scan
	points := make([]Point, len(cohorts))
	for i, c := range cohorts {
		points[i] = BehaviorPoint(c)
	}
	return report, points, nil
}

func writeBehaviorScan(w io.Writer, r behaviorReport) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	s := r.Scan
	p("\nBehavior scan: %d journals, %d cohorts clustered (min %d runs; %d undersampled)\n",
		s.Candidates, len(r.Cohorts), r.MinSamples, r.Undersampled)
	p("  excluded  %d non-terminal, %d undefined EffectiveVersion, %d before --since, %d over --max-runs, %d oversized, %d unreadable\n",
		s.NonTerminal, s.UndefinedProvenance, s.BeforeSince, s.OverLimit, s.Oversized, s.Unreadable)
}

func validStratify(axis string) bool {
	return axis == StratifyGaggle || axis == StratifyCohort || axis == StratifyModel || axis == StratifyHarness ||
		axis == StratifySkills || axis == StratifyTools
}

// gaggleStrata labels each "<gaggle>/<workflow>" point with its gaggle.
func gaggleStrata(points []Point) Strata {
	strata := make(Strata, len(points))
	for _, p := range points {
		gaggle, _, _ := strings.Cut(p.ID, "/")
		strata[p.ID] = gaggle
	}
	return strata
}

func pointsInCohort(points []Point, cohort string) []Point {
	filtered := make([]Point, 0, len(points))
	for _, point := range points {
		if point.Cohort == cohort {
			filtered = append(filtered, point)
		}
	}
	return filtered
}

// configuredPoints loads the instance config under root and returns one
// structural point per configured workflow, keyed "<gaggle>/<workflow>",
// with its provenance strata.
func configuredPoints(root string) ([]Point, pointStrata, error) {
	layout := instance.NewLayout(root)
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		return nil, nil, fmt.Errorf("load instance config: %w", err)
	}
	if set == nil || report == nil || report.HasErrors() {
		return nil, nil, errors.New("instance config has validation errors; run `goobers validate`")
	}
	points := make([]Point, 0, len(set.Workflows))
	for _, wf := range set.Workflows {
		features, err := ExtractDefinition(definitionOf(wf))
		if err != nil {
			return nil, nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		point := StructuralPoint(wf.Spec.Gaggle+"/"+wf.Name, features)
		point.Cohort = Cohort(wf)
		points = append(points, point)
	}
	strata := configuredStrata(set.Workflows, set.Goobers)
	strata[StratifyCohort] = cohortStrata(points)
	return points, strata, nil
}

func writeReport(w io.Writer, m Model) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	subject, unit := "workflow structure", "workflows"
	if m.FeatureVersion == BehaviorFeatureVersion {
		subject, unit = "run behavior", "EffectiveVersion cohorts"
	}
	p("Labs: experimental, advisory clustering of %s.\n", subject)
	p("model %s (%d %s, %d clusters, %d noise)\n", m.ID, m.Population, unit, len(m.Clusters), len(m.Noise))
	for _, c := range m.Clusters {
		stable := "stable"
		if !c.Stable {
			stable = "UNSTABLE"
		}
		p("\n%s  size %d  stability %.2f (%s)\n", c.ID, len(c.Members), c.Stability, stable)
		p("  medoid   %s\n", c.Medoid.ID)
		p("  members  %s\n", strings.Join(c.Members, ", "))
		for _, a := range c.Distinguishing {
			name := a.Feature
			if a.Value != "" {
				name += "=" + a.Value
			}
			p("  %+.2f    %s\n", a.Effect, name)
		}
	}
	if len(m.Noise) > 0 {
		noise := append([]string(nil), m.Noise...)
		sort.Strings(noise)
		p("\nnoise  %s\n", strings.Join(noise, ", "))
	}
}

func writeConfounds(w io.Writer, axis string, r ConfoundReport) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("\nConfounds (stratified by %s; evidence: %s)\n", axis, r.Evidence)
	p("  %s %.2f  p %.3f  (%d permutations, %d tested, %d unlabeled)\n",
		r.Statistic, r.Association, r.PValue, r.Options.Permutations, r.Tested, r.Unlabeled)
	if r.Confounded {
		p("  CONFOUNDED: clusters track %s boundaries\n", axis)
	} else {
		p("  not confounded by %s\n", axis)
	}
	strata := make([]string, len(r.Strata))
	for i, s := range r.Strata {
		strata[i] = fmt.Sprintf("%s=%d", s.Key, s.Size)
	}
	p("  strata      %s\n", strings.Join(strata, ", "))
	if len(r.Undersized) > 0 {
		p("  undersized  %s (below %d; reported, not pooled)\n", strings.Join(r.Undersized, ", "), r.Options.MinStratumSize)
	}
}

func writeAblation(w io.Writer, report AblationReport) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("\nFeature ablation (baseline stability %.2f; dominant at change >= %.2f)\n",
		report.BaselineStability, report.DominanceCutoff)
	for _, feature := range report.Features {
		flag := ""
		if feature.Dominant {
			flag = " DOMINANT"
		}
		p("  %-28s stability %.2f  delta %.2f  assignments %.1f%%%s\n",
			feature.Feature, feature.ClusterStability, feature.StabilityChange,
			100*feature.AssignmentChange, flag)
	}
}
