package journalequivalence

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

// These fixtures have one retained owner and no provider claims. Journal
// ownership, admission and intervention writes use the production services.
type corpusOwner struct{}

func (corpusOwner) Resolve(_, _ string, fallback *runner.Runner) (*runner.Runner, bool) {
	return fallback, false
}
func (corpusOwner) TrackCompatible(string, *runner.Runner) (func(), bool) { return func() {}, true }

type corpusClaims struct{}

func (corpusClaims) History(string, apiv1.Provider) ([]localscheduler.ClaimEntry, error) {
	return nil, nil
}
func (corpusClaims) Reclaim(context.Context, []localscheduler.ClaimEntry, string, string, string) (bool, string, error) {
	return true, "", nil
}
func (corpusClaims) Release(string) error { return nil }

func corpusInterventions(t *testing.T, r *runner.Runner, machine *workflow.Machine, runsDir string) *intervention.Service {
	t.Helper()
	key := localscheduler.WorkflowIdentity{Gaggle: "equivalence", Workflow: machine.Def.Name}
	service := intervention.New(intervention.Config{
		Definitions: func() intervention.Definitions {
			return intervention.Definitions{
				Runners: map[string]*runner.Runner{"equivalence": r}, Machines: map[localscheduler.WorkflowIdentity]*workflow.Machine{key: machine}, RepoRefs: map[localscheduler.WorkflowIdentity]apiv1.RepoRef{key: fixtureRepoRef()},
			}
		}, Runners: corpusOwner{}, Claims: corpusClaims{},
		LocateRun: func(_ []string, runID string, _ bool) (string, string, error) {
			return filepath.Join(runsDir, runID), "equivalence", nil
		},
	})
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := log.Close(); err != nil {
			t.Error(err)
		}
	})
	service.AttachScheduler(localscheduler.New([]localscheduler.WorkflowEntry{{Workflow: machine.Def.Name, Gaggle: "equivalence", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}}}, log))
	return service
}

func startAndOverrideGate(t *testing.T, ctx context.Context, r *runner.Runner, machine *workflow.Machine, runsDir, runID string) runner.Result {
	t.Helper()
	result, err := r.Start(ctx, startInput(runID, machine))
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseEscalated {
		t.Fatalf("override initial phase=%s", result.Phase)
	}
	response, err := corpusInterventions(t, r, machine, runsDir).Override(ctx, httpapi.InterventionRequest{RunID: runID, Stage: "review", Actor: "operator:fixture", IdempotencyKey: "fixture", Rationale: "operator reviewed deterministic fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("override result=%+v", response)
	}
	return runner.Result{Phase: journal.RunPhase(response.Phase), FinalState: response.State}
}

func TestGatelessTerminalResumeRunnerSemantics(t *testing.T) {
	// No operator route reaches this runner primitive today (issue #8149).
	// Keep the lower-level contract, but not in operator parity: cloud's raw
	// terminal intent necessarily adds a keyed intervention marker absent here.
	scenario := corpusScenario{name: "terminal-resume", runID: strings.Repeat("5", 32), fixture: "terminal-resume.yaml", run: startAndResumeTerminal, script: deterministicConfig{businessFailFirst: true}, expect: []journal.EventType{journal.EventRunFinished, journal.EventRunResumed, journal.EventStageStarted}}
	runScenario(t, scenario)
}
