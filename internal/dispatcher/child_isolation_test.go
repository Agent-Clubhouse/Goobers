package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

type childPodFake struct {
	fakePodAPI
	changeUID     bool
	missingStatus bool
	deletedUID    types.UID
}

func (f *childPodFake) CreatePodWithIdentity(ctx context.Context, p *corev1.Pod) (*corev1.Pod, error) {
	p = p.DeepCopy()
	p.UID = "created-uid"
	if err := f.CreatePod(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}
func (f *childPodFake) DeletePodWithIdentity(ctx context.Context, ns, name string, uid types.UID) error {
	f.deletedUID = uid
	return f.DeletePod(ctx, ns, name)
}
func (f *childPodFake) GetPod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	p, err := f.fakePodAPI.GetPod(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	if f.changeUID {
		p.UID = "replacement-uid"
	}
	if p.Status.Phase == corev1.PodSucceeded && !f.missingStatus {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: StageContainerName, ContainerID: "containerd://exact", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
	}
	return p, nil
}

func TestChildDispatchRequiresExactUIDAndObservedAllWriters(t *testing.T) {
	for _, scenario := range []string{"joined", "replaced", "phase-only", "name-only-api"} {
		t.Run(scenario, func(t *testing.T) {
			pods := &childPodFake{changeUID: scenario == "replaced", missingStatus: scenario == "phase-only"}
			var api PodAPI = pods
			if scenario == "name-only-api" {
				api = &pods.fakePodAPI
			}
			cfg := testConfig()
			cfg.EnvPassthrough = []string{"FAKE_INHERITED_SECRET"}
			t.Setenv("FAKE_INHERITED_SECRET", "must not enter pod")
			d, err := New(cfg, api, nil, confirmGate{confirmed: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			clock := &fakeClock{}
			d.now = clock.Now
			d.sleep = clock.Sleep
			a := testAttempt()
			a.ChildExecutionDigest = "sha256:" + strings.Repeat("a", 64)
			report, err := d.Dispatch(t.Context(), a, []RunnerSpec{linuxRunner()})
			if scenario == "joined" {
				if err != nil || !report.WorkspaceWritersStopped || report.ChildPodUID != "created-uid" || pods.deletedUID != "created-uid" {
					t.Fatalf("report=%+v err=%v", report, err)
				}
				for _, v := range pods.createdSpecs[0].Spec.Containers[0].Env {
					if v.Name == "FAKE_INHERITED_SECRET" {
						t.Fatal("inherited credential passthrough")
					}
				}
			} else if report.WorkspaceWritersStopped || pods.deletedUID != "" {
				t.Fatalf("unproved writer was acknowledged/deleted: %+v", report)
			}
		})
	}
}

func TestChildPodRejectsContainmentChanges(t *testing.T) {
	a := testAttempt()
	a.ChildExecutionDigest = "sha256:" + strings.Repeat("a", 64)
	p, err := RenderPod(testConfig(), a, linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	p, err = hardenChildPod(p, a)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"hostpid":   func(p *corev1.Pod) { p.Spec.HostPID = true },
		"sharedpid": func(p *corev1.Pod) { p.Spec.ShareProcessNamespace = ptr.To(true) },
		"token":     func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = ptr.To(true) },
		"sidecar":   func(p *corev1.Pod) { p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "writer"}) },
		"hostvolume": func(p *corev1.Pod) {
			p.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}
		},
		"privileged": func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true) },
	} {
		t.Run(name, func(t *testing.T) {
			copy := p.DeepCopy()
			mutate(copy)
			if err := validateChildPod(copy, a); !errors.Is(err, ErrChildIsolation) {
				t.Fatal(err)
			}
		})
	}
}
