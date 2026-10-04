# Session blocker-resolution host adapter

The command package owns the existing scheduler directory, blocked-record decoder
and claims.lock lifecycle. This slice connects those host-owned resources to the
reusable workbench resolution service, holding the same claim scope through the
provider effect and durable receipt save. It does not introduce another scheduler
state store or put provider orchestration in the command package.

Reusable source inspection, evidence validation, current session authority and
command custody remain in internal/workbenchservice, workbenchprovider and
triggerqueue. The added command file is the adapter for existing scheduler state;
its tests exercise repository scoping, legacy ambiguity, exact record digests and
claim-lock ownership.
