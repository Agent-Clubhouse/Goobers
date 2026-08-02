package runner

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
		Worktrees: manager,
		RunsDir:   filepath.Join(root, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return repo, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	machine := humanGateFixtureMachine(t)
	ref := humanGateRepoRef()
	ref.Checkout = &apiv1.CheckoutSpec{Mode: apiv1.CheckoutModePinned}
	start := func(runID string) (Result, error) {
		return r.Start(context.Background(), StartInput{
			RunID: runID, Machine: machine, Gaggle: "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: ref,
		})
	}

	first, err := start("pinned-paused-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Phase != journal.PhaseRunning || first.FinalState != "approval" {
		t.Fatalf("first Start result = %+v, want running at approval", first)
	}

	secondDone := make(chan struct{})
	var second Result
	var secondErr error
	go func() {
		second, secondErr = start("pinned-paused-2")
		close(secondDone)
	}()

	deadline := time.Now().Add(2 * time.Second)
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

	resumed, err := r.Resume(context.Background(), ResumeInput{
		RunID: "pinned-paused-1", Machine: machine, RepoRef: ref,
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
	case <-time.After(2 * time.Second):
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
