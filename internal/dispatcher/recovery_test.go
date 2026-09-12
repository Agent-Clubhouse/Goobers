package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestWritablePodDisposalRequiresRecoveryAcknowledgment(t *testing.T) {
	for _, mode := range []string{"repo", "repo-pinned", "unknown", ""} {
		for _, acknowledged := range []bool{false, true} {
			t.Run(mode+"/"+map[bool]string{false: "missing", true: "acknowledged"}[acknowledged], func(t *testing.T) {
				plane := testPlane(t)
				attempt := testAttempt()
				data, err := json.Marshal(SurrenderedResult{
					Result:               apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: "failed", Message: "stage failed"}},
					RecoveryAcknowledged: acknowledged,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := plane.Put(t.Context(), attempt.RunID, attempt.Stage, attempt.Number, data); err != nil {
					t.Fatal(err)
				}
				pods := &fakePodAPI{}
				d, err := New(testConfig(), pods, nil, PlaneSurrenderGate{Plane: plane}, nil)
				if err != nil {
					t.Fatal(err)
				}
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: "web"},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: StageContainerName,
						Env: []corev1.EnvVar{{Name: EnvStageWorkspace, Value: mode}}}}}}
				err = d.disposePod(context.Background(), pod, attempt)
				if acknowledged {
					if err != nil || len(pods.deleted) != 1 {
						t.Fatalf("acknowledged disposal: %v %v", pods.deleted, err)
					}
				} else if !errors.Is(err, ErrRecoveryUnconfirmed) || len(pods.deleted) != 0 {
					t.Fatalf("unacknowledged source disposed: %v %v", pods.deleted, err)
				}
			})
		}
	}
}

func TestOrphanSweepPreservesWritablePodUntilDurableRecovery(t *testing.T) {
	cfg := testConfig()
	pods := &fakePodAPI{}
	attempt := testAttempt()
	attempt.Workspace = "repo"
	pod, err := RenderPod(cfg, attempt, linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	if err := pods.CreatePod(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	plane := testPlane(t)
	restarted, err := New(cfg, pods, nil, PlaneSurrenderGate{Plane: plane}, nil)
	if err != nil {
		t.Fatal(err)
	}
	states := stateTable{attempt.RunID: RunStateTerminal}
	deleted, err := restarted.SweepOrphans(t.Context(), states)
	if !errors.Is(err, ErrRecoveryUnconfirmed) || len(deleted) != 0 || len(pods.deleted) != 0 {
		t.Fatalf("restart removed unacknowledged source: %v %v", deleted, err)
	}
	data, err := json.Marshal(SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultFailure,
			Error: &apiv1.ErrorInfo{Code: "failed", Message: "implementation failed"}},
		RecoveryAcknowledged: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := plane.Put(t.Context(), attempt.RunID, attempt.Stage, attempt.Number, data); err != nil {
		t.Fatal(err)
	}
	// Another process can recover the acknowledgment from the durable plane;
	// neither its original in-memory gate nor the first sweep is required.
	reopened, err := NewSurrenderDir(plane.Root)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err = New(cfg, pods, nil, PlaneSurrenderGate{Plane: reopened}, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err = restarted.SweepOrphans(t.Context(), states)
	if err != nil || len(deleted) != 1 || deleted[0] != pod.Name {
		t.Fatalf("acknowledged orphan not removed: %v %v", deleted, err)
	}
}
