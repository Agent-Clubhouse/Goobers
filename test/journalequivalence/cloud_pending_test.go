//go:build cloudintegration

package journalequivalence

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/workflow"
)

// #7899 owns terminal interventions on the live journal. Pin the known
// acknowledge-then-fail behavior without permitting unrelated differences:
// every event and the full projection before intervention must still match.
func compareKnownTerminalDivergence(t *testing.T, scenario corpusScenario) {
	issue, known := PendingFixtures()[scenario.name]
	if !known {
		t.Fatal("unregistered known divergence")
	}
	local := runScenario(t, scenario)
	if canonicalDigest(t, local) != localBaselineDigests[scenario.name] {
		t.Fatal("local fixture drifted")
	}
	initial := scenario
	initial.expect = nil
	initial.run = func(t *testing.T, ctx context.Context, r *runner.Runner, machine *workflow.Machine, _ string, runID string) runner.Result {
		result, err := r.Start(ctx, startInput(runID, machine))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	localBefore := runScenario(t, initial)
	backend, _, activities, input := cloudScenario(t, scenario)
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(activities)
	intent := engine.HITLIntent{Protocol: engine.HITLProtocol, Version: engine.HITLProtocolVersion, RunID: scenario.runID, RequestID: "fixture", Actor: "operator:fixture", ExpectedTerminalGeneration: 1}
	if scenario.name == "gate-fail-rerun" {
		intent.Kind = engine.HITLRerunStage
		intent.Stage = "review"
		intent.InstructionAddendum = "accept the deterministic fixture"
	} else {
		intent.Kind = engine.HITLResumeFromTerminal
		intent.Target = "recover"
		intent.Rationale = "retry deterministic fixture"
	}
	accepted, acknowledged := false, false
	var before canonicalRun
	env.RegisterDelayedCallback(func() {
		reader, err := backend.OpenRun(t.Context(), journal.RunRef{ID: scenario.runID})
		if err != nil {
			t.Fatal(err)
		}
		before = canonicalReader(t, initial, reader)
		assertCanonicalEqual(t, localBefore, before)
		env.UpdateWorkflow(engine.HITLUpdateName, engine.HITLIntentUpdateID(intent), &testsuite.TestUpdateCallback{
			OnAccept: func() { accepted = true },
			OnReject: func(err error) { t.Errorf("known failure changed to up-front refusal: %v", err) },
			OnComplete: func(_ any, err error) {
				if err != nil {
					t.Errorf("update did not acknowledge: %v", err)
				} else {
					acknowledged = true
				}
			},
		}, intent)
	}, 10*time.Second)
	env.ExecuteWorkflow(engine.Run, input)
	err := env.GetWorkflowError()
	if !accepted || !acknowledged || err == nil || !strings.Contains(err.Error(), livejournal.ErrTerminal.Error()) {
		t.Fatalf("pending #%s failure changed: accepted=%v acknowledged=%v workflowErr=%v; qualify and remove the pending fixture", issue, accepted, acknowledged, err)
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
	after := canonicalReader(t, initial, reader)
	assertCanonicalEqual(t, before, after)
	if reflect.DeepEqual(local, after) {
		t.Fatalf("pending #%s no longer diverges; remove the exemption", issue)
	}
	// This is a passing regression for the exact known bug, NOT a claim that
	// these terminal intervention fixtures are equivalent. CI checks the issue
	// is still open; ordinary failures above remain failures, never skips.
	t.Logf("KNOWN divergence pending #%s: %s; Temporal update acknowledged, then live journal append refused", issue, scenario.name)
}
