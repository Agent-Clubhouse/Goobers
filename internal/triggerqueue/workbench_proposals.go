package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

const (
	// MaxWorkbenchProposalRequestBytes bounds encoded typed edit input. Each
	// source file still has the stricter one-MiB decoded source bound.
	MaxWorkbenchProposalRequestBytes = 2 << 20
	// MaxWorkbenchProposalPlanBytes excludes the two separately bounded files.
	MaxWorkbenchProposalPlanBytes = 16 << 10
	// MaxWorkbenchProposalHistoryBytes bounds four immutable attempts plus the
	// most recent sixteen observations. Earlier observation count stays visible.
	MaxWorkbenchProposalHistoryBytes = 64 << 10
	// MaxWorkbenchProposalObservations bounds retained read evidence per command.
	MaxWorkbenchProposalObservations = 16
	workbenchProposalAllowance       = 2*workbench.MaxSourceBytes + MaxWorkbenchProposalPlanBytes + MaxWorkbenchProposalHistoryBytes + 32*1024
)

// WorkbenchProposalInput is a source-owned metadata edit command, independent of
// the native field command kind. It carries no caller credential or repository.
type WorkbenchProposalInput struct {
	Scope                                    WorkbenchCommandScope
	RequestID, TargetDigest, OperationDigest string
	Request                                  workbench.MetadataChangeRequest
}

// WorkbenchProposalPlan is trusted preview custody attached before any effect.
// Native retains exact destination and generated text; Content equals After.
type WorkbenchProposalPlan struct {
	Scope   workbench.Scope
	Kind    string
	Preview workbench.MetadataPreview
	Native  providers.RepositoryProposal
}

// WorkbenchProposalPhase retains the immutable result of one claimed phase.
// An unknown result can be observed, but can never become acknowledged later.
type WorkbenchProposalPhase struct {
	Name       string
	ClaimedAt  time.Time
	FinishedAt *time.Time
	Outcome    string
	Result     *providers.RepositoryProposalPhaseResult
}

// WorkbenchProposalObservation records only a read of exact admitted intent.
type WorkbenchProposalObservation struct {
	Phase  int
	At     time.Time
	Result providers.RepositoryProposalObservation
}

// WorkbenchProposal keeps intent, immutable attempts and separate observations.
// Current authority must be rechecked on every read and explicit next-phase call.
type WorkbenchProposal struct {
	ID                                              string
	Input                                           WorkbenchProposalInput
	RequestDigest, State, PlanDigest, HistoryDigest string
	AcceptedAt                                      time.Time
	CompletedAt, TombstonedAt                       *time.Time
	Plan                                            *WorkbenchProposalPlan
	Phases                                          []WorkbenchProposalPhase
	Observations                                    []WorkbenchProposalObservation
	OmittedObservations                             int64
}

// AcceptWorkbenchProposal reserves full bounded preview/phase custody before
// source preflight. Exact repeats never read the provider or allocate another ID.
func (s *Store) AcceptWorkbenchProposal(ctx context.Context, input WorkbenchProposalInput, now time.Time) (WorkbenchProposal, bool, error) {
	raw, digest, err := canonicalWorkbenchProposal(input)
	if err != nil || now.IsZero() {
		return WorkbenchProposal{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkbenchProposal{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	key := workbenchCommandKey(input.Scope, input.RequestID)
	previous, err := scanWorkbenchProposal(tx.QueryRowContext(ctx, "SELECT "+workbenchProposalColumns+" FROM workbench_proposals WHERE key_digest=?", key))
	if err == nil {
		if previous.RequestDigest != digest || previous.Input.TargetDigest != input.TargetDigest || previous.Input.OperationDigest != input.OperationDigest {
			return WorkbenchProposal{}, false, ErrConflict
		}
		if previous.TombstonedAt != nil {
			return previous, true, ErrWorkbenchCommandExpired
		}
		return previous, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WorkbenchProposal{}, false, err
	}
	count, err := workbenchCommandCount(ctx, tx, input.Scope.Gaggle)
	if err != nil {
		return WorkbenchProposal{}, false, err
	}
	if count >= MaxWorkbenchCommands {
		return WorkbenchProposal{}, false, ErrFull
	}
	if err = childByteCapacity(ctx, tx, len(raw)+workbenchProposalAllowance+16*1024); err != nil {
		return WorkbenchProposal{}, false, err
	}
	id := fmt.Sprintf("workbench-%x", randomID())
	_, err = tx.ExecContext(ctx, `INSERT INTO workbench_proposals(id,key_digest,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,reserved_bytes) VALUES(?,?,?,?,?,?,?,?,?,?,?,'accepted',?,?)`, id, key, input.Scope.Gaggle, input.Scope.SourceBindingID, input.Scope.Actor.Issuer, input.Scope.Actor.Subject, input.RequestID, digest, input.TargetDigest, input.OperationDigest, raw, now.UnixNano(), workbenchProposalAllowance)
	if err != nil {
		return WorkbenchProposal{}, false, err
	}
	record, err := workbenchProposalTx(ctx, tx, input.Scope, id)
	if err != nil {
		return WorkbenchProposal{}, false, err
	}
	return record, false, tx.Commit()
}

// WorkbenchProposal loads actor/source-scoped custody; it grants no permission.
func (s *Store) WorkbenchProposal(ctx context.Context, scope WorkbenchCommandScope, id string) (WorkbenchProposal, error) {
	if !validWorkbenchScope(scope) || !validWorkbenchID(id) {
		return WorkbenchProposal{}, ErrTransition
	}
	record, err := scanWorkbenchProposal(s.db.QueryRowContext(ctx, "SELECT "+workbenchProposalColumns+" FROM workbench_proposals WHERE "+workbenchScopeWhere, workbenchScopeArgs(scope, id)...))
	if err == nil && record.TombstonedAt != nil {
		err = ErrWorkbenchCommandExpired
	}
	return record, err
}

func workbenchProposalTx(ctx context.Context, tx *sql.Tx, scope WorkbenchCommandScope, id string) (WorkbenchProposal, error) {
	return scanWorkbenchProposal(tx.QueryRowContext(ctx, "SELECT "+workbenchProposalColumns+" FROM workbench_proposals WHERE "+workbenchScopeWhere, workbenchScopeArgs(scope, id)...))
}
