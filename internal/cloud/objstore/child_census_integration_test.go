//go:build cloudintegration

package objstore

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// installCensusChild supplies real 0012 custody for the census's blob pointers.
// Parent/occurrence/lineage constraints must remain enforced by the migrations.
func installCensusChild(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `
 INSERT INTO control.acceptance
  (instance_id,id,idem_key,actor,fingerprint,payload,state,owner_epoch,run_id,accepted_at)
 VALUES ('instance','census-acceptance','census-key','actor','\x00','\x00','accepted',1,'census-run',clock_timestamp());
 INSERT INTO control.child_parent (instance_id,gaggle,parent_run_id,owner_epoch)
 VALUES ('instance','g','census-parent',1);
 INSERT INTO control.child_occurrence
  (instance_id,gaggle,parent_run_id,occurrence,accepted_count,max_children,owner_epoch)
 VALUES ('instance','g','census-parent','visit',1,1,1);
 INSERT INTO control.child_lineage
  (instance_id,gaggle,parent_run_id,occurrence,invocation_key,sequence,child_id,
   acceptance_id,start_key,actor_digest,payload_digest,state,accepted_at,updated_at,owner_epoch)
 VALUES ('instance','g','census-parent','visit','call',1,'census-child',
  'census-acceptance','census-key','','','queued',clock_timestamp(),clock_timestamp(),1);
 `)
	if err != nil {
		t.Fatal(err)
	}
}
