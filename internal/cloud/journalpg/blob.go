package journalpg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/journal"
)

// RecordBlob commits scrubbed content and its event atomically.
func (b *Backend) RecordBlob(ctx context.Context, h journal.RunHandle, o journal.OwnerToken, record journal.BlobRecord) (journal.Ref, error) {
	r, err := b.writeRun(h, o)
	if err != nil {
		return journal.Ref{}, err
	}
	if !record.Event.Integrity.Valid() {
		return journal.Ref{}, errors.New("journalpg: blob integrity required")
	}
	if record.Event.Stage == "" && record.Event.Branch == 0 {
		record.Event.Attempt = 0
		record.Event.AttemptClass = ""
	}
	clean := r.options.Scrubber.Scrub(record.Data)
	if record.MaxBytes > 0 && len(clean) > record.MaxBytes {
		return journal.Ref{}, errors.New("journalpg: scrubbed blob exceeds size limit")
	}
	ref, err := journal.ArtifactRef(clean)
	if err != nil {
		return ref, err
	}
	ref.Integrity = record.Event.Integrity
	if expected := record.Expected; expected != nil {
		if expected.Digest != ref.Digest || expected.Path != ref.Path || expected.Size != ref.Size {
			return journal.Ref{}, errors.New("journalpg: scrubbed blob does not match expected ref")
		}
		ref.MediaType = expected.MediaType
	}
	return b.recordBlob(ctx, r, record.Event, preparedBlob{ref: ref, data: clean})
}

// RecordSpan persists scrubbed span bytes in the shared content-address layout.
func (b *Backend) RecordSpan(ctx context.Context, h journal.RunHandle, o journal.OwnerToken, record journal.SpanRecord) (journal.Ref, error) {
	r, err := b.writeRun(h, o)
	if err != nil {
		return journal.Ref{}, err
	}
	return b.recordSpan(ctx, r, journal.Event{Type: journal.EventSpanRecorded, Branch: record.Branch, Stage: record.Stage, Name: record.Name, DataSchema: record.DataSchema, Runner: record.Annotations}, record.Data, "")
}
func (b *Backend) recordSpan(ctx context.Context, r *runOwner, event journal.Event, data []byte, expected string) (journal.Ref, error) {
	clean := r.options.Scrubber.Scrub(data)
	ref, err := journal.SpanRef(clean)
	if err != nil {
		return ref, err
	}
	if expected != "" && expected != ref.Digest {
		return journal.Ref{}, errors.New("journalpg: transcript digest changed after scrubbing")
	}
	return b.recordBlob(ctx, r, event, preparedBlob{ref: ref, data: clean})
}
func (b *Backend) recordBlob(ctx context.Context, r *runOwner, event journal.Event, blob preparedBlob) (journal.Ref, error) {
	event.Ref = &blob.ref
	clean, err := prepareEvent(r, event)
	if err != nil {
		return journal.Ref{}, err
	}
	var committed journal.Event
	var seq int64
	err = b.withRun(ctx, r, func(ctx context.Context, tx pgx.Tx, last int64) error {
		if err := b.putBlob(ctx, tx, r, blob); err != nil {
			return err
		}
		var err error
		seq, committed, err = b.appendClean(ctx, tx, r, last, clean)
		if err != nil {
			return err
		}
		if committed.Ref == nil || committed.Ref.Digest != blob.ref.Digest {
			return errors.New("journalpg: emit key content mismatch")
		}
		return nil
	})
	if err != nil {
		return journal.Ref{}, err
	}
	b.changed(r, uint64(seq))
	return *committed.Ref, nil
}
func (b *Backend) putBlob(ctx context.Context, tx pgx.Tx, r *runOwner, blob preparedBlob) error {
	tag, err := tx.Exec(ctx, `INSERT INTO journal.blob(instance_id,run_id,path,digest,body)
 SELECT instance_id,run_id,$5,$6,$7 FROM journal.run_seq WHERE instance_id=$1 AND run_id=$2 AND owner_token=$3 AND owner_epoch=$4
 ON CONFLICT(instance_id,run_id,path) DO NOTHING`, b.config.InstanceID, r.key.RunID, r.token, r.epoch, blob.ref.Path, blob.ref.Digest, blob.data)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var digest string
	var data []byte
	if err := tx.QueryRow(ctx, `SELECT digest,body FROM journal.blob WHERE instance_id=$1 AND run_id=$2 AND path=$3`, b.config.InstanceID, r.key.RunID, blob.ref.Path).Scan(&digest, &data); err != nil {
		return err
	}
	if digest != blob.ref.Digest || journal.Digest(data) != digest {
		return fmt.Errorf("journalpg: content-address conflict")
	}
	return nil
}

// ReadBlob verifies stored bytes against the requested content address.
func (b *Backend) ReadBlob(ctx context.Context, h journal.RunHandle, ref journal.Ref) ([]byte, error) {
	r, err := b.readRun(h)
	if err != nil {
		return nil, err
	}
	return b.readBlob(ctx, r.key.RunID, ref, 0)
}
