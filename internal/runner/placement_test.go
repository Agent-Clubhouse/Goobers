package runner

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// TestPlacementSelfMatchesInventorySelfName pins journal.PlacementRunnerSelf
// to the runners inventory's implicit self entry (instance.RunnerHostSelfName):
// the journal cannot import instance, so this is the drift guard for the two
// declarations of "self".
func TestPlacementSelfMatchesInventorySelfName(t *testing.T) {
	if journal.PlacementRunnerSelf != instance.RunnerHostSelfName {
		t.Fatalf("journal.PlacementRunnerSelf = %q, instance.RunnerHostSelfName = %q — the self runner has two names",
			journal.PlacementRunnerSelf, instance.RunnerHostSelfName)
	}
}

// TestPlacementHostSurvivesTheJournalRoundTrip: the new host field is carried
// on the runner.* payload like every other placement scalar, so a reader that
// only ever sees the journal can still tell node from host.
func TestPlacementHostSurvivesTheJournalRoundTrip(t *testing.T) {
	event := journal.PlacementEvent("implement", 1, journal.AttemptPolicy, journal.Placement{
		Runner: journal.PlacementRunnerSelf,
		Host:   "build-box-07",
		OS:     "linux",
	})
	if _, ok := event.Runner["node"]; ok {
		t.Fatalf("placement payload invented a node key: %+v", event.Runner)
	}
	decoded, ok := journal.PlacementFromEvent(event)
	if !ok {
		t.Fatalf("PlacementFromEvent rejected %+v", event.Runner)
	}
	if decoded.Host != "build-box-07" || decoded.Node != "" {
		t.Fatalf("decoded placement = %+v, want host=build-box-07 node=\"\"", decoded)
	}
}

// newPlacementTestRunner is newTestRunnerWithDeterministic with the runners:
// inventory declaration wired, so a test can exercise the declared context
// without touching the process environment.
func newPlacementTestRunner(t *testing.T, newDet NewDeterministicFunc, runnersDeclared bool) (*Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: newDet,
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RunnersDeclared:  runnersDeclared,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, runsDir
}

// runPlacementFixture walks the three-attempt retry fixture to completion and
// returns its journal events.
func runPlacementFixture(t *testing.T, runID string, r *Runner, runsDir string, machine *workflow.Machine) []journal.Event {
	t.Helper()
	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	return events
}

// assertPlacementPerAttempt is the shared declared-context assertion: every
// stage.started is immediately followed by ITS runner.placement (same stage,
// attempt, class), each placement decodes to the self identity, and none of
// them is conformance surface.
func assertPlacementPerAttempt(t *testing.T, events []journal.Event, wantCount int) {
	t.Helper()
	var placements []journal.Event
	for i, event := range events {
		switch event.Type {
		case journal.EventStageStarted:
			if i+1 >= len(events) {
				t.Fatalf("stage.started attempt %d is the journal's last event", event.Attempt)
			}
			next := events[i+1]
			if next.Type != journal.EventRunnerPlacement ||
				next.Stage != event.Stage || next.Attempt != event.Attempt ||
				next.AttemptClass != event.AttemptClass {
				t.Fatalf("event after stage.started attempt %d = %s (%s/%d/%s), want its runner.placement",
					event.Attempt, next.Type, next.Stage, next.Attempt, next.AttemptClass)
			}
		case journal.EventRunnerPlacement:
			placements = append(placements, event)
		}
	}
	if len(placements) != wantCount {
		t.Fatalf("runner.placement events = %d, want %d (one per attempt)", len(placements), wantCount)
	}
	for _, event := range placements {
		if event.IsConformanceNormative() {
			t.Fatalf("runner.placement journaled as conformance-normative: %+v", event)
		}
		placement, ok := journal.PlacementFromEvent(event)
		if !ok {
			t.Fatalf("undecodable placement payload: %+v", event.Runner)
		}
		if placement.Runner != journal.PlacementRunnerSelf || placement.OS != runtime.GOOS || placement.Host == "" {
			t.Fatalf("attempt %d placement = %+v, want runner=self os=%s host!=\"\"", event.Attempt, placement, runtime.GOOS)
		}
	}
}

// TestRunnerJournalsNoPlacementWithoutDeclaration is the zero-declaration
// invariance guard (goobernetes-architecture.md §11 item 1) at the emission
// site: an instance that declares no runners: inventory and sets no
// GOOBERS_RUNNER_* env writes the same journal it wrote before placement
// provenance existed — not one extra event. The three exact-event-sequence
// tests (TestRunnerAdvancesFixtureWorkflowToCompletion,
// TestConformanceRunnerResumeRetriesInterruptedAttempt, and e2e
// TestConformanceWalkingSkeletonCrashResume) are the same guard stated as
// pinned sequences; this one states it as an explicit absence.
func TestRunnerJournalsNoPlacementWithoutDeclaration(t *testing.T) {
	t.Setenv(runsemantics.EnvPlacementNode, "")
	t.Setenv(runsemantics.EnvPlacementPod, "")
	t.Setenv(runsemantics.EnvPlacementImage, "")

	machine := retryFixtureMachine(t, 3)
	flaky := &flakyDeterministic{failUntil: 2}
	r, runsDir := newPlacementTestRunner(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, false)

	events := runPlacementFixture(t, "run-placement-undeclared", r, runsDir, machine)
	var started int
	for _, event := range events {
		if event.Type == journal.EventRunnerPlacement {
			t.Fatalf("zero-declaration instance journaled placement provenance: %+v", event)
		}
		if event.Type == journal.EventStageStarted {
			started++
		}
	}
	if started != 3 {
		t.Fatalf("stage.started events = %d, want 3 — the fixture did not retry, so the absence proves nothing", started)
	}
}

// TestRunnerJournalsPlacementWhenInventoryDeclared is the mode-1 end-to-end
// proof for goobernetes-smoke.md §3 item 1 in the DECLARED context an
// inventory creates: every stage attempt — the initial one and each retry —
// journals its own runner.placement immediately after its stage.started, so
// the observer machinery S1–S6 read exists before any distributed substrate
// does.
func TestRunnerJournalsPlacementWhenInventoryDeclared(t *testing.T) {
	t.Setenv(runsemantics.EnvPlacementNode, "")
	t.Setenv(runsemantics.EnvPlacementPod, "")
	t.Setenv(runsemantics.EnvPlacementImage, "")

	machine := retryFixtureMachine(t, 3)
	flaky := &flakyDeterministic{failUntil: 2}
	r, runsDir := newPlacementTestRunner(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, true)

	events := runPlacementFixture(t, "run-placement-inventory", r, runsDir, machine)
	assertPlacementPerAttempt(t, events, 3)
	for _, event := range events {
		if event.Type != journal.EventRunnerPlacement {
			continue
		}
		placement, _ := journal.PlacementFromEvent(event)
		if placement.Node != "" {
			t.Fatalf("inventory-only placement claimed node %q — no authority declared one", placement.Node)
		}
	}
}

// TestRunnerJournalsPlacementWhenEnvDeclared is the other declared context:
// a containerized deployment that supplies its identity through the downward
// API records placement even with no runners: inventory, and the declared node
// (never the hostname) lands in the node field.
func TestRunnerJournalsPlacementWhenEnvDeclared(t *testing.T) {
	t.Setenv(runsemantics.EnvPlacementNode, "aks-linux-0001")
	t.Setenv(runsemantics.EnvPlacementPod, "goobers-daemon-0")
	t.Setenv(runsemantics.EnvPlacementImage, "ghcr.io/goobers/goobers-base:v0.2.0")

	machine := retryFixtureMachine(t, 3)
	flaky := &flakyDeterministic{failUntil: 2}
	r, runsDir := newPlacementTestRunner(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, false)

	events := runPlacementFixture(t, "run-placement-env", r, runsDir, machine)
	assertPlacementPerAttempt(t, events, 3)
	for _, event := range events {
		if event.Type != journal.EventRunnerPlacement {
			continue
		}
		placement, _ := journal.PlacementFromEvent(event)
		if placement.Node != "aks-linux-0001" || placement.Pod != "goobers-daemon-0" ||
			placement.Image != "ghcr.io/goobers/goobers-base:v0.2.0" {
			t.Fatalf("attempt %d placement = %+v, want the deployment-declared identity", event.Attempt, placement)
		}
	}
}
