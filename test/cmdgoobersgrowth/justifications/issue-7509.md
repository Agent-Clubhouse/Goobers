# Issue #7509: finish in-flight recovery publication after terminal release

This change adds 42 net non-test lines and no production files to
`cmd/goobers`. The existing recovery authorization adapter now distinguishes
publication from download and permits a normally terminated local claim to
finish publication only within its original lease window, while the claim lock
excludes a successor through durable acknowledgement. Released shared claims,
administrative revocations, and unknown release reasons remain denied.

This policy belongs beside the existing command-owned recovery service because
it combines authenticated run identity, the instance journal, the local claim
lock, and acknowledgement into that instance's recovery inventory. The generic
claim ledger and recovery archive packages do not own that composition. The
heartbeat transport diagnostics and upload response handling remain in their
reusable internal packages.
