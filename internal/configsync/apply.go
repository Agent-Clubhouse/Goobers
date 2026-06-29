package configsync

import (
	"context"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/goobers/goobers/api/v1alpha1"
)

// Applier applies a desired RenderSet to a target. The default GitOps path
// renders manifests for ArgoCD (see RenderSet.WriteManifests); Applier is the
// optional direct-apply path, kept behind this interface so callers (and tests)
// choose the mechanism. Apply must be idempotent and must prune managed objects
// that are no longer desired.
type Applier interface {
	Apply(ctx context.Context, rs *RenderSet) error
}

// NoopApplier records intent without touching a cluster (default/testing).
type NoopApplier struct{ Log *slog.Logger }

// Apply logs the object count and returns nil.
func (n NoopApplier) Apply(_ context.Context, rs *RenderSet) error {
	if n.Log != nil {
		n.Log.Info("config-sync noop apply", "namespace", rs.Namespace, "objects", len(rs.Objects))
	}
	return nil
}

// NewScheme builds a runtime scheme with the four Goobers config CR kinds. The
// v1alpha1 package ships types only, so registration happens here.
func NewScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	s.AddKnownTypes(v1alpha1.GroupVersion,
		&v1alpha1.Manifest{}, &v1alpha1.ManifestList{},
		&v1alpha1.Gaggle{}, &v1alpha1.GaggleList{},
		&v1alpha1.Goober{}, &v1alpha1.GooberList{},
		&v1alpha1.Workflow{}, &v1alpha1.WorkflowList{},
	)
	metav1.AddToGroupVersion(s, v1alpha1.GroupVersion)
	return s, nil
}

// ClientApplier applies a RenderSet to a cluster via a controller-runtime client,
// then prunes managed CRs that are no longer in the desired set (GitOps removal).
type ClientApplier struct {
	Client client.Client
	Log    *slog.Logger
}

// Apply upserts every desired object then prunes stale managed objects.
func (a *ClientApplier) Apply(ctx context.Context, rs *RenderSet) error {
	// Capture desired identity keys up front: a client Create/Update clears an
	// object's in-memory TypeMeta, so objKey must be read before upserting.
	desired := make(map[string]bool, len(rs.Objects))
	for _, obj := range rs.Objects {
		desired[objKey(obj)] = true
	}
	for _, obj := range rs.Objects {
		if err := a.upsert(ctx, obj); err != nil {
			return fmt.Errorf("apply %s/%s: %w", obj.GetName(), obj.GetNamespace(), err)
		}
	}
	return a.prune(ctx, rs.Namespace, desired)
}

// upsert creates the object, or updates it in place if it already exists.
func (a *ClientApplier) upsert(ctx context.Context, obj client.Object) error {
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object %T is not a client.Object", obj)
	}
	err := a.Client.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		return a.Client.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return a.Client.Update(ctx, obj)
}

// prune deletes managed CRs in the namespace that are not in the desired set.
func (a *ClientApplier) prune(ctx context.Context, namespace string, desired map[string]bool) error {
	lists := []client.ObjectList{
		&v1alpha1.GaggleList{}, &v1alpha1.GooberList{}, &v1alpha1.WorkflowList{}, &v1alpha1.ManifestList{},
	}
	for _, list := range lists {
		if err := a.Client.List(ctx, list,
			client.InNamespace(namespace),
			client.MatchingLabels{ManagedByLabel: ManagedByValue},
		); err != nil {
			return fmt.Errorf("list for prune: %w", err)
		}
		objs, err := metaItems(list)
		if err != nil {
			return err
		}
		for _, obj := range objs {
			if desired[objKey(obj)] {
				continue
			}
			if err := a.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("prune %s/%s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
			}
			if a.Log != nil {
				a.Log.Info("config-sync pruned", "name", obj.GetName(), "namespace", obj.GetNamespace())
			}
		}
	}
	return nil
}

// objKey identifies an object by kind/namespace/name for desired-set membership.
func objKey(obj client.Object) string {
	return obj.GetObjectKind().GroupVersionKind().Kind + "/" + obj.GetNamespace() + "/" + obj.GetName()
}

// metaItems extracts the items of a typed CR list as client.Objects, tagging each
// with its Kind (list items don't carry TypeMeta) so objKey works during prune.
func metaItems(list client.ObjectList) ([]client.Object, error) {
	switch l := list.(type) {
	case *v1alpha1.GaggleList:
		return tagged(l.Items, "Gaggle", func(i int) client.Object { return &l.Items[i] }), nil
	case *v1alpha1.GooberList:
		return tagged(l.Items, "Goober", func(i int) client.Object { return &l.Items[i] }), nil
	case *v1alpha1.WorkflowList:
		return tagged(l.Items, "Workflow", func(i int) client.Object { return &l.Items[i] }), nil
	case *v1alpha1.ManifestList:
		return tagged(l.Items, "Manifest", func(i int) client.Object { return &l.Items[i] }), nil
	default:
		return nil, fmt.Errorf("unsupported list type %T", list)
	}
}

// tagged sets the Kind on each list item and returns them as client.Objects.
func tagged[T any](items []T, kind string, at func(int) client.Object) []client.Object {
	out := make([]client.Object, 0, len(items))
	for i := range items {
		obj := at(i)
		gvk := v1alpha1.GroupVersion.WithKind(kind)
		obj.GetObjectKind().SetGroupVersionKind(gvk)
		out = append(out, obj)
	}
	return out
}
