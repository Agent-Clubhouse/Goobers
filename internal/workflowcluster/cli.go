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
	"github.com/goobers/goobers/internal/workflow"
)

// CLIHelp documents `goobers workflow clusters`.
const CLIHelp = "Usage: goobers workflow clusters [--min-cluster-size <n>] [--seed <n>] [--json] [path]\n\n" +
	"Labs (experimental, advisory). Cluster the instance's configured workflows by\n" +
	"structural shape (node kinds, gates, retries, parallelism, graph topology)\n" +
	"with HDBSCAN, and report each cluster's medoid, members, stability, and\n" +
	"distinguishing features. Workflows that fit no dense group are reported as\n" +
	"noise. Node names and goal text never contribute. Nothing is modified\n" +
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
	asJSON := fs.Bool("json", false, "emit the fitted model as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 1 || *minSize < 2 {
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
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(model); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		return 0
	}
	writeReport(stdout, model)
	return 0
}

// ConfiguredPoints loads the instance config under root and returns one
// structural point per configured workflow, keyed "<gaggle>/<workflow>".
func ConfiguredPoints(root string) ([]Point, error) {
	layout := instance.NewLayout(root)
	set, _, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		return nil, fmt.Errorf("load instance config: %w", err)
	}
	points := make([]Point, 0, len(set.Workflows))
	for _, wf := range set.Workflows {
		features, err := ExtractDefinition(workflow.Definition{
			Name: wf.Name, Version: 1, DSLVersion: wf.DSLVersion, Spec: wf.Spec, Annotations: wf.Annotations,
		})
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
