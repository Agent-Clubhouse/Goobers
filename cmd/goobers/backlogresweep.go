package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const defaultBacklogResweepInterval = 24 * time.Hour

type backlogResweepPolicy struct {
	maxItems   int
	interval   time.Duration
	readyLabel string
}

type backlogResweepState struct {
	Generation    uint64               `json:"generation"`
	LastSweepAt   time.Time            `json:"lastSweepAt,omitempty"`
	LastSweptAt   map[string]time.Time `json:"lastSweptAt,omitempty"`
	Cursor        string               `json:"cursor,omitempty"`
	BlockedCursor string               `json:"blockedCursor,omitempty"`
}

func compactLabels(values ...string) []string {
	labels := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, label := range values {
		label = strings.TrimSpace(label)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		labels = append(labels, label)
	}
	return labels
}

func readBacklogResweepPolicy(maxItems int) (backlogResweepPolicy, bool, error) {
	rawMax := strings.TrimSpace(providerInput("resweepMaxItems", ""))
	if rawMax == "" {
		return backlogResweepPolicy{}, false, nil
	}
	resweepMax, err := strconv.Atoi(rawMax)
	if err != nil || resweepMax < 1 || resweepMax > maxItems {
		return backlogResweepPolicy{}, false, fmt.Errorf(
			"invalid resweepMaxItems %q (want an integer from 1 through maxItems=%d)",
			rawMax,
			maxItems,
		)
	}
	rawInterval := strings.TrimSpace(providerInput("resweepInterval", defaultBacklogResweepInterval.String()))
	interval, err := time.ParseDuration(rawInterval)
	if err != nil || interval <= 0 {
		return backlogResweepPolicy{}, false, fmt.Errorf(
			"invalid resweepInterval %q (want a positive duration)",
			rawInterval,
		)
	}
	readyLabel := strings.TrimSpace(providerInput("resweepReadyLabel", providers.LabelReady))
	if readyLabel == "" {
		return backlogResweepPolicy{}, false, errors.New("resweepReadyLabel must not be empty")
	}
	return backlogResweepPolicy{maxItems: resweepMax, interval: interval, readyLabel: readyLabel}, true, nil
}

func backlogResweepStatePath(
	schedulerDir string,
	repo providers.RepositoryRef,
	gaggle, trustLabel, readyLabel string,
) string {
	key, _ := json.Marshal(struct {
		Repository providers.RepositoryRef `json:"repository"`
		Gaggle     string                  `json:"gaggle,omitempty"`
		TrustLabel string                  `json:"trustLabel"`
		ReadyLabel string                  `json:"readyLabel"`
	}{
		Repository: repo,
		Gaggle:     gaggle,
		TrustLabel: trustLabel,
		ReadyLabel: readyLabel,
	})
	sum := sha256.Sum256(key)
	return filepath.Join(schedulerDir, fmt.Sprintf("backlog-resweep-%x.json", sum))
}

func loadBacklogResweepState(path string) (backlogResweepState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return backlogResweepState{LastSweptAt: map[string]time.Time{}}, nil
	}
	if err != nil {
		return backlogResweepState{}, err
	}
	var state backlogResweepState
	if err := json.Unmarshal(data, &state); err != nil {
		return backlogResweepState{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if state.LastSweptAt == nil {
		state.LastSweptAt = map[string]time.Time{}
	}
	return state, nil
}

func readBacklogResweepState(lockPath, statePath string) (backlogResweepState, error) {
	var state backlogResweepState
	err := withClaimLock(lockPath, claimLockOperationBacklogResweep, func() error {
		var err error
		state, err = loadBacklogResweepState(statePath)
		return err
	})
	return state, err
}

func advanceBacklogResweepState(
	lockPath, statePath string,
	observedGeneration uint64,
	state backlogResweepState,
) error {
	return withClaimLock(lockPath, claimLockOperationBacklogResweep, func() error {
		current, err := loadBacklogResweepState(statePath)
		if err != nil {
			return err
		}
		if current.Generation != observedGeneration {
			return nil
		}
		state.Generation = observedGeneration + 1
		data, err := json.Marshal(state)
		if err != nil {
			return fmt.Errorf("marshal backlog re-sweep state: %w", err)
		}
		if err := journal.WriteFileAtomic(statePath, data, 0o644); err != nil {
			return fmt.Errorf("write backlog re-sweep state: %w", err)
		}
		return nil
	})
}

func backlogResweepDue(state backlogResweepState, observedAt time.Time, interval time.Duration) bool {
	return state.LastSweepAt.IsZero() || !observedAt.Before(state.LastSweepAt.Add(interval))
}

func sortBacklogResweepCandidates(
	items []providers.WorkItem,
	priorityLabels []string,
	fieldOrder fieldpredicate.Order,
	lastSweptAt map[string]time.Time,
) error {
	if err := sortEligibleByFields(items, priorityLabels, fieldOrder); err != nil {
		return err
	}
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := itemPriorityRank(items[i], priorityLabels), itemPriorityRank(items[j], priorityLabels)
		if ri != rj {
			return ri < rj
		}
		ti, tj := lastSweptAt[items[i].ID], lastSweptAt[items[j].ID]
		if ti.Equal(tj) {
			return false
		}
		if ti.IsZero() {
			return true
		}
		if tj.IsZero() {
			return false
		}
		return ti.Before(tj)
	})
	return nil
}

func recordBacklogResweep(
	state backlogResweepState,
	selected []providers.WorkItem,
	observedAt time.Time,
	interval time.Duration,
) backlogResweepState {
	if state.LastSweptAt == nil {
		state.LastSweptAt = map[string]time.Time{}
	}
	retainFor := max(30*24*time.Hour, 4*interval)
	for id, sweptAt := range state.LastSweptAt {
		if observedAt.Sub(sweptAt) > retainFor {
			delete(state.LastSweptAt, id)
		}
	}
	state.LastSweepAt = observedAt.UTC()
	for _, item := range selected {
		state.LastSweptAt[item.ID] = observedAt.UTC()
	}
	return state
}
