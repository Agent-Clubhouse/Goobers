package operator

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/configsync"
)

// generationPointerReconciler fans a published generation out to the Gaggle
// controller. Returning API errors keeps the pointer on controller-runtime's
// rate-limited retry queue until discovery succeeds or the controller stops.
type generationPointerReconciler struct {
	client.Client
	events chan<- event.GenericEvent
}

func isGenerationPointer(obj client.Object) bool {
	return obj.GetName() == configsync.GenerationConfigMapName
}

func (r *generationPointerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cm corev1.ConfigMap
	if err := r.Get(ctx, req.NamespacedName, &cm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	generation := cm.Data[configsync.GenerationConfigMapKey]
	if generation == "" {
		return ctrl.Result{}, nil
	}
	var gaggles v1alpha1.GaggleList
	if err := r.List(ctx, &gaggles,
		client.InNamespace(cm.Namespace), configsync.GenerationSelector(generation),
	); err != nil {
		log.FromContext(ctx).Error(err, "listing gaggles for generation pointer failed; requeuing",
			"namespace", cm.Namespace, "generation", generation)
		return ctrl.Result{}, fmt.Errorf("list gaggles for generation %q: %w", generation, err)
	}
	for i := range gaggles.Items {
		select {
		case r.events <- event.GenericEvent{Object: &gaggles.Items[i]}:
		case <-ctx.Done():
			return ctrl.Result{}, ctx.Err()
		}
	}
	return ctrl.Result{}, nil
}
