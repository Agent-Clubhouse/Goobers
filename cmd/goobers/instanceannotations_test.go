package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readprobe"
	"github.com/goobers/goobers/providers"
)

func TestHotAnnotationReadsCostDoesNotGrowWithInstanceHistory(t *testing.T) {
	measure := func(t *testing.T, historySize int) uint64 {
		t.Helper()
		layout := instance.NewLayout(t.TempDir())
		log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
		if err != nil {
			t.Fatal(err)
		}
		for range historySize {
			if err := log.Append(journal.Event{Type: journal.EventTickSkipped, Reason: strings.Repeat("padding", 256)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}

		// Establish the fold and its sequence watermark before the hot read.
		repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}
		if _, err := loadItemRepositories(layout, "run-0", []string{"0"}); err != nil {
			t.Fatal(err)
		}
		log, _, err = journal.OpenInstanceLog(layout.SchedulerDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range []journal.Event{
			{Type: journal.EventRunnerAnnotation, RunID: "run-1", Runner: map[string]any{
				"annotation": itemRepoAnnotation, "key": itemRepoKey("run-1", "17"), "itemId": "17",
				"provider": string(repo.Provider), "owner": repo.Owner, "name": repo.Name, "kind": "issue",
			}},
			// A legacy failure-streak annotation the fold no longer parses
			// (Goobers#3025 moved that state onto the scheduler-state KV
			// plane): still journaled for audit, and must not cost the fold
			// anything or make it choke on an annotation kind it no longer
			// understands.
			{Type: journal.EventRunnerAnnotation, RunID: "run-1", Runner: map[string]any{
				"annotation": failureStreakAnnotation, "key": failureStreakKey(repo, "17"), "count": 4,
			}},
			{Type: journal.EventRunnerAnnotation, RunID: "run-1", Runner: map[string]any{
				"worktreeID": "worktree-1", "worktreeStatus": "kept",
			}},
		} {
			if err := log.Append(event); err != nil {
				t.Fatal(err)
			}
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}

		readprobe.Enable()
		t.Cleanup(readprobe.Disable)
		if found, err := loadItemRepositories(layout, "run-1", []string{"17"}); err != nil || found["17"].repo != repo {
			t.Fatalf("item repositories = %+v, %v", found, err)
		}
		if kept, err := keptWorktreeJournaled(layout.SchedulerDir(), "run-1", "worktree-1"); err != nil || !kept {
			t.Fatalf("kept worktree = %v, %v", kept, err)
		}
		work := readprobe.Take()
		readprobe.Disable()
		if work.InstanceTailReads != 2 {
			t.Fatalf("instance tail reads = %d, want one per lookup", work.InstanceTailReads)
		}
		return work.InstanceTailBytes
	}

	short := measure(t, 64)
	long := measure(t, 640)
	if long > short+512 {
		t.Fatalf("hot annotation reads parsed %d bytes with short history and %d with 10x history", short, long)
	}
}

// itemRepoAnnotationEvent builds a journal.EventRunnerAnnotation the fold's
// itemRepositories records, used below as the vehicle for exercising the
// fold's generic reset/compaction/restart behavior (any annotation kind the
// fold understands would do; failure-streak moved off this fold entirely
// onto the scheduler-state KV plane per Goobers#3025).
func itemRepoAnnotationEvent(runID, itemID string, repo providers.RepositoryRef) journal.Event {
	return journal.Event{Type: journal.EventRunnerAnnotation, RunID: runID, Runner: map[string]any{
		"annotation": itemRepoAnnotation, "key": itemRepoKey(runID, itemID), "itemId": itemID,
		"provider": string(repo.Provider), "owner": repo.Owner, "name": repo.Name, "kind": "issue",
	}}
}

func recordedRepo(fold *instanceAnnotationFold, schedulerDir, runID, itemID string) (providers.RepositoryRef, error) {
	found, err := fold.itemRepositories(schedulerDir, runID, []string{itemID})
	if err != nil {
		return providers.RepositoryRef{}, err
	}
	return found[itemID].repo, nil
}

func TestInstanceAnnotationFoldResetsAcrossCompactionRestartAndRemoval(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithClock(func() time.Time { return old }))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(itemRepoAnnotationEvent("run-1", "17", repo)); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	log, _, err = journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithClock(func() time.Time { return recent }))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	fold := annotationsForInstance(layout.SchedulerDir())
	if got, err := recordedRepo(fold, layout.SchedulerDir(), "run-1", "17"); err != nil || got != repo {
		t.Fatalf("pre-compaction repo = %+v, %v; want %+v", got, err, repo)
	}
	if _, err := journal.CompactInstanceEvents(layout.SchedulerDir(), recent.Add(-time.Hour), recent.Add(-time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if got, err := recordedRepo(fold, layout.SchedulerDir(), "run-1", "17"); err != nil || got != (providers.RepositoryRef{}) {
		t.Fatalf("post-compaction retained repo = %+v, %v; want zero value", got, err)
	}
	if got, err := recordedRepo(new(instanceAnnotationFold), layout.SchedulerDir(), "run-1", "17"); err != nil || got != (providers.RepositoryRef{}) {
		t.Fatalf("restart repo = %+v, %v; want zero value", got, err)
	}
	if err := os.RemoveAll(layout.SchedulerDir()); err != nil {
		t.Fatal(err)
	}
	if got, err := recordedRepo(fold, layout.SchedulerDir(), "run-1", "17"); err != nil || got != (providers.RepositoryRef{}) {
		t.Fatalf("removed-journal repo = %+v, %v; want zero value", got, err)
	}
}

func TestInstanceAnnotationFoldResetsWhenJournalIsRecreatedBeforeNextRead(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(itemRepoAnnotationEvent("run-1", "17", repo)); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	fold := new(instanceAnnotationFold)
	if got, err := recordedRepo(fold, layout.SchedulerDir(), "run-1", "17"); err != nil || got != repo {
		t.Fatalf("initial repo = %+v, %v; want %+v", got, err, repo)
	}

	// Recreate generation zero without giving the fold an opportunity to see
	// the journal while it is absent. Path+generation alone cannot distinguish
	// this from the former file.
	if err := os.RemoveAll(layout.SchedulerDir()); err != nil {
		t.Fatal(err)
	}
	log, _, err = journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := recordedRepo(fold, layout.SchedulerDir(), "run-1", "17"); err != nil || got != (providers.RepositoryRef{}) {
		t.Fatalf("recreated-journal repo = %+v, %v; want zero value", got, err)
	}
}
