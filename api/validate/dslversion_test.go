package validate

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
)

// syntheticDSLMatrix registers every lifecycle level so checkWorkflowDSLVersion
// can be exercised end to end — the live, compiled-in supportmatrix only ever
// carries "supported" entries (DVL-3, #863).
func syntheticDSLMatrix(t *testing.T) {
	t.Helper()
	original := dslSupportMatrix
	t.Cleanup(func() { dslSupportMatrix = original })
	dslSupportMatrix = func() supportmatrix.SupportMatrix {
		return supportmatrix.SupportMatrix{
			"1.4": {Level: supportmatrix.LevelSupported},
			"1.2": {Level: supportmatrix.LevelDeprecated, Replacement: "1.4", UnsupportedAfter: "2.9"},
			"1.0": {Level: supportmatrix.LevelUnsupported, Replacement: "1.4"},
			"2.0": {Level: supportmatrix.LevelPreview},
		}
	}
}

func dslWorkflow(name, version string) apiv1.Workflow {
	w := apiv1.Workflow{
		DSLVersion: version,
		Spec:       apiv1.WorkflowSpec{Gaggle: "web"},
	}
	w.Name = name
	return w
}

func TestCheckWorkflowDSLVersionMissingPinDefaultsAndWarns(t *testing.T) {
	syntheticDSLMatrix(t)
	r := &Report{}
	checkWorkflowDSLVersion(r, dslWorkflow("w", ""), "w.yaml", false)

	if r.HasErrors() {
		t.Fatalf("missing dslVersion must not fail validation: %v", r.Issues)
	}
	warnings := r.Warnings()
	if len(warnings) != 1 || warnings[0].Code != WarningMissingDSLVersion {
		t.Fatalf("warnings = %+v, want a single DVL001", warnings)
	}
	if !strings.Contains(warnings[0].Explanation, `defaulting to "1.4"`) {
		t.Errorf("explanation = %q, want it to name the default version", warnings[0].Explanation)
	}
}

func TestCheckWorkflowDSLVersionSupportedIsSilent(t *testing.T) {
	syntheticDSLMatrix(t)
	r := &Report{}
	checkWorkflowDSLVersion(r, dslWorkflow("w", "1.4"), "w.yaml", false)
	if len(r.Issues) != 0 {
		t.Fatalf("issues = %+v, want none for a supported pin", r.Issues)
	}
}

func TestCheckWorkflowDSLVersionDeprecatedWarnsWithReplacement(t *testing.T) {
	syntheticDSLMatrix(t)
	r := &Report{}
	checkWorkflowDSLVersion(r, dslWorkflow("w", "1.2"), "w.yaml", false)

	if r.HasErrors() {
		t.Fatalf("deprecated pin must not fail validation: %v", r.Issues)
	}
	warnings := r.Warnings()
	if len(warnings) != 1 || warnings[0].Code != WarningDeprecatedDSLVersion {
		t.Fatalf("warnings = %+v, want a single DVL020", warnings)
	}
	for _, want := range []string{`replacement "1.4"`, "unsupported after 2.9", "goobers fix --to 1.4"} {
		if !strings.Contains(warnings[0].Explanation, want) {
			t.Errorf("explanation = %q, want it to contain %q", warnings[0].Explanation, want)
		}
	}
}

func TestCheckWorkflowDSLVersionUnsupportedFailsWithCode(t *testing.T) {
	syntheticDSLMatrix(t)
	r := &Report{}
	checkWorkflowDSLVersion(r, dslWorkflow("w", "1.0"), "w.yaml", false)

	if !r.HasErrors() {
		t.Fatal("unsupported pin must fail validation")
	}
	var found bool
	for _, issue := range r.Issues {
		if issue.Code == ErrorUnsupportedDSLVersion && issue.Severity == Error {
			found = true
			if !strings.Contains(issue.Message, `replacement "1.4"`) {
				t.Errorf("message = %q, want it to name the replacement", issue.Message)
			}
		}
	}
	if !found {
		t.Fatalf("issues = %+v, want a DVL030 error", r.Issues)
	}
}

func TestCheckWorkflowDSLVersionUnrecognizedVersionFails(t *testing.T) {
	syntheticDSLMatrix(t)
	r := &Report{}
	checkWorkflowDSLVersion(r, dslWorkflow("w", "9.9"), "w.yaml", false)

	if !r.HasErrors() {
		t.Fatal("an unrecognized dslVersion must fail validation")
	}
	var found bool
	for _, issue := range r.Issues {
		if issue.Code == ErrorUnsupportedDSLVersion {
			found = true
			if !strings.Contains(issue.Message, "known versions:") {
				t.Errorf("message = %q, want it to list known versions", issue.Message)
			}
		}
	}
	if !found {
		t.Fatalf("issues = %+v, want a DVL030 error", r.Issues)
	}
}

func TestCheckWorkflowDSLVersionPreviewBlockedByDefault(t *testing.T) {
	syntheticDSLMatrix(t)
	r := &Report{}
	checkWorkflowDSLVersion(r, dslWorkflow("w", "2.0"), "w.yaml", false)

	if !r.HasErrors() {
		t.Fatal("a preview pin without opt-in must fail validation — closed by default")
	}
	var found bool
	for _, issue := range r.Issues {
		if issue.Code == ErrorPreviewDSLVersionBlocked {
			found = true
		}
	}
	if !found {
		t.Fatalf("issues = %+v, want a DVL011 error", r.Issues)
	}
}

func TestCheckWorkflowDSLVersionPreviewOptedInWarnsOnly(t *testing.T) {
	syntheticDSLMatrix(t)
	r := &Report{}
	checkWorkflowDSLVersion(r, dslWorkflow("w", "2.0"), "w.yaml", true)

	if r.HasErrors() {
		t.Fatalf("an opted-in preview pin must not fail validation: %v", r.Issues)
	}
	warnings := r.Warnings()
	if len(warnings) != 1 || warnings[0].Code != WarningPreviewDSLVersionOptedIn {
		t.Fatalf("warnings = %+v, want a single DVL010", warnings)
	}
}
