# Defer remediation for a live or unverified origin (#6290)

The command package grows to apply the existing journal ownership API to command-private remediation demand, ordinary/pinned selection and direct claim admission. These adapters parse the branch convention already owned by the command, select the stage's file or daemon journal backend, and emit the supported no-work result with the owner/workflow and deferral reason.

Ownership verification remains in `internal/journalclient`; hard workspace ownership remains in `internal/worktree`. No separate ownership authority or generic journal state machine is added to the command package. The local helper connects those existing services to the CLI selection paths and fails closed when the origin evidence is unreadable or unknown.
