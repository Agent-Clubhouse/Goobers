// Package operator implements the Goobers Kubernetes operator (M9, DEP-012): it
// reconciles Gaggle CRDs into running desired state — a per-gaggle namespace
// (SEC-001) plus a worker Deployment per Goober bound to the gaggle — and
// registers the gaggle's workflows with the engine via a WorkflowRegistrar.
//
// Tier-3 (V2) — quarantined, not on the V0 path. See docs/ARCHITECTURE.md §11.
// Revived in V2.
package operator

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/goobers/goobers/api/v1alpha1"
)

const (
	// Label keys applied to operator-managed worker objects.
	managedByLabel = "app.kubernetes.io/managed-by"
	gaggleLabel    = "goobers.dev/gaggle"
	// gaggleNamespaceLabel records the namespace of the owning Gaggle CR (the
	// control/config namespace), which differs from the worker's own isolation
	// namespace. Used to map Deployment events back to the correct Gaggle.
	gaggleNamespaceLabel = "goobers.dev/gaggle-namespace"
	gooberLabel          = "goobers.dev/goober"
	roleLabel            = "goobers.dev/role"

	managedByValue = "goobers-operator"

	// readyConditionType summarizes whether the gaggle is fully reconciled.
	readyConditionType = "Ready"

	// defaultWorkerImage is a placeholder until the goober-runtime image (M8) is
	// published; the reconcile shape does not depend on the image contents.
	defaultWorkerImage = "ghcr.io/goobers/goober-runtime:latest"
)

// GaggleReconciler reconciles Gaggle objects.
type GaggleReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Registrar WorkflowRegistrar
	// WorkerImage overrides the worker container image (defaults to defaultWorkerImage).
	WorkerImage string
}

// +kubebuilder:rbac:groups=goobers.dev,resources=gaggles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=goobers.dev,resources=gaggles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=goobers.dev,resources=goobers,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create

// Reconcile drives a Gaggle's desired state and updates its status.
func (r *GaggleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var gaggle v1alpha1.Gaggle
	if err := r.Get(ctx, req.NamespacedName, &gaggle); err != nil {
		// Not found => deleted; nothing to do (cleanup is left to a future finalizer).
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.ensureNamespace(ctx, &gaggle); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure namespace: %w", err)
	}

	goobers, err := r.goobersFor(ctx, gaggle.Namespace, gaggle.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list goobers: %w", err)
	}

	desired := map[string]bool{}
	readyWorkers := 0
	for i := range goobers {
		dep := desiredWorkerDeployment(&gaggle, &goobers[i], r.workerImage())
		desired[dep.Name] = true
		ready, err := r.applyDeployment(ctx, dep)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("apply worker %s: %w", dep.Name, err)
		}
		if ready {
			readyWorkers++
		}
	}

	if err := r.pruneWorkers(ctx, &gaggle, desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("prune workers: %w", err)
	}

	// Workflow registration is best-effort: a registration failure must not wedge
	// the rest of the reconcile (the engine may not be up yet).
	if err := r.Registrar.EnsureRegistered(ctx, gaggle.Name, workflowNames(goobers)); err != nil {
		logger.Error(err, "workflow registration failed (continuing)")
	}

	newStatus := computeStatus(gaggle.Generation, len(goobers), readyWorkers, gaggle.Status)
	if !statusEqual(gaggle.Status, newStatus) {
		gaggle.Status = newStatus
		if err := r.Status().Update(ctx, &gaggle); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

// ensureNamespace creates the gaggle's isolation namespace if it does not exist.
func (r *GaggleReconciler) ensureNamespace(ctx context.Context, g *v1alpha1.Gaggle) error {
	ns := gaggleNamespaceObject(g)
	var existing corev1.Namespace
	err := r.Get(ctx, types.NamespacedName{Name: ns.Name}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return r.Create(ctx, ns)
}

// goobersFor returns the Goobers bound to the named gaggle. Goobers are matched
// only within the Gaggle CR's own namespace, so a same-named gaggle in another
// namespace cannot pull in foreign Goobers.
func (r *GaggleReconciler) goobersFor(ctx context.Context, namespace, gaggle string) ([]v1alpha1.Goober, error) {
	var list v1alpha1.GooberList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var mine []v1alpha1.Goober
	for i := range list.Items {
		if list.Items[i].Spec.Gaggle == gaggle {
			mine = append(mine, list.Items[i])
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Name < mine[j].Name })
	return mine, nil
}

// applyDeployment creates or updates a worker Deployment and reports readiness.
func (r *GaggleReconciler) applyDeployment(ctx context.Context, dep *appsv1.Deployment) (bool, error) {
	live := &appsv1.Deployment{}
	live.Name = dep.Name
	live.Namespace = dep.Namespace
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, live, func() error {
		if live.Labels == nil {
			live.Labels = map[string]string{}
		}
		for k, v := range dep.Labels {
			live.Labels[k] = v
		}
		live.Spec = dep.Spec
		return nil
	}); err != nil {
		return false, err
	}
	return deploymentReady(live), nil
}

// pruneWorkers deletes operator-managed worker Deployments in the gaggle
// namespace that are no longer desired (their Goober was removed).
func (r *GaggleReconciler) pruneWorkers(ctx context.Context, g *v1alpha1.Gaggle, desired map[string]bool) error {
	var list appsv1.DeploymentList
	if err := r.List(ctx, &list,
		client.InNamespace(g.Spec.Isolation.Namespace),
		client.MatchingLabels{managedByLabel: managedByValue, gaggleLabel: g.Name},
	); err != nil {
		return err
	}
	for i := range list.Items {
		if desired[list.Items[i].Name] {
			continue
		}
		if err := r.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *GaggleReconciler) workerImage() string {
	if r.WorkerImage != "" {
		return r.WorkerImage
	}
	return defaultWorkerImage
}

// SetupWithManager wires the reconciler to watch Gaggles, plus Goober and
// Deployment changes mapped back to their owning gaggle.
func (r *GaggleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Gaggle{}).
		Watches(&v1alpha1.Goober{}, handler.EnqueueRequestsFromMapFunc(gooberToGaggle)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(workerToGaggle)).
		Complete(r)
}

// gooberToGaggle maps a Goober change to a reconcile request for its gaggle.
// Gaggle and Goober CRs are synced from the same config-repo namespace, so the
// gaggle request shares the Goober's namespace.
func gooberToGaggle(_ context.Context, obj client.Object) []reconcile.Request {
	gb, ok := obj.(*v1alpha1.Goober)
	if !ok || gb.Spec.Gaggle == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: gb.GetNamespace(), Name: gb.Spec.Gaggle}}}
}

// workerToGaggle maps an operator-managed Deployment back to its gaggle via
// labels. The Gaggle CR lives in the control/config namespace recorded on the
// worker (gaggleNamespaceLabel), NOT the worker's own isolation namespace, so
// readiness events enqueue the actual Gaggle.
func workerToGaggle(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[managedByLabel] != managedByValue || labels[gaggleLabel] == "" || labels[gaggleNamespaceLabel] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: labels[gaggleNamespaceLabel], Name: labels[gaggleLabel]}}}
}

// ---------------------------------------------------------------------------
// Pure helpers (no client) — these carry the reconcile logic and are unit-tested.
// ---------------------------------------------------------------------------

// workerName is the deterministic Deployment name for a goober's worker pool.
func workerName(gooberName string) string { return "goober-" + gooberName }

// workerLabels are the labels applied to a goober's worker Deployment + pods.
// gaggleNamespace is the namespace of the owning Gaggle CR (not the worker's
// isolation namespace) so Deployment events can be mapped back to it.
func workerLabels(gaggleName, gaggleNamespace, gooberName, role string) map[string]string {
	return map[string]string{
		managedByLabel:                managedByValue,
		gaggleLabel:                   gaggleName,
		gaggleNamespaceLabel:          gaggleNamespace,
		gooberLabel:                   gooberName,
		roleLabel:                     role,
		"app.kubernetes.io/name":      "goober",
		"app.kubernetes.io/component": "worker",
		"app.kubernetes.io/part-of":   "goobers",
	}
}

// gaggleNamespaceObject builds the per-gaggle namespace (SEC-001).
func gaggleNamespaceObject(g *v1alpha1.Gaggle) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: g.Spec.Isolation.Namespace,
			Labels: map[string]string{
				managedByLabel:              managedByValue,
				gaggleLabel:                 g.Name,
				"app.kubernetes.io/part-of": "goobers",
			},
		},
	}
}

// desiredWorkerDeployment computes the worker Deployment for one goober. It is
// fully deterministic from the gaggle + goober specs.
func desiredWorkerDeployment(g *v1alpha1.Gaggle, gb *v1alpha1.Goober, image string) *appsv1.Deployment {
	replicas := gb.Spec.ScaleFactor
	if replicas < 1 {
		replicas = 1
	}
	labels := workerLabels(g.Name, g.Namespace, gb.Name, gb.Spec.Role)
	selector := map[string]string{gaggleLabel: g.Name, gooberLabel: gb.Name}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workerName(gb.Name),
			Namespace: g.Spec.Isolation.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "goober",
						Image: image,
						Env: []corev1.EnvVar{
							{Name: "GOOBER_GAGGLE", Value: g.Name},
							{Name: "GOOBER_NAME", Value: gb.Name},
							{Name: "GOOBER_ROLE", Value: gb.Spec.Role},
						},
					}},
				},
			},
		},
	}
}

// deploymentReady reports whether a Deployment has all desired replicas available.
func deploymentReady(dep *appsv1.Deployment) bool {
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	return dep.Status.AvailableReplicas >= want
}

// workflowNames returns the sorted, de-duplicated set of workflows referenced by
// the gaggle's goobers (the registration set).
func workflowNames(goobers []v1alpha1.Goober) []string {
	seen := map[string]bool{}
	for i := range goobers {
		for _, w := range goobers[i].Spec.Workflows {
			seen[w] = true
		}
	}
	out := make([]string, 0, len(seen))
	for w := range seen {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// computeStatus derives the new GaggleStatus from observed counts, preserving
// (and updating) the Ready condition.
func computeStatus(generation int64, gooberCount, readyWorkers int, prev v1alpha1.GaggleStatus) v1alpha1.GaggleStatus {
	allReady := readyWorkers >= gooberCount
	phase := v1alpha1.GagglePhaseReady
	condStatus := metav1.ConditionTrue
	reason := "Reconciled"
	msg := fmt.Sprintf("%d/%d worker deployments ready", readyWorkers, gooberCount)
	if !allReady {
		phase = v1alpha1.GagglePhaseDegraded
		condStatus = metav1.ConditionFalse
		reason = "WorkersNotReady"
	}

	next := v1alpha1.GaggleStatus{
		ObservedGeneration: generation,
		Phase:              phase,
		GooberCount:        int32(gooberCount),
		ReadyWorkers:       int32(readyWorkers),
		Conditions:         prev.Conditions,
	}
	apimeta.SetStatusCondition(&next.Conditions, metav1.Condition{
		Type:               readyConditionType,
		Status:             condStatus,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: generation,
	})
	return next
}

// statusEqual compares the meaningful status fields, ignoring condition
// timestamps so an unchanged reconcile does not churn the status subresource.
func statusEqual(a, b v1alpha1.GaggleStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration ||
		a.Phase != b.Phase ||
		a.GooberCount != b.GooberCount ||
		a.ReadyWorkers != b.ReadyWorkers {
		return false
	}
	ca := apimeta.FindStatusCondition(a.Conditions, readyConditionType)
	cb := apimeta.FindStatusCondition(b.Conditions, readyConditionType)
	if ca == nil || cb == nil {
		return ca == cb
	}
	return ca.Status == cb.Status && ca.Reason == cb.Reason && ca.ObservedGeneration == cb.ObservedGeneration
}
