package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

type childPodFake struct {
	fakePodAPI
	changeUID     bool
	missingStatus bool
	deletedUID    types.UID
	finalizedUID  types.UID
	holdUntilStop bool
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
	return nil // graceful stop retains the finalized object for proof
}
func (f *childPodFake) FinalizePodWithIdentity(ctx context.Context, ns, name string, uid types.UID) error {
	f.finalizedUID = uid
	return f.DeletePod(ctx, ns, name)
}
func (f *childPodFake) GetPod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	p, err := f.fakePodAPI.GetPod(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	if f.holdUntilStop && f.deletedUID == "" {
		p.Status.Phase = corev1.PodRunning
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
			cfg.TokenMinter = childMinter{}
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
			a.PodToken = ""
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

// The fake separates graceful stop from finalizer release, so termination
// cannot disappear before the dispatcher verifies it.
func TestChildCancellationWaitsForSurrenderBeforeFinalizing(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		t.Run(fmt.Sprintf("surrender-%t", confirmed), func(t *testing.T) {
			pods := &childPodFake{holdUntilStop: true}
			cfg := testConfig()
			cfg.TokenMinter = childMinter{}
			d, err := New(cfg, pods, nil, confirmGate{confirmed: confirmed}, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			clock := &fakeClock{}
			d.now = clock.Now
			d.sleep = func(ctx context.Context, duration time.Duration) error {
				if pods.deletedUID == "" {
					cancel()
					return ctx.Err()
				}
				return clock.Sleep(ctx, duration)
			}
			a := testAttempt()
			a.PodToken = ""
			a.ChildExecutionDigest = "sha256:" + strings.Repeat("a", 64)
			report, err := d.Dispatch(ctx, a, []RunnerSpec{linuxRunner()})
			if !report.WorkspaceWritersStopped || pods.deletedUID != "created-uid" {
				t.Fatal("graceful stop lost exact pod proof", report, err)
			}
			if confirmed {
				if err != nil || !report.SurrenderConfirmed || pods.finalizedUID != "created-uid" {
					t.Fatal(report, err)
				}
			} else if !errors.Is(err, ErrSurrenderUnconfirmed) || pods.finalizedUID != "" {
				t.Fatal("released custody without surrender", report, err)
			}
		})
	}
}

type childMinter struct{}

func TestChildOrphanRetainsUnconfirmedCustodyAfterOrdinaryHoldExpires(t *testing.T) {
	pods := &childPodFake{}
	cfg := testConfig()
	d, err := New(cfg, pods, nil, confirmGate{confirmed: false}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := testAttempt()
	a.PodAttempt = 1
	a.ChildExecutionDigest = "sha256:" + strings.Repeat("a", 64)
	pod, err := RenderPod(cfg, a, linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	pod, err = hardenChildPod(pod, a)
	if err != nil {
		t.Fatal(err)
	}
	pod.Name = "retained-child"
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * cfg.heldPodRetention()))
	if _, err := pods.CreatePodWithIdentity(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	key := pods.key(pod.Namespace, pod.Name)
	pods.observations[key] = 3
	pods.pods[key].Status.Phase = corev1.PodSucceeded
	deleted, err := d.SweepOrphans(t.Context(), stateTable{a.RunID: RunStateTerminal})
	if !errors.Is(err, ErrRecoveryUnconfirmed) || len(deleted) != 0 || len(pods.deleted) != 0 || pods.finalizedUID != "" {
		t.Fatalf("unconfirmed child custody expired: deleted=%v finalized=%s err=%v", deleted, pods.finalizedUID, err)
	}
}

func (childMinter) Mint(string, time.Duration) (string, error) { return "ordinary", nil }
func (childMinter) MintChildPod(string, string, time.Duration) (string, error) {
	return "generated", nil
}

func TestChildTokenCannotUseOrdinaryOrCallerBearer(t *testing.T) {
	for _, scenario := range []string{"signed", "ordinary-only", "caller"} {
		t.Run(scenario, func(t *testing.T) {
			a := testAttempt()
			a.ChildExecutionDigest = "sha256:" + strings.Repeat("a", 64)
			a.PodToken = ""
			d := &Dispatcher{cfg: Config{TokenMinter: childMinter{}}}
			if scenario == "ordinary-only" {
				d.cfg.TokenMinter = &mintOnce{}
			}
			if scenario == "caller" {
				a.PodToken = "caller"
			}
			err := d.mintAttemptToken(&a)
			if scenario == "signed" {
				if err != nil || a.PodToken != "generated" {
					t.Fatal(a.PodToken, err)
				}
			} else if !errors.Is(err, ErrChildIsolation) {
				t.Fatal(err)
			}
		})
	}
}

func TestOrphanDisposalRetainsPhysicalIdentityAndIsolation(t *testing.T) {
	a := testAttempt()
	a.ChildExecutionDigest = "sha256:" + strings.Repeat("a", 64)
	a.PodAttempt = 17
	p, err := RenderPod(testConfig(), a, linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	p, err = hardenChildPod(p, a)
	if err != nil {
		t.Fatal(err)
	}
	got := orphanDisposalAttempt(p, PodAttempt{RunID: a.RunID, Stage: a.Stage, Attempt: a.Number})
	if got.IdentityAttempt() != 17 || got.ChildExecutionDigest != a.ChildExecutionDigest {
		t.Fatal(got)
	}
	for _, physical := range []string{"invalid", "0", "-1", "", "missing"} {
		p.Labels[LabelPodAttempt] = physical
		if physical == "missing" {
			delete(p.Labels, LabelPodAttempt)
		}
		got = orphanDisposalAttempt(p, PodAttempt{RunID: a.RunID, Stage: a.Stage, Attempt: a.Number})
		if childContractDigest.MatchString(got.ChildExecutionDigest) {
			t.Fatalf("physical attempt %q fell back to logical ordinal", physical)
		}
	}
}
