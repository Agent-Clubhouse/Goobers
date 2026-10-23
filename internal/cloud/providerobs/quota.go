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
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/localscheduler"
)

const (
	quotaKeyPrefix = "provider-quota/"
	quotaVersion   = 1
)

type quotaPayload struct {
	Version   int       `json:"version"`
	Provider  string    `json:"provider"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"resetAt"`
}

// Quota persists a provider quota ledger. The same State must be the one the
// scheduler and provider clients record into.
type Quota struct {
	State *localscheduler.ProviderQuotaState
	// Now defaults to time.Now.
	Now func() time.Time
}

func (q Quota) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// Capture snapshots the active quota windows for control.Observations.Replace.
func (q Quota) Capture(context.Context) ([]control.Observation, error) {
	if q.State == nil {
		return nil, fmt.Errorf("provider quota observations require a quota state")
	}
	now := q.now()
	var observations []control.Observation
	for _, window := range q.State.Windows() {
		if !window.ResetAt.After(now) {
			continue // an expired window carries no budget worth restoring
		}
		payload, err := json.Marshal(quotaPayload{Version: quotaVersion, Provider: string(window.Provider), Remaining: window.Remaining, ResetAt: window.ResetAt.UTC()})
		if err != nil {
			return nil, err
		}
		observations = append(observations, control.Observation{Key: quotaKeyPrefix + string(window.Provider), Payload: payload, ObservedAt: now.UTC()})
	}
	return observations, nil
}

// Restore replays a predecessor's snapshot into the ledger. Windows already
// expired are skipped, and Record ratchets (a lower remaining in the same
// window wins, an older window loses), so a replay never adds budget back.
// Keys this adapter does not own, and payload versions it does not know, are
// ignored so a newer or sibling adapter's state cannot block a controller; a
// malformed payload of a known version is an error (corruption is visible).
func (q Quota) Restore(_ context.Context, snapshot []control.Observation) error {
	if q.State == nil {
		return fmt.Errorf("provider quota observations require a quota state")
	}
	now := q.now()
	for _, observation := range snapshot {
		provider, owned := strings.CutPrefix(observation.Key, quotaKeyPrefix)
		if !owned {
			continue
		}
		var payload quotaPayload
		if err := json.Unmarshal(observation.Payload, &payload); err != nil {
			return fmt.Errorf("provider observation %q: %w", observation.Key, err)
		}
		if payload.Version != quotaVersion {
			continue
		}
		if payload.Provider != provider || provider == "" || payload.Remaining < 0 || payload.ResetAt.IsZero() {
			return fmt.Errorf("provider observation %q is invalid", observation.Key)
		}
		if !payload.ResetAt.After(now) {
			continue
		}
		q.State.Record(apiv1.Provider(provider), payload.Remaining, payload.ResetAt)
	}
	return nil
}
