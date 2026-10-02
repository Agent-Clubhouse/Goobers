package main

import (
	"strings"
	"testing"

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
		if kept, err := worktreeDispositionJournaled(layout.SchedulerDir(), "run-1", "worktree-1", "kept"); err != nil || !kept {
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
