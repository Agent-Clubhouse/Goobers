package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
)

// Source custody and queue disposition change in one transaction. Child result
// capture remains with the existing terminal observer, which consumes this
// durable rejection; the parent slot stays owned until that result is acknowledged.
func settleUnstartedControlledSource(ctx context.Context, tx *sql.Tx, c StartControl, disposition string, now time.Time) error {
	var child, session bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM child_lineages WHERE acceptance_id=?),EXISTS(SELECT 1 FROM interactive_turns WHERE acceptance_id=?)`, c.Record.ID, c.Record.ID).Scan(&child, &session); err != nil {
		return err
	}
	if child && session {
		return ErrTypedStartSettlement
	}
	if child || c.Scope.Source == "child" {
		if err := checkUnstartedControlledChild(ctx, tx, c); err != nil {
			return err
		}
	} else if session || c.Scope.Source == "session" {
		if err := settleUnstartedControlledSession(ctx, tx, c, disposition, now); err != nil {
			return err
		}
	}
	return settleControlledReceipt(ctx, tx, c, disposition, now)
}

func checkUnstartedControlledChild(ctx context.Context, tx *sql.Tx, c StartControl) error {
	child, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+" WHERE c.acceptance_id=?", c.Record.ID))
	if err != nil {
		return ErrTypedStartSettlement
	}
	if c.Scope.Source != "child" || c.Scope.Gaggle != child.Identity.Gaggle || c.Scope.ReservedRunID != child.RunID || child.State != ChildQueued || child.ExecutionEpoch != 0 || child.ExecutionRunID != "" || child.ResultRef != "" || !child.TerminalAt.IsZero() || !child.TombstonedAt.IsZero() {
		return ErrTypedStartSettlement
	}
	return nil
}

func settleUnstartedControlledSession(ctx context.Context, tx *sql.Tx, c StartControl, disposition string, now time.Time) error {
	t, err := readSessionTurn(ctx, tx, c.Record.ID)
	if err != nil {
		return ErrTypedStartSettlement
	}
	if c.Scope.Source != "session" || c.Scope.Gaggle != t.Session.Gaggle || c.Scope.Generation != t.Session.ConfigGeneration || c.Scope.Workflow != "session/"+t.Session.ID || c.Scope.ReservedRunID != strings.TrimPrefix(c.Record.ID, "trigger-") {
		return ErrTypedStartSettlement
	}
	if t.State != "queued" || t.Record.State != Accepted || t.Record.RunID != "" || t.Session.ActiveTurnID == t.ID || t.Session.State == sessioning.Closed || t.Session.State == sessioning.CancelRequested {
		return ErrTypedStartSettlement
	}
	result := SessionCompletion{Outcome: "cancelled", Text: "This queued turn was cancelled before an agent started."}
	if disposition == "expired" {
		result = SessionCompletion{Outcome: "rejected", Text: "This queued turn expired before an agent started."}
	}
	raw, _ := json.Marshal(result)
	return keepSessionCompletion(ctx, tx, t, result, "sha256:"+childDigest(raw), now)
}
