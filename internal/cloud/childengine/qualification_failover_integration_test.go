//go:build cloudintegration

package childengine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestChildQualificationFailover(t *testing.T) {
	h := newQualificationHarness(t)
	barrier := h.InstallHTTPBarrier(func(request *http.Request) bool {
		return request.Method == http.MethodPost && request.URL.Path == apicontract.EngineChildAuthorityPath
	})
	run := h.StartParent()
	if err := barrier.Wait(h.ctx); err != nil {
		t.Fatal(err)
	}
	var boundID string
	var owner int64
	if err := h.pool.QueryRow(h.ctx, `SELECT grant_id,owner_epoch FROM control.child_authority
		WHERE instance_id=$1 AND parent_run_id=$2 AND NOT revoked`, testInstance, run.GetID()).Scan(&boundID, &owner); err != nil || owner != h.epoch.handle.Epoch {
		t.Fatalf("bind did not commit in predecessor epoch: %d (%v)", owner, err)
	}
	var children int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_lineage WHERE instance_id=$1 AND parent_run_id=$2`, testInstance, run.GetID()).Scan(&children); err != nil || children != 0 {
		t.Fatalf("submit overtook failover boundary: %d (%v)", children, err)
	}
	// Voluntary takeover occurs after the real bound row commits, with its
	// acknowledgement held outside routerMu, before the worker can submit.
	// No lease-expiry or Kubernetes claim is made.
	h.FailoverControl()
	if h.epoch.handle.Epoch <= owner {
		t.Fatal("control epoch did not advance")
	}
	// With one live parent and a one-slot limit, at most one extra admission
	// is permitted by R2. Hold any successful probes together so a second
	// over-admission cannot hide behind releasing the first probe's slot.
	var releases []func()
	for i := range 2 {
		release, ok, reason := h.epoch.scheduler.ReserveContinuation(fmt.Sprintf("qualification-capacity-%d", i), "example", "default-implement")
		if ok {
			releases = append(releases, release)
		} else if reason != localscheduler.ReasonMaxParallel {
			t.Fatalf("capacity refusal=%q, want %q", reason, localscheduler.ReasonMaxParallel)
		}
	}
	for _, release := range releases {
		release()
	}
	if len(releases) > 1 {
		t.Fatalf("failover over-admitted %d runs for one parent, bound is 1", len(releases))
	}
	t.Logf("failover over-admission: %d extra runs for one parent", len(releases))
	request := qualificationBoundRequest(h)
	grant := qualificationGrant(h, request)
	if grant.ID != boundID {
		t.Fatal("takeover replaced the predecessor's bound grant")
	}
	var first, duplicate apicontract.ChildWorkflowResponse
	raw := qualificationProbe(h, request, apicontract.ChildWorkflowStartPath, "qualification-child", http.StatusAccepted, "")
	if err := json.Unmarshal(raw, &first); err != nil || first.RunID == "" || first.Duplicate {
		t.Fatalf("successor submit: %+v (%v)", first, err)
	}
	raw = qualificationProbe(h, request, apicontract.ChildWorkflowStartPath, "qualification-child", http.StatusAccepted, "")
	if err := json.Unmarshal(raw, &duplicate); err != nil || !duplicate.Duplicate || duplicate.RunID != first.RunID || duplicate.AcceptanceID != first.AcceptanceID {
		t.Fatalf("successor replay lost custody: %+v (%v)", duplicate, err)
	}
	qualificationProbe(h, request, apicontract.ChildWorkflowStartPath, "second-child", http.StatusConflict, "child_workflow_unresolved")
	var adopted bool
	if err := h.pool.QueryRow(h.ctx, `SELECT a.owner_epoch=$4 AND p.owner_epoch=$4 AND a.grant_id=$3
		FROM control.child_authority a JOIN control.child_parent p USING (instance_id,gaggle,parent_run_id)
		WHERE a.instance_id=$1 AND a.parent_run_id=$2`, testInstance, run.GetID(), grant.ID, h.epoch.handle.Epoch).Scan(&adopted); err != nil || !adopted {
		t.Fatalf("successor lost bound custody: adopted=%t (%v)", adopted, err)
	}
	barrier.Release()
	qualificationCompleted(h, run)
	var current int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_lineage
		WHERE instance_id=$1 AND parent_run_id=$2 AND owner_epoch=$3 AND acceptance_id=$4`, testInstance, run.GetID(), h.epoch.handle.Epoch, first.AcceptanceID).Scan(&current); err != nil || current != 1 {
		t.Fatalf("completed child custody in successor: %d (%v)", current, err)
	}
}
