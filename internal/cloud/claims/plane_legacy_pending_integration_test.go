//go:build cloudintegration

package claims

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/cloud/pgroles"
)

func TestExpiredLegacyPendingCustodyBlocksNamespacedAdmissionAsController(t *testing.T) {
	for _, mode := range []string{"local", "shared"} {
		for _, custody := range []string{"expired", "release-pending", "admission-pending"} {
			t.Run(mode+"/"+custody, func(t *testing.T) {
				f := newPlaneFixture(t)
				r := f.leader(t, "a")
				p := r.plane
				remote := &sharedRemote{}
				if mode == "shared" {
					p = sharedPlane(t, f, r, remote)
				}
				ctx := t.Context()
				var role string
				if err := p.pool.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil || role != pgroles.Controller {
					t.Fatalf("runtime role=%s, %v", role, err)
				}
				err := p.fenced(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
					_, err := tx.Exec(ctx, `INSERT INTO control.claim
						(instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
						claimed_at,renewed_at,expires_at,release_pending_at,shared_owner,shared_deadline,shared_admission_pending)
						VALUES($1,$2,$2,'42','legacy-run','wf','token',$3,
						now()-interval '2 hours',now()-interval '2 hours',now()-interval '1 hour',
						CASE WHEN $4 THEN now() END,
						CASE WHEN $5 THEN '{"instance":"legacy","run":"legacy-run","token":"incarnation"}'::jsonb ELSE '{}'::jsonb END,
						CASE WHEN $5 THEN now()-interval '1 hour' END,$5)`,
						p.instanceID, legacyNamespace, epoch, custody == "release-pending", custody == "admission-pending")
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				got, err := p.Acquire(ctx, request("42", "new-run"))
				want := custody == "expired"
				if err != nil || got.Ok != want || (!want && got.Holder != "legacy-run") {
					t.Fatalf("admission=%+v, %v; want admitted=%v", got, err, want)
				}
				if !want && remote.revision != 0 {
					t.Fatal("pending legacy custody allowed a remote CAS")
				}
			})
		}
	}
}
