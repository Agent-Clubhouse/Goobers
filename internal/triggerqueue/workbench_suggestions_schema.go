package triggerqueue

const workbenchSuggestionSchema = `
CREATE TABLE workbench_suggestions (
 id TEXT PRIMARY KEY,key_digest TEXT NOT NULL UNIQUE,
 gaggle TEXT NOT NULL,issuer TEXT NOT NULL,subject TEXT NOT NULL,suggestion_key TEXT NOT NULL,
 input_digest TEXT NOT NULL,input BLOB NOT NULL CHECK(length(input)<=32768),
 state TEXT NOT NULL CHECK(state IN ('accepting','linked','rejected','tombstoned')),
 accepted_ns INTEGER NOT NULL,linked_ns INTEGER,completed_ns INTEGER,tombstoned_ns INTEGER,
 proposal_id TEXT NOT NULL DEFAULT '',origin_run TEXT NOT NULL,config_generation TEXT NOT NULL,
 reserved_bytes INTEGER NOT NULL DEFAULT 0 CHECK(reserved_bytes>=0)
);
CREATE INDEX workbench_suggestion_origin ON workbench_suggestions(origin_run,tombstoned_ns);
CREATE INDEX workbench_suggestion_generation ON workbench_suggestions(config_generation,tombstoned_ns);
CREATE INDEX workbench_suggestion_proposal ON workbench_suggestions(proposal_id,tombstoned_ns);
CREATE INDEX workbench_suggestion_retention ON workbench_suggestions(state,completed_ns,tombstoned_ns,id);
`
