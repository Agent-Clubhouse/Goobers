package journalpg

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
)

type preparedBlob struct {
	ref  journal.Ref
	data []byte
}

// CreateRun commits immutable identity, input blobs and run.started together.
func (b *Backend) CreateRun(ctx context.Context, request journal.CreateRunRequest) (journal.RunHandle, journal.OwnerToken, error) {
	if !apiv1.ValidRunID(request.Identity.RunID) {
		return "", "", errors.New("journalpg: invalid run ID")
	}
	if err := request.Identity.ValidateChildLineage(); err != nil {
		return "", "", err
	}
	h, err := b.liveOwner()
	if err != nil {
		return "", "", err
	}
	r := &runOwner{key: journal.RunKey{Gaggle: request.Identity.Gaggle, RunID: request.Identity.RunID}, token: journal.OwnerToken(rand.Text()), epoch: h.Epoch, options: normalizedOptions(request.Options)}
	id, blobs, err := prepareIdentity(r, request)
	if err != nil {
		return "", "", err
	}
	raw, err := yaml.Marshal(id)
	if err != nil {
		return "", "", err
	}
	started, err := prepareEvent(r, journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning), SourceRunID: id.ContinuedFromRunID, SourceTerminalSeq: id.SourceTerminalSeq, Actor: id.Operator, Target: id.RequestedTarget})
	if err != nil {
		return "", "", err
	}
	started.Seq = 1
	checkpoint, err := prepareCheckpoint(r.key.RunID, []journal.Event{started})
	if err != nil {
		return "", "", err
	}
	err = b.config.Lease.WithFence(ctx, h, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO journal.run(instance_id,run_id,gaggle,workflow,run_yaml,created_at)
  SELECT instance_id,$5,$6,$7,$8,$9 FROM control.lease WHERE instance_id=$1 AND name=$2 AND holder=$3 AND epoch=$4`, h.InstanceID, h.Name, h.Holder, h.Epoch, id.RunID, id.Gaggle, id.Workflow, raw, id.StartedAt)
		if err = requireWrite(tag.RowsAffected(), err); err != nil {
			return err
		}
		tag, err = tx.Exec(ctx, `INSERT INTO journal.run_seq(instance_id,run_id,owner_token,owner_epoch)
  SELECT instance_id,$5,$6,epoch FROM control.lease WHERE instance_id=$1 AND name=$2 AND holder=$3 AND epoch=$4`, h.InstanceID, h.Name, h.Holder, h.Epoch, id.RunID, r.token)
		if err = requireWrite(tag.RowsAffected(), err); err != nil {
			return err
		}
		for _, blob := range blobs {
			if err := b.putBlob(ctx, tx, r, blob); err != nil {
				return err
			}
		}
		if _, _, err = b.appendClean(ctx, tx, r, 0, started); err != nil {
			return err
		}
		return b.storeCheckpoint(ctx, tx, r, checkpoint)
	})
	if err != nil {
		return "", "", err
	}
	handle := b.keepRun(r)
	b.changed(r, 1)
	return handle, r.token, nil
}
func prepareIdentity(r *runOwner, request journal.CreateRunRequest) (journal.RunIdentity, []preparedBlob, error) {
	id := request.Identity
	id.Schema = journal.RunSchema
	if id.StartedAt.IsZero() {
		id.StartedAt = r.options.Clock()
	}
	id.Inputs = nil
	names := make([]string, 0, len(request.Inputs))
	for name := range request.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	var blobs []preparedBlob
	for _, name := range names {
		if name == "" || name == "." || name == ".." || path.Base(name) != name || strings.ContainsAny(name, `/\`) {
			return id, nil, fmt.Errorf("journalpg: invalid input name %q", name)
		}
		data := r.options.Scrubber.Scrub(request.Inputs[name])
		integrity := apiv1.IntegrityTrusted
		if configured, ok := r.options.InputIntegrity[name]; ok {
			integrity = configured
		}
		if !integrity.Valid() {
			return id, nil, errors.New("journalpg: invalid input integrity")
		}
		ref := journal.Ref{Path: "inputs/" + name, Digest: journal.Digest(data), Size: int64(len(data)), Integrity: integrity}
		id.Inputs = append(id.Inputs, journal.InputRef{Name: name, Ref: ref, Integrity: integrity, Source: r.options.InputSource[name]})
		blobs = append(blobs, preparedBlob{ref: ref, data: data})
	}
	return id, blobs, nil
}

// RecoverRun acquires a new writer incarnation and returns its exact committed
// history. Same-epoch live owners contend; a newer controller epoch may take over.
func (b *Backend) RecoverRun(ctx context.Context, key journal.RunKey, options journal.RunWriterOptions) (journal.RunHandle, journal.OwnerToken, journal.RecoverReport, error) {
	h, err := b.liveOwner()
	if err != nil {
		return "", "", journal.RecoverReport{}, err
	}
	if b.ownsRun(key, h.Epoch) {
		return "", "", journal.RecoverReport{}, journal.ErrRunOwned
	}
	if err := ctx.Err(); err != nil {
		return "", "", journal.RecoverReport{}, errors.Join(journal.ErrRunOwned, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		r, report, err := b.tryRecover(waitCtx, key, options)
		if err == nil {
			return b.keepRun(r), r.token, report, nil
		}
		if errors.Is(err, errHistoryChanged) {
			continue
		}
		if !errors.Is(err, journal.ErrRunOwned) {
			return "", "", report, err
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return "", "", report, errors.Join(journal.ErrRunOwned, ctx.Err())
			}
			return "", "", report, errors.Join(journal.ErrRunOwned, journal.ErrLockTimeout)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ownsRun forgets only incarnations from strictly older controller epochs.
// Handles already handed to callers retain their old epoch and cannot write.
func (b *Backend) ownsRun(key journal.RunKey, epoch int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	owned := false
	for handle, r := range b.runs {
		if r.epoch < epoch {
			delete(b.runs, handle)
		} else if r.key == key {
			owned = true
		}
	}
	return owned
}

func (b *Backend) tryRecover(ctx context.Context, key journal.RunKey, options journal.RunWriterOptions) (*runOwner, journal.RecoverReport, error) {
	h, err := b.liveOwner()
	if err != nil {
		return nil, journal.RecoverReport{}, err
	}
	r := &runOwner{key: key, token: journal.OwnerToken(rand.Text()), epoch: h.Epoch, options: normalizedOptions(options)}
	snapshot, err := b.snapshot(ctx, key.RunID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = os.ErrNotExist
	}
	if err != nil {
		return nil, journal.RecoverReport{}, err
	}
	checkpoint, err := prepareCheckpoint(key.RunID, snapshot.events)
	if err != nil {
		return nil, journal.RecoverReport{}, err
	}
	paths := completedTranscriptPartials(snapshot.events)
	report := journal.RecoverReport{LastSeq: uint64(snapshot.seq), Events: snapshot.events}
	err = b.config.Lease.WithFence(ctx, h, func(ctx context.Context, tx pgx.Tx) error {
		if err := b.claimRun(ctx, tx, h, r, snapshot.seq); err != nil {
			return err
		}
		return b.storeCheckpoint(ctx, tx, r, checkpoint)
	})
	if err == nil {
		err = b.removeTranscriptPartials(ctx, r, paths)
		if err != nil {
			// A failed cleanup must not strand a same-epoch owner that was never
			// returned to the caller. Preserve the original error as well.
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lease.FencedTransactionTimeout)
			err = errors.Join(err, b.retireOwner(releaseCtx, r))
			cancel()
		}
	}
	return r, report, err
}

// claimRun validates the pre-fence snapshot under the counter lock. No event
// history is read here; takeover plus the prepared checkpoint is one short TX.
func (b *Backend) claimRun(ctx context.Context, tx pgx.Tx, h *lease.Handle, r *runOwner, expected int64) error {
	var epoch, seq int64
	var token *string
	var gaggle string
	err := tx.QueryRow(ctx, `SELECT s.owner_epoch,s.owner_token,s.last_seq,r.gaggle FROM journal.run_seq s JOIN journal.run r USING(instance_id,run_id) WHERE s.instance_id=$1 AND s.run_id=$2 FOR UPDATE OF s`, b.config.InstanceID, r.key.RunID).Scan(&epoch, &token, &seq, &gaggle)
	if errors.Is(err, pgx.ErrNoRows) {
		return os.ErrNotExist
	}
	if err != nil {
		return err
	}
	if gaggle != r.key.Gaggle {
		return os.ErrNotExist
	}
	if epoch > h.Epoch {
		return lease.ErrStale
	}
	if token != nil && epoch == h.Epoch {
		return journal.ErrRunOwned
	}
	if seq != expected {
		return errHistoryChanged
	}
	tag, err := tx.Exec(ctx, `UPDATE journal.run_seq s SET owner_epoch=$4,owner_token=$6 WHERE s.instance_id=$1 AND s.run_id=$5 AND (s.owner_token IS NULL OR s.owner_epoch<$4) AND s.owner_epoch<=$4
 AND EXISTS(SELECT 1 FROM control.lease l WHERE l.instance_id=$1 AND l.name=$2 AND l.holder=$3 AND l.epoch=$4)`, h.InstanceID, h.Name, h.Holder, h.Epoch, r.key.RunID, r.token)
	if err = requireWrite(tag.RowsAffected(), err); err != nil {
		return err
	}
	tag, err = tx.Exec(ctx, `UPDATE journal.run SET retired_at=NULL WHERE instance_id=$1 AND run_id=$2 AND EXISTS(SELECT 1 FROM journal.run_seq s WHERE s.instance_id=$1 AND s.run_id=$2 AND s.owner_token=$3 AND s.owner_epoch=$4)`, b.config.InstanceID, r.key.RunID, r.token, r.epoch)
	return requireWrite(tag.RowsAffected(), err)
}

// RunStatus reads committed sequence and phase for a still-open handle.
func (b *Backend) RunStatus(ctx context.Context, h journal.RunHandle) (journal.RunStatus, error) {
	r, err := b.readRun(h)
	if err != nil {
		return journal.RunStatus{}, err
	}
	var seq int64
	var phase string
	err = b.config.Pool.QueryRow(ctx, `SELECT s.last_seq,c.phase FROM journal.run_seq s JOIN journal.run_checkpoint c USING(instance_id,run_id) WHERE s.instance_id=$1 AND s.run_id=$2 AND s.owner_token=$3 AND s.owner_epoch=$4`, b.config.InstanceID, r.key.RunID, r.token, r.epoch).Scan(&seq, &phase)
	if errors.Is(err, pgx.ErrNoRows) {
		return journal.RunStatus{}, journal.ErrClosed
	}
	return journal.RunStatus{Seq: uint64(seq), Phase: journal.RunPhase(phase)}, err
}

// Retire releases only this writer incarnation, not the logical run.
func (b *Backend) Retire(ctx context.Context, h journal.RunHandle, o journal.OwnerToken) error {
	r, err := b.writeRun(h, o)
	if err != nil {
		return err
	}
	err = b.retireOwner(ctx, r)
	if err == nil {
		b.mu.Lock()
		delete(b.runs, h)
		b.mu.Unlock()
	}
	return err
}

func (b *Backend) retireOwner(ctx context.Context, r *runOwner) error {
	return b.withRun(ctx, r, func(ctx context.Context, tx pgx.Tx, _ int64) error {
		tag, err := tx.Exec(ctx, `UPDATE journal.run SET retired_at=clock_timestamp() WHERE instance_id=$1 AND run_id=$2 AND EXISTS(SELECT 1 FROM journal.run_seq s WHERE s.instance_id=$1 AND s.run_id=$2 AND s.owner_token=$3 AND s.owner_epoch=$4)`, b.config.InstanceID, r.key.RunID, r.token, r.epoch)
		if err = requireWrite(tag.RowsAffected(), err); err != nil {
			return err
		}
		tag, err = tx.Exec(ctx, `UPDATE journal.run_seq SET owner_token=NULL WHERE instance_id=$1 AND run_id=$2 AND owner_token=$3 AND owner_epoch=$4`, b.config.InstanceID, r.key.RunID, r.token, r.epoch)
		return requireWrite(tag.RowsAffected(), err)
	})
}
