package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/stateclient"
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

// backlogResweepStateKey is the scheduler-state key holding the re-sweep
// state for one distinct re-sweep shape. A pure function of the shape —
// repository, gaggle, trust label, ready label — so two differently-scoped
// re-sweeps never share state, and the SAME shape reaches the same state
// whether it runs in the daemon's process or in a stage pod talking to the
// scheduler-state plane (Goobers#3898).
//
// This replaces backlogResweepStatePath, which joined the digest onto the
// scheduler directory. The key namespace is a BARE FILENAME on both backends;
// the file store rejoins it onto the same directory, so the on-disk path a
// type-1/type-2 instance uses is byte-identical to the one this function's
// predecessor produced, and an in-flight re-sweep's state survives the change.
func backlogResweepStateKey(
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
	return stateclient.ResweepStateKey(fmt.Sprintf("%x", sum))
}

// decodeBacklogResweepState is loadBacklogResweepState over a scheduler-state
// value: an absent key is the empty state, exactly as a missing file was.
func decodeBacklogResweepState(value stateclient.Value) (backlogResweepState, error) {
	if !value.Exists() {
		return backlogResweepState{LastSweptAt: map[string]time.Time{}}, nil
	}
	var state backlogResweepState
	if err := json.Unmarshal(value.Data, &state); err != nil {
		return backlogResweepState{}, fmt.Errorf("decode backlog re-sweep state: %w", err)
	}
	if state.LastSweptAt == nil {
		state.LastSweptAt = map[string]time.Time{}
	}
	return state, nil
}

func readBacklogResweepState(ctx context.Context, store stateclient.Store, key string) (backlogResweepState, error) {
	value, err := store.Get(ctx, key)
	if err != nil {
		return backlogResweepState{}, err
	}
	return decodeBacklogResweepState(value)
}

// advanceBacklogResweepState publishes this cycle's re-sweep state, and does
// nothing if the stored generation is no longer the one this cycle observed —
// another sweeper already advanced it and this cycle's state would rewind it.
//
// The generation check and the write are ONE read-modify-write: on the file
// backend they are the claims.lock section this has always been, and on the
// scheduler-state plane they are a compare-and-swap the daemon serves under
// that same claims.lock. Identical in shape to advanceBacklogScanCursor, and
// for the identical reason.
func advanceBacklogResweepState(
	ctx context.Context,
	store stateclient.Store,
	key string,
	observedGeneration uint64,
	state backlogResweepState,
) error {
	return store.Update(ctx, key, claimLockOperationBacklogResweep,
		func(value stateclient.Value) ([]byte, bool, error) {
			current, err := decodeBacklogResweepState(value)
			if err != nil {
				return nil, false, err
			}
			if current.Generation != observedGeneration {
				return nil, false, nil
			}
			state.Generation = observedGeneration + 1
			data, err := json.Marshal(state)
			if err != nil {
				return nil, false, fmt.Errorf("marshal backlog re-sweep state: %w", err)
			}
			return data, true, nil
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
