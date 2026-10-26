package workercontract

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workflow/ir"
)

func sourceIR(t *testing.T, dsl string) ir.Document {
	t.Helper()
	doc, err := ir.Normalize(workflow.Definition{Name: "sample", Version: 1, DSLVersion: dsl,
		Spec: apiv1.WorkflowSpec{Gaggle: "g", Start: "build", Tasks: []apiv1.Task{{Name: "build", Type: apiv1.TaskAgentic, Goal: "build", Goober: "coder"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestInspectExactSourceAndRefuseUnverifiableIR(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ir.Document)
		state  string
	}{
		{"exact", func(*ir.Document) {}, "compatible"},
		{"source-less", func(d *ir.Document) { d.SourceDefinition = nil }, "unverifiable"},
		{"forged-source-digest", func(d *ir.Document) { d.Source.Digest = "sha256:unverified" }, "unverifiable"},
		{"different-graph", func(d *ir.Document) { d.Nodes[0].Task.Goal = "another goal" }, "unverifiable"},
		{"unknown-compiler", func(d *ir.Document) { d.Compiler.Version = "workflow-ir/v99" }, "unverifiable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := sourceIR(t, "2.0")
			tc.change(&doc)
			got := Inspect(doc, false)
			if got.State != tc.state || got.Enforced {
				t.Fatalf("inspection = %+v", got)
			}
			if tc.state == "unverifiable" && got.SourceDigest != "" {
				t.Fatalf("unverified digest presented as evidence: %+v", got)
			}
		})
	}
}

func TestInspectPreviewAndRequirementsUseCompilerAuthority(t *testing.T) {
	doc := sourceIR(t, "3.1")
	doc.SourceDefinition.Spec.Tasks[0].ArtifactSlots = []apiv1.ArtifactSlot{{Name: "report", MediaType: "application/json", MaxSize: 2048}}
	var err error
	doc, err = ir.Normalize(*doc.SourceDefinition)
	if err != nil {
		t.Fatal(err)
	}
	if got := Inspect(doc, false); got.State != "incompatible" {
		t.Fatalf("preview without opt-in = %+v", got)
	}
	doc.FeatureGates = []string{"fabricated.runtime.feature"}
	got := Inspect(doc, true)
	if got.State != "compatible" || len(got.Requirements) == 0 {
		t.Fatalf("preview with opt-in = %+v", got)
	}
	for _, feature := range got.Requirements {
		if feature.ID == doc.FeatureGates[0] {
			t.Fatal("cosmetic feature gate became a runtime requirement")
		}
	}
}

func TestManifestDeclaresExecutableAndRegisteredContracts(t *testing.T) {
	manifest, err := Current("build-a", "sha256:artifact")
	if err != nil {
		t.Fatal(err)
	}
	first, err := manifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.IRSchema != ir.SchemaVersion || manifest.Compiler.Version != ir.CompilerVersion {
		t.Fatalf("manifest = %+v", manifest)
	}
	for _, dsl := range manifest.DSL {
		if dsl.Version == "1.4" {
			t.Fatal("removed interpreter advertised")
		}
	}
	manifest.ExecutableDigest = "sha256:different-artifact"
	second, err := manifest.Digest()
	if err != nil || first == second || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("digest %q / %q: %v", first, second, err)
	}
}
