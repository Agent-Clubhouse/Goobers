// Package instanceannotations folds durable instance annotations for runner lookups.
package instanceannotations

import (
	"fmt"
	"maps"
	"strings"
	"sync"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// Fold is the shared, incremental view used by hot runner
// lookups into the instance journal. The first lookup establishes a sequence
// watermark; subsequent lookups parse only records appended after it (#4863).
type Fold struct {
	mu             sync.Mutex
	seq            uint64
	journalState   journal.InstanceLogState
	initialized    bool
	itemRepos      map[string]ItemRepository
	itemReposByRun map[string]map[string]ItemRepository
	worktreeStates map[string]string
}

var folds sync.Map // scheduler directory -> *Fold

// ForInstance returns the shared incremental fold for a scheduler directory.
func ForInstance(schedulerDir string) *Fold {
	value, _ := folds.LoadOrStore(schedulerDir, &Fold{})
	return value.(*Fold)
}

func (f *Fold) refresh(schedulerDir string) error {
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

func (f *Fold) reset(state journal.InstanceLogState) {
	f.seq = 0
	f.journalState = state
	f.initialized = true
	f.itemRepos = nil
	f.itemReposByRun = nil
	f.worktreeStates = nil
}

func (f *Fold) apply(events []journal.Event) {
	for _, event := range events {
		if event.Seq > f.seq {
			f.seq = event.Seq
		}
		if event.Type != journal.EventRunnerAnnotation {
			continue
		}
		switch event.Runner["annotation"] {
		case ItemRepositoryAnnotation:
			key, _ := event.Runner["key"].(string)
			itemID, _ := event.Runner["itemId"].(string)
			if key == "" || itemID == "" {
				continue
			}
			if f.itemRepos == nil {
				f.itemRepos = make(map[string]ItemRepository)
			}
			provider, _ := event.Runner["provider"].(string)
			owner, _ := event.Runner["owner"].(string)
			project, _ := event.Runner["project"].(string)
			name, _ := event.Runner["name"].(string)
			kind, _ := event.Runner["kind"].(string)
			purpose, _ := event.Runner["purpose"].(string)
			f.itemRepos[key] = ItemRepository{
				Repository: providers.RepositoryRef{Provider: providers.ProviderKind(provider), Owner: owner, Project: project, Name: name},
				Kind:       kind,
				Purpose:    purpose,
			}
			run, keyedItem, ok := strings.Cut(key, "#")
			if ok && run != "" {
				if f.itemReposByRun == nil {
					f.itemReposByRun = make(map[string]map[string]ItemRepository)
				}
				if f.itemReposByRun[run] == nil {
					f.itemReposByRun[run] = make(map[string]ItemRepository)
				}
				entry := f.itemRepos[key]
				repositoryKey, _ := event.Runner["repositoryKey"].(string)
				previous, seen := f.itemReposByRun[run][itemID]
				if repositoryKey == "" || repositoryKey != entry.Repository.CanonicalKey() || seen && previous != entry || keyedItem != itemID || event.RunID != "" && event.RunID != run {
					entry = ItemRepository{}
				}
				f.itemReposByRun[run][itemID] = entry
			}
		}
		f.applyWorktreeState(event)
	}
}

func (f *Fold) applyWorktreeState(event journal.Event) {
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

// WorktreeState returns the latest disposition of one worktree.
func (f *Fold) WorktreeState(schedulerDir, runID, worktreeID string) (string, error) {
	if err := f.refresh(schedulerDir); err != nil {
		return "", fmt.Errorf("read instance log for worktree disposition: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.worktreeStates[runID+"\x00"+worktreeID], nil
}

// ItemRepositories returns the permissive latest-write view for requested items.
func (f *Fold) ItemRepositories(schedulerDir, runID string, itemIDs []string) (map[string]ItemRepository, error) {
	if err := f.refresh(schedulerDir); err != nil {
		return nil, fmt.Errorf("read instance log for item repositories: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	found := make(map[string]ItemRepository, len(itemIDs))
	for _, itemID := range itemIDs {
		if recorded, ok := f.itemRepos[ItemRepositoryKey(runID, itemID)]; ok {
			found[itemID] = recorded
		}
	}
	return found, nil
}

// AllItemRepositories returns a clone of the strict ownership view for a run.
func (f *Fold) AllItemRepositories(schedulerDir, runID string) (map[string]ItemRepository, error) {
	if err := f.refresh(schedulerDir); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.itemReposByRun[runID]), nil
}

// ItemRepository retains the selection-time repository, item kind and the
// claim's declared selection purpose ("" for an ordinary work claim).
type ItemRepository struct {
	Repository providers.RepositoryRef
	Kind       string
	Purpose    string
}

// ItemRepositoryAnnotation identifies a durable selection-time ownership record.
const ItemRepositoryAnnotation = "item-repo"

// ItemRepositoryKey scopes a claimed item's recorded ownership to its run.
func ItemRepositoryKey(runID, itemID string) string { return runID + "#" + itemID }
