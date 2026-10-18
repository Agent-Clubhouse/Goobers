package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runnercap"
)

// priorAttemptDeadlineMargin covers the kubelet's enforcement of a held prior
// pod's activeDeadlineSeconds before the retry is admitted again.
const priorAttemptDeadlineMargin = time.Minute

// PriorAttemptLiveError refuses a retry while an earlier attempt's writable
// pod still runs without confirmed recovery custody (#6750). That pod is
// never deleted, so the retry must not start until RetryAt, by which the
// pod's activeDeadlineSeconds has stopped it.
type PriorAttemptLiveError struct {
	RunID   string
	Stage   string
	Pod     string
	RetryAt time.Time
}

// Error names the held pod and when the retry may be admitted.
func (e *PriorAttemptLiveError) Error() string {
	return fmt.Sprintf("dispatcher: run %s stage %s: prior attempt pod %s is still running without confirmed recovery custody; retry not before %s",
		e.RunID, e.Stage, e.Pod, e.RetryAt.UTC().Format(time.RFC3339))
}

// admitAttempt clears the way for one attempt's fresh pod: earlier pods of
// the same stage are retired, then the bounded capacity wait runs.
func (d *Dispatcher) admitAttempt(ctx context.Context, attempt Attempt, runner RunnerSpec) error {
	if err := d.fencePriorAttempts(ctx, attempt); err != nil {
		return err
	}
	return d.waitForCapacity(ctx, runner)
}

// fencePriorAttempts keeps a retry from running beside the attempt it
// replaces (#6750). A worker lost mid-dispatch cannot dispose its pod, and the
// orphan sweep leaves it running while the owning workflow is alive, so a
// retry would otherwise execute the stage twice at once. Each earlier pod of
// this run and stage whose stage container may still run is disposed through
// the recovery-custody gate and awaited until it stops. A writable pod
// without confirmed custody is never deleted; the retry is refused with a
// PriorAttemptLiveError instead, so the wait is not spent from the retry's
// own execution window.
func (d *Dispatcher) fencePriorAttempts(ctx context.Context, attempt Attempt) error {
	current := attempt.IdentityAttempt()
	if current <= 1 {
		return nil
	}
	namespace, err := d.cfg.namespaceFor(attempt.Gaggle)
	if err != nil {
		return err
	}
	selector := map[string]string{
		LabelManagedBy:      ManagedByValue,
		runnercap.LabelRole: runnercap.RoleStage,
		LabelRun:            sanitizeNameSegment(attempt.RunID, 63),
		LabelStage:          sanitizeNameSegment(attempt.Stage, 63),
	}
	if instance.ValidIdentity(d.cfg.InstanceID) {
		selector[LabelInstance] = d.cfg.InstanceID
	}
	for {
		pods, err := d.pods.ListPods(ctx, namespace, selector)
		if err != nil {
			return fmt.Errorf("dispatcher: list prior stage pods for run %s stage %s: %w", attempt.RunID, attempt.Stage, err)
		}
		terminating, err := d.retireLivePriorPods(ctx, pods, attempt, current)
		if err != nil || terminating == 0 {
			return err
		}
		if err := d.sleep(ctx, d.cfg.supervisionInterval()); err != nil {
			return fmt.Errorf("dispatcher: wait for %d prior stage pod(s) of run %s stage %s to stop: %w", terminating, attempt.RunID, attempt.Stage, err)
		}
	}
}

// retireLivePriorPods requests disposal of every earlier attempt's pod that
// may still run and reports how many are still terminating. A held pod
// refuses the retry.
func (d *Dispatcher) retireLivePriorPods(ctx context.Context, pods []corev1.Pod, attempt Attempt, current int) (int, error) {
	terminating := 0
	var held *PriorAttemptLiveError
	for i := range pods {
		pod := &pods[i]
		prior, ok := priorAttempt(pod, attempt, current)
		if !ok {
			continue
		}
		if _, stopped := stageStoppedAt(pod); stopped {
			continue
		}
		if pod.DeletionTimestamp != nil {
			terminating++
			continue
		}
		err := d.disposePod(ctx, pod, prior)
		if errors.Is(err, ErrRecoveryUnconfirmed) && !stageContainerStarted(pod) {
			// A stage that never started holds no work for custody to protect.
			err = d.pods.DeletePod(ctx, pod.Namespace, pod.Name)
		}
		switch {
		case err == nil:
			terminating++
		case errors.Is(err, ErrRecoveryUnconfirmed):
			if retryAt := d.priorPodDeadline(pod); held == nil || retryAt.After(held.RetryAt) {
				held = &PriorAttemptLiveError{RunID: attempt.RunID, Stage: attempt.Stage, Pod: pod.Name, RetryAt: retryAt}
			}
		default:
			return terminating, fmt.Errorf("dispatcher: retire prior stage pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	if held != nil {
		return terminating, held
	}
	return terminating, nil
}

// priorPodDeadline is when a held pod's activeDeadlineSeconds stops it.
func (d *Dispatcher) priorPodDeadline(pod *corev1.Pod) time.Time {
	if pod.Status.StartTime != nil && pod.Spec.ActiveDeadlineSeconds != nil {
		return pod.Status.StartTime.Add(time.Duration(*pod.Spec.ActiveDeadlineSeconds)*time.Second + priorAttemptDeadlineMargin)
	}
	return d.now().Add(priorAttemptDeadlineMargin)
}

func stageContainerStarted(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodPending {
		return true
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == StageContainerName && (status.State.Running != nil || status.State.Terminated != nil) {
			return true
		}
	}
	return false
}

// priorAttempt selects pods of exactly this run and stage (the labels are
// sanitized, the annotations verbatim) dispatched before the current physical
// attempt, returning the pod's own attempt identity.
func priorAttempt(pod *corev1.Pod, attempt Attempt, current int) (Attempt, bool) {
	if pod.Annotations[AnnotationRunID] != attempt.RunID || pod.Annotations[AnnotationStage] != attempt.Stage {
		return Attempt{}, false
	}
	prior := Attempt{RunID: attempt.RunID, Stage: attempt.Stage}
	var err error
	if prior.Number, err = strconv.Atoi(pod.Labels[LabelAttempt]); err != nil {
		return Attempt{}, false
	}
	var ok bool
	if prior.PodAttempt, ok = podPhysicalAttempt(pod); !ok {
		return Attempt{}, false
	}
	return prior, prior.IdentityAttempt() < current
}

// podPhysicalAttempt reads a pod's optional physical attempt label; zero
// means the pod predates it and is keyed by its journal attempt.
func podPhysicalAttempt(pod *corev1.Pod) (int, bool) {
	raw, stamped := pod.Labels[LabelPodAttempt]
	if !stamped {
		return 0, true
	}
	physical, err := strconv.Atoi(raw)
	return physical, err == nil
}
