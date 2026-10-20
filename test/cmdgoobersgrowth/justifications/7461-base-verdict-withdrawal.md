# #7461: native verdict base invalidation and reconciliation

Growth: about +400 non-test lines and three new files in cmd/goobers.

The shared immutable base-delta rule lives in internal/prstatus. The command
package connects existing provider factories, review credentials, claims-plane
merge custody, and the post-merge reconciliation state session. verdictbase.go
owns the stage adapter; verdictcustody.go shares publication/withdrawal custody;
verdictbasesweep.go keeps the resumable page and fair retry queue in the existing
reconciliation ledger. Existing post-merge and reconciliation stages are the
registered callers. No new command or API resource is introduced.

The publication-custody follow-up adds the shared durable-claim helper
`verdictfence.go` (approximately 105 lines) instead of using different local
flock/remote lease domains. It is identical to the publication portion of
#7464's helper, so either PR can land independently and the shared helper can
be retained when reconciling the second branch.
