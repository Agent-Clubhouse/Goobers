package dispatcher

import (
	"context"
	"errors"
	"slices"

	corev1 "k8s.io/api/core/v1"
)

const parentHandoffFinalizer = "goobers.dev/parent-handoff-custody"

// Child-capable parents need the same single-container, private-volume writer
// boundary as children. Their existing parent credentials and protocol remain
// unchanged; they are not generated-child executions.
func prepareParentHandoffPod(pod *corev1.Pod, attempt Attempt) (*corev1.Pod, error) {
	if attempt.ChildExecutionDigest != "" {
		return nil, ErrChildIsolation
	}
	if pod == nil {
		return nil, ErrChildIsolation
	}
	finalizers := slices.Clone(pod.Finalizers)
	pod, err := hardenChildPod(pod, attempt)
	if err != nil {
		return nil, err
	}
	pod.Finalizers = append(finalizers, parentHandoffFinalizer)
	pod.Spec.Containers[0].Env = slices.DeleteFunc(pod.Spec.Containers[0].Env, func(v corev1.EnvVar) bool { return v.Name == EnvChildExecutionDigest })
	return pod, nil
}

func parentWritersStopped(created, observed *corev1.Pod) bool {
	if created == nil || observed == nil || created.UID == "" || created.UID != observed.UID || created.Name != observed.Name || created.Namespace != observed.Namespace || !slices.Contains(observed.Finalizers, parentHandoffFinalizer) || !childNamespaceIsolated(observed.Spec) {
		return false
	}
	container := observed.Spec.Containers[0]
	if !childContainerIsolated(container) || !childVolumesIsolated(observed.Spec, container) || len(observed.Status.ContainerStatuses) != 1 {
		return false
	}
	status := observed.Status.ContainerStatuses[0]
	return status.Name == StageContainerName && status.ContainerID != "" && status.State.Terminated != nil
}

// Preserve the API object until the kubelet reports exact writer termination.
// A missing/replaced object, failed graceful stop, or timeout is not evidence.
func (d *Dispatcher) stopParentHandoffPod(ctx context.Context, attempt Attempt, pod *corev1.Pod, report *Report, phase corev1.PodPhase, cause error) (corev1.PodPhase, error) {
	if workerHandoffRequested(ctx) && ctx.Err() != nil {
		return phase, cause
	}
	api, ok := d.pods.(childPodAPI)
	if !ok || pod.UID == "" {
		return phase, errors.Join(cause, ErrChildIsolation)
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), childStopTimeout)
	defer cancel()
	if ctx.Err() != nil {
		if err := api.DeletePodWithIdentity(cleanup, pod.Namespace, pod.Name, pod.UID); err != nil {
			return phase, errors.Join(cause, err)
		}
		var err error
		phase, err = d.supervise(cleanup, attempt, pod.Namespace, pod.Name, report)
		if err != nil {
			return phase, errors.Join(cause, err)
		}
	}
	observed, err := d.pods.GetPod(cleanup, pod.Namespace, pod.Name)
	if err != nil {
		return phase, errors.Join(cause, err)
	}
	report.ParentWritersStopped = parentWritersStopped(pod, observed)
	if !report.ParentWritersStopped {
		return phase, errors.Join(cause, ErrChildIsolation)
	}
	return phase, cause
}

func (d *Dispatcher) disposeParentHandoffPod(ctx context.Context, pod *corev1.Pod, attempt Attempt) error {
	api, ok := d.pods.(childPodAPI)
	if !ok {
		return ErrChildIsolation
	}
	observed, err := d.pods.GetPod(ctx, pod.Namespace, pod.Name)
	if err != nil {
		return err
	}
	if !parentWritersStopped(pod, observed) {
		return ErrChildIsolation
	}
	if podHasWritableWorkspace(observed) {
		gate, ok := d.gate.(recoveryGate)
		if !ok {
			return ErrRecoveryUnconfirmed
		}
		confirmed, err := gate.RecoveryConfirmed(ctx, attempt)
		if err != nil || !confirmed {
			return errors.Join(ErrRecoveryUnconfirmed, err)
		}
	}
	if err := api.DeletePodWithIdentity(ctx, pod.Namespace, pod.Name, pod.UID); err != nil {
		return err
	}
	return api.FinalizePodWithIdentity(ctx, pod.Namespace, pod.Name, pod.UID)
}
