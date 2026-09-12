package main

import (
	"fmt"
	"sync"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// instanceAnnotationFold is the shared, incremental view used by hot runner
// lookups into the instance journal. The first lookup establishes a sequence
// watermark; subsequent lookups parse only records appended after it (#4863).
type instanceAnnotationFold struct {
	mu             sync.Mutex
	seq            uint64
	journalState   journal.InstanceLogState
	initialized    bool
	itemRepos      map[string]recordedItemRepo
	failureStreaks map[string]int
	keptWorktrees  map[string]bool
}

var instanceAnnotationFolds sync.Map // scheduler directory -> *instanceAnnotationFold

func annotationsForInstance(schedulerDir string) *instanceAnnotationFold {
	value, _ := instanceAnnotationFolds.LoadOrStore(schedulerDir, &instanceAnnotationFold{})
	return value.(*instanceAnnotationFold)
}

func (f *instanceAnnotationFold) refresh(schedulerDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		state, err := journal.ReadInstanceLogState(schedulerDir)
		if err != nil {
			return err
		}
		if !state.Exists {
			f.reset(state)
			return nil
		}
		if !f.initialized || !f.journalState.SameJournal(state) {
			f.reset(state)
		}
		events, err := journal.ReadInstanceLogAfterSeq(schedulerDir, f.seq)
		if err != nil {
			return err
		}
		current, err := journal.ReadInstanceLogState(schedulerDir)
		if err != nil {
			return err
		}
		if !state.SameJournal(current) {
			f.reset(current)
			if !current.Exists {
				return nil
			}
			continue
		}
		f.apply(events)
		f.journalState = current
		return nil
	}
}

func (f *instanceAnnotationFold) reset(state journal.InstanceLogState) {
	f.seq = 0
	f.journalState = state
	f.initialized = true
	f.itemRepos = nil
	f.failureStreaks = nil
	f.keptWorktrees = nil
}

func (f *instanceAnnotationFold) apply(events []journal.Event) {
	for _, event := range events {
		if event.Seq > f.seq {
			f.seq = event.Seq
		}
		if event.Type != journal.EventRunnerAnnotation {
			continue
		}
		switch event.Runner["annotation"] {
		case itemRepoAnnotation:
			key, _ := event.Runner["key"].(string)
			itemID, _ := event.Runner["itemId"].(string)
			if key == "" || itemID == "" {
				continue
			}
			if f.itemRepos == nil {
				f.itemRepos = make(map[string]recordedItemRepo)
			}
			provider, _ := event.Runner["provider"].(string)
			owner, _ := event.Runner["owner"].(string)
			project, _ := event.Runner["project"].(string)
			name, _ := event.Runner["name"].(string)
			kind, _ := event.Runner["kind"].(string)
			f.itemRepos[key] = recordedItemRepo{
				repo: providers.RepositoryRef{Provider: providers.ProviderKind(provider), Owner: owner, Project: project, Name: name},
				kind: kind,
			}
		case failureStreakAnnotation:
			key, _ := event.Runner["key"].(string)
			count, ok := event.Runner["count"].(float64)
			if key == "" || !ok {
				continue
			}
			if f.failureStreaks == nil {
				f.failureStreaks = make(map[string]int)
			}
			f.failureStreaks[key] = int(count)
		}
		if event.RunID != "" && event.Runner["worktreeID"] != nil && event.Runner["worktreeStatus"] == "kept" {
			worktreeID, _ := event.Runner["worktreeID"].(string)
			if worktreeID != "" {
				if f.keptWorktrees == nil {
					f.keptWorktrees = make(map[string]bool)
				}
				f.keptWorktrees[event.RunID+"\x00"+worktreeID] = true
			}
		}
	}
}

func (f *instanceAnnotationFold) itemRepositories(schedulerDir, runID string, itemIDs []string) (map[string]recordedItemRepo, error) {
	if err := f.refresh(schedulerDir); err != nil {
		return nil, fmt.Errorf("read instance log for item repositories: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	found := make(map[string]recordedItemRepo, len(itemIDs))
	for _, itemID := range itemIDs {
		if recorded, ok := f.itemRepos[itemRepoKey(runID, itemID)]; ok {
			found[itemID] = recorded
		}
	}
	return found, nil
}

func (f *instanceAnnotationFold) failureStreak(schedulerDir, key string) (int, error) {
	if err := f.refresh(schedulerDir); err != nil {
		return 0, fmt.Errorf("read instance log for failure streak: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failureStreaks[key], nil
}

func (f *instanceAnnotationFold) worktreeKept(schedulerDir, runID, worktreeID string) (bool, error) {
	if err := f.refresh(schedulerDir); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keptWorktrees[runID+"\x00"+worktreeID], nil
}
