# #7461: native verdict base invalidation and reconciliation

Growth: about +400 non-test lines and three new files in cmd/goobers.

The shared immutable base-delta rule lives in internal/prstatus. The command
package connects existing provider factories, review credentials, claims-plane
merge custody, and the post-merge reconciliation state session. verdictbase.go
owns the stage adapter; verdictcustody.go shares publication/withdrawal custody;
verdictbasesweep.go keeps the resumable page and fair retry queue in the existing
reconciliation ledger. Existing post-merge and reconciliation stages are the
registered callers. No new command or API resource is introduced.
