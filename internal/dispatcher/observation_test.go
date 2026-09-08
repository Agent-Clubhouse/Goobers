package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type podPlacementObservation struct {
	phase     corev1.PodPhase
	node      string
	selector  string
	os        *corev1.PodOS
	stageExit bool
}

type placementPodAPI struct {
	*fakePodAPI
	snapshots []podPlacementObservation
	read      int
}

func (p *placementPodAPI) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	pod, err := p.fakePodAPI.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	if p.read >= len(p.snapshots) {
		return nil, fmt.Errorf("unexpected poll after last observation")
	}
	observation := p.snapshots[p.read]
	p.read++
	pod.Status.Phase = observation.phase
	pod.Spec.NodeName = observation.node
	pod.Spec.NodeSelector = map[string]string{NodeSelectorOSKey: observation.selector}
	pod.Spec.OS = observation.os
	if observation.stageExit {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: pod.Spec.Containers[0].Name,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0,
			}},
		}}
	}
	return pod, nil
}

func TestDispatchRetainsObservedPodAssignmentWithoutRunningPoll(t *testing.T) {
	windows := &corev1.PodOS{Name: corev1.Windows}
	for _, tc := range []struct {
		name      string
		snapshots []podPlacementObservation
		node, os  string
		wantErr   error
	}{
		{
			name: "short Linux success",
			snapshots: []podPlacementObservation{
				{phase: corev1.PodPending},
				{phase: corev1.PodSucceeded, node: "linux-node-1", selector: "linux"},
			}, node: "linux-node-1", os: "linux",
		},
		{
			name: "short Windows failure uses API OS not runner claim",
			snapshots: []podPlacementObservation{
				{phase: corev1.PodPending},
				{phase: corev1.PodFailed, node: "windows-node-2", os: windows},
			}, node: "windows-node-2", os: "windows", wantErr: ErrStageFailed,
		},
		{
			name: "assigned pending is retained",
			snapshots: []podPlacementObservation{
				{phase: corev1.PodPending, node: "linux-node-3", selector: "linux"},
				{phase: corev1.PodSucceeded},
			}, node: "linux-node-3", os: "linux",
		},
		{
			name: "unassigned pod does not claim runner OS",
			snapshots: []podPlacementObservation{
				{phase: corev1.PodPending, selector: "linux"},
				{phase: corev1.PodFailed, selector: "linux"},
			}, wantErr: ErrStageFailed,
		},
		{
			name: "terminal stage with resident sidecar",
			snapshots: []podPlacementObservation{
				{phase: corev1.PodRunning, node: "linux-node-4", selector: "linux", stageExit: true},
			}, node: "linux-node-4", os: "linux",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pods := &placementPodAPI{fakePodAPI: &fakePodAPI{}, snapshots: tc.snapshots}
			relay := &recordingRelay{}
			d, err := New(testConfig(), pods, relay, confirmGate{confirmed: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			clock := &fakeClock{}
			d.now, d.sleep = clock.Now, clock.Sleep
			report, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{linuxRunner()})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("dispatch error=%v; want %v", err, tc.wantErr)
			}
			if report.Node != tc.node || report.OS != tc.os {
				t.Fatalf("placement=%s/%s; want %s/%s", report.Node, report.OS, tc.node, tc.os)
			}
			var observed []corev1.PodPhase
			for _, snapshot := range tc.snapshots {
				observed = append(observed, snapshot.phase)
			}
			if !slices.Equal(relay.phases, observed) {
				t.Fatalf("relay invented or dropped observed phases: %v; want %v", relay.phases, observed)
			}
			if !report.Disposed || !report.SurrenderConfirmed || len(pods.deleted) != 1 {
				t.Fatalf("placement capture changed settlement/disposal: %+v", report)
			}
			// Existing timestamps describe acceptance/create; no observed phase
			// may manufacture a later container start or alter those instants.
			if !report.PodStartedAt.Equal(report.QueuedAt) {
				t.Fatalf("observation changed creation timestamp: %+v", report)
			}
		})
	}
}

func TestObservedPodOSStaysUnknownWithoutConsistentSupportedFields(t *testing.T) {
	for _, tc := range []struct {
		name, selector, explicit string
	}{
		{"absent", "", ""},
		{"unknown selector", "plan9", ""},
		{"unknown explicit OS", "", "plan9"},
		{"conflicting fields", "linux", "windows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Spec: corev1.PodSpec{NodeName: "node-1", NodeSelector: map[string]string{NodeSelectorOSKey: tc.selector}}}
			if tc.explicit != "" {
				pod.Spec.OS = &corev1.PodOS{Name: corev1.OSName(tc.explicit)}
			}
			created := time.Date(2026, 9, 8, 1, 0, 0, 0, time.UTC)
			report := Report{QueuedAt: created, PodStartedAt: created}
			observePodPlacement(&report, pod)
			if report.Node != "node-1" || report.OS != "" || !report.PodStartedAt.Equal(created) {
				t.Fatalf("unproven OS or changed timestamp: %+v", report)
			}
		})
	}
}

func TestObservedPodConflictingOSDoesNotRetainEarlierClaim(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		NodeName: "node-1", NodeSelector: map[string]string{NodeSelectorOSKey: "linux"},
		OS: &corev1.PodOS{Name: corev1.Windows},
	}}
	report := Report{Node: "node-1", OS: "linux"}
	observePodPlacement(&report, pod)
	if report.Node != "node-1" || report.OS != "" {
		t.Fatalf("conflicting current OS must supersede earlier claim: %+v", report)
	}
}
