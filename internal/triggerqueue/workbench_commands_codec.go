package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/workbench"
)

const workbenchCommandColumns = "id,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,attempted_ns,completed_ns,tombstoned_ns,receipt,receipt_digest"
const workbenchScopeWhere = "gaggle=? AND source_binding=? AND issuer=? AND subject=? AND id=?"

var workbenchBindingID = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

func validWorkbenchScope(scope WorkbenchCommandScope) bool {
	return validChildText(scope.Gaggle, 128, true) && workbenchBindingID.MatchString(scope.SourceBindingID) && validChildText(scope.Actor.Issuer, 2048, true) && !strings.HasPrefix(scope.Actor.Issuer, "goobers/") && validChildText(scope.Actor.Subject, 512, true)
}
func validWorkbenchID(id string) bool {
	return strings.HasPrefix(id, "workbench-") && len(id) == 42 && strings.Trim(id[10:], "0123456789abcdef") == ""
}
func validWorkbenchDigest(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}
func workbenchScopeArgs(scope WorkbenchCommandScope, id string) []any {
	return []any{scope.Gaggle, scope.SourceBindingID, scope.Actor.Issuer, scope.Actor.Subject, id}
}
func workbenchCommandKey(scope WorkbenchCommandScope, key string) string {
	raw, _ := json.Marshal([]string{scope.Gaggle, scope.SourceBindingID, scope.Actor.Issuer, scope.Actor.Subject, key})
	return childDigest(raw)
}
func canonicalWorkbenchInput(input WorkbenchCommandInput) ([]byte, string, error) {
	if !validWorkbenchScope(input.Scope) || !validChildText(input.RequestID, 256, true) || !validWorkbenchDigest(input.TargetDigest) || !validWorkbenchDigest(input.OperationDigest) || !validWorkbenchPatch(input.Request) {
		return nil, "", ErrTransition
	}
	// Nil and empty list fields have the same native replacement semantics. Go's
	// typed JSON encoding also fixes field order; no arbitrary authority keys exist.
	raw, err := json.Marshal(input.Request)
	if err != nil || len(raw) > MaxWorkbenchRequestBytes {
		return nil, "", ErrTransition
	}
	return raw, childDigest(raw), nil
}
func validWorkbenchPatch(request workbench.BacklogPatchRequest) bool {
	if !validChildText(request.ID, 128, true) || !validChildText(request.SourceID, 512, true) || !validChildText(request.ExpectedRevision, 256, true) {
		return false
	}
	text := func(value string) bool {
		return len(value) <= MaxWorkbenchRequestBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
	}
	switch request.Field {
	case "title", "description", "state":
		return request.Value != nil && len(request.Values) == 0 && text(*request.Value)
	case "labels", "assignees":
		if request.Value != nil || len(request.Values) > 128 {
			return false
		}
		for _, value := range request.Values {
			if !validChildText(value, 400, true) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
func scanWorkbenchCommand(row scanner) (WorkbenchCommand, error) {
	var record WorkbenchCommand
	var raw, receipt []byte
	var accepted int64
	var attempted, completed, tombstoned sql.NullInt64
	i := &record.Input
	err := row.Scan(&record.ID, &i.Scope.Gaggle, &i.Scope.SourceBindingID, &i.Scope.Actor.Issuer, &i.Scope.Actor.Subject, &i.RequestID, &record.RequestDigest, &i.TargetDigest, &i.OperationDigest, &raw, &record.State, &accepted, &attempted, &completed, &tombstoned, &receipt, &record.ReceiptDigest)
	if err != nil {
		return record, err
	}
	record.AcceptedAt = time.Unix(0, accepted).UTC()
	record.AttemptedAt = workbenchTime(attempted)
	record.CompletedAt = workbenchTime(completed)
	record.TombstonedAt = workbenchTime(tombstoned)
	if !validWorkbenchScope(i.Scope) || !validWorkbenchID(record.ID) || !validWorkbenchDigest(record.RequestDigest) || !validWorkbenchDigest(i.TargetDigest) || !validWorkbenchDigest(i.OperationDigest) {
		return record, errors.New("invalid workbench command custody")
	}
	if record.TombstonedAt != nil {
		return record, validateWorkbenchRecordState(record)
	}
	if len(raw) > MaxWorkbenchRequestBytes || childDigest(raw) != record.RequestDigest || json.Unmarshal(raw, &i.Request) != nil || !validWorkbenchPatch(i.Request) {
		return record, errors.New("workbench request custody differs")
	}
	if len(receipt) > 0 {
		record.Receipt = &workbench.BacklogPatchReceipt{}
		if len(receipt) > MaxWorkbenchReceiptBytes || childDigest(receipt) != record.ReceiptDigest || json.Unmarshal(receipt, record.Receipt) != nil || validateWorkbenchReceipt(record, *record.Receipt) != nil {
			return record, errors.New("workbench receipt custody differs")
		}
	}
	return record, validateWorkbenchRecordState(record)
}
func workbenchTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	v := time.Unix(0, value.Int64).UTC()
	return &v
}
func workbenchCommandTx(ctx context.Context, tx *sql.Tx, scope WorkbenchCommandScope, id string) (WorkbenchCommand, error) {
	return scanWorkbenchCommand(tx.QueryRowContext(ctx, "SELECT "+workbenchCommandColumns+" FROM workbench_commands WHERE "+workbenchScopeWhere, workbenchScopeArgs(scope, id)...))
}

func validateWorkbenchRecordState(record WorkbenchCommand) error {
	invalid := errors.New("workbench command lifecycle custody differs")
	if record.AttemptedAt != nil && record.AttemptedAt.Before(record.AcceptedAt) {
		return invalid
	}
	if record.CompletedAt != nil && (record.AttemptedAt == nil || record.CompletedAt.Before(*record.AttemptedAt)) {
		return invalid
	}
	valid := false
	switch record.State {
	case "accepted", "attempting":
		valid = validPendingWorkbenchRecord(record)
	case "confirmed", "not-applied", "unknown":
		valid = record.AttemptedAt != nil && record.CompletedAt != nil && record.Receipt != nil && record.Receipt.Outcome == record.State && record.TombstonedAt == nil
	case "tombstoned":
		valid = validWorkbenchTombstone(record)
	}
	if !valid {
		return invalid
	}
	return nil
}
func validPendingWorkbenchRecord(record WorkbenchCommand) bool {
	attempted := record.State == "attempting"
	return (record.AttemptedAt != nil) == attempted && record.CompletedAt == nil && record.Receipt == nil && record.ReceiptDigest == "" && record.TombstonedAt == nil
}
func validWorkbenchTombstone(record WorkbenchCommand) bool {
	return record.CompletedAt != nil && record.TombstonedAt != nil && !record.TombstonedAt.Before(*record.CompletedAt) && validWorkbenchDigest(record.ReceiptDigest)
}
