package triggerqueue

const needsHumanCommandSchema = `
CREATE TABLE needs_human_commands (
 id TEXT PRIMARY KEY, key_digest TEXT NOT NULL UNIQUE,
 gaggle TEXT NOT NULL, source_binding TEXT NOT NULL, issuer TEXT NOT NULL, subject TEXT NOT NULL, request_id TEXT NOT NULL,
 request_digest TEXT NOT NULL, target_digest TEXT NOT NULL, operation_digest TEXT NOT NULL,
 request BLOB NOT NULL CHECK(length(request)<=32768),
 evidence BLOB NOT NULL CHECK(length(evidence)<=1048576), evidence_digest TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('accepted','attempting','confirmed','not-applied','unknown','tombstoned')),
 accepted_ns INTEGER NOT NULL, attempted_ns INTEGER, completed_ns INTEGER, tombstoned_ns INTEGER,
 receipt BLOB NOT NULL DEFAULT '' CHECK(length(receipt)<=278528), receipt_digest TEXT NOT NULL DEFAULT '',
 reserved_bytes INTEGER NOT NULL CHECK(reserved_bytes>=0)
);
CREATE INDEX needs_human_command_scope ON needs_human_commands(gaggle,source_binding,issuer,subject,id);
CREATE INDEX needs_human_command_retention ON needs_human_commands(state,completed_ns,tombstoned_ns,id);
`
