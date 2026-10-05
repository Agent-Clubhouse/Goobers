package triggerqueue

const workbenchProposalSchema = `
CREATE TABLE workbench_proposals (
 id TEXT PRIMARY KEY,key_digest TEXT NOT NULL UNIQUE,
 gaggle TEXT NOT NULL,source_binding TEXT NOT NULL,issuer TEXT NOT NULL,subject TEXT NOT NULL,request_id TEXT NOT NULL,
 request_digest TEXT NOT NULL,target_digest TEXT NOT NULL,operation_digest TEXT NOT NULL,
 request BLOB NOT NULL CHECK(length(request)<=2097152),
 state TEXT NOT NULL CHECK(state IN ('accepted','prepared','attempting','unknown','blocked','confirmed','observed','not-applied','tombstoned')),
 accepted_ns INTEGER NOT NULL,completed_ns INTEGER,tombstoned_ns INTEGER,
 plan BLOB NOT NULL DEFAULT '' CHECK(length(plan)<=16384),plan_digest TEXT NOT NULL DEFAULT '',
 before_source BLOB NOT NULL DEFAULT '' CHECK(length(before_source)<=1048576),
 after_source BLOB NOT NULL DEFAULT '' CHECK(length(after_source)<=1048576),
 history BLOB NOT NULL DEFAULT '' CHECK(length(history)<=65536),history_digest TEXT NOT NULL DEFAULT '',
 reserved_bytes INTEGER NOT NULL CHECK(reserved_bytes>=0)
);
CREATE INDEX workbench_proposal_scope ON workbench_proposals(gaggle,source_binding,issuer,subject,id);
CREATE INDEX workbench_proposal_retention ON workbench_proposals(state,completed_ns,tombstoned_ns,id);
`
