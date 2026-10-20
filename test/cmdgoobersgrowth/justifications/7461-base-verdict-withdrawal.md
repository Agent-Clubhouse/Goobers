# #7461: native verdict base invalidation and reconciliation

Growth: about +400 non-test lines and three new files in cmd/goobers.

The shared immutable base-delta rule lives in internal/prstatus. The command
package connects existing provider factories, review credentials, claims-plane
merge custody, and the post-merge reconciliation state session. verdictbase.go
owns the stage adapter; verdictcustody.go shares publication/withdrawal custody;
verdictbasesweep.go keeps the resumable page and fair retry queue in the existing
reconciliation ledger. Existing post-merge and reconciliation stages are the
registered callers. No new command or API resource is introduced.

Native publication and withdrawal call the existing publication-custody helper
from #7464 directly. This PR adds no claim namespace or custody implementation;
the inherited file/plane, timeout, lifecycle and operator-override rules remain
unchanged. Base-withdrawal tests cover ordering against a fresh native pass and
rechecking publication pins, while the existing custody suite covers the helper.
