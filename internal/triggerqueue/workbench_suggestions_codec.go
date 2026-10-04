package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workbench"
)

const workbenchSuggestionColumns = "id,suggestion_key,gaggle,issuer,subject,input_digest,input,state,proposal_id,accepted_ns,linked_ns,completed_ns,tombstoned_ns,origin_run,config_generation"
const suggestionScopeWhere = "gaggle=? AND issuer=? AND subject=? AND id=?"

func validSuggestionScope(scope WorkbenchSuggestionScope) bool {
	return validWorkbenchScope(WorkbenchCommandScope{Gaggle: scope.Gaggle, Actor: scope.Actor, SourceBindingID: "suggestion"})
}
func suggestionScopeArgs(scope WorkbenchSuggestionScope, id string) []any {
	return []any{scope.Gaggle, scope.Actor.Issuer, scope.Actor.Subject, id}
}
func suggestionReviewKey(scope WorkbenchSuggestionScope, key string) string {
	raw, _ := json.Marshal([]string{scope.Gaggle, scope.Actor.Issuer, scope.Actor.Subject, key})
	return childDigest(raw)
}
func canonicalWorkbenchSuggestion(input WorkbenchSuggestionInput) ([]byte, string, error) {
	if !validSuggestionScope(input.Scope) || !validWorkbenchDigest(input.ConfigGeneration) || !validSuggestionOrigin(input) || !validSuggestionShape(input) || !validChildText(input.Reason, 4096, false) {
		return nil, "", ErrTransition
	}
	if err := validSuggestionDecision(input); err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(input)
	if err != nil || len(raw) > MaxWorkbenchSuggestionBytes {
		return nil, "", ErrTransition
	}
	return raw, childDigest(raw), nil
}
func validSuggestionShape(input WorkbenchSuggestionInput) bool {
	v := input.Suggestion.Proposal
	if !workbench.MatchesSuggestionKey(input.Suggestion) || !validChildText(v.Rationale, 4096, true) || !validChildText(v.Kind, 32, true) {
		return false
	}
	return validSuggestionEndpoint(input.Scope.Gaggle, v.From) && validSuggestionEndpoint(input.Scope.Gaggle, v.To)
}
func validSuggestionEndpoint(gaggle string, e workbench.SuggestionEndpoint) bool {
	if e.Creation != nil {
		return e.Ref == nil && e.Evidence == nil && workbenchBindingID.MatchString(e.Creation.SourceBindingID) && validChildText(e.Creation.RequestID, 128, true)
	}
	if e.Ref == nil || e.Evidence == nil || !validWorkbenchDigest(e.Evidence.SourceTargetDigest) {
		return false
	}
	scope := workbench.Scope{GaggleID: gaggle, Bindings: map[string]bool{e.Ref.SourceBindingID: true}}
	return scope.ValidateRef(*e.Ref) == nil
}
func validSuggestionOrigin(input WorkbenchSuggestionInput) bool {
	o := input.Suggestion.Origin
	path, err := journal.ArtifactPath("sha256:" + o.ArtifactDigest)
	return err == nil && o.ArtifactPath == path && apiv1.ValidRunID(o.RunID) && validChildText(o.StageID, 128, true) && o.Attempt > 0 && input.Branch >= 0 && input.StageSequence > 0 && input.ArtifactSequence > input.StageSequence
}
func validSuggestionDecision(input WorkbenchSuggestionInput) error {
	switch input.Decision {
	case "reject":
		if input.Proposal != nil {
			return ErrTransition
		}
	case "accept":
		if err := validSuggestionAcceptance(input); err != nil {
			return err
		}
	default:
		return ErrTransition
	}
	return nil
}
func validSuggestionAcceptance(input WorkbenchSuggestionInput) error {
	p := input.Proposal
	v := input.Suggestion.Proposal
	if p == nil || p.Scope.Gaggle != input.Scope.Gaggle || p.Scope.Actor != input.Scope.Actor || p.RequestID != SuggestionProposalKey(input.Suggestion.Key) || v.From.Ref == nil || v.To.Ref == nil || v.From.Creation != nil || v.To.Creation != nil {
		return ErrTransition
	}
	if _, _, err := canonicalWorkbenchProposal(*p); err != nil {
		return err
	}
	edit := p.Request.Relationship
	if edit == nil || edit.Action != "add" || (v.Kind != "references" && v.Kind != "contributes-to") {
		return ErrTransition
	}
	e := edit.Edge
	if e.Kind != v.Kind || e.From != *v.From.Ref || e.To != *v.To.Ref || e.Rationale != v.Rationale {
		return ErrTransition
	}
	return nil
}

// SuggestionProposalKey is stable within the existing actor/source command scope.
// It is not a provider idempotency key or a new remote retry authorization.
func SuggestionProposalKey(key string) string { return "suggestion-" + key }

func sameSuggestionDecision(a, b WorkbenchSuggestionInput) bool {
	if a.Decision != b.Decision || a.Reason != b.Reason {
		return false
	}
	if a.Proposal == nil || b.Proposal == nil {
		return a.Proposal == nil && b.Proposal == nil
	}
	return sameSuggestionProposal(*a.Proposal, *b.Proposal)
}
func sameSuggestionProposal(a, b WorkbenchProposalInput) bool { return reflect.DeepEqual(a, b) }

func scanWorkbenchSuggestion(row scanner) (WorkbenchSuggestion, error) {
	var r WorkbenchSuggestion
	var scope WorkbenchSuggestionScope
	var raw []byte
	var origin, generation string
	var accepted int64
	var linked, completed, tombstoned sql.NullInt64
	err := row.Scan(&r.ID, &r.Key, &scope.Gaggle, &scope.Actor.Issuer, &scope.Actor.Subject, &r.Digest, &raw, &r.State, &r.ProposalID, &accepted, &linked, &completed, &tombstoned, &origin, &generation)
	if err != nil {
		return r, err
	}
	r.AcceptedAt = time.Unix(0, accepted).UTC()
	r.LinkedAt, r.CompletedAt, r.TombstonedAt = workbenchTime(linked), workbenchTime(completed), workbenchTime(tombstoned)
	if !validSuggestionScope(scope) || !validWorkbenchID(r.ID) || !validWorkbenchDigest(r.Key) || !validWorkbenchDigest(r.Digest) {
		return r, ErrTransition
	}
	if r.TombstonedAt != nil {
		r.Input.Scope = scope
		if len(raw) != 0 || r.State != "tombstoned" || r.CompletedAt == nil || r.TombstonedAt.Before(*r.CompletedAt) {
			return r, ErrTransition
		}
		return r, nil
	}
	if len(raw) > MaxWorkbenchSuggestionBytes || childDigest(raw) != r.Digest || json.Unmarshal(raw, &r.Input) != nil {
		return r, ErrTransition
	}
	if r.Input.Scope != scope || r.Input.Suggestion.Key != r.Key || r.Input.Suggestion.Origin.RunID != origin || r.Input.ConfigGeneration != generation {
		return r, ErrTransition
	}
	if _, _, err = canonicalWorkbenchSuggestion(r.Input); err != nil {
		return r, err
	}
	return r, validSuggestionState(r)
}
func validSuggestionState(r WorkbenchSuggestion) error {
	if r.LinkedAt != nil && r.LinkedAt.Before(r.AcceptedAt) {
		return ErrTransition
	}
	if r.CompletedAt != nil && r.CompletedAt.Before(r.AcceptedAt) {
		return ErrTransition
	}
	switch r.State {
	case "accepting":
		if r.Input.Decision == "accept" && r.ProposalID == "" && r.LinkedAt == nil && r.CompletedAt == nil {
			return nil
		}
	case "linked":
		if r.Input.Decision == "accept" && validWorkbenchID(r.ProposalID) && r.LinkedAt != nil && r.CompletedAt == nil {
			return nil
		}
	case "rejected":
		if r.Input.Decision == "reject" && r.ProposalID == "" && r.LinkedAt == nil && r.CompletedAt != nil {
			return nil
		}
	}
	return ErrTransition
}
func suggestionTx(ctx context.Context, tx *sql.Tx, scope WorkbenchSuggestionScope, id string) (WorkbenchSuggestion, error) {
	return scanWorkbenchSuggestion(tx.QueryRowContext(ctx, "SELECT "+workbenchSuggestionColumns+" FROM workbench_suggestions WHERE "+suggestionScopeWhere, suggestionScopeArgs(scope, id)...))
}
