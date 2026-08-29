package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

func TestInstanceUsedFeaturesRoutesGooberByWorkflowDSLVersion(t *testing.T) {
	root := initIntrospectionInstance(t)
	// A second workflow at a DIFFERENT loadable interpreter version, which the
	// coder goober does NOT reference, proves the goober resolver routes by the
	// goober's own referenced-workflow version — not by every workflow in the
	// gaggle. CurrentDSLVersion (1.4) is dropped (#3507), so the other loadable
	// version is V3DSLVersion (3.0); it is preview, so opt the instance in.
	optInPreviewFeatures(t, instance.NewLayout(root))
	writeLegacyV3Workflow(t, filepath.Dir(defaultWorkflowPath(root)))

	const nextOnly workflow.FeatureID = "goober.spec.next-only"
	var versions []string
	features, code := instanceUsedFeaturesWithResolver(
		root,
		&bytes.Buffer{},
		workflow.FeaturesForGaggle,
		func(def workflow.Definition, _ apiv1.GooberSpec) ([]workflow.Feature, error) {
			versions = append(versions, def.DSLVersion)
			if def.DSLVersion != supportmatrix.NextDSLVersion {
				return nil, nil
			}
			return []workflow.Feature{{
				ID: nextOnly,
				DSLVersions: []workflow.DSLFeatureSupport{{
					Version: supportmatrix.NextDSLVersion,
					Level:   workflow.SupportPreview,
				}},
			}}, nil
		},
	)
	if code != 0 {
		t.Fatalf("instanceUsedFeaturesWithResolver code = %d", code)
	}
	if len(versions) != 1 || versions[0] != supportmatrix.NextDSLVersion {
		t.Fatalf("resolver versions = %v, want only %q", versions, supportmatrix.NextDSLVersion)
	}
	if !slices.ContainsFunc(features, func(feature workflow.Feature) bool {
		return feature.ID == nextOnly
	}) {
		t.Fatalf("used features do not contain %q: %+v", nextOnly, features)
	}
}

// TestFeaturesListsBuildMatrix: the bare command prints the full build feature
// matrix — every registry feature, with the table header and a trailing count —
// and needs no instance to do it.
func TestFeaturesListsBuildMatrix(t *testing.T) {
	code, stdout, stderr := runArgs(t, "features")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "FEATURE") || !strings.Contains(stdout, "SUPPORT") || !strings.Contains(stdout, "SINCE") {
		t.Fatalf("output missing table header:\n%s", stdout)
	}
	all := workflow.AllFeatures()
	rowCount := 0
	for _, feature := range all {
		if !strings.Contains(stdout, string(feature.ID)) {
			t.Errorf("feature %q missing from output", feature.ID)
		}
		rowCount += len(feature.DSLVersions)
	}
	if footer := strconv.Itoa(rowCount) + " feature/version row(s)"; !strings.Contains(stdout, footer) {
		t.Errorf("output missing %q count footer:\n%s", footer, stdout)
	}
}

func TestFeaturesScopesToDSLVersion(t *testing.T) {
	// CurrentDSLVersion (1.4) is dropped (#3507): its interpreter is gone and
	// the feature registry carries no 1.4 rows, so scope over the two versions
	// the registry still enumerates — NextDSLVersion (2.0) and V3DSLVersion
	// (3.0).
	for _, version := range []string{supportmatrix.NextDSLVersion, supportmatrix.V3DSLVersion} {
		t.Run(version, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, "features", "--dsl-version", version)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, stderr)
			}
			if !strings.Contains(stdout, "DSL VERSION") || !strings.Contains(stdout, version) {
				t.Fatalf("output missing scoped DSL version:\n%s", stdout)
			}
			features, err := workflow.FeaturesAtDSLVersion(workflow.AllFeatures(), version)
			if err != nil {
				t.Fatal(err)
			}
			if footer := strconv.Itoa(len(features)) + " feature/version row(s)"; !strings.Contains(stdout, footer) {
				t.Errorf("output missing %q count footer:\n%s", footer, stdout)
			}
		})
	}
}

func TestFeaturesRejectsUnknownDSLVersion(t *testing.T) {
	code, stdout, stderr := runArgs(t, "features", "--dsl-version", "9.9")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, `unknown DSL version "9.9"`) {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestFeaturesUsedListsInstanceSubset: --used narrows the matrix to the features
// a real instance references. Every reported feature must be a real registry
// feature, and the set must be a non-empty subset of the full matrix.
func TestFeaturesUsedListsInstanceSubset(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "features", "--used", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "has no schedule trigger; it will not fire autonomously") {
		t.Fatalf("stderr = %q, want config validation warning", stderr)
	}

	known := map[string]bool{}
	for _, feature := range workflow.AllFeatures() {
		known[string(feature.ID)] = true
	}

	usedCount := 0
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "FEATURE" || strings.HasSuffix(line, "feature(s)") {
			continue
		}
		id := fields[0]
		if strings.HasPrefix(id, "goober.") || strings.HasPrefix(id, "workflow.") ||
			strings.HasPrefix(id, "task.") || strings.HasPrefix(id, "trigger.") ||
			strings.HasPrefix(id, "stage.") || strings.HasPrefix(id, "gate.") {
			if !known[id] {
				t.Errorf("reported feature %q is not in the registry", id)
			}
			usedCount++
		}
	}
	if usedCount == 0 {
		t.Fatalf("no features reported as used:\n%s", stdout)
	}
	if usedCount > len(known) {
		t.Fatalf("used feature count %d exceeds full matrix size %d", usedCount, len(known))
	}

	// The demo instance's default workflow must exercise at least its gaggle and
	// start features.
	for _, want := range []string{"workflow.spec.gaggle", "workflow.spec.start"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("expected used feature %q in demo instance output:\n%s", want, stdout)
		}
	}
}

func TestFeaturesUsedPreservesMixedWorkflowVersions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	layout := instance.NewLayout(root)
	// The starter scaffold pins NextDSLVersion (2.0). CurrentDSLVersion (1.4)
	// is dropped (#3507) and no longer loads, so the second interpreter row
	// comes from a V3DSLVersion (3.0) workflow — the other loadable version.
	// 3.0 is preview, so opt the instance in on the Manifest (DVL011 otherwise)
	// and keep the workflow WF022-clean with a single scratch-workspace task.
	optInPreviewFeatures(t, layout)
	writeLegacyV3Workflow(t, filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows"))

	code, stdout, stderr := runArgs(t, "features", "--used", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	const feature = "workflow.spec.gaggle"
	seen := map[string]bool{}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == feature {
			seen[fields[1]] = true
		}
	}
	if len(seen) != 2 {
		t.Fatalf("%s versions = %v, want one row per interpreter version:\n%s", feature, seen, stdout)
	}
	for _, version := range []string{supportmatrix.NextDSLVersion, supportmatrix.V3DSLVersion} {
		if !seen[version] {
			t.Errorf("output missing %s row for DSL %s:\n%s", feature, version, stdout)
		}
	}
}

// writeLegacyV3Workflow drops a minimal, WF022-clean V3DSLVersion (3.0)
// workflow into workflowsDir — a single scratch-workspace deterministic task,
// so it needs no goober and declares no repo handoff. It is the "second
// interpreter version" the mixed-version features tests need now that 1.4 is
// gone. The instance must already carry the preview opt-in (optInPreviewFeatures).
func writeLegacyV3Workflow(t *testing.T, workflowsDir string) {
	t.Helper()
	legacy := "apiVersion: goobers.dev/v1alpha1\n" +
		"kind: Workflow\n" +
		"dslVersion: \"" + supportmatrix.V3DSLVersion + "\"\n" +
		"metadata:\n" +
		"  name: legacy-implement\n" +
		"spec:\n" +
		"  gaggle: example\n" +
		"  triggers:\n" +
		"    - type: manual\n" +
		"  start: noop\n" +
		"  tasks:\n" +
		"    - name: noop\n" +
		"      type: deterministic\n" +
		"      goal: A no-op task exercising the 3.0 interpreter.\n" +
		"      run:\n" +
		"        command: [\"true\"]\n" +
		"        workspace: scratch\n"
	if err := os.WriteFile(filepath.Join(workflowsDir, "legacy-implement.yaml"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
}

// optInPreviewFeatures adds the Manifest opt-in a preview-level dslVersion pin
// (DSL 3.0) requires — without it the pin is refused with DVL011.
func optInPreviewFeatures(t *testing.T, layout instance.Layout) {
	t.Helper()
	manifestPath := filepath.Join(layout.ConfigDir(), "manifest.yaml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	const anchor = "metadata:\n  name: example-instance\n"
	updated := strings.Replace(string(raw), anchor,
		anchor+"  annotations:\n    "+workflow.PreviewFeaturesAnnotation+": \"true\"\n", 1)
	if updated == string(raw) {
		t.Fatalf("scaffolded manifest missing expected metadata anchor:\n%s", raw)
	}
	if err := os.WriteFile(manifestPath, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFeaturesUsedIncludesGaggleScopedFeatures: gaggle-scoped features live on
// the GaggleSpec, not on any workflow, so --used only sees them through the
// FeaturesForGaggle fan-out (#3297) — before that wiring, a gaggle declaring a
// sparse checkout reported nothing gaggle-scoped at all. Sparse checkout is
// the GA gaggle feature, so the instance loads without a preview
// acknowledgement.
func TestFeaturesUsedIncludesGaggleScopedFeatures(t *testing.T) {
	root := initIntrospectionInstance(t)
	replaceInFile(t, filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml"),
		"    branch: main",
		"    branch: main\n    checkout:\n      sparse:\n        - docs")

	code, stdout, stderr := runArgs(t, "features", "--used", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	const feature = "gaggle.spec.project.checkout.sparse"
	if !strings.Contains(stdout, feature) {
		t.Fatalf("gaggle-scoped feature %q missing from --used output:\n%s", feature, stdout)
	}
}

// TestFeaturesUsedRejectsNonInstance: --used on a directory that is not an
// instance root fails with a usage/IO exit code and a clear diagnostic.
func TestFeaturesUsedRejectsNonInstance(t *testing.T) {
	code, stdout, stderr := runArgs(t, "features", "--used", t.TempDir())
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("stderr = %q, want a not-an-instance diagnostic", stderr)
	}
}

// TestFeaturesRejectsExtraArg: at most one positional path is accepted.
func TestFeaturesRejectsExtraArg(t *testing.T) {
	code, _, stderr := runArgs(t, "features", "a", "b")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "goobers features") {
		t.Fatalf("stderr = %q, want usage", stderr)
	}
}
