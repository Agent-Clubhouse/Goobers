package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

const featuresHelp = "Usage: goobers features [--dsl-version <version>] [--used] [path]\n\n" +
	"List the workflow-DSL features this build understands by DSL version,\n" +
	"including each feature's support level (preview/ga/deprecated/removed).\n" +
	"Use --dsl-version to scope the matrix to one declared version. This reads\n" +
	"the same registry and SupportMatrix the committed\n" +
	"docs/feature-matrix.md is generated from, so the two never disagree.\n\n" +
	"With --used, list only the features the instance at path (default \".\")\n" +
	"actually references across its workflows and goobers — the subset that\n" +
	"instance's config exercises. Exit codes: 0 = OK, 1 = invalid instance\n" +
	"config, 2 = usage/IO error.\n"

func runFeatures(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("features", flag.ContinueOnError)
	fs.SetOutput(stderr)
	usedOnly := fs.Bool("used", false, "list only the features the instance at path references")
	dslVersion := fs.String("dsl-version", "", "list only features contained in this DSL version")
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
	rows, err := featureMatrixRows(features, *dslVersion)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	writeFeatureTable(stdout, rows)
	return 0
}

type featureMatrixRow struct {
	DSLVersion string
	DSLLevel   supportmatrix.Level
	Feature    workflow.Feature
}

func featureMatrixRows(features []workflow.Feature, onlyVersion string) ([]featureMatrixRow, error) {
	matrix := supportmatrix.GetDSL()
	versions := matrix.Versions()
	if onlyVersion != "" {
		support, ok := matrix.Lookup(onlyVersion)
		if !ok {
			return nil, errors.New("unknown DSL version " + strconv.Quote(onlyVersion))
		}
		versions = []supportmatrix.Version{{
			Version:          onlyVersion,
			Level:            support.Level,
			UnsupportedAfter: support.UnsupportedAfter,
			Replacement:      support.Replacement,
			History:          support.History,
		}}
	}

	var rows []featureMatrixRow
	for _, version := range versions {
		versionFeatures, err := workflow.FeaturesAtDSLVersion(features, version.Version)
		if err != nil {
			return nil, err
		}
		for _, feature := range versionFeatures {
			rows = append(rows, featureMatrixRow{
				DSLVersion: version.Version,
				DSLLevel:   version.Level,
				Feature:    feature,
			})
		}
	}
	return rows, nil
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
	set, report, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		printValidationIssues(stderr, report)
		if errors.Is(err, instance.ErrInvalidConfig) {
			pf(stderr, "error: instance config failed validation: %v\n", err)
			return nil, 1
		}
		pf(stderr, "error: %v\n", err)
		return nil, 2
	}
	printValidationWarnings(stderr, report.CLIWarnings())

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
			addUsedFeature(used, feature)
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
			addUsedFeature(used, feature)
		}
	}

	out := make([]workflow.Feature, 0, len(used))
	for _, feature := range used {
		out = append(out, feature)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, 0
}

func addUsedFeature(used map[workflow.FeatureID]workflow.Feature, feature workflow.Feature) {
	existing, ok := used[feature.ID]
	if !ok {
		used[feature.ID] = feature
		return
	}
	versions := make(map[string]bool, len(existing.DSLVersions))
	for _, support := range existing.DSLVersions {
		versions[support.Version] = true
	}
	for _, support := range feature.DSLVersions {
		if !versions[support.Version] {
			existing.DSLVersions = append(existing.DSLVersions, support)
		}
	}
	used[feature.ID] = existing
}

// writeFeatureTable prints versioned feature rows followed by a count.
func writeFeatureTable(w io.Writer, rows []featureMatrixRow) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	pf(tw, "FEATURE\tDSL VERSION\tFEATURE SUPPORT\tVERSION SUPPORT\tSINCE\n")
	for _, row := range rows {
		pf(tw, "%s\t%s\t%s\t%s\t%s\n",
			row.Feature.ID,
			row.DSLVersion,
			row.Feature.Level,
			row.DSLLevel,
			row.Feature.SinceVersion,
		)
	}
	_ = tw.Flush()
	pf(w, "\n%d feature/version row(s)\n", len(rows))
}
