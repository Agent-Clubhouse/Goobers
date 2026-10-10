package engine

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
)

// These same fixtures exercise the migrator. Running the untouched 2.0/3.0
// documents and the 3.1 output proves the pin does not rename or reorder
// positional artifacts, even when expectedOutputs names exist. The producer
// emits those names because DSL 3.1 enforces them for shell stages (#5175).
func TestDSL31MigrationPreservesLegacyArtifactExecution(t *testing.T) {
	for _, version := range []string{"2.0", "3.0", "3.1"} {
		t.Run(version, func(t *testing.T) {
			source, err := os.ReadFile("../dslmigrate/testdata/v" + strings.ReplaceAll(version, ".", "_") + "/legacy-artifacts.yaml")
			if err != nil {
				t.Fatal(err)
			}
			var doc apiv1.Workflow
			if err := yaml.UnmarshalStrict(source, &doc); err != nil {
				t.Fatal(err)
			}
			artifacts := []apiv1.ArtifactPointer{
				{Path: "stages/produce/1/z.json", Digest: apiv1.Digest([]byte("first")), Size: 5, MediaType: "application/json", Integrity: apiv1.IntegrityDerived},
				{Path: "stages/produce/1/a.log", Digest: apiv1.Digest([]byte("second")), Size: 6, MediaType: "text/plain", Integrity: apiv1.IntegrityDerived},
			}
			var captured apiv1.InvocationEnvelope
			det := &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				if strings.HasSuffix(env.TaskID, ":produce") {
					return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"report": "ok", "trace": "ok"}, Artifacts: artifacts}, nil
				}
				captured = env
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}}
			in := runInput(doc.Name, doc.Spec)
			in.DSLVersion = doc.DSLVersion
			in.Gaggle = doc.Spec.Gaggle
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(&Activities{Det: det, Workspaces: testWorkspaces(t)})
			env.ExecuteWorkflow(Run, in)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var result RunResult
			if err := env.GetWorkflowResult(&result); err != nil {
				t.Fatal(err)
			}
			if result.Status != StatusCompleted || !reflect.DeepEqual(result.Outputs["produce"].Artifacts, artifacts) {
				t.Fatalf("legacy artifact result drift: %+v", result)
			}
			if len(captured.ContextPointers) != len(artifacts) {
				t.Fatalf("context pointers = %+v", captured.ContextPointers)
			}
			for i, name := range []string{"produce.artifact[0]", "produce.artifact[1]"} {
				pointer := captured.ContextPointers[i]
				if pointer.Name != name || pointer.Artifact == nil || *pointer.Artifact != artifacts[i] {
					t.Fatalf("positional artifact %d changed: %+v", i, pointer)
				}
			}
			if captured.Inputs["evidence"] != "produce.artifact[0]" || captured.Inputs["trace"] != "produce.artifact[1]" {
				t.Fatalf("legacy inputs changed: %+v", captured.Inputs)
			}
		})
	}
}

// The engine applies the same DSL 3.1 expectedOutputs contract as the local
// runner (#5175): the producer fails before its successor runs, while the
// unchanged 3.0 document keeps the advisory behaviour.
func TestDSL31EngineEnforcesExpectedOutputs(t *testing.T) {
	for version, wantStatus := range map[string]string{"3.0": StatusCompleted, "3.1": StatusFailed} {
		t.Run(version, func(t *testing.T) {
			source, err := os.ReadFile("../dslmigrate/testdata/v" + strings.ReplaceAll(version, ".", "_") + "/legacy-artifacts.yaml")
			if err != nil {
				t.Fatal(err)
			}
			var doc apiv1.Workflow
			if err := yaml.UnmarshalStrict(source, &doc); err != nil {
				t.Fatal(err)
			}
			consumed := false
			det := &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				if strings.HasSuffix(env.TaskID, ":produce") {
					return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"report": "ok"}}, nil
				}
				consumed = true
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}}
			in := runInput(doc.Name, doc.Spec)
			in.DSLVersion = doc.DSLVersion
			in.Gaggle = doc.Spec.Gaggle
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(&Activities{Det: det, Workspaces: testWorkspaces(t)})
			env.ExecuteWorkflow(Run, in)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var result RunResult
			if err := env.GetWorkflowResult(&result); err != nil {
				t.Fatal(err)
			}
			if result.Status != wantStatus || consumed != (wantStatus == StatusCompleted) {
				t.Fatalf("status = %s consumed = %v, want %s: %+v", result.Status, consumed, wantStatus, result)
			}
			if wantStatus == StatusFailed && (result.FailureCode != runner.MissingExpectedOutputsCode || result.FinalState != "produce" ||
				!strings.Contains(result.FailureMessage, `["trace"]`)) {
				t.Fatalf("failure = %s at %s: %s", result.FailureCode, result.FinalState, result.FailureMessage)
			}
		})
	}
}
