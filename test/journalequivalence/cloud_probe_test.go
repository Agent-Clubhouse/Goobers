//go:build cloudintegration

package journalequivalence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/workerhost"
	"github.com/goobers/goobers/internal/worktree"
)

func TestCloudJournalEquivalentToLocal(t *testing.T) {
	for _, scenario := range localCorpus() {
		t.Run(scenario.name, func(t *testing.T) {
			for repetition := 0; repetition < 3; repetition++ {
				t.Run(fmt.Sprint(repetition+1), func(t *testing.T) {
					if _, pending := PendingFixtures()[scenario.name]; pending {
						compareKnownTerminalDivergence(t, scenario)
					} else if scenario.name == "stage-boundary-cancellation" {
						compareCloudDrain(t, scenario)
					} else {
						compareCloudScenario(t, scenario)
					}
				})
			}
		})
	}
}

func compareCloudScenario(t *testing.T, scenario corpusScenario) {
	local := runScenario(t, scenario)
	backend, _, activities, input := cloudScenario(t, scenario)
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(activities)
	request := engine.GateDecisionRequest{Protocol: engine.GateDecisionProtocol, Version: engine.GateDecisionProtocolVersion, RunID: scenario.runID, RequestID: "fixture", Actor: "operator:fixture", Gate: "approval", GateOccurrence: 2, Decision: "pass"}
	emitted := 0
	env.SetOnActivityCompletedListener(func(info *activity.Info, _ converter.EncodedValue, err error) {
		if err != nil || info.ActivityType.Name != engine.ActEmitJournal {
			return
		}
		emitted++
		if emitted == 2 && scenario.name == "human-gate" {
			env.UpdateWorkflow(engine.GateDecisionUpdateName, engine.GateDecisionUpdateID(request), &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
				if err != nil {
					t.Error(err)
				}
			}}, request)
		}
	})

	env.ExecuteWorkflow(engine.Run, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	reader, err := backend.OpenRun(t.Context(), journal.RunRef{ID: scenario.runID})
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCloudEmitKeys(events, scenario.runID); err != nil {
		t.Fatal(err)
	}
	cloud := canonicalReader(t, scenario, reader)
	assertCanonicalEqual(t, local, cloud)
}

// The production worker binds the trusted artifact reporter per invocation.
// Reuse that composition so the cloud fixture exercises receipt publication.
type cloudDeterministic struct {
	script *deterministicScript
	dir    string
	store  blobstore.Store
}

func (s *cloudDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	rec := workerhost.NewStagingArtifactsForInvocation(ctx, s.dir, journal.NewRegistryScrubber(), s.store)
	det := scriptedDeterministic{script: s.script, rec: rec, cancel: func() {}}
	return det.Run(ctx, env, run)
}

func cloudScenario(t *testing.T, scenario corpusScenario) (*journalpg.Backend, *livejournal.Writer, *engine.Activities, engine.RunInput) {
	t.Helper()
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

	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return root, true }, livejournal.WithBackend(backend), livejournal.WithSpanSource(store), livejournal.WithArtifactSource(store))
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
	manager, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	repository := fixtureRepository(t)
	activities := &engine.Activities{Journal: writer, Det: &cloudDeterministic{script: newDeterministicScript(scenario.script), dir: t.TempDir(), store: store}, Goober: scriptedGoober{config: scenario.goober}, Auto: passingAutomated{}, Workspaces: &workerhost.WorktreeWorkspaces{Manager: manager, CloneURL: func(apiv1.RepoRef) (string, error) { return repository, nil }}}

	preview := true
	input := engine.RunInput{RunID: scenario.runID, Gaggle: "equivalence", WorkflowName: doc.Name, Version: 1, DSLVersion: doc.DSLVersion, WorkflowDigest: machine.Digest(), Spec: doc.Spec, RepoRef: fixtureRepoRef(), TriggerKind: string(journal.TriggerManual), PreviewFeaturesEnabled: &preview, LiveJournal: true, HITL: &engine.HITLPolicy{Enabled: true, WaitSeconds: 60, Actors: []string{"operator:fixture"}}}
	return backend, writer, activities, input
}

func assertCanonicalEqual(t *testing.T, local, cloud canonicalRun) {
	t.Helper()
	if !reflect.DeepEqual(local, cloud) {
		t.Errorf("canonical mismatch: cloud=%s local=%s", canonicalDigest(t, cloud), canonicalDigest(t, local))
		for i, event := range cloud.Events {
			if i >= len(local.Events) {
				t.Errorf("extra cloud event[%d]: %s", i, event)
				continue
			}
			if event != local.Events[i] {
				t.Errorf("event[%d] local=%s cloud=%s", i, local.Events[i], event)
			}
		}
		if len(local.Events) != len(cloud.Events) {
			t.Errorf("event counts local=%d cloud=%d", len(local.Events), len(cloud.Events))
		}
		if local.Projection != cloud.Projection {
			t.Errorf("projection local=%s cloud=%s", local.Projection, cloud.Projection)
		}
	}
}
