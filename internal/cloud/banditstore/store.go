// Package banditstore supplies the fenced PostgreSQL experiment observation store.
// It is used by the control plane; workers access it through the HTTP plane.
package banditstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/bandit"
	"github.com/goobers/goobers/internal/cloud/lease"
)

// Store binds all reads and writes to one acquired controller epoch.
type Store struct {
	manager *lease.Manager
	handle  *lease.Handle
}

// New requires a live lease binding; an unbound worker cannot construct a store.
func New(manager *lease.Manager, handle *lease.Handle) (*Store, error) {
	if manager == nil || handle == nil || handle.InstanceID == "" || handle.Epoch <= 0 {
		return nil, errors.New("bandit store requires an acquired lease")
	}
	return &Store{manager: manager, handle: handle}, nil
}

// Record inserts an immutable outcome, refusing a conflicting retry. The epoch
// comes only from the bound handle and is checked under the lease row lock.
func (s *Store) Record(ctx context.Context, record bandit.ObservationRecord) error {
	key, payload, err := bandit.EncodeRecord(record)
	if err != nil {
		return err
	}
	return s.manager.WithFence(ctx, s.handle, func(ctx context.Context, tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `INSERT INTO control.runner_bandit
			(instance_id, record_key, stage, owner_epoch, payload) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (instance_id, record_key) DO NOTHING`,
			s.handle.InstanceID, key, record.Key.Stage, s.handle.Epoch, payload)
		if err != nil || result.RowsAffected() == 1 {
			return err
		}
		var same bool
		if err := tx.QueryRow(ctx, `SELECT payload=$3::jsonb FROM control.runner_bandit
			WHERE instance_id=$1 AND record_key=$2`, s.handle.InstanceID, key, payload).Scan(&same); err != nil {
			return err
		}
		if !same {
			return bandit.ErrObservationConflict
		}
		return nil
	})
}

// Observations reads the primary under the live lease fence, including immutable
// predecessor observations. Key order breaks equal assignment-sequence ties in
// exactly the same way as the file backend's directory order.
func (s *Store) Observations(ctx context.Context, stage string) ([]bandit.Observation, error) {
	if strings.TrimSpace(stage) == "" || len(stage) > 256 {
		return nil, errors.New("bandit: invalid observation stage")
	}
	var observations []bandit.Observation
	err := s.manager.WithFence(ctx, s.handle, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT record_key, payload FROM control.runner_bandit
			WHERE instance_id=$1 AND stage=$2 ORDER BY record_key`, s.handle.InstanceID, stage)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			var payload []byte
			if err := rows.Scan(&key, &payload); err != nil {
				return err
			}
			var record bandit.ObservationRecord
			if err := json.Unmarshal(payload, &record); err != nil {
				return err
			}
			encodedKey, _, err := bandit.EncodeRecord(record)
			if err != nil || encodedKey != key || record.Key.Stage != stage {
				return errors.New("bandit: corrupt observation record")
			}
			observations = append(observations, record.Observation)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	bandit.SortObservations(observations)
	return observations, nil
}

var _ bandit.ObservationStore = (*Store)(nil)
