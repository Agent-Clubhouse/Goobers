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
		// Reads the annotation fold directly (not loadFailureStreakCount,
		// which is now scheduler-state-KV-first per Goobers#3025) since this
		// test measures the shared instance-annotation fold's own tail-read
		// cost across all three annotation kinds it still covers.
		repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}
		if _, err := annotationsForInstance(layout.SchedulerDir()).failureStreak(layout.SchedulerDir(), failureStreakKey(repo, "17")); err != nil {
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
		if count, err := annotationsForInstance(layout.SchedulerDir()).failureStreak(layout.SchedulerDir(), failureStreakKey(repo, "17")); err != nil || count != 4 {
			t.Fatalf("failure streak = %d, %v", count, err)
		}
		if kept, err := keptWorktreeJournaled(layout.SchedulerDir(), "run-1", "worktree-1"); err != nil || !kept {
			t.Fatalf("kept worktree = %v, %v", kept, err)
		}
		work := readprobe.Take()
		readprobe.Disable()
		if work.InstanceTailReads != 3 {
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

func TestInstanceAnnotationFoldResetsAcrossCompactionRestartAndRemoval(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithClock(func() time.Time { return old }))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{
		"annotation": failureStreakAnnotation, "key": failureStreakKey(repo, "17"), "count": 4,
	}}); err != nil {
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
	if count, err := fold.failureStreak(layout.SchedulerDir(), failureStreakKey(repo, "17")); err != nil || count != 4 {
		t.Fatalf("pre-compaction count = %d, %v; want 4", count, err)
	}
	if _, err := journal.CompactInstanceEvents(layout.SchedulerDir(), recent.Add(-time.Hour), recent.Add(-time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if count, err := fold.failureStreak(layout.SchedulerDir(), failureStreakKey(repo, "17")); err != nil || count != 0 {
		t.Fatalf("post-compaction retained count = %d, %v; want 0", count, err)
	}
	if count, err := new(instanceAnnotationFold).failureStreak(layout.SchedulerDir(), failureStreakKey(repo, "17")); err != nil || count != 0 {
		t.Fatalf("restart count = %d, %v; want 0", count, err)
	}
	if err := os.RemoveAll(layout.SchedulerDir()); err != nil {
		t.Fatal(err)
	}
	if count, err := fold.failureStreak(layout.SchedulerDir(), failureStreakKey(repo, "17")); err != nil || count != 0 {
		t.Fatalf("removed-journal count = %d, %v; want 0", count, err)
	}
}

func TestInstanceAnnotationFoldResetsWhenJournalIsRecreatedBeforeNextRead(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}
	key := failureStreakKey(repo, "17")
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{
		"annotation": failureStreakAnnotation, "key": key, "count": 4,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	fold := new(instanceAnnotationFold)
	if count, err := fold.failureStreak(layout.SchedulerDir(), key); err != nil || count != 4 {
		t.Fatalf("initial count = %d, %v; want 4", count, err)
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
	if count, err := fold.failureStreak(layout.SchedulerDir(), key); err != nil || count != 0 {
		t.Fatalf("recreated-journal count = %d, %v; want 0", count, err)
	}
}
