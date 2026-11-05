package dispatcher

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
)

// PodCustody binds an ordinary stage attempt to one exact Kubernetes object.
type PodCustody struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	UID       string `json:"uid"`
	Attempt   int    `json:"attempt"`
	Custody   bool   `json:"custody"`
}

type podCustodyObserverKey struct{}
type workerHandoffDecisionKey struct{}

// WithPodCustodyObserver records the atomic create receipt for heartbeats.
func WithPodCustodyObserver(ctx context.Context, observe func(PodCustody)) context.Context {
	return context.WithValue(ctx, podCustodyObserverKey{}, observe)
}

func observeCreatedPod(ctx context.Context, custody PodCustody) {
	if observe, ok := ctx.Value(podCustodyObserverKey{}).(func(PodCustody)); ok {
		observe(custody)
	}
}

// WithWorkerHandoffDecision lets the engine distinguish worker drain from a
// server-requested activity cancellation without importing Temporal here.
func WithWorkerHandoffDecision(ctx context.Context, handoff func() bool) context.Context {
	return context.WithValue(ctx, workerHandoffDecisionKey{}, handoff)
}

func workerHandoffRequested(ctx context.Context) bool {
	handoff, _ := ctx.Value(workerHandoffDecisionKey{}).(func() bool)
	return handoff != nil && handoff()
}

// ErrWorkerHandoff leaves the exact live pod for another worker to adopt.
var ErrWorkerHandoff = errors.New("dispatcher: worker handoff")

// AdoptPod supervises an existing exact-UID attempt without creating work.
func (d *Dispatcher) AdoptPod(ctx context.Context, attempt Attempt, custody PodCustody) (Report, error) {
	pod, err := d.recordedStagePod(ctx, attempt, custody)
	report := Report{Pod: custody.Pod}
	if err != nil {
		return report, err
	}
	report.Image = stageContainerImage(pod)
	report.PodStartedAt = d.now().UTC()
	return report, d.finishAttemptPod(ctx, attempt, pod, &report)
}

func (d *Dispatcher) recordedStagePod(ctx context.Context, attempt Attempt, custody PodCustody) (*corev1.Pod, error) {
	namespace, err := d.cfg.namespaceFor(attempt.Gaggle)
	if err != nil || custody.Namespace != namespace || custody.Pod != PodName(attempt) || custody.UID == "" ||
		custody.Attempt != attempt.IdentityAttempt() || !custody.Custody {
		return nil, errors.New("dispatcher: invalid stage pod custody")
	}
	pod, err := d.pods.GetPod(ctx, custody.Namespace, custody.Pod)
	if err != nil {
		return nil, err
	}
	identity, valid := podAttempt(pod)
	physical, physicalValid := podPhysicalAttempt(pod)
	if !valid || !physicalValid || string(pod.UID) != custody.UID ||
		pod.Labels[LabelInstance] != d.cfg.InstanceID ||
		identity.RunID != attempt.RunID || identity.Stage != attempt.Stage ||
		identity.Attempt != attempt.Number || physical != attempt.PodAttempt ||
		identity.OwningWorkflowID != attempt.OwningWorkflowID {
		return nil, errors.New("dispatcher: stage pod custody no longer matches")
	}
	return pod, nil
}
