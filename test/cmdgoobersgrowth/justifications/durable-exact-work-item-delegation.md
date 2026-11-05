# Exact-work-item delegation command growth

The external-coordinator integration adds command-side plumbing for an exact
backlog claim and checked draft publication. `backlogexact.go` interprets the
runner's trigger reference and rejects selecting a different provider item.
The existing backlog policy and scan paths retain their normal trust, state,
dependency and claim checks. `open-pr` reads its optional draft input, verifies
the remote branch against the inspected commit, and records the checked full
head in its result.

These are CLI stage responsibilities: they connect stage inputs, provider
calls, journal context and result files. Their reusable API validation,
durable trigger acceptance and recovery, target-ID validation and scheduler
policy remain in the corresponding internal packages; they are not duplicated
in the command package. The small command helpers decompose these boundaries
to satisfy the existing complexity and body-length gates without exemptions
or threshold changes.

The additional command file isolates exact-target handling from the already
large backlog command. This is additive integration plumbing, not a new
command framework or a relocation of reusable runtime logic into `package main`.
