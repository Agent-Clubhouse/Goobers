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
const CLIHelp = "Usage: goobers workflow clusters [--features structural|behavior] [--since <duration>] [--max-runs <n>] [--min-samples <n>] [--min-cluster-size <n>] [--seed <n>] [--stratify gaggle] [--json] [path]\n\n" +
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
	"behavior features are never mixed in one model. With --stratify gaggle,\n" +
	"also test whether clusters merely track gaggle boundaries (a confound);\n" +
	"that check is observational, not gym-controlled. Nothing is modified\n" +
	"(default path \".\").\n"

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
	since := fs.Duration("since", 0, "behavior: only runs started within this duration")
	maxRuns := fs.Int("max-runs", DefaultBehaviorMaxRuns, "behavior: most run journals read, newest first")
	minSamples := fs.Int("min-samples", cliMinSamples, "behavior: smallest EffectiveVersion cohort clustered")
	minSize := fs.Int("min-cluster-size", cliMinClusterSize, "smallest group reported as a cluster")
	seed := fs.Uint64("seed", 0, "seed for stability resampling")
	stratify := fs.String("stratify", "", "confound axis to test cluster assignment against (gaggle)")
	asJSON := fs.Bool("json", false, "emit the fitted model as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	behaviorOnly := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "since", "max-runs", "min-samples":
			behaviorOnly = true
		}
	})
	invalid := fs.NArg() > 1 || *minSize < 2 || (*stratify != "" && *stratify != stratifyGaggle) ||
		(*features != featuresStructural && *features != featuresBehavior) ||
		(behaviorOnly && *features != featuresBehavior) || *since < 0 || *maxRuns < 1 || *minSamples < 1
	if invalid {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	featureVersion := StructuralFeatureVersion
	var behavior *behaviorReport
	var points []Point
	var err error
	if *features == featuresBehavior {
		featureVersion = BehaviorFeatureVersion
		scanOpts := BehaviorScanOptions{MaxRuns: *maxRuns}
		if *since > 0 {
			scanOpts.Since = time.Now().Add(-*since)
		}
		behavior, points, err = behaviorPoints(root, scanOpts, *minSamples)
	} else {
		points, err = ConfiguredPoints(root)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
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
		report := ConfoundCheck(model, gaggleStrata(points), ConfoundOptions{Seed: *seed})
		confounds = &report
	}
	if *asJSON {
		var out any = model
		switch {
		case behavior != nil:
			out = struct {
				Model     Model           `json:"model"`
				Confounds *ConfoundReport `json:"confounds,omitempty"`
				Behavior  behaviorReport  `json:"behavior"`
			}{model, confounds, *behavior}
		case confounds != nil:
			out = struct {
				Model     Model           `json:"model"`
				Confounds *ConfoundReport `json:"confounds"`
			}{model, confounds}
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
	return 0
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

// stratifyGaggle is the only --stratify axis the instance config supports;
// model, harness, and task-cohort axes need run-behavior descriptors.
const stratifyGaggle = "gaggle"

// gaggleStrata labels each "<gaggle>/<workflow>" point with its gaggle.
func gaggleStrata(points []Point) Strata {
	strata := make(Strata, len(points))
	for _, p := range points {
		gaggle, _, _ := strings.Cut(p.ID, "/")
		strata[p.ID] = gaggle
	}
	return strata
}

// ConfiguredPoints loads the instance config under root and returns one
// structural point per configured workflow, keyed "<gaggle>/<workflow>".
func ConfiguredPoints(root string) ([]Point, error) {
	layout := instance.NewLayout(root)
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		return nil, fmt.Errorf("load instance config: %w", err)
	}
	if set == nil || report == nil || report.HasErrors() {
		return nil, errors.New("instance config has validation errors; run `goobers validate`")
	}
	points := make([]Point, 0, len(set.Workflows))
	for _, wf := range set.Workflows {
		features, err := ExtractDefinition(definitionOf(wf))
		if err != nil {
			return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		points = append(points, StructuralPoint(wf.Spec.Gaggle+"/"+wf.Name, features))
	}
	return points, nil
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
