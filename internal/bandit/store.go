package bandit

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ErrObservationConflict refuses reuse of an attempt identity for another result.
var ErrObservationConflict = errors.New("bandit: observation identity belongs to another result")

// ObservationKey identifies one task attempt, including repasses and branches.
// It contains no filesystem paths or database expressions.
type ObservationKey struct {
	RunID      string `json:"runId"`
	Stage      string `json:"stage"`
	Branch     int    `json:"branch"`
	Occurrence int64  `json:"occurrence"`
	Attempt    int    `json:"attempt"`
}

// ObservationRecord is an immutable outcome. Retrying the same record is a no-op.
type ObservationRecord struct {
	Key         ObservationKey `json:"key"`
	Observation Observation    `json:"observation"`
}

// ObservationStore supplies the durable sample history behind the bandit plane.
// Stage scope preserves the local runner's existing instance-wide sample domain.
type ObservationStore interface {
	Observations(context.Context, string) ([]Observation, error)
	Record(context.Context, ObservationRecord) error
}

// EncodeRecord validates and canonicalizes an outcome before any store mutation.
// The digest is an attempt identity, not a digest of the mutable result.
func EncodeRecord(record ObservationRecord) (key string, payload []byte, err error) {
	k := record.Key
	if !apiv1.ValidRunID(k.RunID) || strings.TrimSpace(k.Stage) == "" || len(k.Stage) > 256 ||
		k.Branch < 0 || k.Occurrence <= 0 || k.Attempt <= 0 ||
		record.Observation.RunID != k.RunID || record.Observation.Stage != k.Stage {
		return "", nil, errors.New("bandit: invalid observation identity")
	}
	if record.Observation.Schema != "" && record.Observation.Schema != Schema {
		return "", nil, errors.New("bandit: unsupported observation schema")
	}
	identity, err := json.Marshal(k)
	if err != nil {
		return "", nil, err
	}
	payload, err = json.Marshal(record)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("%x", sha256.Sum256(identity)), payload, nil
}
