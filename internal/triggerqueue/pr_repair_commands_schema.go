package triggerqueue

const prRepairCommandSchema = `
CREATE TABLE pr_repair_commands (
 id TEXT PRIMARY KEY, key_digest TEXT NOT NULL UNIQUE,
 gaggle TEXT NOT NULL, source_binding TEXT NOT NULL, issuer TEXT NOT NULL, subject TEXT NOT NULL, request_id TEXT NOT NULL,
 physical_target TEXT NOT NULL, request_digest TEXT NOT NULL, target_digest TEXT NOT NULL, operation_digest TEXT NOT NULL,
 request BLOB NOT NULL CHECK(length(request)<=2097152),
 evidence BLOB NOT NULL CHECK(length(evidence)<=131072), evidence_digest TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('accepted','attempting','confirmed','not-applied','unknown','tombstoned')),
 accepted_ns INTEGER NOT NULL, attempted_ns INTEGER, completed_ns INTEGER, tombstoned_ns INTEGER,
 receipt BLOB NOT NULL DEFAULT '' CHECK(length(receipt)<=16384), receipt_digest TEXT NOT NULL DEFAULT '',
 reserved_bytes INTEGER NOT NULL CHECK(reserved_bytes>=0),
 turn_id TEXT REFERENCES interactive_turns(id) ON DELETE RESTRICT,
 parent_id TEXT REFERENCES pr_repair_commands(id) ON DELETE RESTRICT
);
CREATE INDEX pr_repair_command_physical ON pr_repair_commands(physical_target,state);
CREATE INDEX pr_repair_command_scope ON pr_repair_commands(gaggle,source_binding,issuer,subject,id);
CREATE INDEX pr_repair_command_retention ON pr_repair_commands(state,completed_ns,tombstoned_ns,id);
CREATE INDEX pr_repair_command_turn ON pr_repair_commands(turn_id);
CREATE INDEX pr_repair_command_parent ON pr_repair_commands(parent_id);

-- Shared queue connections do not enable foreign_keys. These guards enforce
-- custody on every delete/compaction path without changing unrelated tables.
CREATE TRIGGER pr_repair_turn_delete BEFORE DELETE ON interactive_turns
WHEN EXISTS(SELECT 1 FROM pr_repair_commands WHERE turn_id=OLD.id)
BEGIN SELECT RAISE(ABORT,'PR repair retains source turn'); END;
CREATE TRIGGER pr_repair_turn_compact BEFORE UPDATE OF inputs,authority,tombstoned_ns ON interactive_turns
WHEN EXISTS(SELECT 1 FROM pr_repair_commands WHERE turn_id=OLD.id)
 AND (NEW.inputs!=OLD.inputs OR NEW.authority!=OLD.authority OR NEW.tombstoned_ns IS NOT OLD.tombstoned_ns)
BEGIN SELECT RAISE(ABORT,'PR repair retains source input'); END;
CREATE TRIGGER pr_repair_message_delete BEFORE DELETE ON interactive_messages
WHEN EXISTS(SELECT 1 FROM pr_repair_commands WHERE turn_id=OLD.turn_id)
BEGIN SELECT RAISE(ABORT,'PR repair retains source message'); END;
CREATE TRIGGER pr_repair_session_delete BEFORE DELETE ON interactive_sessions
WHEN EXISTS(SELECT 1 FROM pr_repair_commands r JOIN interactive_turns t ON t.id=r.turn_id WHERE t.session_id=OLD.id)
BEGIN SELECT RAISE(ABORT,'PR repair retains source session'); END;
CREATE TRIGGER pr_repair_parent_delete BEFORE DELETE ON pr_repair_commands
WHEN EXISTS(SELECT 1 FROM pr_repair_commands WHERE parent_id=OLD.id)
BEGIN SELECT RAISE(ABORT,'PR repair retains confirmed ancestor'); END;
CREATE TRIGGER pr_repair_parent_compact BEFORE UPDATE OF tombstoned_ns ON pr_repair_commands
WHEN NEW.tombstoned_ns IS NOT OLD.tombstoned_ns AND EXISTS(SELECT 1 FROM pr_repair_commands WHERE parent_id=OLD.id)
BEGIN SELECT RAISE(ABORT,'PR repair retains ancestor receipt'); END;
`
