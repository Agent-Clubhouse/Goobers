package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

// MaxWorkbenchSuggestionBytes bounds one review's evidence and linked edit.
// The source artifact has its own independent 256-KiB read bound.
const MaxWorkbenchSuggestionBytes = 32 << 10

// WorkbenchSuggestionScope is an attributed human decision, not a source grant.
type WorkbenchSuggestionScope struct {
	Gaggle string
	Actor  sessioning.Actor
}

// WorkbenchSuggestionInput retains host-proven evidence and the exact reviewed
// metadata command. No artifact-supplied identity can authorize this operation.
// Decision is accept or reject; Proposal is required only for acceptance.
type WorkbenchSuggestionInput struct {
	Scope                           WorkbenchSuggestionScope
	Suggestion                      workbench.BoundSuggestion
	ConfigGeneration                string
	ArtifactSequence, StageSequence uint64
	Branch                          int
	Decision, Reason                string
	Proposal                        *WorkbenchProposalInput
}

// WorkbenchSuggestion is operational review custody, never source graph truth.
// Linked means handed to a proposal command, not that its effects succeeded.
type WorkbenchSuggestion struct {
	ID, Key, Digest, State, ProposalID  string
	Input                               WorkbenchSuggestionInput
	AcceptedAt                          time.Time
	LinkedAt, CompletedAt, TombstonedAt *time.Time
}

// AcceptWorkbenchSuggestion commits a decision before any proposal effect.
// Same actor/key repeats preserve the first origin and rationale. A changed
// decision or accepted edit conflicts rather than manufacturing another PR.
func (s *Store) AcceptWorkbenchSuggestion(ctx context.Context, input WorkbenchSuggestionInput, now time.Time) (WorkbenchSuggestion, bool, error) {
	raw, digest, err := canonicalWorkbenchSuggestion(input)
	if err != nil || now.IsZero() {
		return WorkbenchSuggestion{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkbenchSuggestion{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	key := suggestionReviewKey(input.Scope, input.Suggestion.Key)
	previous, err := scanWorkbenchSuggestion(tx.QueryRowContext(ctx, "SELECT "+workbenchSuggestionColumns+" FROM workbench_suggestions WHERE key_digest=?", key))
	if err == nil {
		if previous.TombstonedAt != nil {
			return previous, true, ErrWorkbenchCommandExpired
		}
		if !sameSuggestionDecision(previous.Input, input) {
			return WorkbenchSuggestion{}, false, ErrConflict
		}
		return previous, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WorkbenchSuggestion{}, false, err
	}
	count, err := workbenchCommandCount(ctx, tx, input.Scope.Gaggle)
	if err != nil {
		return WorkbenchSuggestion{}, false, err
	}
	if count >= MaxWorkbenchCommands {
		return WorkbenchSuggestion{}, false, ErrFull
	}
	if err = childByteCapacity(ctx, tx, len(raw)+1024); err != nil {
		return WorkbenchSuggestion{}, false, err
	}
	id := fmt.Sprintf("workbench-%x", randomID())
	state, completed := "accepting", any(nil)
	if input.Decision == "reject" {
		state, completed = "rejected", now.UnixNano()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workbench_suggestions(id,key_digest,gaggle,issuer,subject,suggestion_key,input_digest,input,state,accepted_ns,completed_ns,origin_run,config_generation) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, key, input.Scope.Gaggle, input.Scope.Actor.Issuer, input.Scope.Actor.Subject, input.Suggestion.Key, digest, raw, state, now.UnixNano(), completed, input.Suggestion.Origin.RunID, input.ConfigGeneration)
	if err != nil {
		return WorkbenchSuggestion{}, false, err
	}
	record, err := suggestionTx(ctx, tx, input.Scope, id)
	if err != nil {
		return WorkbenchSuggestion{}, false, err
	}
	return record, false, tx.Commit()
}

// FindWorkbenchSuggestion finds a prior normalized decision without source I/O.
// Every caller must independently reauthorize all retained source references.
func (s *Store) FindWorkbenchSuggestion(ctx context.Context, scope WorkbenchSuggestionScope, key string) (WorkbenchSuggestion, error) {
	if !validSuggestionScope(scope) || !validWorkbenchDigest(key) {
		return WorkbenchSuggestion{}, ErrTransition
	}
	return checkedSuggestion(scanWorkbenchSuggestion(s.db.QueryRowContext(ctx, "SELECT "+workbenchSuggestionColumns+" FROM workbench_suggestions WHERE key_digest=?", suggestionReviewKey(scope, key))))
}

// WorkbenchSuggestion reads exact actor-scoped review custody.
func (s *Store) WorkbenchSuggestion(ctx context.Context, scope WorkbenchSuggestionScope, id string) (WorkbenchSuggestion, error) {
	if !validSuggestionScope(scope) || !validWorkbenchID(id) {
		return WorkbenchSuggestion{}, ErrTransition
	}
	return checkedSuggestion(scanWorkbenchSuggestion(s.db.QueryRowContext(ctx, "SELECT "+workbenchSuggestionColumns+" FROM workbench_suggestions WHERE "+suggestionScopeWhere, suggestionScopeArgs(scope, id)...)))
}

// LinkWorkbenchSuggestion validates actual proposal custody in the same database
// transaction. A retry may link the same command, but never another intent.
func (s *Store) LinkWorkbenchSuggestion(ctx context.Context, scope WorkbenchSuggestionScope, id, proposalID string, now time.Time) (WorkbenchSuggestion, error) {
	if !validSuggestionScope(scope) || !validWorkbenchID(id) || !validWorkbenchID(proposalID) || now.IsZero() {
		return WorkbenchSuggestion{}, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkbenchSuggestion{}, err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := suggestionTx(ctx, tx, scope, id)
	if err != nil {
		return r, err
	}
	if r.Input.Proposal == nil || r.TombstonedAt != nil || now.Before(r.AcceptedAt) {
		return r, ErrTransition
	}
	proposal, err := workbenchProposalTx(ctx, tx, r.Input.Proposal.Scope, proposalID)
	if err != nil || !sameSuggestionProposal(*r.Input.Proposal, proposal.Input) {
		return r, ErrConflict
	}
	if r.ProposalID != "" {
		if r.ProposalID != proposalID {
			return r, ErrConflict
		}
		return r, nil
	}
	if r.State != "accepting" {
		return r, ErrTransition
	}
	_, err = tx.ExecContext(ctx, `UPDATE workbench_suggestions SET state='linked',proposal_id=?,linked_ns=? WHERE id=? AND state='accepting'`, proposalID, now.UnixNano(), id)
	if err != nil {
		return r, err
	}
	r, err = suggestionTx(ctx, tx, scope, id)
	if err != nil {
		return r, err
	}
	return r, tx.Commit()
}

func checkedSuggestion(r WorkbenchSuggestion, err error) (WorkbenchSuggestion, error) {
	if err == nil && r.TombstonedAt != nil {
		err = ErrWorkbenchCommandExpired
	}
	return r, err
}
