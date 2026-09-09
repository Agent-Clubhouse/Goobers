package dispatcher

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type unconfirmedDisposalPods struct {
	*cancellationPodAPI
	observeErr error
}

func (p *unconfirmedDisposalPods) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	if len(p.deleted) == 0 {
		return p.cancellationPodAPI.GetPod(ctx, namespace, name)
	}
	return &corev1.Pod{}, p.observeErr
}

func TestDispatchCancellationReportsUnconfirmedDisposal(t *testing.T) {
	for _, observationFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "observation failure"}[observationFails], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pods := &unconfirmedDisposalPods{cancellationPodAPI: &cancellationPodAPI{fakePodAPI: &fakePodAPI{}, cancel: cancel}}
			want := context.DeadlineExceeded
			if observationFails {
				want = errors.New("Kubernetes unavailable")
				pods.observeErr = want
			}
			d, err := New(testConfig(), pods, nil, confirmGate{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			d.sleep = func(cleanup context.Context, _ time.Duration) error {
				deadline, bounded := cleanup.Deadline()
				if cleanup.Err() != nil || !bounded || time.Until(deadline) > DefaultDisposalTimeout {
					t.Fatal("cleanup lost its independent bound")
				}
				return context.DeadlineExceeded
			}
			report, err := d.Dispatch(ctx, testAttempt(), []RunnerSpec{linuxRunner()})
			if !errors.Is(err, context.Canceled) || !report.Disposed || !errors.Is(report.DisposeErr, want) || report.SurrenderConfirmed {
				t.Fatalf("cancellation/DELETE acceptance/uncertainty = %+v, %v", report, err)
			}
		})
	}
}

func TestObserveDisposalHonorsDeadline(t *testing.T) {
	pods := &unconfirmedDisposalPods{cancellationPodAPI: &cancellationPodAPI{fakePodAPI: &fakePodAPI{deleted: []string{"pod"}}}}
	d, err := New(testConfig(), pods, nil, confirmGate{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := d.awaitDisposedPod(ctx, "test", "pod"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unobserved pod absence = %v", err)
	}
}
