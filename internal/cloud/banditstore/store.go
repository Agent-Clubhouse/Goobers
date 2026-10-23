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
	_, err = s.put(ctx, key, record.Key.Stage, payload)
	return err
}

func (s *Store) put(ctx context.Context, key, stage string, payload []byte) (bool, error) {
	var inserted bool
	err := s.manager.WithFence(ctx, s.handle, func(ctx context.Context, tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `INSERT INTO control.runner_bandit
			(instance_id, record_key, stage, owner_epoch, payload) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (instance_id, record_key) DO NOTHING`,
			s.handle.InstanceID, key, stage, s.handle.Epoch, payload)
		inserted = result.RowsAffected() == 1
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
	return inserted && err == nil, err
}

// Observations reads the primary under the live lease fence, including immutable
// predecessor observations. Run/sequence ordering preserves the local journal
// sampler's tie order for imported events with equal assignment sequences.
func (s *Store) Observations(ctx context.Context, stage string) ([]bandit.Observation, error) {
	if strings.TrimSpace(stage) == "" || len(stage) > 256 {
		return nil, errors.New("bandit: invalid observation stage")
	}
	var observations []bandit.Observation
	err := s.manager.WithFence(ctx, s.handle, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT record_key, payload FROM control.runner_bandit
			WHERE instance_id=$1 AND stage=$2 ORDER BY payload#>>'{key,runId}',
			(payload#>>'{key,occurrence}')::bigint, (payload#>>'{key,branch}')::bigint,
			(payload#>>'{key,attempt}')::bigint, record_key`, s.handle.InstanceID, stage)
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
			if strings.HasPrefix(key, "journal:") && record.Key.Attempt == 1 {
				encodedKey = journalObservationKey(record.Key.RunID, uint64(record.Key.Occurrence))
			}
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
