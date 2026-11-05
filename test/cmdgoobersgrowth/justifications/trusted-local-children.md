# Trusted-local child composition

The daemon owns the retained parent configuration, instance credential grants,
shared journal handles, queue and recovery admission. The new command-layer
adapters bind those existing owners to a trusted-local parent and child executor;
they cannot be constructed by the standalone runner or selected as a fallback
from a distributed transport failure. Exact parent authentication selection also
belongs at this boundary, before generated runtime construction.

Reusable process execution, fresh attempt checkout, snapshot import and writer
proof live in internal/childpod, internal/invoke and internal/platform/proc.
Backend-neutral child admission and authoring remain in internal/childworkflow.
The command-layer additions compose these packages without a second lifecycle,
result contract or credential store. Startup uses the existing workflow preview
opt-in and does not add a CLI command or flag.
