//go:build cloudintegration

package journalequivalence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestCloudJournalEquivalenceProbe(t *testing.T) {
	config := cloudJournalConfig(t)
	var role string
	if err := config.Pool.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "goobers_controller" {
		t.Fatalf("current_user=%q", role)
	}
	backend, err := journalpg.New(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range localCorpus() {
		if scenario.name != "human-gate" {
			continue
		}
		t.Run(scenario.name, func(t *testing.T) {
			local := runScenario(t, scenario)
			root := t.TempDir()
			writer, err := livejournal.NewWriter(func(string) (string, bool) { return root, true }, livejournal.WithBackend(backend))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(writer.Close)
			data, err := os.ReadFile(filepath.Join("testdata", scenario.fixture))
			if err != nil {
				t.Fatal(err)
			}
			var doc apiv1.Workflow
			if err := yaml.UnmarshalStrict(data, &doc); err != nil {
				t.Fatal(err)
			}
			machine := loadMachine(t, scenario.fixture)
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(&engine.Activities{Journal: writer})
			request := engine.GateDecisionRequest{Protocol: engine.GateDecisionProtocol, Version: engine.GateDecisionProtocolVersion, RunID: scenario.runID, RequestID: "fixture", Actor: "operator:fixture", Gate: "approval", GateOccurrence: 2, Decision: "pass"}
			emitted := 0
			env.SetOnActivityCompletedListener(func(info *activity.Info, _ converter.EncodedValue, err error) {
				if err != nil || info.ActivityType.Name != engine.ActEmitJournal {
					return
				}
				emitted++
				if emitted == 2 {
					env.UpdateWorkflow(engine.GateDecisionUpdateName, engine.GateDecisionUpdateID(request), &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
						if err != nil {
							t.Error(err)
						}
					}}, request)
				}
			})
			preview := true
			env.ExecuteWorkflow(engine.Run, engine.RunInput{RunID: scenario.runID, Gaggle: "equivalence", WorkflowName: doc.Name, Version: 1, DSLVersion: doc.DSLVersion, WorkflowDigest: machine.Digest(), Spec: doc.Spec, RepoRef: fixtureRepoRef(), TriggerKind: string(journal.TriggerManual), PreviewFeaturesEnabled: &preview, LiveJournal: true, HITL: &engine.HITLPolicy{Enabled: true, WaitSeconds: 60, Actors: []string{"operator:fixture"}}})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			reader, err := backend.OpenRun(t.Context(), journal.RunRef{ID: scenario.runID})
			if err != nil {
				t.Fatal(err)
			}
			cloud := canonicalReader(t, scenario, reader)
			for name, value := range map[string]canonicalRun{"local": local, "cloud": cloud} {
				data, err := json.MarshalIndent(value, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join("/tmp", "p3-7872-"+name+"-human.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if canonicalDigest(t, cloud) != canonicalDigest(t, local) {
				t.Fatalf("canonical mismatch: cloud=%s local=%s", canonicalDigest(t, cloud), canonicalDigest(t, local))
			}
		})
	}
}
