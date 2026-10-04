package triggerqueue

// The active occurrence guard remains transactional, but cancelled/expired
// no-effect receipts no longer occupy it. Epoch identity and old plans remain
// immutable; a new command must pass current host preparation before accepting.
const humanRestartRetrySchema = `DROP TRIGGER human_restart_delete;
CREATE TABLE human_restart_plans_replacement (
 acceptance_id TEXT PRIMARY KEY NOT NULL,
 gaggle TEXT NOT NULL, source_run TEXT NOT NULL, terminal_seq INTEGER NOT NULL,
 stage TEXT NOT NULL, epoch TEXT NOT NULL UNIQUE,
 plan BLOB NOT NULL CHECK(length(plan) BETWEEN 1 AND 4194304),
 replay BLOB NOT NULL DEFAULT x'', retiring_ns INTEGER
);
INSERT INTO human_restart_plans_replacement(acceptance_id,gaggle,source_run,terminal_seq,stage,epoch,plan) SELECT acceptance_id,gaggle,source_run,terminal_seq,stage,epoch,plan FROM human_restart_plans;
DROP TABLE human_restart_plans;
ALTER TABLE human_restart_plans_replacement RENAME TO human_restart_plans;
CREATE INDEX human_restart_occurrence ON human_restart_plans(gaggle,source_run,terminal_seq,stage);
CREATE TRIGGER human_restart_delete AFTER DELETE ON triggers BEGIN
 DELETE FROM human_restart_plans WHERE acceptance_id=OLD.id;
END;`

// The cancellation transaction updates trigger state and control disposition
// together. Neither a requested cancellation nor an observed stop of attempted
// execution qualifies. Scoped provenance must still identify this exact epoch.
const humanRestartOccurrenceOccupied = `SELECT EXISTS(
 SELECT 1 FROM human_restart_plans h
 WHERE h.epoch=? OR (h.gaggle=? AND h.source_run=? AND h.terminal_seq=? AND h.stage=?
 AND NOT EXISTS(
  SELECT 1 FROM triggers t JOIN start_controls c ON c.acceptance_id=t.id
  WHERE t.id=h.acceptance_id AND t.state='rejected' AND t.run_id=''
   AND c.gaggle=h.gaggle AND c.disposition IN ('cancelled','expired') AND c.disposed_ns IS NOT NULL
   AND json_extract(c.scope,'$.Source')='human-restart'
   AND json_extract(c.scope,'$.ReservedRunID')=h.epoch
  ))
)`
