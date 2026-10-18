package triggerqueue

// Preserve every original pin, observation and transferred ordinal. A partial
// unique index keeps the historical one-coalesced-obligation-per-scope rule while
// independent occurrences have their own IDs and can coexist with old custody.
const scheduleOccurrenceDemandSchema = `
ALTER TABLE schedule_demands RENAME TO schedule_demands_legacy;
CREATE TABLE schedule_demands (
 id TEXT PRIMARY KEY NOT NULL, scope TEXT NOT NULL, payload BLOB NOT NULL,
 observed_count INTEGER NOT NULL DEFAULT -1, queued INTEGER NOT NULL DEFAULT 0,
 accepted_ns INTEGER NOT NULL,
 independent INTEGER NOT NULL DEFAULT 0 CHECK(independent IN (0,1)),
 fingerprint TEXT NOT NULL DEFAULT ''
);
INSERT INTO schedule_demands(id,scope,payload,observed_count,queued,accepted_ns)
 SELECT id,scope,payload,observed_count,queued,accepted_ns FROM schedule_demands_legacy;
DROP TABLE schedule_demands_legacy;
CREATE UNIQUE INDEX schedule_demands_coalesced ON schedule_demands(scope) WHERE independent=0;
CREATE INDEX schedule_demands_oldest ON schedule_demands(scope,accepted_ns,id);
`
