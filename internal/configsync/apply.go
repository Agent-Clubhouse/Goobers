package configsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/v1alpha1"
)

// Generation barrier identifiers. Every applied object carries the generation it
// belongs to, and a single ConfigMap holds the one generation consumers may
// select. The pointer only advances once the whole generation is committed and
// validated, so a partial apply never becomes visible.
const (
	GenerationLabel = "goobers.dev/config-generation"
	// GenerationConfigMapName is the authoritative reference switched atomically
	// after a complete generation is applied.
	GenerationConfigMapName = "goobers-config-generation"
	// GenerationConfigMapKey holds the authoritative generation value.
	GenerationConfigMapKey = "generation"
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

// Apply publishes rs as a single immutable generation: every desired object is
// stamped with the generation and upserted, the generation is validated as
// complete, the authoritative pointer is switched atomically, and only then are
// stale objects pruned. Any failure before the switch leaves the previous
// generation authoritative and returns an *ApplyError naming every mutation that
// committed or whose outcome is ambiguous.
func (a *ClientApplier) Apply(ctx context.Context, rs *RenderSet) error {
	objects, generation, err := generationObjects(rs)
	if err != nil {
		return err
	}
	// Capture desired identity keys up front: a client Create/Update clears an
	// object's in-memory TypeMeta, so objKey must be read before upserting.
	keys := make([]string, len(objects))
	desired := make(map[string]bool, len(objects))
	for i, obj := range objects {
		keys[i] = objKey(obj)
		desired[keys[i]] = true
	}

	ledger := &mutationLedger{}
	for i, obj := range objects {
		op, err := a.upsert(ctx, obj)
		if op != "" {
			ledger.record(keys[i], op, err)
		}
		if err != nil {
			return ledger.fail(generation, "apply", fmt.Errorf("apply %s: %w", keys[i], err))
		}
	}
	if err := a.validateGeneration(ctx, objects, keys, generation); err != nil {
		return ledger.fail(generation, "validate", err)
	}
	if err := a.switchGeneration(ctx, rs.Namespace, generation, ledger); err != nil {
		return ledger.fail(generation, "switch", err)
	}
	if err := a.prune(ctx, rs.Namespace, desired, ledger); err != nil {
		return ledger.fail(generation, "prune", err)
	}
	if a.Log != nil {
		a.Log.Info("config-sync generation published",
			"namespace", rs.Namespace, "generation", generation, "objects", len(objects))
	}
	return nil
}

// generationObjects deep-copies the desired set and stamps every object with the
// generation derived from the set's content, so an unchanged config re-applies
// to the same generation and the caller's objects are left untouched.
func generationObjects(rs *RenderSet) ([]client.Object, string, error) {
	objects := make([]client.Object, 0, len(rs.Objects))
	for _, obj := range rs.Objects {
		copied, ok := obj.DeepCopyObject().(client.Object)
		if !ok {
			return nil, "", fmt.Errorf("object %T is not a client.Object", obj)
		}
		objects = append(objects, copied)
	}
	generation, err := generationID(rs.Namespace, objects)
	if err != nil {
		return nil, "", err
	}
	for _, obj := range objects {
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[GenerationLabel] = generation
		obj.SetLabels(labels)
	}
	return objects, generation, nil
}

// generationID hashes the desired set so the generation is a content identity:
// distinct config yields a distinct generation, identical config re-applies to
// the same one.
func generationID(namespace string, objects []client.Object) (string, error) {
	sum := sha256.New()
	fmt.Fprintf(sum, "namespace=%s\n", namespace)
	for _, obj := range objects {
		data, err := yaml.Marshal(obj)
		if err != nil {
			return "", fmt.Errorf("hash %s: %w", objKey(obj), err)
		}
		fmt.Fprintf(sum, "object=%s\n%s\n", objKey(obj), data)
	}
	return "g" + hex.EncodeToString(sum.Sum(nil))[:32], nil
}

// upsert creates the object, or updates it in place if it already exists,
// reporting the mutation it attempted (empty when it never got as far as one).
func (a *ClientApplier) upsert(ctx context.Context, obj client.Object) (string, error) {
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return "", fmt.Errorf("object %T is not a client.Object", obj)
	}
	err := a.Client.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		return "create", a.Client.Create(ctx, obj)
	}
	if err != nil {
		// A failed read changed nothing, so there is no mutation to record.
		return "", err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return "update", a.Client.Update(ctx, obj)
}

// validateGeneration re-reads every desired object and requires it to carry the
// new generation, so the pointer is only switched to a complete generation.
func (a *ClientApplier) validateGeneration(ctx context.Context, objects []client.Object, keys []string, generation string) error {
	for i, obj := range objects {
		live, ok := obj.DeepCopyObject().(client.Object)
		if !ok {
			return fmt.Errorf("object %T is not a client.Object", obj)
		}
		if err := a.Client.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
			return fmt.Errorf("validate generation %s: read %s: %w", generation, keys[i], err)
		}
		if got := live.GetLabels()[GenerationLabel]; got != generation {
			return fmt.Errorf("validate generation %s: %s carries generation %q", generation, keys[i], got)
		}
	}
	return nil
}

// switchGeneration atomically moves the authoritative pointer to the validated
// generation. Until it succeeds, consumers keep selecting the previous one.
func (a *ClientApplier) switchGeneration(ctx context.Context, namespace, generation string, ledger *mutationLedger) error {
	key := client.ObjectKey{Namespace: namespace, Name: GenerationConfigMapName}
	var pointer corev1.ConfigMap
	err := a.Client.Get(ctx, key, &pointer)
	target := generationPointerKey(namespace)
	if apierrors.IsNotFound(err) {
		pointer = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GenerationConfigMapName,
				Namespace: namespace,
				Labels:    map[string]string{ManagedByLabel: ManagedByValue},
			},
			Data: map[string]string{GenerationConfigMapKey: generation},
		}
		createErr := a.Client.Create(ctx, &pointer)
		ledger.record(target, "create", createErr)
		if createErr != nil {
			return fmt.Errorf("publish generation %s: %w", generation, createErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read authoritative generation: %w", err)
	}
	if pointer.Data[GenerationConfigMapKey] == generation {
		return nil
	}
	if pointer.Data == nil {
		pointer.Data = map[string]string{}
	}
	pointer.Data[GenerationConfigMapKey] = generation
	if pointer.Labels == nil {
		pointer.Labels = map[string]string{}
	}
	pointer.Labels[ManagedByLabel] = ManagedByValue
	// The Update carries the resourceVersion read above, so a concurrent switch
	// loses the race with a conflict instead of splitting the generation.
	updateErr := a.Client.Update(ctx, &pointer)
	ledger.record(target, "update", updateErr)
	if updateErr != nil {
		return fmt.Errorf("publish generation %s: %w", generation, updateErr)
	}
	return nil
}

// AuthoritativeGeneration returns the generation consumers must select in the
// namespace. It is empty when no generation has been published yet.
func AuthoritativeGeneration(ctx context.Context, c client.Client, namespace string) (string, error) {
	var pointer corev1.ConfigMap
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: GenerationConfigMapName}, &pointer)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read authoritative generation: %w", err)
	}
	return pointer.Data[GenerationConfigMapKey], nil
}

// GenerationSelector is the label selector that restricts a consumer's reads to
// exactly one config generation.
func GenerationSelector(generation string) client.MatchingLabels {
	return client.MatchingLabels{ManagedByLabel: ManagedByValue, GenerationLabel: generation}
}

func generationPointerKey(namespace string) string {
	return "ConfigMap/" + namespace + "/" + GenerationConfigMapName
}

// prune deletes managed CRs in the namespace that are not in the desired set.
func (a *ClientApplier) prune(ctx context.Context, namespace string, desired map[string]bool, ledger *mutationLedger) error {
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
			key := objKey(obj)
			if desired[key] {
				continue
			}
			err := a.Client.Delete(ctx, obj)
			if apierrors.IsNotFound(err) {
				continue
			}
			ledger.record(key, "delete", err)
			if err != nil {
				return fmt.Errorf("prune %s: %w", key, err)
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
