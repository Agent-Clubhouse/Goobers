package workflowcluster

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/instance"
)

// CLIHelp documents `goobers workflow clusters`.
const CLIHelp = "Usage: goobers workflow clusters [--min-cluster-size <n>] [--seed <n>] [--stratify gaggle] [--json] [path]\n\n" +
	"Labs (experimental, advisory). Cluster the instance's configured workflows by\n" +
	"structural shape (node kinds, gates, retries, parallelism, graph topology)\n" +
	"with HDBSCAN, and report each cluster's medoid, members, stability, and\n" +
	"distinguishing features. Workflows that fit no dense group are reported as\n" +
	"noise. Node names and goal text never contribute. With --stratify gaggle,\n" +
	"also test whether clusters merely track gaggle boundaries (a confound);\n" +
	"that check is observational, not gym-controlled. Nothing is modified\n" +
	"(default path \".\").\n"

// cliMinClusterSize is smaller than the library default because a single
// instance usually configures tens of workflows, not hundreds.
const cliMinClusterSize = 3

// RunCLI implements `goobers workflow clusters`. fs is the caller's flag set.
func RunCLI(args []string, fs *flag.FlagSet, stdout, stderr io.Writer) int {
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, CLIHelp) }
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
	if fs.NArg() > 1 || *minSize < 2 || (*stratify != "" && *stratify != stratifyGaggle) {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	points, err := ConfiguredPoints(root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	model, err := Fit(StructuralFeatureVersion, points, Options{MinClusterSize: *minSize, Seed: *seed})
	if err != nil {
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
		if confounds != nil {
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
	if confounds != nil {
		writeConfounds(stdout, *stratify, *confounds)
	}
	return 0
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
	p("Labs: experimental, advisory clustering of workflow structure.\n")
	p("model %s (%d workflows, %d clusters, %d noise)\n", m.ID, m.Population, len(m.Clusters), len(m.Noise))
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
