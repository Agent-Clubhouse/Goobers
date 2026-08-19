package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
	buildversion "github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/internal/workflow"
)

const featuresHelp = "Usage: goobers features [--json] [--dsl-version <version>] [--used] [path]\n\n" +
	"List the workflow-DSL features this build understands by DSL version,\n" +
	"including each feature's support level (preview/ga/deprecated/removed).\n" +
	"Use --dsl-version to scope the matrix to one declared version. This reads\n" +
	"the same registry and SupportMatrix the committed\n" +
	"docs/feature-matrix.md is generated from, so the two never disagree.\n\n" +
	"With --json, emit a versioned feature-discovery envelope instead of the\n" +
	"human-readable table. " +
	"With --used, list only the features the instance at path (default \".\")\n" +
	"actually references across its workflows and goobers — the subset that\n" +
	"instance's config exercises. Exit codes: 0 = OK, 1 = invalid instance\n" +
	"config, 2 = usage/IO error.\n"

func runFeatures(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("features", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit a versioned machine-readable feature-discovery envelope")
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
	if *asJSON {
		if err := encodeSchemaJSON(stdout, schemas.Features, newFeaturesEnvelope(rows, *dslVersion, *usedOnly)); err != nil {
			pf(stderr, "error: encode features: %v\n", err)
			return 1
		}
		return 0
	}
	writeFeatureTable(stdout, rows)
	return 0
}

type featureJSON struct {
	Name         string `json:"name"`
	Stability    string `json:"stability"`
	SinceVersion string `json:"sinceVersion"`
	Used         *bool  `json:"used,omitempty"`
}

type featuresEnvelope struct {
	SchemaVersion string        `json:"schemaVersion"`
	Version       string        `json:"version"`
	Commit        string        `json:"commit"`
	DSLVersion    string        `json:"dslVersion"`
	Features      []featureJSON `json:"features"`
}

func newFeaturesEnvelope(rows []featureMatrixRow, dslVersion string, usedOnly bool) featuresEnvelope {
	info := buildversion.Get()
	if dslVersion == "" {
		dslVersion = "all"
	}
	byName := make(map[string]featureJSON, len(rows))
	for _, row := range rows {
		name := string(row.Feature.ID)
		if _, exists := byName[name]; exists {
			continue
		}
		feature := featureJSON{
			Name:         name,
			Stability:    string(row.Feature.Level),
			SinceVersion: row.Feature.SinceVersion,
		}
		if usedOnly {
			used := true
			feature.Used = &used
		}
		byName[name] = feature
	}
	features := make([]featureJSON, 0, len(byName))
	for _, feature := range byName {
		features = append(features, feature)
	}
	sort.Slice(features, func(i, j int) bool { return features[i].Name < features[j].Name })
	return featuresEnvelope{
		SchemaVersion: featuresSchemaVersion,
		Version:       info.Version,
		Commit:        info.Commit,
		DSLVersion:    dslVersion,
		Features:      features,
	}
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
	return instanceUsedFeaturesWithResolver(root, stderr, workflow.FeaturesForGaggle, workflow.FeaturesForGoober)
}

type gaggleFeatureResolver func(workflow.Definition, apiv1.GaggleSpec) ([]workflow.Feature, error)

type gooberFeatureResolver func(workflow.Definition, apiv1.GooberSpec) ([]workflow.Feature, error)

func instanceUsedFeaturesWithResolver(
	root string,
	stderr io.Writer,
	resolveGaggle gaggleFeatureResolver,
	resolveGoober gooberFeatureResolver,
) ([]workflow.Feature, int) {
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
	// Gaggle-scoped features (sandbox posture, sparse checkout) live on the
	// GaggleSpec, not on any workflow, so without this fan-out they are
	// invisible to --used (#3297).
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		for _, def := range featureDefinitionsForGaggle(set.Workflows, g.Name) {
			features, err := resolveGaggle(def, g.Spec)
			if err != nil {
				pf(stderr, "error: gaggle %q: %v\n", g.Name, err)
				return nil, 1
			}
			for _, feature := range features {
				addUsedFeature(used, feature)
			}
		}
	}
	for i := range set.Goobers {
		g := &set.Goobers[i]
		for _, def := range featureDefinitionsForGoober(set.Workflows, g.Spec) {
			features, err := resolveGoober(def, g.Spec)
			if err != nil {
				pf(stderr, "error: goober %q: %v\n", g.Name, err)
				return nil, 1
			}
			for _, feature := range features {
				addUsedFeature(used, feature)
			}
		}
	}

	out := make([]workflow.Feature, 0, len(used))
	for _, feature := range used {
		out = append(out, feature)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, 0
}

// featureDefinitionsForGaggle adapts the loaded workflow list to the shared
// per-DSL-pin fan-out (workflow.FeatureDefinitionsByDSLVersion, #3297) so this
// CLI and api/validate cannot drift on version-resolution policy — including
// the workflow-less fallback to the newest supported version.
func featureDefinitionsForGaggle(workflows []apiv1.Workflow, gaggle string) []workflow.Definition {
	var definitions []workflow.Definition
	for i := range workflows {
		wf := &workflows[i]
		if wf.Spec.Gaggle != gaggle {
			continue
		}
		definitions = append(definitions, workflow.Definition{
			Name: wf.Name, DSLVersion: wf.DSLVersion, Spec: wf.Spec,
		})
	}
	return workflow.FeatureDefinitionsByDSLVersion(definitions)
}

func featureDefinitionsForGoober(workflows []apiv1.Workflow, spec apiv1.GooberSpec) []workflow.Definition {
	referenced := make(map[string]bool, len(spec.Workflows))
	for _, name := range spec.Workflows {
		referenced[name] = true
	}
	matching := make([]apiv1.Workflow, 0, len(spec.Workflows))
	for i := range workflows {
		wf := workflows[i]
		if wf.Spec.Gaggle == spec.Gaggle && referenced[wf.Name] {
			matching = append(matching, wf)
		}
	}
	return featureDefinitionsForGaggle(matching, spec.Gaggle)
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
