package triggerqueue

const sessionSchema = `
CREATE TABLE interactive_sessions (
 id TEXT PRIMARY KEY, gaggle TEXT NOT NULL, title TEXT NOT NULL,
 profile BLOB NOT NULL CHECK(length(profile)<=4096), creator BLOB NOT NULL CHECK(length(creator)<=4096),
 state TEXT NOT NULL CHECK(state IN ('idle','queued','running','cancel-requested','closed')),
 created_ns INTEGER NOT NULL, updated_ns INTEGER NOT NULL, closed_ns INTEGER,
 next_sequence INTEGER NOT NULL DEFAULT 1, active_turn TEXT NOT NULL DEFAULT '', last_outcome TEXT NOT NULL DEFAULT ''
);
CREATE INDEX interactive_session_scope ON interactive_sessions(gaggle,id);
CREATE INDEX interactive_session_retention ON interactive_sessions(closed_ns,id);
CREATE TABLE interactive_messages (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, sequence INTEGER NOT NULL,
 actor_kind TEXT NOT NULL CHECK(actor_kind IN ('human','agent','system')),
 actor BLOB NOT NULL CHECK(length(actor)<=4096), text TEXT NOT NULL CHECK(length(CAST(text AS BLOB))<=65536),
 created_ns INTEGER NOT NULL, turn_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL DEFAULT '',
 UNIQUE(session_id,sequence)
);
CREATE TABLE interactive_turns (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, gaggle TEXT NOT NULL, message_id TEXT NOT NULL UNIQUE,
 acceptance_id TEXT NOT NULL UNIQUE, sequence INTEGER NOT NULL, authority BLOB NOT NULL CHECK(length(authority) BETWEEN 1 AND 16384),
 state TEXT NOT NULL CHECK(state IN ('queued','dispatching','running','settled')),
 outcome TEXT NOT NULL DEFAULT '', response_id TEXT NOT NULL DEFAULT '', result_digest TEXT NOT NULL DEFAULT '',
 created_ns INTEGER NOT NULL, settled_ns INTEGER, tombstoned_ns INTEGER,
 reserved_bytes INTEGER NOT NULL CHECK(reserved_bytes>=0)
);
CREATE INDEX interactive_turn_order ON interactive_turns(session_id,state,sequence);
CREATE INDEX interactive_turn_scope ON interactive_turns(gaggle,state,id);
CREATE TABLE interactive_requests (
 key_digest TEXT PRIMARY KEY, request_digest TEXT NOT NULL, gaggle TEXT NOT NULL,
 session_id TEXT NOT NULL, message_id TEXT NOT NULL DEFAULT '', acceptance_id TEXT NOT NULL DEFAULT '',
 created_ns INTEGER NOT NULL, tombstoned_ns INTEGER
);
CREATE INDEX interactive_request_owner ON interactive_requests(session_id);
`
