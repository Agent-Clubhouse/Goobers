package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestPinnedLeaseSpansHumanGatePauseAndResume(t *testing.T) {
	root := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}

	repo := newFixtureRepo(t)
	r, err := New(Config{
		PinnedWorkspace: true,
		Worktrees:       manager,
		RunsDir:         filepath.Join(root, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return repo, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	machine := humanGateFixtureMachine(t)
	ref := humanGateRepoRef()
	start := func(ctx context.Context, runID string) (Result, error) {
		return r.Start(ctx, StartInput{
			RunID: runID, Machine: machine, Gaggle: "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: ref,
		})
	}

	first, err := start(context.Background(), "pinned-paused-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Phase != journal.PhaseRunning || first.FinalState != "approval" {
		t.Fatalf("first Start result = %+v, want running at approval", first)
	}

	secondDone := make(chan struct{})
	var second Result
	var secondErr error
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	go func() {
		second, secondErr = start(secondCtx, "pinned-paused-2")
		close(secondDone)
	}()

	deadline := time.Now().Add(runnerTestWaitTimeout)
	for {
		rd, openErr := journal.OpenRead(filepath.Join(root, "runs", "pinned-paused-2"))
		if openErr == nil {
			events, eventsErr := rd.Events()
			if eventsErr != nil {
				t.Fatal(eventsErr)
			}
			for _, event := range events {
				if event.Type == journal.EventRunnerAnnotation && event.Runner["queuePosition"] == float64(2) {
					goto queued
				}
			}

		}
		if time.Now().After(deadline) {
			t.Fatal("second run did not report waiting for the pinned lease")
		}
		time.Sleep(10 * time.Millisecond)
	}

queued:
	select {
	case <-secondDone:
		t.Fatal("second run entered the pinned workspace while the first was paused")
	default:
	}
	const stalledTimeout = 250 * time.Millisecond
	time.Sleep(2 * stalledTimeout)
	if result, escalated, err := r.EscalateStalled("pinned-paused-2", time.Now(), stalledTimeout); err != nil {
		t.Fatal(err)
	} else if escalated || result.Phase != journal.PhaseRunning {
		t.Fatalf("queued run after stall timeout: escalated=%v result=%+v", escalated, result)
	}

	resumed, err := r.Resume(context.Background(), ResumeInput{
		RunID: "pinned-paused-1", RepoRef: ref,
		HumanDecision: &HumanGateDecision{
			Gate: "approval", PauseSeq: latestHumanPauseSeq(t, filepath.Join(root, "runs"), "pinned-paused-1", "approval"), Decision: "pass",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Phase != journal.PhaseCompleted {
		t.Fatalf("first Resume phase = %s, want completed", resumed.Phase)
	}

	select {
	case <-secondDone:
	case <-time.After(runnerTestWaitTimeout):
		cancelSecond()
		<-secondDone
		t.Fatal("second run did not acquire the pinned lease after the first completed")
	}
	if secondErr != nil {
		t.Fatal(secondErr)
	}
	if second.Phase != journal.PhaseRunning || second.FinalState != "approval" {
		t.Fatalf("second Start result = %+v, want running at approval", second)
	}

	second, err = r.Resume(context.Background(), ResumeInput{
		RunID: "pinned-paused-2", Machine: machine, RepoRef: ref,
		HumanDecision: &HumanGateDecision{
			Gate: "approval", PauseSeq: latestHumanPauseSeq(t, filepath.Join(root, "runs"), "pinned-paused-2", "approval"), Decision: "pass",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Phase != journal.PhaseCompleted {
		t.Fatalf("second Resume phase = %s, want completed", second.Phase)
	}
}

func TestPinnedFailureStreakSuggestsResetWithoutResetting(t *testing.T) {
	root := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	repo := newFixtureRepo(t)
	byTask := make(map[string]stubTaskResult)
	for i := 1; i <= worktree.PinnedFailureResetThreshold; i++ {
		runID := "pinned-failure-" + string(rune('0'+i))
		byTask[runID+":implement"] = stubTaskResult{
			status: apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{
				Code:    "build_failed",
				Message: "build failed",
			},
		}
	}
	r, err := New(Config{
		PinnedWorkspace: true,
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: manager,
		RunsDir:   filepath.Join(root, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return repo, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := apiv1.RepoRef{
		Provider: apiv1.ProviderGitHub,
		Owner:    "acme",
		Name:     "web",
		Branch:   "main",
	}

	var markerPath string
	for i := 1; i <= worktree.PinnedFailureResetThreshold; i++ {
		runID := "pinned-failure-" + string(rune('0'+i))
		result, err := r.Start(context.Background(), StartInput{
			RunID: runID, Machine: terminalFailMachine(t), Gaggle: "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: ref,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Phase != journal.PhaseFailed {
			t.Fatalf("%s phase = %s, want failed", runID, result.Phase)
		}
		rd, err := journal.OpenRead(filepath.Join(root, "runs", runID))
		if err != nil {
			t.Fatal(err)
		}
		events, err := rd.Events()
		if err != nil {
			t.Fatal(err)
		}
		suggested := false
		for _, event := range events {
			if event.Type == journal.EventRunnerAnnotation &&
				event.Runner["kind"] == "workspace_reset_suggested" {
				suggested = true
			}
		}
		if suggested != (i == worktree.PinnedFailureResetThreshold) {
			t.Fatalf("%s reset suggestion = %v", runID, suggested)
		}
		if i == 1 {
			pins, err := filepath.Glob(filepath.Join(root, "workcopies", "*", "pin"))
			if err != nil || len(pins) != 1 {
				t.Fatalf("find pinned workspace: paths=%v err=%v", pins, err)
			}
			markerPath = filepath.Join(pins[0], "persistent-build-state")
			if err := os.WriteFile(markerPath, []byte("warm"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("pinned workspace was automatically reset: %v", err)
	}
}
