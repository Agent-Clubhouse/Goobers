package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const childColumns = `
	SELECT c.gaggle,c.parent_run_id,c.occurrence,c.invocation_key,
	       c.sequence,c.child_id,c.acceptance_id,c.start_key,c.proposal_digest,
	       a.run_id,c.state,c.result_ref,c.workspace_ref,c.accepted_at,c.updated_at,
	       c.terminal_at,c.acknowledged_at,c.tombstoned_at,
	       p.cancelled_at IS NOT NULL
	FROM control.child_lineage AS c
	JOIN control.child_parent AS p
	  ON p.instance_id=c.instance_id AND p.gaggle=c.gaggle
	 AND p.parent_run_id=c.parent_run_id
	JOIN control.acceptance AS a
	  ON a.instance_id=c.instance_id AND a.id=c.acceptance_id`

func childIdentityValid(identity triggerqueue.ChildIdentity) bool {
	return validText(identity.Gaggle, 128, true) &&
		validText(identity.ParentRunID, 256, true) &&
		validText(identity.StageOccurrence, 256, true) &&
		validText(identity.InvocationKey, 256, true)
}

func childParentValid(parent triggerqueue.ChildParent) bool {
	return validText(parent.Gaggle, 128, true) && validText(parent.ParentRunID, 256, true)
}

func childDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func childStartKey(identity triggerqueue.ChildIdentity) string {
	encoded, _ := json.Marshal([4]string{
		identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey,
	})
	return "haw-child:v1:" + childDigest(encoded)[len("sha256:"):]
}

// AcceptChild atomically creates acceptance and child custody, or resolves an
// exact retry before cancellation and capacity checks.
func (s *Store) AcceptChild(
	ctx context.Context,
	request triggerqueue.ChildAcceptance,
	now time.Time,
) (record triggerqueue.ChildRecord, duplicate bool, err error) {
	if !childIdentityValid(request.Identity) ||
		!validText(request.Actor, 1024, true) ||
		len(request.Payload) == 0 || len(request.Payload) > triggerqueue.MaxPayloadBytes ||
		request.MaxChildren < 1 || request.MaxChildren > triggerqueue.MaxChildrenPerOccurrence ||
		now.IsZero() {
		return record, false, errors.New("cloud acceptance: invalid child acceptance")
	}
	if request.Authority != nil &&
		(!childAuthorityValid(*request.Authority) ||
			request.Authority.ChildParent != request.Identity.ChildParent ||
			request.Authority.StageOccurrence != request.Identity.StageOccurrence) {
		return record, false, triggerqueue.ErrChildAuthorityChanged
	}
	proposalDigest := ""
	if request.Proposal != nil {
		if s.childBlobs == nil ||
			len(request.Proposal.Source) == 0 ||
			len(request.Proposal.Source) > triggerqueue.MaxChildProposalBytes ||
			request.Proposal.Digest != childDigest(request.Proposal.Source) {
			return record, false, triggerqueue.ErrChildProposalUnavailable
		}
		if err := s.childBlobs.Put(ctx, request.Proposal.Digest, request.Proposal.Source); err != nil {
			return record, false, fmt.Errorf("cloud acceptance: store child proposal: %w", err)
		}
		proposalDigest = request.Proposal.Digest
	}
	actorDigest := childDigest([]byte(request.Actor))
	payloadDigest := childDigest(request.Payload)
	err = s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		identity := request.Identity
		if _, err := tx.Exec(txCtx, `
			SELECT pg_advisory_xact_lock(hashtextextended(
				'child:'||$1||':'||$2||':'||$3||':'||$4,0))`,
			s.instanceID, identity.Gaggle, identity.ParentRunID, identity.StageOccurrence,
		); err != nil {
			return err
		}
		existing, existingActor, existingPayload, readErr :=
			s.readChildRetry(txCtx, tx, identity)
		if readErr == nil {
			if existingActor != actorDigest || existingPayload != payloadDigest ||
				existing.ProposalDigest != proposalDigest {
				return triggerqueue.ErrConflict
			}
			record, duplicate = existing, true
			return nil
		}
		if !errors.Is(readErr, pgx.ErrNoRows) {
			return readErr
		}
		if request.Authority != nil {
			if err := s.checkChildAuthorityTx(txCtx, tx, *request.Authority); err != nil {
				return err
			}
		}
		if err := s.ensureChildParent(txCtx, tx, identity.ChildParent, epoch); err != nil {
			return err
		}
		var cancelled, settled *time.Time
		var parentEpoch int64
		if err := tx.QueryRow(txCtx, `
			SELECT cancelled_at,settled_at,owner_epoch
			FROM control.child_parent
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
			FOR UPDATE`,
			s.instanceID, identity.Gaggle, identity.ParentRunID,
		).Scan(&cancelled, &settled, &parentEpoch); err != nil {
			return err
		}
		if parentEpoch != epoch {
			return triggerqueue.ErrTransition
		}
		if cancelled != nil {
			return triggerqueue.ErrParentCancelled
		}
		if settled != nil {
			return triggerqueue.ErrParentSettled
		}
		var liveCount int
		if err := tx.QueryRow(txCtx, `
			SELECT count(*) FROM control.child_lineage
			WHERE instance_id=$1 AND gaggle=$2 AND tombstoned_at IS NULL`,
			s.instanceID, identity.Gaggle,
		).Scan(&liveCount); err != nil {
			return err
		}
		if liveCount >= triggerqueue.MaxChildLineages {
			return triggerqueue.ErrFull
		}
		if _, err := tx.Exec(txCtx, `
			INSERT INTO control.child_occurrence (
				instance_id,gaggle,parent_run_id,occurrence,
				accepted_count,max_children,owner_epoch
			) VALUES ($1,$2,$3,$4,0,$5,$6)
			ON CONFLICT DO NOTHING`,
			s.instanceID, identity.Gaggle, identity.ParentRunID,
			identity.StageOccurrence, request.MaxChildren, epoch,
		); err != nil {
			return err
		}
		var acceptedCount, ceiling int
		var occurrenceEpoch int64
		if err := tx.QueryRow(txCtx, `
			SELECT accepted_count,max_children,owner_epoch
			FROM control.child_occurrence
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3 AND occurrence=$4
			FOR UPDATE`,
			s.instanceID, identity.Gaggle, identity.ParentRunID, identity.StageOccurrence,
		).Scan(&acceptedCount, &ceiling, &occurrenceEpoch); err != nil {
			return err
		}
		if occurrenceEpoch != epoch {
			return triggerqueue.ErrTransition
		}
		var unresolved bool
		if err := tx.QueryRow(txCtx, `
			SELECT EXISTS (
				SELECT 1 FROM control.child_lineage
				WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
				  AND occurrence=$4 AND acknowledged_at IS NULL
			)`,
			s.instanceID, identity.Gaggle, identity.ParentRunID, identity.StageOccurrence,
		).Scan(&unresolved); err != nil {
			return err
		}
		if unresolved {
			return triggerqueue.ErrChildSlotOccupied
		}
		if acceptedCount >= min(ceiling, request.MaxChildren) {
			return triggerqueue.ErrChildLimit
		}
		runID, err := randomRunID()
		if err != nil {
			return err
		}
		acceptanceID := "trigger-" + runID
		startKey := childStartKey(identity)
		fingerprint := sha256.Sum256(request.Payload)
		var acceptedAt time.Time
		if err := tx.QueryRow(txCtx, `
			INSERT INTO control.acceptance (
				instance_id,id,idem_key,actor,fingerprint,payload,state,
				owner_epoch,run_id,accepted_at
			) VALUES ($1,$2,$3,$4,$5,$6,'accepted',$7,$8,clock_timestamp())
			RETURNING accepted_at`,
			s.instanceID, acceptanceID, startKey, request.Actor,
			fingerprint[:], request.Payload, epoch, runID,
		).Scan(&acceptedAt); err != nil {
			return err
		}
		childID := "child-" + runID
		if _, err := tx.Exec(txCtx, `
			INSERT INTO control.child_lineage (
				instance_id,gaggle,parent_run_id,occurrence,invocation_key,
				sequence,child_id,acceptance_id,start_key,actor_digest,payload_digest,
				proposal_digest,state,accepted_at,updated_at,owner_epoch
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'queued',$13,$13,$14)`,
			s.instanceID, identity.Gaggle, identity.ParentRunID,
			identity.StageOccurrence, identity.InvocationKey,
			acceptedCount+1, childID, acceptanceID, startKey,
			actorDigest, payloadDigest, proposalDigest, acceptedAt, epoch,
		); err != nil {
			return err
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.child_occurrence
			SET accepted_count=accepted_count+1,owner_epoch=$6
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
			  AND occurrence=$4 AND owner_epoch=$5`,
			s.instanceID, identity.Gaggle, identity.ParentRunID,
			identity.StageOccurrence, epoch, epoch,
		)
		if err := changed(tag.RowsAffected(), err); err != nil {
			return err
		}
		record, err = s.scanChild(tx.QueryRow(txCtx,
			childColumns+`
			WHERE c.instance_id=$1 AND c.gaggle=$2 AND c.parent_run_id=$3
			  AND c.occurrence=$4 AND c.invocation_key=$5`,
			s.instanceID, identity.Gaggle, identity.ParentRunID,
			identity.StageOccurrence, identity.InvocationKey))
		return err
	})
	return record, duplicate, err
}

func (s *Store) ensureChildParent(
	ctx context.Context,
	tx pgx.Tx,
	parent triggerqueue.ChildParent,
	epoch int64,
) error {
	var count int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM control.child_parent
		WHERE instance_id=$1 AND gaggle=$2`,
		s.instanceID, parent.Gaggle,
	).Scan(&count); err != nil {
		return err
	}
	if count >= triggerqueue.MaxChildLineages {
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM control.child_parent
				WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
			)`,
			s.instanceID, parent.Gaggle, parent.ParentRunID,
		).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return triggerqueue.ErrFull
		}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO control.child_parent (
			instance_id,gaggle,parent_run_id,owner_epoch
		) VALUES ($1,$2,$3,$4)
		ON CONFLICT DO NOTHING`,
		s.instanceID, parent.Gaggle, parent.ParentRunID, epoch,
	)
	return err
}

func (s *Store) readChildRetry(
	ctx context.Context,
	tx pgx.Tx,
	identity triggerqueue.ChildIdentity,
) (triggerqueue.ChildRecord, string, string, error) {
	var actorDigest, payloadDigest string
	record, err := s.scanChild(tx.QueryRow(ctx,
		childColumns+`
		WHERE c.instance_id=$1 AND c.gaggle=$2 AND c.parent_run_id=$3
		  AND c.occurrence=$4 AND c.invocation_key=$5
		FOR UPDATE`,
		s.instanceID, identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey))
	if err != nil {
		return record, "", "", err
	}
	err = tx.QueryRow(ctx, `
		SELECT actor_digest,payload_digest FROM control.child_lineage
		WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
		  AND occurrence=$4 AND invocation_key=$5`,
		s.instanceID, identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey,
	).Scan(&actorDigest, &payloadDigest)
	return record, actorDigest, payloadDigest, err
}

// GetChild reads one exact tenant-scoped lineage.
func (s *Store) GetChild(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
) (triggerqueue.ChildRecord, error) {
	if !childIdentityValid(identity) {
		return triggerqueue.ChildRecord{}, errors.New("cloud acceptance: invalid child identity")
	}
	record, err := s.scanChild(s.pool.QueryRow(ctx,
		childColumns+`
		WHERE c.instance_id=$1 AND c.gaggle=$2 AND c.parent_run_id=$3
		  AND c.occurrence=$4 AND c.invocation_key=$5`,
		s.instanceID, identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey))
	return record, noRows(err)
}

func (s *Store) scanChild(row pgx.Row) (triggerqueue.ChildRecord, error) {
	var record triggerqueue.ChildRecord
	var terminalAt, acknowledgedAt, tombstonedAt *time.Time
	err := row.Scan(
		&record.Identity.Gaggle, &record.Identity.ParentRunID,
		&record.Identity.StageOccurrence, &record.Identity.InvocationKey,
		&record.Sequence, &record.ChildID, &record.AcceptanceID, &record.StartKey,
		&record.ProposalDigest, &record.RunID, &record.State,
		&record.ResultRef, &record.WorkspaceRef,
		&record.AcceptedAt, &record.UpdatedAt,
		&terminalAt, &acknowledgedAt, &tombstonedAt,
		&record.CancellationRequested,
	)
	if terminalAt != nil {
		record.TerminalAt = terminalAt.UTC()
	}
	if acknowledgedAt != nil {
		record.AcknowledgedAt = acknowledgedAt.UTC()
	}
	if tombstonedAt != nil {
		record.TombstonedAt = tombstonedAt.UTC()
	}
	record.AcceptedAt, record.UpdatedAt = record.AcceptedAt.UTC(), record.UpdatedAt.UTC()
	return record, err
}

// ChildProposal loads exact accepted source bytes by their retained digest.
func (s *Store) ChildProposal(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
) (triggerqueue.ChildProposal, error) {
	record, err := s.GetChild(ctx, identity)
	if err != nil || record.ProposalDigest == "" || !record.TombstonedAt.IsZero() || s.childBlobs == nil {
		return triggerqueue.ChildProposal{}, triggerqueue.ErrChildProposalUnavailable
	}
	source, err := s.childBlobs.Get(ctx, record.ProposalDigest)
	if err != nil || childDigest(source) != record.ProposalDigest {
		return triggerqueue.ChildProposal{}, triggerqueue.ErrChildProposalUnavailable
	}
	return triggerqueue.ChildProposal{Digest: record.ProposalDigest, Source: source}, nil
}

// AdoptChildren moves exact predecessor custody to the current live epoch.
func (s *Store) AdoptChildren(ctx context.Context, predecessorEpoch int64) (int64, error) {
	if predecessorEpoch < 1 || s.handle == nil || predecessorEpoch >= s.handle.Epoch {
		return 0, triggerqueue.ErrTransition
	}
	var adopted int64
	err := s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		for _, table := range []string{
			"child_parent", "child_occurrence", "child_lineage", "child_authority",
			"child_blob", "child_blob_read", "child_publication",
			"child_disposition", "child_disposition_history",
		} {
			tag, err := tx.Exec(txCtx, fmt.Sprintf(`
				UPDATE control.%s SET owner_epoch=$3
				WHERE instance_id=$1 AND owner_epoch=$2`, table),
				s.instanceID, predecessorEpoch, epoch)
			if err != nil {
				return err
			}
			adopted += tag.RowsAffected()
		}
		return nil
	})
	return adopted, err
}

func validChildTransition(from, to triggerqueue.ChildState) bool {
	switch from {
	case triggerqueue.ChildQueued:
		return to == triggerqueue.ChildRunning ||
			to == triggerqueue.ChildFailed || to == triggerqueue.ChildCancelled
	case triggerqueue.ChildRunning:
		return to == triggerqueue.ChildAwaitingHuman || to.Terminal()
	case triggerqueue.ChildAwaitingHuman:
		return to == triggerqueue.ChildRunning || to.Terminal()
	default:
		return false
	}
}

func verifyDigestData(digest string, data []byte) bool {
	return blobstore.ValidDigest(digest) && digest == childDigest(data)
}
