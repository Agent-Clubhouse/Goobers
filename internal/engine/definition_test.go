package engine

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestRunDefinitionAnnotationsRoundTrip(t *testing.T) {
	def := wf.Definition{Name: "annotated", Version: 1, DSLVersion: "3.1", Spec: linearSpec(),
		Annotations: map[string]string{"goobers.dev/allow-preview-features": "true", "example.com/policy": "pinned"}}
	machine, err := wf.Compile(def, wf.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	legacy := def
	legacy.Annotations = nil
	legacyDigest, err := wf.ComputeDigest(legacy)
	if err != nil || legacyDigest == machine.Digest() {
		t.Fatalf("annotations must affect the digest: %s, %v", legacyDigest, err)
	}
	for _, tc := range []struct {
		name       string
		version    workflow.Version
		definition wf.Definition
		digest     string
	}{
		{"new history", 1, def, machine.Digest()},
		{"old version", workflow.DefaultVersion, legacy, legacyDigest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := runInput(def.Name, def.Spec)
			in.Version, in.DSLVersion, in.Annotations, in.WorkflowDigest = def.Version, def.DSLVersion, def.Annotations, machine.Digest()
			payload, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			var decoded RunInput
			if err := json.Unmarshal(payload, &decoded); err != nil {
				t.Fatal(err)
			}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.OnGetVersion(workflowDefinitionAnnotationsChange, workflow.DefaultVersion, 1).Return(tc.version)
			env.RegisterActivity(&Activities{Goober: successInvoker(), Workspaces: testWorkspaces(t)})
			env.ExecuteWorkflow(Run, decoded)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			value, err := env.QueryWorkflow(JournalQuery)
			if err != nil {
				t.Fatal(err)
			}
			var projection JournalProjection
			if err := value.Get(&projection); err != nil {
				t.Fatal(err)
			}
			if projection.Identity.WorkflowDigest != tc.digest {
				t.Fatalf("journal digest %s, want %s", projection.Identity.WorkflowDigest, tc.digest)
			}
			var journaled wf.Definition
			if err := json.Unmarshal(projection.Definition, &journaled); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(journaled, tc.definition) {
				t.Fatalf("journaled definition differs: %+v", journaled)
			}
		})
	}
}

// These checked-in histories predate the annotation field and version marker.
// Replay the production entry so a new marker cannot rewrite old commands.
func TestRunDefinitionAnnotationsReplaysOldHistories(t *testing.T) {
	for _, file := range []string{"testdata/dispatchstage_history.json", "testdata/learning-episode-prechange-history.json", "testdata/continuity-prechange-history.json"} {
		t.Run(file, func(t *testing.T) {
			r := temporalworker.NewWorkflowReplayer()
			r.RegisterWorkflow(Run)
			if err := r.ReplayWorkflowHistoryFromJSONFile(nil, file); err != nil {
				t.Fatal(err)
			}
		})
	}
}
