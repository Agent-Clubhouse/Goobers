package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

const featuresHelp = "Usage: goobers features [--used] [path]\n\n" +
	"List the workflow-DSL features this build understands, with each\n" +
	"feature's support level (preview/ga/deprecated/removed) and the version\n" +
	"it entered that level. This reads the same registry the committed\n" +
	"docs/feature-matrix.md is generated from, so the two never disagree.\n\n" +
	"With --used, list only the features the instance at path (default \".\")\n" +
	"actually references across its workflows and goobers — the subset that\n" +
	"instance's config exercises. Exit codes: 0 = OK, 1 = invalid instance\n" +
	"config, 2 = usage/IO error.\n"

func runFeatures(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("features", flag.ContinueOnError)
	fs.SetOutput(stderr)
	usedOnly := fs.Bool("used", false, "list only the features the instance at path references")
	fs.Usage = helpUsage(stderr, "features")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	features := workflow.AllFeatures()
	if *usedOnly {
		used, code := instanceUsedFeatures(root, stderr)
		if code != 0 {
			return code
		}
		features = used
	}
	writeFeatureTable(stdout, features)
	return 0
}

// instanceUsedFeatures returns the DSL features the instance rooted at root
// references — the union of every workflow's and goober's feature set — in
// stable ID order. The path must be a valid instance root; the returned code is
// 0 on success, 2 for a missing/unreadable root, and 1 for a config that fails
// to load, mirroring `goobers validate`.
func instanceUsedFeatures(root string, stderr io.Writer) ([]workflow.Feature, int) {
	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return nil, 2
	}
	set, _, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		if errors.Is(err, instance.ErrInvalidConfig) {
			pf(stderr, "error: instance config failed validation: %v\n", err)
			return nil, 1
		}
		pf(stderr, "error: %v\n", err)
		return nil, 2
	}

	used := map[workflow.FeatureID]workflow.Feature{}
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		features, err := workflow.FeaturesForWorkflow(workflow.Definition{
			Name: wf.Name, DSLVersion: wf.DSLVersion, Spec: wf.Spec,
		})
		if err != nil {
			pf(stderr, "error: workflow %q: %v\n", wf.Name, err)
			return nil, 1
		}
		for _, feature := range features {
			used[feature.ID] = feature
		}
	}
	for i := range set.Goobers {
		g := &set.Goobers[i]
		features, err := workflow.FeaturesForGoober(g.Spec)
		if err != nil {
			pf(stderr, "error: goober %q: %v\n", g.Name, err)
			return nil, 1
		}
		for _, feature := range features {
			used[feature.ID] = feature
		}
	}

	out := make([]workflow.Feature, 0, len(used))
	for _, feature := range used {
		out = append(out, feature)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, 0
}

// writeFeatureTable prints features as an aligned FEATURE/SUPPORT/SINCE table
// followed by a count. The rows are already in stable ID order.
func writeFeatureTable(w io.Writer, features []workflow.Feature) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	pf(tw, "FEATURE\tSUPPORT\tSINCE\n")
	for _, feature := range features {
		pf(tw, "%s\t%s\t%s\n", feature.ID, feature.Level, feature.SinceVersion)
	}
	_ = tw.Flush()
	pf(w, "\n%d feature(s)\n", len(features))
}
