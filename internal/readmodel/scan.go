package readmodel

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/goobers/goobers/internal/journal"
)

// The single definition of "a run row on the wire".
//
// Both bounded reads — the list (§5.7) and the latest-outcome aggregate (§5.2) —
// select these columns and decode them with these helpers. Sharing them is not
// tidiness: the two queries answer the same question from different pages, and
// a column added to one and not the other is how the Workflows page and the Runs
// page come to disagree about the same run. Here that cannot happen without
// changing one list that both compile against.
//
// The table is aliased `r` in both queries so the column list is literally
// identical text in each.

// runColumns is the complete projected row, in scan order.
const runColumns = `r.run_id, r.gaggle, r.workflow, r.workflow_version, r.workflow_digest,
	r.goober_digest, r.trigger_kind, r.trigger_ref, r.phase, r.terminal, r.current_stage,
	r.started_at, r.finished_at, r.last_activity_at, r.last_seq,
	r.repass_count, r.retry_count, r.policy_retry_count, r.infra_retry_count,
	r.outcome_verdict, r.outcome_target, r.disposition,
	r.any_token_measured, r.any_premium_measured, r.any_cost_measured, r.any_retry_waste,
	r.operator_json`

// nullables holds the columns SQL can return as NULL, between Scan and decode.
//
// They live on the row rather than in the caller so that runScanTargets and
// finishScan are two halves of one operation the caller cannot get wrong by
// forgetting a variable.
type nullables struct {
	digest, gooberDigest, triggerKind, triggerRef sql.NullString
	currentStage, verdict, target                 sql.NullString
	startedAt, finishedAt, lastActivity           sql.NullString
	phase                                         string
	terminal                                      int

	// The measurement rollups are stored NOT NULL DEFAULT 0, so they scan as
	// plain ints -- but they are read through the same struct so that adding a
	// column cannot desynchronise runColumns from runScanTargets.
	anyToken, anyPremium, anyCost, anyRetryWaste int
	operatorJSON                                 string
}

// runScanTargets returns Scan destinations for runColumns, in the same order.
func runScanTargets(out *RunRow) []any {
	out.scratch = &nullables{}
	n := out.scratch
	return []any{
		&out.RunID, &out.Gaggle, &out.Workflow, &out.WorkflowVersion, &n.digest,
		&n.gooberDigest, &n.triggerKind, &n.triggerRef, &n.phase, &n.terminal, &n.currentStage,
		&n.startedAt, &n.finishedAt, &n.lastActivity, &out.LastSeq,
		&out.RepassCount, &out.RetryCount, &out.PolicyRetryCount, &out.InfraRetryCount,
		&n.verdict, &n.target, &out.Disposition,
		&n.anyToken, &n.anyPremium, &n.anyCost, &n.anyRetryWaste,
		&n.operatorJSON,
	}
}

// finishScan resolves the nullable columns onto the row.
func (r *RunRow) finishScan() error {
	n := r.scratch
	if n == nil {
		return fmt.Errorf("readmodel: finishScan called without runScanTargets")
	}
	r.scratch = nil

	r.WorkflowDigest = n.digest.String
	r.GooberDigest = n.gooberDigest.String
	r.TriggerKind = n.triggerKind.String
	r.TriggerRef = n.triggerRef.String
	r.Phase = journal.RunPhase(n.phase)
	r.Terminal = n.terminal != 0
	r.CurrentStage = n.currentStage.String
	r.OutcomeVerdict = n.verdict.String
	r.OutcomeTarget = n.target.String
	r.AnyTokenMeasured = n.anyToken != 0
	r.AnyPremiumMeasured = n.anyPremium != 0
	r.AnyCostMeasured = n.anyCost != 0
	r.AnyRetryWaste = n.anyRetryWaste != 0
	if err := json.Unmarshal([]byte(n.operatorJSON), &r.Operator); err != nil {
		return fmt.Errorf("readmodel: decode operator facts: %w", err)
	}

	var err error
	if r.StartedAt, err = requiredTime(n.startedAt); err != nil {
		return err
	}
	if r.FinishedAt, err = optionalTime(n.finishedAt); err != nil {
		return err
	}
	if n.lastActivity.Valid {
		if r.LastActivity, err = requiredTime(n.lastActivity); err != nil {
			return err
		}
	}
	return nil
}

// scanRunRow decodes one complete run row.
func scanRunRow(rows *sql.Rows) (RunRow, error) {
	var row RunRow
	if err := rows.Scan(runScanTargets(&row)...); err != nil {
		return RunRow{}, fmt.Errorf("readmodel: scan run row: %w", err)
	}
	if err := row.finishScan(); err != nil {
		return RunRow{}, err
	}
	return row, nil
}
