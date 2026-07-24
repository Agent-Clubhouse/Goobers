package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

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
	for _, version := range []string{supportmatrix.CurrentDSLVersion, supportmatrix.NextDSLVersion} {
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
	currentPath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
	raw, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	next := strings.Replace(string(raw), "kind: Workflow\n", "kind: Workflow\ndslVersion: \""+supportmatrix.NextDSLVersion+"\"\n", 1)
	next = strings.Replace(next, "name: default-implement", "name: next-implement", 1)
	if err := os.WriteFile(filepath.Join(filepath.Dir(currentPath), "next-implement.yaml"), []byte(next), 0o644); err != nil {
		t.Fatal(err)
	}

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
	for _, version := range []string{supportmatrix.CurrentDSLVersion, supportmatrix.NextDSLVersion} {
		if !seen[version] {
			t.Errorf("output missing %s row for DSL %s:\n%s", feature, version, stdout)
		}
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
