package main

import (
	"fmt"
	"maps"
	"strings"
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
	itemReposByRun map[string]map[string]recordedItemRepo
	worktreeStates map[string]string
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
	f.itemReposByRun = nil
	f.worktreeStates = nil
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
			run, keyedItem, ok := strings.Cut(key, "#")
			if ok && run != "" {
				if f.itemReposByRun == nil {
					f.itemReposByRun = make(map[string]map[string]recordedItemRepo)
				}
				if f.itemReposByRun[run] == nil {
					f.itemReposByRun[run] = make(map[string]recordedItemRepo)
				}
				entry := f.itemRepos[key]
				repositoryKey, _ := event.Runner["repositoryKey"].(string)
				previous, seen := f.itemReposByRun[run][itemID]
				if repositoryKey == "" || repositoryKey != entry.repo.CanonicalKey() || seen && previous != entry || keyedItem != itemID || event.RunID != "" && event.RunID != run {
					entry = recordedItemRepo{}
				}
				f.itemReposByRun[run][itemID] = entry
			}
		}
		f.applyWorktreeState(event)
	}
}

func (f *instanceAnnotationFold) applyWorktreeState(event journal.Event) {
	if event.RunID == "" || event.Runner["worktreeID"] == nil {
		return
	}
	worktreeID, _ := event.Runner["worktreeID"].(string)
	worktreeStatus, _ := event.Runner["worktreeStatus"].(string)
	if worktreeID == "" || worktreeStatus == "" {
		return
	}
	if f.worktreeStates == nil {
		f.worktreeStates = make(map[string]string)
	}
	key := event.RunID + "\x00" + worktreeID
	f.worktreeStates[key] = worktreeStatus
}

func (f *instanceAnnotationFold) worktreeState(schedulerDir, runID, worktreeID string) (string, error) {
	if err := f.refresh(schedulerDir); err != nil {
		return "", fmt.Errorf("read instance log for worktree disposition: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.worktreeStates[runID+"\x00"+worktreeID], nil
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

func (f *instanceAnnotationFold) allItemRepositories(schedulerDir, runID string) (map[string]recordedItemRepo, error) {
	if err := f.refresh(schedulerDir); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.itemReposByRun[runID]), nil
}
