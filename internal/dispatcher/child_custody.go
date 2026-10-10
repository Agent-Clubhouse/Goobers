package dispatcher

import (
	"context"
	"errors"
	"strconv"

	"k8s.io/apimachinery/pkg/types"
)

// ChildPodCustody is the exact API object observed after creation. It grants
// only observation and conditional teardown; it cannot create a replacement.
type ChildPodCustody struct {
	Namespace string
	Name      string
	UID       string
}

type childCustodyObserverKey struct{}

// WithChildCustodyObserver supplies the activity's durable heartbeat recorder.
// No token, mutable report, or execution grant is handed to the recorder.
func WithChildCustodyObserver(ctx context.Context, observe func(ChildPodCustody)) context.Context {
	return context.WithValue(ctx, childCustodyObserverKey{}, observe)
}

func observeCreatedChild(ctx context.Context, custody ChildPodCustody) {
	if observe, ok := ctx.Value(childCustodyObserverKey{}).(func(ChildPodCustody)); ok {
		observe(custody)
	}
}

// ReconcileChildPod stops and observes one recorded pod after dispatcher loss.
// Missing objects, changed UIDs, or changed ownership cannot become proof.
func (d *Dispatcher) ReconcileChildPod(ctx context.Context, attempt Attempt, custody ChildPodCustody) (Report, error) {
	report := Report{ChildCreateAttempted: true, ChildPodUID: custody.UID, Pod: custody.Name}
	namespace, err := d.cfg.namespaceFor(attempt.Gaggle)
	if err != nil || !childContractDigest.MatchString(attempt.ChildExecutionDigest) || custody.UID == "" || custody.Namespace != namespace || custody.Name != PodName(attempt) || d.cfg.InstanceID == "" {
		return report, ErrChildIsolation
	}
	api, ok := d.pods.(childPodAPI)
	if !ok {
		return report, ErrChildIsolation
	}
	pod, err := d.pods.GetPod(ctx, custody.Namespace, custody.Name)
	if err != nil {
		return report, err
	}
	if pod == nil {
		return report, ErrChildIsolation
	}
	identity, valid := podAttempt(pod)
	if !valid || string(pod.UID) != custody.UID || pod.Labels[LabelInstance] != d.cfg.InstanceID || identity.RunID != attempt.RunID || identity.Stage != attempt.Stage || identity.Attempt != attempt.Number || pod.Labels[LabelPodAttempt] != strconv.Itoa(attempt.PodAttempt) || identity.OwningWorkflowID != attempt.OwningWorkflowID {
		return report, ErrChildIsolation
	}
	if err := validateChildPod(pod, attempt); err != nil {
		return report, err
	}
	report.Image = stageContainerImage(pod)
	stopCtx, cancel := context.WithTimeout(ctx, childStopTimeout)
	defer cancel()
	if err := api.DeletePodWithIdentity(stopCtx, custody.Namespace, custody.Name, types.UID(custody.UID)); err != nil {
		return report, err
	}
	phase, err := d.supervise(stopCtx, attempt, custody.Namespace, custody.Name, &report)
	report.Phase = phase
	if err != nil {
		return report, err
	}
	settleCtx, settleCancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultDisposalTimeout)
	defer settleCancel()
	confirmed, err := d.gate.Confirmed(settleCtx, attempt)
	report.SurrenderConfirmed = confirmed && err == nil
	if err != nil || !confirmed {
		return report, errors.Join(ErrSurrenderUnconfirmed, err)
	}
	cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultDisposalTimeout)
	defer cleanupCancel()
	if err := d.disposeChildPod(cleanup, pod, attempt); err != nil {
		report.DisposeErr = err
	} else {
		report.Disposed = true
		report.DisposeErr = d.awaitDisposedPod(cleanup, custody.Namespace, custody.Name)
	}
	// Recovery settles custody after a lost control path; the parent owns any
	// retry. The surrendered output still carries the actual command result.
	return report, errors.New("isolated child dispatch worker lost")
}
