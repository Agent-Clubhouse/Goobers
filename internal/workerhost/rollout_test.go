package workerhost

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/workerrollout"
)

type rolloutStatus struct {
	status workerrollout.DeploymentStatus
}

func (s rolloutStatus) Status(context.Context) (workerrollout.DeploymentStatus, error) {
	return s.status, nil
}

func TestDrainingAttestedBuildDoesNotReconcileOntoNewCurrent(t *testing.T) {
	h := newTestHost(t, Config{
		TaskQueues:           []string{"goobers-engine"},
		BuildVersion:         "v1",
		Versioning:           true,
		VersionRetention:     workerrollout.DefaultVersionLimit,
		RolloutNamespace:     "goobers-system",
		AutoUpgradeRetention: true,
	}, &fakeFleet{})
	calls := 0
	h.reconcileRollout = func(context.Context, workerrollout.StatusSource, workerrollout.TemporalClient, int, string) (workerrollout.Result, error) {
		calls++
		return workerrollout.Result{}, nil
	}
	result, err := h.reconcileRolloutOnce(t.Context(), rolloutStatus{
		status: workerrollout.DeploymentStatus{CurrentBuildID: "v2"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != (workerrollout.Result{}) || calls != 0 {
		t.Fatalf("result = %+v, reconciler calls = %d; draining v1 must not act for current v2", result, calls)
	}
}
