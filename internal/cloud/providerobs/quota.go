// Package providerobs adapts the scheduler's in-memory provider state to the
// durable observation store the single-active controller restores on every
// lease acquisition (control.Runtime, HA-P3 group D #7818).
//
// Only quota windows are persisted. Open-PR counts are rebuilt by the first
// refresh poll and are cheap to re-derive; a quota window is not, and losing
// it after a takeover would re-spend a provider's polling budget the previous
// leader had already used.
package providerobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/localscheduler"
)

const (
	// passthroughMaxAge bounds how long a foreign or unknown-version row is carried
	// forward without its writer refreshing it. A newer binary that is still active
	// rewrites its rows every L1 tick, so a day-old row is an orphan of a binary
	// that was rolled back, not live state.
	passthroughMaxAge = 24 * time.Hour
	// maxObservations is control.Observations' snapshot bound.
	maxObservations = 128
	quotaKeyPrefix  = "provider-quota/"
	quotaVersion    = 1
)

type quotaPayload struct {
	Version   int       `json:"version"`
	Provider  string    `json:"provider"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"resetAt"`
}

// Quota persists a provider quota ledger. The same State must be the one the
// scheduler and provider clients record into.
//
// control.Observations.Replace swaps the WHOLE snapshot, so rows this adapter
// does not own (another adapter's keys, or a newer binary's payload version
// seen during a mixed-version failover) are carried through untouched: Restore
// remembers them and Capture re-emits them.
type Quota struct {
	State *localscheduler.ProviderQuotaState
	// Now defaults to time.Now.
	Now func() time.Time
	// Report receives rows that were skipped as corrupt. The ledger is an
	// optimisation, so a bad row is reported and dropped, never fatal.
	Report func(error)

	mu          sync.Mutex
	passthrough []control.Observation
}

func (q *Quota) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

func (q *Quota) report(err error) {
	if q.Report != nil {
		q.Report(err)
	}
}

// fresh reports whether a row we do not own is young enough to carry forward.
func (q *Quota) fresh(observation control.Observation, now time.Time) bool {
	return !observation.ObservedAt.IsZero() && now.Sub(observation.ObservedAt) <= passthroughMaxAge
}

// Capture snapshots the active quota windows for control.Observations.Replace,
// followed by the foreign rows Restore passed through.
func (q *Quota) Capture(context.Context) ([]control.Observation, error) {
	if q.State == nil {
		return nil, fmt.Errorf("provider quota observations require a quota state")
	}
	now := q.now()
	var observations []control.Observation
	owned := map[string]bool{}
	for _, window := range q.State.Windows() {
		if !window.ResetAt.After(now) {
			continue // an expired window carries no budget worth restoring
		}
		payload, err := json.Marshal(quotaPayload{Version: quotaVersion, Provider: string(window.Provider), Remaining: window.Remaining, ResetAt: window.ResetAt.UTC()})
		if err != nil {
			return nil, err
		}
		key := quotaKeyPrefix + string(window.Provider)
		owned[key] = true
		observations = append(observations, control.Observation{Key: key, Payload: payload, ObservedAt: now.UTC()})
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, foreign := range q.passthrough {
		if owned[foreign.Key] {
			continue // a current v1 window supersedes an unknown-version row for its key
		}
		if len(observations) >= maxObservations {
			q.report(fmt.Errorf("provider observation %q dropped: snapshot is full", foreign.Key))
			continue
		}
		observations = append(observations, foreign)
	}
	return observations, nil
}

// Restore replays a predecessor's snapshot into the ledger. Windows already
// expired are skipped, and Record ratchets (a lower remaining in the same
// window wins, an older window loses), so a replay never adds budget back.
//
// A restore never fails the controller's epoch. The version is decoded FIRST: a
// payload of a version this binary does not know (a newer binary's shape) and
// keys this adapter does not own are kept untouched for Capture to re-emit, and
// a corrupt row of the known version is reported and skipped.
func (q *Quota) Restore(_ context.Context, snapshot []control.Observation) error {
	if q.State == nil {
		return fmt.Errorf("provider quota observations require a quota state")
	}
	now := q.now()
	var passthrough []control.Observation
	for _, observation := range snapshot {
		provider, owned := strings.CutPrefix(observation.Key, quotaKeyPrefix)
		if !owned {
			if q.fresh(observation, now) {
				passthrough = append(passthrough, observation)
			}
			continue
		}
		var head struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(observation.Payload, &head); err != nil {
			q.report(fmt.Errorf("provider observation %q is corrupt: %w", observation.Key, err))
			continue
		}
		if head.Version != quotaVersion {
			if q.fresh(observation, now) {
				passthrough = append(passthrough, observation)
			}
			continue
		}
		var payload quotaPayload
		if err := json.Unmarshal(observation.Payload, &payload); err != nil {
			q.report(fmt.Errorf("provider observation %q is corrupt: %w", observation.Key, err))
			continue
		}
		if payload.Provider != provider || provider == "" || payload.Remaining < 0 || payload.ResetAt.IsZero() {
			q.report(fmt.Errorf("provider observation %q is invalid", observation.Key))
			continue
		}
		if !payload.ResetAt.After(now) {
			continue
		}
		q.State.Record(apiv1.Provider(provider), payload.Remaining, payload.ResetAt)
	}
	q.mu.Lock()
	q.passthrough = passthrough
	q.mu.Unlock()
	return nil
}
