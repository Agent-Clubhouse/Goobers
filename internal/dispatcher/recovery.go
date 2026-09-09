package dispatcher

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// ErrRecoveryUnconfirmed preserves a writable pod whose recovery handoff is
// absent or could not be verified. It is not permission to reuse the attempt.
var ErrRecoveryUnconfirmed = errors.New("dispatcher: writable pod recovery custody is unconfirmed")

type recoveryGate interface {
	RecoveryConfirmed(context.Context, Attempt) (bool, error)
}

// Both ordinary disposal and restart sweeping use the rendered pod's workspace
// contract. A retained pod is never reused for a new attempt.
func (d *Dispatcher) disposePod(ctx context.Context, pod *corev1.Pod, attempt Attempt) error {
	if podHasWritableWorkspace(pod) {
		gate, ok := d.gate.(recoveryGate)
		if !ok {
			return ErrRecoveryUnconfirmed
		}
		confirmed, err := gate.RecoveryConfirmed(ctx, attempt)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrRecoveryUnconfirmed, err)
		}
		if !confirmed {
			return ErrRecoveryUnconfirmed
		}
	}
	return d.pods.DeletePod(ctx, pod.Namespace, pod.Name)
}

func podHasWritableWorkspace(pod *corev1.Pod) bool {
	for _, container := range pod.Spec.Containers {
		if container.Name != StageContainerName {
			continue
		}
		for _, variable := range container.Env {
			if variable.Name == EnvStageWorkspace {
				// Only explicitly non-writable modes may bypass custody.
				return variable.ValueFrom != nil || (variable.Value != "scratch" && variable.Value != "repo-readonly")
			}
		}
		// Legacy pods without this field do not provision a repo checkout.
		return false
	}
	return false
}
