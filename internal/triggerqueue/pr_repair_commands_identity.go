package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/providers"
)

func validPRRepairID(id string) bool {
	return len(id) == 39 && strings.HasPrefix(id, "repair-") && strings.Trim(id[7:], "0123456789abcdef") == ""
}
func repairNullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func validPRRepairOrigin(origin sessioning.PRRepairOrigin) bool {
	if !apiv1.ValidRunID(origin.RunID) || !validChildText(origin.SessionID, 128, true) || !validChildText(origin.TurnID, 128, true) || !validChildText(origin.MessageID, 128, true) {
		return false
	}
	for _, digest := range []string{origin.MessageDigest, origin.ConfigGeneration, origin.GooberDigest, origin.EnvelopeDigest, origin.InputDigest} {
		if !validSessionDigest(digest) {
			return false
		}
	}
	return true
}

// PRRepairDigests binds exact physical target and intent independently from a
// configuration generation. A harmless declaration edit cannot retarget a replay.
func PRRepairDigests(scope WorkbenchCommandScope, selection sessioning.PRRepairTarget, request sessioning.PRRepairRequest) (string, string, error) {
	if !validWorkbenchScope(scope) || selection.SourceBindingID != scope.SourceBindingID || sessioning.ValidatePRRepairTarget(&selection) != nil || !validChildText(request.RequestID, 128, true) || !validSessionText(request.Rationale) || len(request.Rationale) > 4096 || strings.TrimSpace(request.Rationale) == "" || (request.ParentCommandID != "" && !validPRRepairID(request.ParentCommandID)) {
		return "", "", ErrTransition
	}
	target := providers.RepairPullRequest{Repository: repairRepository(selection), RepositoryID: selection.RepositorySourceID, ID: selection.ID, StableID: selection.SourceID, Open: true, Head: "repair", Base: "base", HeadSHA: request.ExpectedHeadSHA, BaseSHA: selection.ExpectedHeadSHA}
	if _, err := repairNative("repair-"+strings.Repeat("0", 32), request, target); err != nil {
		return "", "", err
	}
	physical := selection
	physical.ExpectedHeadSHA = ""
	raw, _ := json.Marshal(struct {
		Kind, Gaggle string
		Target       sessioning.PRRepairTarget
	}{"pr-repair-target/v1", scope.Gaggle, physical})
	targetDigest := childDigest(raw)
	raw, _ = json.Marshal(struct {
		Kind, TargetDigest string
		Selection          sessioning.PRRepairTarget
		Request            sessioning.PRRepairRequest
	}{"pr-repair-operation/v1", targetDigest, selection, request})
	return targetDigest, childDigest(raw), nil
}
func repairRepository(selection sessioning.PRRepairTarget) providers.RepositoryRef {
	r := selection.Repository
	return providers.RepositoryRef{Provider: providers.ProviderKind(r.Provider), Owner: r.Owner, Project: r.Project, Name: r.Name}
}
func repairNative(id string, request sessioning.PRRepairRequest, target providers.RepairPullRequest) (providers.PullRequestRepair, error) {
	if len(request.Changes) < 1 || len(request.Changes) > providers.MaxPRRepairFiles {
		return providers.PullRequestRepair{}, ErrTransition
	}
	result := providers.PullRequestRepair{Target: target, CommandID: id, Message: "Goobers PR repair\n\n" + request.Rationale + "\n\nGoobers-Repair: " + id + "\n"}
	for _, change := range request.Changes {
		result.Changes = append(result.Changes, providers.PRRepairChange{Path: change.Path, PreviousBlob: change.PreviousBlob, Content: change.Content})
	}
	return result, providers.ValidatePullRequestRepair(result)
}

// Native returns exact retained provider intent, without acquiring credentials or
// claiming another attempt. The host must hold current permission and PR custody.
func (record PRRepairCommand) Native() (providers.PullRequestRepair, error) {
	if record.TombstonedAt != nil || record.Input.Target == nil || !validPRRepairID(record.ID) {
		return providers.PullRequestRepair{}, ErrTransition
	}
	return repairNative(record.ID, record.Input.Request, *record.Input.Target)
}

func verifyPRRepairTurn(ctx context.Context, tx *sql.Tx, input PRRepairCommandInput) error {
	origin := input.Origin
	turn, err := readSessionTurn(ctx, tx, "trigger-"+origin.RunID)
	if err != nil {
		return err
	}
	if turn.State != "running" || turn.Record.State != Dispatched || turn.Record.RunID != origin.RunID || turn.Session.State != sessioning.Running || turn.Session.ActiveTurnID != turn.ID || turn.ID != origin.TurnID || turn.Session.ID != origin.SessionID || turn.Session.Gaggle != input.Scope.Gaggle || turn.Message.ID != origin.MessageID || turn.Message.Actor == nil || *turn.Message.Actor != input.Scope.Actor || turn.Message.RepairTarget == nil || *turn.Message.RepairTarget != input.Selection {
		return ErrConflict
	}
	if turn.Session.ConfigGeneration != origin.ConfigGeneration || turn.Session.GooberDigest != origin.GooberDigest || sessioning.Digest(turn.Record.Payload) != origin.EnvelopeDigest || sessioning.MessageDigest(turn.Message.Text, turn.Message.RepairTarget) != origin.MessageDigest {
		return ErrConflict
	}
	var raw []byte
	var digest string
	if err = tx.QueryRowContext(ctx, `SELECT inputs,input_digest FROM interactive_turns WHERE id=?`, turn.ID).Scan(&raw, &digest); err != nil {
		return err
	}
	if digest != origin.InputDigest || sessioning.Digest(raw) != digest {
		return ErrConflict
	}
	parsed, err := sessioning.ParseExecutionInputs(raw, origin.RunID, input.Scope.Gaggle)
	if err != nil {
		return err
	}
	start, _ := json.Marshal(parsed.Start)
	if string(start) != string(turn.Record.Payload) {
		return ErrConflict
	}
	return nil
}
func verifyPRRepairParent(ctx context.Context, tx *sql.Tx, input PRRepairCommandInput) error {
	if input.Request.ParentCommandID == "" {
		if input.Request.ExpectedHeadSHA != input.Selection.ExpectedHeadSHA {
			return ErrConflict
		}
		return nil
	}
	prior, err := prRepairCommandTx(ctx, tx, input.Scope, input.Request.ParentCommandID)
	if err != nil {
		return err
	}
	if prior.TombstonedAt != nil || prior.State != "confirmed" || prior.Receipt == nil || !prior.Receipt.ProviderAcknowledged || !prior.Receipt.ObservedMatches || prior.Input.Origin != input.Origin || prior.Input.Selection != input.Selection || prior.Input.TargetDigest != input.TargetDigest || prior.Receipt.CommitID != input.Request.ExpectedHeadSHA {
		return ErrConflict
	}
	if prior.Input.Target == nil || input.Target == nil || prior.Input.Target.Head != input.Target.Head || prior.Input.Target.Base != input.Target.Base {
		return ErrConflict
	}
	return nil
}

func prRepairPhysicalTarget(selection sessioning.PRRepairTarget) string {
	// Native IDs retain writer custody across configured names, bindings and
	// gaggles. Case normalization here does not rewrite authority or audit input.
	raw, _ := json.Marshal(struct{ Provider, Owner, RepositoryID, SourceID string }{selection.Repository.Provider, strings.ToLower(selection.Repository.Owner), strings.ToLower(selection.RepositorySourceID), selection.SourceID})
	return childDigest(raw)
}

func verifyPRRepairWriterCustody(ctx context.Context, tx *sql.Tx, selection sessioning.PRRepairTarget) error {
	var active int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pr_repair_commands WHERE physical_target=? AND state IN ('accepted','attempting','unknown'))`, prRepairPhysicalTarget(selection)).Scan(&active)
	if err != nil {
		return err
	}
	if active != 0 {
		return ErrConflict
	}
	return nil
}
