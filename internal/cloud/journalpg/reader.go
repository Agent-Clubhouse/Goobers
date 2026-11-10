package journalpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type runReader struct {
	backend  *Backend
	ctx      context.Context
	id       string
	identity journal.RunIdentity
}

// OpenRun returns a read-only view; writes always require an acquired owner.
func (b *Backend) OpenRun(ctx context.Context, ref journal.RunRef) (journal.RunReader, error) {
	if !apiv1.ValidRunID(ref.ID) {
		return nil, journal.ErrNotRunDirectory
	}
	var raw []byte
	err := b.config.Pool.QueryRow(ctx, `SELECT run_yaml FROM journal.run WHERE instance_id=$1 AND run_id=$2`, b.config.InstanceID, ref.ID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	r := &runReader{backend: b, ctx: ctx, id: ref.ID}
	if err := yaml.Unmarshal(raw, &r.identity); err != nil {
		return nil, err
	}
	if r.identity.Schema != journal.RunSchema {
		return nil, journal.ErrJournalMigrationRequired
	}
	return r, r.identity.ValidateChildLineage()
}

// ReadEvents returns original committed JSON in sequence order after the cursor.
func (b *Backend) ReadEvents(ctx context.Context, ref journal.RunRef, after uint64, limit int) ([]journal.EventRecord, error) {
	if !apiv1.ValidRunID(ref.ID) {
		return nil, journal.ErrNotRunDirectory
	}
	bound := int64(limit)
	if limit <= 0 {
		bound = 1<<63 - 1
	}
	if after > 1<<63-1 {
		return nil, nil
	}
	rows, err := b.config.Pool.Query(ctx, `SELECT body,recorded_at FROM journal.run_event WHERE instance_id=$1 AND run_id=$2 AND seq>$3 ORDER BY seq LIMIT $4`, b.config.InstanceID, ref.ID, int64(after), bound)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []journal.EventRecord
	for rows.Next() {
		var raw []byte
		var recordedAt *time.Time
		if err := rows.Scan(&raw, &recordedAt); err != nil {
			return nil, err
		}
		var event journal.Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, err
		}
		if !event.KnownSchema() {
			return nil, journal.ErrJournalMigrationRequired
		}
		record := journal.EventRecord{Event: event, Raw: raw}
		if recordedAt != nil {
			// Rows written before migration 0025 have no database-clock stamp.
			record.RecordedAt = recordedAt.UTC()
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

// ListRuns uses a stable lexical run-ID cursor within one instance.
func (b *Backend) ListRuns(ctx context.Context, filter journal.RunFilter) ([]journal.RunRef, error) {
	bound := int64(filter.Limit)
	if bound <= 0 {
		bound = 1<<63 - 1
	}
	rows, err := b.config.Pool.Query(ctx, `SELECT run_id FROM journal.run WHERE instance_id=$1 AND run_id>$2 ORDER BY run_id LIMIT $3`, b.config.InstanceID, filter.After, bound)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []journal.RunRef
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, journal.RunRef{ID: id, ReadOnly: true})
	}
	return result, rows.Err()
}
func (r *runReader) Schema() journal.SchemaInfo {
	return journal.CurrentSchemaInfo()
}
func (r *runReader) Identity() (journal.RunIdentity, error) { return r.identity, nil }
func (r *runReader) EventRecords() ([]journal.EventRecord, error) {
	return r.backend.ReadEvents(r.ctx, journal.RunRef{ID: r.id}, 0, 0)
}
func (r *runReader) Events() ([]journal.Event, error) {
	records, err := r.EventRecords()
	return recordEvents(records), err
}
func recordEvents(records []journal.EventRecord) []journal.Event {
	result := make([]journal.Event, len(records))
	for i := range records {
		result[i] = records[i].Event
	}
	return result
}
func (r *runReader) State() (journal.State, error) {
	events, err := r.Events()
	if err != nil {
		return journal.State{}, err
	}
	return stateForEvents(r.id, events), nil
}
func (r *runReader) Phase() (journal.RunPhase, error) { return r.PhaseBounded(r.ctx) }
func (r *runReader) PhaseBounded(ctx context.Context) (journal.RunPhase, error) {
	records, err := r.backend.ReadEvents(ctx, journal.RunRef{ID: r.id}, 0, 0)
	if err != nil {
		return "", err
	}
	return journal.PhaseFromEvents(recordEvents(records)), nil
}
func (r *runReader) ArtifactBytes(ref journal.Ref) ([]byte, error) {
	return r.backend.readBlob(r.ctx, r.id, ref, 0)
}
func (r *runReader) ArtifactBytesBounded(ref journal.Ref, maxBytes int64) ([]byte, error) {
	return r.backend.readBlob(r.ctx, r.id, ref, maxBytes)
}
func (r *runReader) SpanBytes(ref journal.Ref) ([]byte, error) { return r.ArtifactBytes(ref) }
func (r *runReader) ArtifactByDigest(digest string) ([]byte, error) {
	path, err := journal.ArtifactPath(digest)
	if err != nil {
		return nil, err
	}
	return r.ArtifactBytes(journal.Ref{Path: path, Digest: digest})
}
func (b *Backend) readBlob(ctx context.Context, runID string, ref journal.Ref, maxBytes int64) ([]byte, error) {
	var raw []byte
	var size int64
	err := b.config.Pool.QueryRow(ctx, `SELECT octet_length(body),CASE WHEN $4<=0 OR octet_length(body)<=$4 THEN body ELSE NULL END FROM journal.blob WHERE instance_id=$1 AND run_id=$2 AND path=$3`, b.config.InstanceID, runID, ref.Path, maxBytes).Scan(&size, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && size > maxBytes {
		return nil, fmt.Errorf("journalpg: blob exceeds %d bytes", maxBytes)
	}
	if journal.Digest(raw) != ref.Digest {
		return nil, errors.New("journalpg: blob digest mismatch")
	}
	return raw, nil
}

// stateForEvents preserves exact explicit lifecycle cursors without pretending
// to reconstruct workflow routing, which remains the engine's responsibility.
func stateForEvents(runID string, events []journal.Event) journal.State {
	prior := journal.State{RunID: runID}
	for _, event := range events {
		prior.LastSeq = max(prior.LastSeq, event.Seq)
		switch event.Type {
		case journal.EventRunResumed, journal.EventGateOverridden:
			prior.MachineState = event.Target
		case journal.EventStageRerunRequested:
			prior.MachineState = event.Stage
		case journal.EventRunFinished:
			prior.MachineState = ""
		}
	}
	return journal.RecoverState(prior, events)
}
