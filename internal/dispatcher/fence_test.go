package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// seedPriorPod places attempt 1's pod in the fake cluster with status, as a
// worker lost mid-dispatch leaves it.
func seedPriorPod(t *testing.T, pods *fakePodAPI, workspace string, status corev1.PodStatus) *corev1.Pod {
	t.Helper()
	prior := testAttempt()
	prior.Workspace = workspace
	pod, err := RenderPod(testConfig(), prior, linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	if err := pods.CreatePod(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	pods.pods[pods.key(pod.Namespace, pod.Name)].Status = status
	return pod
}

func runningStageStatus() corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: StageContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}}
}

func retryAttempt(workspace string) Attempt {
	retry := testAttempt()
	retry.Number = 2
	retry.Workspace = workspace
	return retry
}

// #6750: a retry after worker loss must not run beside the attempt it
// replaces. A still-running prior pod without writable work is disposed
// before the retry's pod exists; a prior pod that already stopped is left to
// the orphan sweep.
func TestDispatchRetryRetiresLivePriorAttemptPod(t *testing.T) {
	for _, tc := range []struct {
		name        string
		workspace   string
		status      corev1.PodStatus
		wantRetired bool
	}{
		{name: "running scratch pod", workspace: "scratch", status: runningStageStatus(), wantRetired: true},
		{name: "unstarted writable pod", workspace: "repo", status: corev1.PodStatus{Phase: corev1.PodPending}, wantRetired: true},
		{name: "stopped pod", workspace: "scratch", status: stageTerminatedStatus(time.Unix(0, 0))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pods := &fakePodAPI{}
			prior := seedPriorPod(t, pods, tc.workspace, tc.status)
			d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			clock := &fakeClock{}
			d.now = clock.Now
			d.sleep = clock.Sleep
			if _, err := d.Dispatch(t.Context(), retryAttempt("scratch"), []RunnerSpec{linuxRunner()}); err != nil {
				t.Fatalf("Dispatch retry: %v", err)
			}
			if len(pods.created) != 2 {
				t.Fatalf("created %v: the retry must still get its own fresh pod", pods.created)
			}
			retired := len(pods.deleted) > 0 && pods.deleted[0] == prior.Name
			if retired != tc.wantRetired {
				t.Fatalf("deleted %v: prior pod %s retired=%v, want %v", pods.deleted, prior.Name, retired, tc.wantRetired)
			}
		})
	}
}

// A running writable prior pod without confirmed recovery custody is never
// deleted, and the retry does not spend its own execution window waiting:
// it is refused until the held pod's activeDeadlineSeconds has stopped it.
func TestDispatchRetryDefersWhileUnconfirmedWritablePriorPodRuns(t *testing.T) {
	pods := &fakePodAPI{}
	status := runningStageStatus()
	started := metav1.NewTime(time.Unix(1000, 0))
	status.StartTime = &started
	prior := seedPriorPod(t, pods, "repo", status)
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{}
	d.now = clock.Now
	d.sleep = clock.Sleep
	_, err = d.Dispatch(t.Context(), retryAttempt("scratch"), []RunnerSpec{linuxRunner()})
	var live *PriorAttemptLiveError
	if !errors.As(err, &live) {
		t.Fatalf("Dispatch retry = %v, want PriorAttemptLiveError", err)
	}
	want := started.Add(time.Duration(*prior.Spec.ActiveDeadlineSeconds)*time.Second + priorAttemptDeadlineMargin)
	if live.Pod != prior.Name || !live.RetryAt.Equal(want) {
		t.Fatalf("held pod %s retry at %v, want %s at %v", live.Pod, live.RetryAt, prior.Name, want)
	}
	if len(pods.created) != 1 || len(pods.deleted) != 0 {
		t.Fatalf("created %v deleted %v: no retry pod beside the held prior, and the prior survives", pods.created, pods.deleted)
	}
}

// Recovery custody is read under the pod's physical attempt. On a graph
// repass the journal ordinal repeats, so an acknowledgment surrendered by an
// earlier visit's pod must not release a later visit's running workspace.
func TestRetryFenceReadsCustodyUnderPhysicalAttempt(t *testing.T) {
	pods := &fakePodAPI{}
	visit := testAttempt()
	visit.Workspace = "repo"
	visit.PodAttempt = 3
	pod, err := RenderPod(testConfig(), visit, linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	if err := pods.CreatePod(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	pods.pods[pods.key(pod.Namespace, pod.Name)].Status = runningStageStatus()
	plane := testPlane(t)
	acknowledge := func(attempt int) {
		data, err := json.Marshal(SurrenderedResult{RecoveryAcknowledged: true, Result: apiv1.ResultEnvelope{Status: apiv1.ResultFailure,
			Error: &apiv1.ErrorInfo{Code: "failed", Message: "stage failed"}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := plane.Put(t.Context(), visit.RunID, visit.Stage, attempt, data); err != nil {
			t.Fatal(err)
		}
	}
	d, err := New(testConfig(), pods, nil, PlaneSurrenderGate{Plane: plane}, nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{}
	d.now = clock.Now
	d.sleep = clock.Sleep
	retry := visit
	retry.PodAttempt = 4

	acknowledge(visit.Number)
	var live *PriorAttemptLiveError
	if err := d.fencePriorAttempts(t.Context(), retry); !errors.As(err, &live) || len(pods.deleted) != 0 {
		t.Fatalf("fence = %v, deleted %v: another visit's acknowledgment released the running pod", err, pods.deleted)
	}
	acknowledge(visit.PodAttempt)
	if err := d.fencePriorAttempts(t.Context(), retry); err != nil || len(pods.deleted) != 1 || pods.deleted[0] != pod.Name {
		t.Fatalf("fence = %v, deleted %v: the pod's own acknowledgment must release it", err, pods.deleted)
	}
}

// Cancellation while a prior pod terminates returns rather than creating a
// concurrent pod.
func TestRetryFenceHonoursCancellation(t *testing.T) {
	pods := &fakePodAPI{}
	prior := seedPriorPod(t, pods, "scratch", runningStageStatus())
	deleting := metav1.Now()
	pods.pods[pods.key(prior.Namespace, prior.Name)].DeletionTimestamp = &deleting
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d.sleep = sleepCtx
	if err := d.fencePriorAttempts(ctx, retryAttempt("scratch")); !errors.Is(err, context.Canceled) {
		t.Fatalf("fence = %v, want cancellation while the prior pod terminates", err)
	}
	if len(pods.created) != 1 {
		t.Fatalf("created %v: no retry pod may exist beside the terminating prior", pods.created)
	}
}
