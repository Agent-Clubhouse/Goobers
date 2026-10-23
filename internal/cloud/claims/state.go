package claims

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/localscheduler"
)

const (
	stateScopeDemand      = "schedule-demand"
	stateScopeEvaluations = "trigger-evaluations"
	stateSnapshotKey      = "snapshot"
)

type demandRecord struct {
	Gaggle, Workflow string
	Demand           bool
}

type evaluationRecord struct {
	Gaggle, Workflow string
	LastEval         time.Time
}

// ReadScheduleDemand reads the shared demand snapshot. dir is ignored.
func (s *Store) ReadScheduleDemand(string) (map[localscheduler.WorkflowIdentity]bool, error) {
	var records []demandRecord
	if err := s.readState(stateScopeDemand, &records); err != nil {
		return nil, err
	}
	result := make(map[localscheduler.WorkflowIdentity]bool, len(records))
	for _, record := range records {
		result[localscheduler.WorkflowIdentity{Gaggle: record.Gaggle, Workflow: record.Workflow}] = record.Demand
	}
	return result, nil
}

// ReadTriggerEvaluations reads the shared monotonic evaluation snapshot.
func (s *Store) ReadTriggerEvaluations(string) (map[localscheduler.WorkflowIdentity]time.Time, error) {
	var records []evaluationRecord
	if err := s.readState(stateScopeEvaluations, &records); err != nil {
		return nil, err
	}
	result := make(map[localscheduler.WorkflowIdentity]time.Time, len(records))
	for _, record := range records {
		result[localscheduler.WorkflowIdentity{Gaggle: record.Gaggle, Workflow: record.Workflow}] = record.LastEval.UTC()
	}
	return result, nil
}

// WriteScheduleDemand compare-and-swaps the shared demand snapshot.
func (s *Store) WriteScheduleDemand(_ string, state map[localscheduler.WorkflowIdentity]bool) error {
	records := make([]demandRecord, 0, len(state))
	for identity, demand := range state {
		records = append(records, demandRecord{
			Gaggle: identity.Gaggle, Workflow: identity.Workflow, Demand: demand,
		})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Gaggle != records[j].Gaggle {
			return records[i].Gaggle < records[j].Gaggle
		}
		return records[i].Workflow < records[j].Workflow
	})
	return s.writeState(stateScopeDemand, records, nil)
}

// WriteTriggerEvaluations compare-and-swaps without moving a cursor backward.
func (s *Store) WriteTriggerEvaluations(
	_ string,
	state map[localscheduler.WorkflowIdentity]time.Time,
) error {
	records := make([]evaluationRecord, 0, len(state))
	for identity, lastEval := range state {
		if identity.Workflow == "" || lastEval.IsZero() {
			return errors.New("cloud claims: invalid trigger evaluation")
		}
		records = append(records, evaluationRecord{
			Gaggle: identity.Gaggle, Workflow: identity.Workflow, LastEval: lastEval.UTC(),
		})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Gaggle != records[j].Gaggle {
			return records[i].Gaggle < records[j].Gaggle
		}
		return records[i].Workflow < records[j].Workflow
	})
	monotonic := func(previous []byte) error {
		var prior []evaluationRecord
		if len(previous) > 0 {
			if err := json.Unmarshal(previous, &prior); err != nil {
				return err
			}
		}
		positions := make(map[localscheduler.WorkflowIdentity]time.Time, len(prior))
		for _, record := range prior {
			positions[localscheduler.WorkflowIdentity{Gaggle: record.Gaggle, Workflow: record.Workflow}] = record.LastEval
		}
		for _, record := range records {
			identity := localscheduler.WorkflowIdentity{Gaggle: record.Gaggle, Workflow: record.Workflow}
			if before := positions[identity]; record.LastEval.Before(before) {
				return errors.New("cloud claims: trigger evaluation cannot move backward")
			}
		}
		return nil
	}
	return s.writeState(stateScopeEvaluations, records, monotonic)
}

func (s *Store) readState(scope string, target any) error {
	var raw []byte
	err := s.pool.QueryRow(context.Background(), `
		SELECT value FROM control.scheduler_state
		WHERE instance_id=$1 AND scope=$2 AND key=$3`,
		s.instanceID, scope, stateSnapshotKey,
	).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func (s *Store) writeState(scope string, value any, validate func([]byte) error) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.mutate(func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		if err := lockClaims(ctx, tx, s.instanceID); err != nil {
			return err
		}
		var previous []byte
		var etag int64
		var ownerEpoch int64
		err := tx.QueryRow(ctx, `
			SELECT value,etag,owner_epoch FROM control.scheduler_state
			WHERE instance_id=$1 AND scope=$2 AND key=$3
			FOR UPDATE`,
			s.instanceID, scope, stateSnapshotKey,
		).Scan(&previous, &etag, &ownerEpoch)
		if errors.Is(err, pgx.ErrNoRows) {
			_, err = tx.Exec(ctx, `
				INSERT INTO control.scheduler_state
					(instance_id,scope,key,value,etag,owner_epoch,updated_at)
				VALUES ($1,$2,$3,$4,1,$5,clock_timestamp())`,
				s.instanceID, scope, stateSnapshotKey, raw, epoch,
			)
			return err
		}
		if err != nil {
			return err
		}
		if ownerEpoch > epoch {
			return ErrTransition
		}
		if validate != nil {
			if err := validate(previous); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `
			UPDATE control.scheduler_state
			SET value=$5,etag=etag+1,owner_epoch=$6,updated_at=clock_timestamp()
			WHERE instance_id=$1 AND scope=$2 AND key=$3
			  AND etag=$4 AND owner_epoch<=$6`,
			s.instanceID, scope, stateSnapshotKey, etag, raw, epoch,
		)
		return changed(tag.RowsAffected(), err)
	})
}

var _ localscheduler.SchedulerState = (*Store)(nil)
