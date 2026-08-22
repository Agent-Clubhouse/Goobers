package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
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

// TestSelfPlacementCapturesSubstrateIdentity: the self capture records what
// this process can actually know — GOOS and host always; pod/image (and a
// deployment-declared node) only when the deployment says so via env.
func TestSelfPlacementCapturesSubstrateIdentity(t *testing.T) {
	t.Setenv(EnvPlacementNode, "")
	t.Setenv(EnvPlacementPod, "")
	t.Setenv(EnvPlacementImage, "")
	bare := selfPlacement()
	if bare.Runner != journal.PlacementRunnerSelf || bare.OS != runtime.GOOS {
		t.Fatalf("bare placement = %+v, want runner=self os=%s", bare, runtime.GOOS)
	}
	if host, err := os.Hostname(); err == nil && bare.Node != host {
		t.Fatalf("bare placement node = %q, want hostname %q", bare.Node, host)
	}
	if bare.Pod != "" || bare.Image != "" {
		t.Fatalf("bare placement invented pod/image identity: %+v", bare)
	}

	t.Setenv(EnvPlacementNode, "aks-linux-0001")
	t.Setenv(EnvPlacementPod, "goobers-daemon-0")
	t.Setenv(EnvPlacementImage, "ghcr.io/goobers/goobers-base:v0.2.0")
	declared := selfPlacement()
	if declared.Node != "aks-linux-0001" || declared.Pod != "goobers-daemon-0" ||
		declared.Image != "ghcr.io/goobers/goobers-base:v0.2.0" {
		t.Fatalf("deployment-declared placement = %+v", declared)
	}
}

// TestRunnerJournalsPlacementPerStageAttempt is the mode-1 end-to-end proof
// for goobernetes-smoke.md §3 item 1: every stage attempt — the initial one
// and each retry — journals its own runner.placement event immediately after
// its stage.started, carrying the self runner identity, so the observer
// machinery S1–S6 read exists before any distributed substrate does.
func TestRunnerJournalsPlacementPerStageAttempt(t *testing.T) {
	machine := retryFixtureMachine(t, 3)
	flaky := &flakyDeterministic{failUntil: 2}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-placement-provenance",
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

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-placement-provenance"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	var placements []journal.Event
	for i, event := range events {
		switch event.Type {
		case journal.EventStageStarted:
			// The provenance must follow ITS attempt's stage.started: same
			// stage, attempt, and class, journaled before the attempt does
			// anything else.
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
	if len(placements) != 3 {
		t.Fatalf("runner.placement events = %d, want 3 (one per attempt)", len(placements))
	}
	for _, event := range placements {
		if event.IsConformanceNormative() {
			t.Fatalf("runner.placement journaled as conformance-normative: %+v", event)
		}
		placement, ok := journal.PlacementFromEvent(event)
		if !ok {
			t.Fatalf("undecodable placement payload: %+v", event.Runner)
		}
		if placement.Runner != journal.PlacementRunnerSelf || placement.OS != runtime.GOOS || placement.Node == "" {
			t.Fatalf("attempt %d placement = %+v, want runner=self os=%s node!=\"\"", event.Attempt, placement, runtime.GOOS)
		}
	}
}
