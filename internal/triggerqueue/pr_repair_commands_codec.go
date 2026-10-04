package triggerqueue

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

const prRepairColumns = "id,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,evidence,evidence_digest,state,accepted_ns,attempted_ns,completed_ns,tombstoned_ns,receipt,receipt_digest,turn_id,parent_id,physical_target"

type prRepairRequestEnvelope struct {
	Request   sessioning.PRRepairRequest
	Origin    sessioning.PRRepairOrigin
	Selection sessioning.PRRepairTarget
}

func canonicalPRRepairRequest(input PRRepairCommandInput) ([]byte, string, error) {
	if !validChildText(input.RequestID, 256, true) || !validPRRepairOrigin(input.Origin) {
		return nil, "", ErrTransition
	}
	target, operation, err := PRRepairDigests(input.Scope, input.Selection, input.Request)
	if err != nil || target != input.TargetDigest || operation != input.OperationDigest {
		return nil, "", ErrConflict
	}
	raw, err := json.Marshal(prRepairRequestEnvelope{input.Request, input.Origin, input.Selection})
	if err != nil || len(raw) > MaxPRRepairIntentBytes {
		return nil, "", ErrTransition
	}
	return raw, childDigest(raw), nil
}
func encodePRRepairTarget(input PRRepairCommandInput) ([]byte, error) {
	target := input.Target
	selection := input.Selection
	if target == nil || target.Repository != repairRepository(selection) || target.RepositoryID != selection.RepositorySourceID || target.ID != selection.ID || target.StableID != selection.SourceID || target.HeadSHA != input.Request.ExpectedHeadSHA {
		return nil, ErrConflict
	}
	if _, err := repairNative("repair-00000000000000000000000000000000", input.Request, *target); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(target)
	if err != nil || len(raw) > MaxPRRepairEvidenceBytes {
		return nil, ErrTransition
	}
	return raw, nil
}
func scanPRRepairCommand(row scanner) (PRRepairCommand, error) {
	var record PRRepairCommand
	var raw, evidence, receipt []byte
	var evidenceDigest, physicalTarget string
	var accepted int64
	var attempted, completed, tombstoned sql.NullInt64
	var turn, parent sql.NullString
	input := &record.Input
	err := row.Scan(&record.ID, &input.Scope.Gaggle, &input.Scope.SourceBindingID, &input.Scope.Actor.Issuer, &input.Scope.Actor.Subject, &input.RequestID, &record.RequestDigest, &input.TargetDigest, &input.OperationDigest, &raw, &evidence, &evidenceDigest, &record.State, &accepted, &attempted, &completed, &tombstoned, &receipt, &record.ReceiptDigest, &turn, &parent, &physicalTarget)
	if err != nil {
		return record, err
	}
	record.AcceptedAt = time.Unix(0, accepted).UTC()
	record.AttemptedAt, record.CompletedAt, record.TombstonedAt = workbenchTime(attempted), workbenchTime(completed), workbenchTime(tombstoned)
	if !validWorkbenchDigest(physicalTarget) || !validWorkbenchScope(input.Scope) || !validPRRepairID(record.ID) || !validWorkbenchDigest(record.RequestDigest) || !validWorkbenchDigest(input.TargetDigest) || !validWorkbenchDigest(input.OperationDigest) {
		return record, ErrTransition
	}
	if record.TombstonedAt != nil {
		if turn.Valid || parent.Valid || len(raw) > 0 || len(evidence) > 0 || len(receipt) > 0 {
			return record, ErrTransition
		}
		return record, validatePRRepairState(record)
	}
	if err = decodePRRepairInput(input, raw, evidence, record.RequestDigest, evidenceDigest); err != nil {
		return record, err
	}
	if physicalTarget != prRepairPhysicalTarget(input.Selection) {
		return record, ErrConflict
	}
	if !turn.Valid || turn.String != input.Origin.TurnID || parent.String != input.Request.ParentCommandID || parent.Valid != (input.Request.ParentCommandID != "") {
		return record, ErrConflict
	}
	if err = decodePRRepairReceipt(&record, receipt); err != nil {
		return record, err
	}

	return record, validatePRRepairState(record)
}
func decodePRRepairInput(input *PRRepairCommandInput, raw, evidence []byte, digest, evidenceDigest string) error {
	var envelope prRepairRequestEnvelope
	if len(raw) > MaxPRRepairIntentBytes || childDigest(raw) != digest || json.Unmarshal(raw, &envelope) != nil {
		return ErrConflict
	}
	input.Request, input.Origin, input.Selection = envelope.Request, envelope.Origin, envelope.Selection
	canonical, _, err := canonicalPRRepairRequest(*input)
	if err != nil || !bytes.Equal(raw, canonical) {
		return ErrConflict
	}
	input.Target = &providers.RepairPullRequest{}
	if len(evidence) > MaxPRRepairEvidenceBytes || childDigest(evidence) != evidenceDigest || json.Unmarshal(evidence, input.Target) != nil {
		return ErrConflict
	}
	canonical, err = encodePRRepairTarget(*input)
	if err != nil || !bytes.Equal(evidence, canonical) {
		return ErrConflict
	}
	return nil
}
func prRepairCommandTx(ctx context.Context, tx *sql.Tx, scope WorkbenchCommandScope, id string) (PRRepairCommand, error) {
	return scanPRRepairCommand(tx.QueryRowContext(ctx, "SELECT "+prRepairColumns+" FROM pr_repair_commands WHERE "+workbenchScopeWhere, workbenchScopeArgs(scope, id)...))
}
func validatePRRepairReceipt(record PRRepairCommand, receipt sessioning.PRRepairReceipt) error {
	if receipt.OperationDigest != record.Input.OperationDigest {
		return ErrConflict
	}
	if (receipt.ProviderAcknowledged || receipt.ObservedMatches) && (!receipt.MutationAttempted || !providers.ValidSourceCommit(receipt.CommitID) || receipt.CommitID == record.Input.Request.ExpectedHeadSHA) {
		return ErrTransition
	}
	if receipt.CommitID != "" && !receipt.ProviderAcknowledged && !receipt.ObservedMatches {
		return ErrTransition
	}
	switch receipt.Outcome {
	case "confirmed":
		if !receipt.ProviderAcknowledged || !receipt.ObservedMatches {
			return ErrTransition
		}
	case "not-applied":
		if receipt.MutationAttempted || receipt.ProviderAcknowledged || receipt.ObservedMatches || receipt.CommitID != "" {
			return ErrTransition
		}
	case "unknown":
	default:
		return ErrTransition
	}
	return nil
}
func validatePRRepairState(record PRRepairCommand) error {
	if record.State == "not-applied" && record.AttemptedAt == nil {
		if record.TombstonedAt != nil || record.CompletedAt == nil || record.CompletedAt.Before(record.AcceptedAt) || record.Receipt == nil || record.Receipt.Outcome != "not-applied" || !validWorkbenchDigest(record.ReceiptDigest) {
			return ErrTransition
		}
		return validatePRRepairReceipt(record, *record.Receipt)
	}

	projection := WorkbenchCommand{State: record.State, AcceptedAt: record.AcceptedAt, AttemptedAt: record.AttemptedAt, CompletedAt: record.CompletedAt, TombstonedAt: record.TombstonedAt, ReceiptDigest: record.ReceiptDigest}
	if record.Receipt != nil {
		projection.Receipt = &workbench.BacklogPatchReceipt{Outcome: record.Receipt.Outcome}
	}
	return validateWorkbenchRecordState(projection)
}

func decodePRRepairReceipt(record *PRRepairCommand, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	record.Receipt = &sessioning.PRRepairReceipt{}
	if len(raw) > prRepairReceiptAllowance || childDigest(raw) != record.ReceiptDigest || json.Unmarshal(raw, record.Receipt) != nil || validatePRRepairReceipt(*record, *record.Receipt) != nil {
		return ErrConflict
	}
	canonical, _ := json.Marshal(record.Receipt)
	if !bytes.Equal(raw, canonical) {
		return ErrConflict
	}
	return nil
}
