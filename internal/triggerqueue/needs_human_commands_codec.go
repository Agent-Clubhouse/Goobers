package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/lifecycle"
	"github.com/goobers/goobers/internal/workbench"
)

const needsHumanColumns = "id,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,evidence,evidence_digest,state,accepted_ns,attempted_ns,completed_ns,tombstoned_ns,receipt,receipt_digest"

type needsHumanRequestEnvelope struct {
	Request workbench.NeedsHumanResolutionRequest
	Origin  workbench.NeedsHumanResolutionOrigin
}

func validNeedsHumanID(id string) bool {
	return strings.HasPrefix(id, "resolution-") && len(id) == 43 && strings.Trim(id[11:], "0123456789abcdef") == ""
}
func canonicalNeedsHumanRequest(input NeedsHumanCommandInput) ([]byte, string, error) {
	if !validWorkbenchScope(input.Scope) || !validChildText(input.RequestID, 256, true) || !validWorkbenchDigest(input.TargetDigest) || !validWorkbenchDigest(input.OperationDigest) || !validNeedsHumanRequest(input.Request) || !validNeedsHumanOrigin(input.Origin) {
		return nil, "", ErrTransition
	}
	raw, err := json.Marshal(needsHumanRequestEnvelope{input.Request, input.Origin})
	if err != nil || len(raw) > 32<<10 {
		return nil, "", ErrTransition
	}
	return raw, childDigest(raw), nil
}
func validNeedsHumanOrigin(origin workbench.NeedsHumanResolutionOrigin) bool {
	return apiv1.ValidRunID(origin.RunID) && validChildText(origin.SessionID, 128, true) && validChildText(origin.TurnID, 128, true) && validChildText(origin.MessageID, 128, true) && validWorkbenchDigest(origin.MessageDigest) && validSessionDigest(origin.GooberDigest)
}
func validNeedsHumanRequest(request workbench.NeedsHumanResolutionRequest) bool {
	if !validChildText(request.ID, 128, true) || !validChildText(request.SourceID, 512, true) || !validChildText(request.ExpectedRevision, 256, true) || !validWorkbenchDigest(request.ObservationDigest) || (!validSessionText(request.Rationale) || len(request.Rationale) > 8192 || strings.TrimSpace(request.Rationale) == "") || len(request.Evidence) < 1 || len(request.Evidence) > 8 {
		return false
	}
	if request.Basis.Kind != "current-human-message" && request.Basis.Kind != "learned-record" {
		return false
	}
	if !validNeedsHumanEvidence(request.Basis) {
		return false
	}
	for _, evidence := range request.Evidence {
		if !validNeedsHumanEvidence(evidence) {
			return false
		}
	}
	return true
}
func validNeedsHumanEvidence(ref workbench.NeedsHumanEvidenceRef) bool {
	if !validChildText(ref.ID, 128, true) || !validWorkbenchDigest(ref.Digest) {
		return false
	}
	switch ref.Kind {
	case "current-human-message", "learned-record", "session-message", "native-command", "source-comment":
		return true
	}
	return false
}
func encodeNeedsHumanObservation(input NeedsHumanCommandInput) ([]byte, error) {
	observation := input.Observation
	if observation == nil || len(observation.Comments) > 100 || !validWorkbenchDigest(observation.LearnedRecordDigest) {
		return nil, ErrTransition
	}
	digest, err := workbench.NeedsHumanObservationDigest(*observation)
	if err != nil || digest != observation.Digest || digest != input.Request.ObservationDigest {
		return nil, ErrConflict
	}
	if !observation.MarkerPresent || !observation.CommentsComplete || !observation.DependenciesComplete || !observation.LearnedComplete || len(observation.WaitReasons) > 0 {
		return nil, ErrTransition
	}
	item := observation.Item
	if item.Ref.GaggleID != input.Scope.Gaggle || item.Ref.SourceBindingID != input.Scope.SourceBindingID || item.Ref.Kind != "work-item" || item.Ref.SourceID != input.Request.SourceID || item.Locator.ID != input.Request.ID || item.Revision != input.Request.ExpectedRevision {
		return nil, ErrConflict
	}
	if !validNeedsHumanBasis(input) || !resolvedNeedsHumanDependencies(observation.Dependencies) || !resolvedNeedsHumanDependencies(observation.LearnedDependencies) {
		return nil, ErrTransition
	}
	raw, err := json.Marshal(observation)
	if err != nil || len(raw) > MaxNeedsHumanEvidenceBytes {
		return nil, ErrTransition
	}
	return raw, nil
}
func validNeedsHumanBasis(input NeedsHumanCommandInput) bool {
	basis := input.Request.Basis
	if basis.Kind == "current-human-message" {
		return basis.ID == input.Origin.MessageID && basis.Digest == input.Origin.MessageDigest
	}
	return input.Observation.LearnedReason != "" && basis.ID == input.Request.ID && basis.Digest == input.Observation.LearnedRecordDigest
}
func resolvedNeedsHumanDependencies(dependencies []workbench.NeedsHumanDependency) bool {
	if len(dependencies) > 64 {
		return false
	}
	for _, dependency := range dependencies {
		if dependency.Open || !dependency.Verified {
			return false
		}
	}
	return true
}
func scanNeedsHumanCommand(row scanner) (NeedsHumanCommand, error) {
	var record NeedsHumanCommand
	var raw, evidence, receipt []byte
	var evidenceDigest string
	var accepted int64
	var attempted, completed, tombstoned sql.NullInt64
	input := &record.Input
	err := row.Scan(&record.ID, &input.Scope.Gaggle, &input.Scope.SourceBindingID, &input.Scope.Actor.Issuer, &input.Scope.Actor.Subject, &input.RequestID, &record.RequestDigest, &input.TargetDigest, &input.OperationDigest, &raw, &evidence, &evidenceDigest, &record.State, &accepted, &attempted, &completed, &tombstoned, &receipt, &record.ReceiptDigest)
	if err != nil {
		return record, err
	}
	record.AcceptedAt = time.Unix(0, accepted).UTC()
	record.AttemptedAt = workbenchTime(attempted)
	record.CompletedAt = workbenchTime(completed)
	record.TombstonedAt = workbenchTime(tombstoned)
	if !validWorkbenchScope(input.Scope) || !validNeedsHumanID(record.ID) || !validWorkbenchDigest(record.RequestDigest) || !validWorkbenchDigest(input.TargetDigest) || !validWorkbenchDigest(input.OperationDigest) {
		return record, ErrTransition
	}
	if record.TombstonedAt != nil {
		return record, validateNeedsHumanState(record)
	}
	if err = decodeNeedsHumanInput(input, raw, evidence, record.RequestDigest, evidenceDigest); err != nil {
		return record, err
	}
	if len(receipt) > 0 {
		record.Receipt = &workbench.NeedsHumanResolutionReceipt{}
		if len(receipt) > MaxWorkbenchReceiptBytes || childDigest(receipt) != record.ReceiptDigest || json.Unmarshal(receipt, record.Receipt) != nil || validateNeedsHumanReceipt(record, *record.Receipt) != nil {
			return record, errors.New("needs-human receipt custody differs")
		}
	}
	return record, validateNeedsHumanState(record)
}
func decodeNeedsHumanInput(input *NeedsHumanCommandInput, raw, evidence []byte, digest, evidenceDigest string) error {
	var envelope needsHumanRequestEnvelope
	if len(raw) > 32<<10 || childDigest(raw) != digest || json.Unmarshal(raw, &envelope) != nil {
		return ErrConflict
	}
	input.Request = envelope.Request
	input.Origin = envelope.Origin
	if _, _, err := canonicalNeedsHumanRequest(*input); err != nil {
		return err
	}
	input.Observation = &workbench.NeedsHumanObservation{}
	if len(evidence) > MaxNeedsHumanEvidenceBytes || childDigest(evidence) != evidenceDigest || json.Unmarshal(evidence, input.Observation) != nil {
		return ErrConflict
	}
	_, err := encodeNeedsHumanObservation(*input)
	return err
}
func needsHumanCommandTx(ctx context.Context, tx *sql.Tx, scope WorkbenchCommandScope, id string) (NeedsHumanCommand, error) {
	return scanNeedsHumanCommand(tx.QueryRowContext(ctx, "SELECT "+needsHumanColumns+" FROM needs_human_commands WHERE "+workbenchScopeWhere, workbenchScopeArgs(scope, id)...))
}
func validateNeedsHumanReceipt(record NeedsHumanCommand, receipt workbench.NeedsHumanResolutionReceipt) error {
	if receipt.ObservedClear && receipt.Observed != nil && slices.Contains(receipt.Observed.Labels, lifecycle.LabelNeedsHuman) {
		return ErrTransition
	}
	// Reuse the existing strict acknowledgement/observation validation without
	// exposing a generic labels command or synthesizing a provider operation.
	projection := WorkbenchCommand{Input: WorkbenchCommandInput{Scope: record.Input.Scope, OperationDigest: record.Input.OperationDigest, Request: workbench.BacklogPatchRequest{ID: record.Input.Request.ID, SourceID: record.Input.Request.SourceID}}}
	return validateWorkbenchReceipt(projection, workbench.BacklogPatchReceipt{OperationDigest: receipt.OperationDigest, Outcome: receipt.Outcome, RevisionSemantics: receipt.RevisionSemantics, ProviderAcknowledged: receipt.ProviderAcknowledged, ObservedMatches: receipt.ObservedClear, Observed: receipt.Observed})
}
func validateNeedsHumanState(record NeedsHumanCommand) error {
	projection := WorkbenchCommand{State: record.State, AcceptedAt: record.AcceptedAt, AttemptedAt: record.AttemptedAt, CompletedAt: record.CompletedAt, TombstonedAt: record.TombstonedAt, ReceiptDigest: record.ReceiptDigest}
	if record.Receipt != nil {
		projection.Receipt = &workbench.BacklogPatchReceipt{Outcome: record.Receipt.Outcome}
	}
	return validateWorkbenchRecordState(projection)
}
