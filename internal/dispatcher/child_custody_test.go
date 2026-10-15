package dispatcher

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type childRecoveryPods struct{ childPodFake }

func (p *childRecoveryPods) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	pod, err := p.childPodFake.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
	}
	return pod, nil
}

func TestChildRecoveryRequiresOriginalIdentityAndNeverCreates(t *testing.T) {
	for _, scenario := range []string{"joined", "changed-uid", "changed-workflow", "changed-physical-attempt", "changed-instance", "wrong-namespace", "missing-uid", "missing-writer-proof", "missing-surrender"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := testConfig()
			a := testAttempt()
			a.ChildExecutionDigest = "sha256:" + strings.Repeat("a", 64)
			a.PodAttempt = 7
			a.OwningWorkflowID = "owning-execution"
			pod, err := RenderPod(cfg, a, linuxRunner())
			if err != nil {
				t.Fatal(err)
			}
			pod, err = hardenChildPod(pod, a)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "changed-workflow":
				pod.Annotations[AnnotationOwningWorkflowID] = "other"
			case "changed-physical-attempt":
				pod.Labels[LabelPodAttempt] = "99"
			case "changed-instance":
				pod.Labels[LabelInstance] = "other"
			}
			pods := &childRecoveryPods{childPodFake: childPodFake{changeUID: scenario == "changed-uid", missingStatus: scenario == "missing-writer-proof"}}
			created, err := pods.CreatePodWithIdentity(t.Context(), pod)
			if err != nil {
				t.Fatal(err)
			}
			custody := ChildPodCustody{Namespace: created.Namespace, Name: created.Name, UID: string(created.UID)}
			if scenario == "wrong-namespace" {
				custody.Namespace = "other"
			}
			if scenario == "missing-uid" {
				custody.UID = ""
			}
			d, err := New(cfg, pods, nil, confirmGate{confirmed: scenario != "missing-surrender"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			clock := &fakeClock{}
			d.now, d.sleep = clock.Now, clock.Sleep
			report, err := d.ReconcileChildPod(t.Context(), a, custody)
			if err == nil || len(pods.created) != 1 {
				t.Fatal("recovery claimed execution success or created replacement", report, err, len(pods.created))
			}
			if scenario == "joined" {
				if !report.WorkspaceWritersStopped || !report.SurrenderConfirmed || !report.Disposed || report.DisposeErr != nil || report.ChildPodUID != custody.UID || pods.finalizedUID != created.UID {
					t.Fatal("original custody not joined", report, err)
				}
			} else if report.Disposed || pods.finalizedUID != "" {
				t.Fatal("unproved custody disposed", report, err)
			}
			if scenario != "joined" && scenario != "missing-writer-proof" && scenario != "missing-surrender" && pods.deletedUID != "" {
				t.Fatal("foreign identity was stopped", report, err)
			}
		})
	}
}
