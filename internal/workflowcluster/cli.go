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
	"github.com/goobers/goobers/internal/workflow"
)

// CLIHelp documents `goobers workflow clusters`.
const CLIHelp = "Usage: goobers workflow clusters [--features structural|behavior] [--cohort <label>] [--fitness] [--since <duration>] [--max-runs <n>] [--min-samples <n>] [--min-cluster-size <n>] [--seed <n>] [--stratify gaggle|cohort|template|model|harness|skills|tools] [--ablation] [--json] [path]\n" +
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
	"clusters merely track gaggle, task-cohort, shipped-template, model, harness,\n" +
	"skill-set, or tool-set boundaries (a confound). Undefined provenance is excluded and\n" +
	"counted; that check is observational, not gym-controlled. Nothing is\n" +
	"modified (default path \".\").\n\n" +
	"Template provenance fingerprints the workflow templates embedded in this\n" +
	"build: reference workflows, config examples, and the starter, demo,\n" +
	"quickstart, and scaffold workflows. A workflow is template-derived only\n" +
	"when it lies within 0.35 structural distance of a shipped template. The\n" +
	"metadata.annotations[\"goobers.dev/template-provenance\"] written by init and\n" +
	"scaffolding is reported as declared provenance; it names the attributed\n" +
	"template while the workflow is still within 0.35 of it, and otherwise the\n" +
	"nearest shipped template is shown. Cluster medoids farther than 0.35 from\n" +
	"every shipped template are reported as convergence beyond templates, and\n" +
	"--stratify template groups derived workflows by template and the rest as\n" +
	"\"beyond-templates\". Provenance is reported even when too few\n" +
	"workflows are configured to cluster. Bare --json stays the frozen model\n" +
	"that --cluster-model reads; JSON carries the template report alongside\n" +
	"any other requested report (for example --stratify template --json).\n\n" +
	"--fitness joins retained terminal runs to the current structural clusters\n" +
	"by workflow digest and reports sample size, success with a 95% Wilson\n" +
	"interval, wall time, cost, human-gate, escalation, and retry/loop rates.\n" +
	"Clusters with fewer than 5 runs are marked insufficient and are never\n" +
	"ranked. --since limits the retained runs included.\n\n" +
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
	featuresStructural = FleetFeatureStructural
	featuresBehavior   = FleetFeatureBehavior
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
	stratify := fs.String("stratify", "", "confound axis (gaggle, cohort, template, model, harness, skills, or tools)")
	asJSON := fs.Bool("json", false, "emit the fitted model as JSON")
	fitness := fs.Bool("fitness", false, "structural: report retained-run fitness for each cluster")
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
		case "max-runs", "min-samples":
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
		fitness: *fitness,
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
	var workflowDigests map[string]string
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
		points, strata, workflowDigests, err = configuredPoints(root)
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
	if *features == featuresStructural && len(points) < *minSize {
		return runInsufficientStructural(points, strata[StratifyTemplate], *minSize, *asJSON, stdout, stderr)
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
	var templateReport *TemplateReport
	if *features == featuresStructural {
		report, templateStrata, analyzeErr := analyzeTemplates(points, strata[StratifyTemplate], &model)
		if analyzeErr != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", analyzeErr)
			return 2
		}
		templateReport, strata[StratifyTemplate] = &report, templateStrata
	}
	if *stratify != "" {
		report := ConfoundCheck(model, strata[*stratify], ConfoundOptions{Seed: *seed})
		confounds = &report
	}
	var fitnessReport *FitnessReport
	if *fitness {
		scanOpts := BehaviorScanOptions{MaxRuns: *maxRuns}
		if *since > 0 {
			scanOpts.Since = time.Now().Add(-*since)
		}
		scan, scanErr := ScanBehavior(root, scanOpts)
		if scanErr != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", scanErr)
			return 2
		}
		report := EvaluateFitness(model, workflowDigests, scan, DefaultFitnessMinRuns)
		fitnessReport = &report
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
	return writeCLIReports(stdout, stderr, *asJSON, *stratify, model, cliReports{
		confounds: confounds, behavior: behavior, ablation: ablations, fitness: fitnessReport, templates: templateReport,
	})
}

// cliReports holds the optional reports RunCLI attaches to a fitted model.
type cliReports struct {
	confounds *ConfoundReport
	behavior  *behaviorReport
	ablation  *AblationReport
	fitness   *FitnessReport
	templates *TemplateReport
}

func writeCLIReports(stdout, stderr io.Writer, asJSON bool, stratify string, model Model, r cliReports) int {
	if asJSON {
		// Bare --json stays the frozen model LoadModel reads; template
		// provenance rides along with any other requested report.
		var out any = model
		if r.behavior != nil || r.confounds != nil || r.ablation != nil || r.fitness != nil {
			out = struct {
				Model     Model           `json:"model"`
				Confounds *ConfoundReport `json:"confounds,omitempty"`
				Behavior  *behaviorReport `json:"behavior,omitempty"`
				Ablation  *AblationReport `json:"ablation,omitempty"`
				Fitness   *FitnessReport  `json:"fitness,omitempty"`
				Templates *TemplateReport `json:"templates,omitempty"`
			}{model, r.confounds, r.behavior, r.ablation, r.fitness, r.templates}
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
	if r.templates != nil {
		writeWorkflowTemplateReport(stdout, *r.templates, true)
	}
	if r.behavior != nil {
		writeBehaviorScan(stdout, *r.behavior)
	}
	if r.confounds != nil {
		writeConfounds(stdout, stratify, *r.confounds)
	}
	if r.fitness != nil {
		writeFitness(stdout, *r.fitness)
	}
	if r.ablation != nil {
		writeAblation(stdout, *r.ablation)
	}
	return 0
}

// cliArgs holds the parsed flags RunCLI validates together.
type cliArgs struct {
	features, cohort, stratify, fleetDir                            string
	since                                                           time.Duration
	maxRuns, minSamples, minSize, nargs                             int
	asJSON, snapshot, ablation, behaviorOnly, snapshotOnly, fitness bool
}

// invalid reports flag combinations RunCLI rejects with usage.
func (a cliArgs) invalid() bool {
	return a.nargs > 1 || a.minSize < 2 || (a.stratify != "" && !validStratify(a.stratify)) ||
		(a.features != featuresStructural && a.features != featuresBehavior) ||
		((a.cohort != "" || a.stratify == StratifyCohort || a.stratify == StratifyTemplate) && a.features != featuresStructural) ||
		(a.cohort != "" && a.stratify == StratifyCohort) ||
		(a.behaviorOnly && a.features != featuresBehavior) || a.since < 0 || a.maxRuns < 1 || a.minSamples < 1 ||
		(a.fitness && a.features != featuresStructural) || (a.since > 0 && a.features != featuresBehavior && !a.fitness) ||
		(a.snapshotOnly && !a.snapshot) || (a.snapshot && (a.fleetDir != "" || a.asJSON || a.stratify != "" || a.cohort != "" || a.ablation || a.fitness)) ||
		(a.fleetDir != "" && (a.nargs > 0 || a.stratify != "" || a.cohort != "" || a.ablation || a.behaviorOnly || a.fitness))
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
	return axis == StratifyGaggle || axis == StratifyCohort || axis == StratifyTemplate || axis == StratifyModel || axis == StratifyHarness ||
		axis == StratifySkills || axis == StratifyTools
}

// runInsufficientStructural reports template provenance for a structural
// population too small to fit, so starter, demo, and quickstart instances
// still see it, then fails as Fit would.
func runInsufficientStructural(points []Point, explicit Strata, minSize int, asJSON bool, stdout, stderr io.Writer) int {
	report, _, err := analyzeTemplates(points, explicit, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(struct {
			Status         string          `json:"status"`
			Population     int             `json:"population"`
			MinClusterSize int             `json:"minClusterSize"`
			Templates      *TemplateReport `json:"templates"`
		}{ViewInsufficientData, len(points), minSize, &report}); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
	} else {
		_, _ = fmt.Fprintf(stdout, "Too few workflows to cluster: %d configured; clustering needs at least %d.\n", len(points), minSize)
		writeWorkflowTemplateReport(stdout, report, false)
	}
	_, _ = fmt.Fprintf(stderr, "error: workflowcluster: population %d is below minimum cluster size %d\n", len(points), minSize)
	return 1
}

func writeWorkflowTemplateReport(w io.Writer, report TemplateReport, clustered bool) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("\nTemplate provenance (%d shipped templates; derived at distance <= %.2f)\n",
		len(report.Catalog), report.DistanceThreshold)
	ids := sortedKeys(report.Workflows)
	for _, id := range ids {
		provenance := report.Workflows[id]
		relation, declared := "nearest", ""
		switch {
		case provenance.Derived && provenance.DeclaredTemplateID == provenance.TemplateID:
			relation = "declared from"
		case provenance.Derived:
			relation = "derived from"
		}
		if provenance.Explicit && provenance.DeclaredTemplateID != provenance.TemplateID {
			declared = "; declared " + provenance.DeclaredTemplateID + ", diverged"
		}
		p("  %s  %s %s (distance %.3f%s)\n", id, relation, provenance.TemplateID, provenance.Distance, declared)
	}
	if !clustered {
		return
	}
	p("Convergence beyond templates: %d clusters\n", len(report.BeyondTemplates))
	for _, cluster := range report.BeyondTemplates {
		p("  %s medoid %s (nearest %s, distance %.3f)\n",
			cluster.ClusterID, cluster.MedoidID, cluster.NearestTemplate, cluster.Distance)
	}
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
// with its provenance strata and compiled digest as run journals record it.
func configuredPoints(root string) ([]Point, pointStrata, map[string]string, error) {
	layout := instance.NewLayout(root)
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load instance config: %w", err)
	}
	if set == nil || report == nil || report.HasErrors() {
		return nil, nil, nil, errors.New("instance config has validation errors; run `goobers validate`")
	}
	points := make([]Point, 0, len(set.Workflows))
	digests := make(map[string]string, len(set.Workflows))
	for _, wf := range set.Workflows {
		def := definitionOf(wf)
		features, err := ExtractDefinition(def)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		// Run journals pin machine.Digest(), which Compile computes after
		// normalizing the definition (e.g. inheriting outboxMirrorPath into
		// tasks), so the raw definition's digest would never match.
		machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(workflow.PreviewFeaturesEnabled(wf.Annotations)))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		id := wf.Spec.Gaggle + "/" + wf.Name
		point := StructuralPoint(id, features)
		point.Cohort = Cohort(wf)
		points = append(points, point)
		digests[id] = machine.Digest()
	}
	strata := configuredStrata(set.Workflows, set.Goobers)
	strata[StratifyCohort] = cohortStrata(points)
	return points, strata, digests, nil
}

func writeFitness(w io.Writer, report FitnessReport) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("\nFitness (advisory; clusters are not ranked)\n")
	p("cluster  runs  success (95%% Wilson)  wall p50/p90  cost p50/p90  human  escalated  retry/loop\n")
	for _, f := range report.Clusters {
		if f.InsufficientRuns {
			p("%s  %d  insufficient runs (need %d)\n", f.ClusterID, f.RunCount, f.MinimumRuns)
			continue
		}
		p("%s  %d  %.1f%% (%.1f-%.1f%%)  %.1fs/%.1fs  $%.2f/$%.2f  %.1f%%  %.1f%%  %.1f%%\n",
			f.ClusterID, f.RunCount, 100*f.SuccessRate, 100*f.SuccessInterval.Lower, 100*f.SuccessInterval.Upper,
			f.MedianWallTimeSeconds, f.P90WallTimeSeconds, f.MedianCostUSD, f.P90CostUSD,
			100*f.HumanGateRate, 100*f.EscalationRate, 100*f.RetryLoopRate)
	}
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
