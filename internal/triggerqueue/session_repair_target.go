package triggerqueue

// Existing immutable inputs and absent-target message digests remain unchanged.
const sessionRepairTargetSchema = `
ALTER TABLE interactive_messages ADD COLUMN repair_target BLOB NOT NULL DEFAULT '' CHECK(length(repair_target)<=4096);
`
