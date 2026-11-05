package control

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
)

// Observation is non-secret provider policy state (quota and open-PR counts),
// preserved across controller restart. Provider adapters own its versioned JSON.
type Observation struct {
	Key        string
	Payload    json.RawMessage
	ObservedAt time.Time
}

// Observations stores a bounded snapshot; writes require the bound epoch.
type Observations struct {
	Pool       *pgxpool.Pool
	InstanceID string
	Lease      *lease.Manager
	Handle     *lease.Handle
}

func (s Observations) validate() error {
	if s.Pool == nil || s.Lease == nil || s.Handle == nil || s.InstanceID == "" || s.Handle.InstanceID != s.InstanceID {
		return errors.New("provider observations require an instance-bound lease")
	}
	return nil
}

// Load reads the primary, including predecessor observations for warm restart.
func (s Observations) Load(ctx context.Context) ([]Observation, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, lease.FencedTransactionTimeout)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `SELECT observation_key,payload,observed_at FROM control.provider_observation WHERE instance_id=$1 ORDER BY observation_key LIMIT 129`, s.InstanceID)
	if err != nil {
		return nil, err
	}
	result, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Observation, error) {
		var o Observation
		err := row.Scan(&o.Key, &o.Payload, &o.ObservedAt)
		return o, err
	})
	if len(result) > 128 {
		return nil, errors.New("provider observation snapshot exceeds bound")
	}
	return result, err
}

// Replace atomically persists policy observations before admitting more work.
func (s Observations) Replace(ctx context.Context, observations []Observation) error {
	if err := s.validate(); err != nil {
		return err
	}
	if len(observations) > 128 {
		return errors.New("provider observation snapshot exceeds bound")
	}
	seen := make(map[string]bool, len(observations))
	for _, o := range observations {
		if o.Key == "" || len(o.Key) > 128 || seen[o.Key] || !json.Valid(o.Payload) || len(o.Payload) > 8192 || o.ObservedAt.IsZero() {
			return errors.New("invalid provider observation")
		}
		seen[o.Key] = true
	}
	return s.Lease.WithFence(ctx, s.Handle, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM control.provider_observation WHERE instance_id=$1`, s.InstanceID); err != nil {
			return err
		}
		for _, o := range observations {
			_, err := tx.Exec(ctx, `INSERT INTO control.provider_observation(instance_id,observation_key,payload,observed_at,owner_epoch) VALUES($1,$2,$3,$4,$5)`, s.InstanceID, o.Key, o.Payload, o.ObservedAt, s.Handle.Epoch)
			if err != nil {
				return err
			}
		}
		return nil
	})
}
