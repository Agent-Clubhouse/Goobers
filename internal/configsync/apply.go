package configsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/goobers/goobers/api/v1alpha1"
)

// Generation barrier identifiers. Every applied object is stamped with the
// generation it belongs to, and a single ConfigMap holds the one generation
// consumers are allowed to select, so a partially applied set is never visible.
const (
	// GenerationLabel names the config generation an applied object belongs to.
	GenerationLabel = "goobers.dev/config-generation"
	// GenerationConfigMapName is the authoritative reference switched, in one
	// mutation, once a complete new generation has been applied and validated.
	GenerationConfigMapName = "goobers-config-generation"
	// GenerationKey is the ConfigMap key holding the authoritative generation.
	GenerationKey = "generation"
)

// ErrNoAuthoritativeGeneration is returned when no generation has been published
// yet, so consumers must not select any managed object.
var ErrNoAuthoritativeGeneration = errors.New("no authoritative config generation published")

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

// Apply publishes the desired set as a single generation: every object is
// stamped with that generation and upserted, the complete generation is
// validated against the cluster, the authoritative reference is switched in one
// mutation, and only then are stale managed objects pruned. Any failure before
// the switch leaves the previous generation authoritative and is reported as an
// *ApplyError naming every committed or ambiguous mutation.
//
// Objects are updated in place, so a failure before the switch can leave some
// of them already relabelled to the abandoned generation: the previous
// generation stays authoritative, but a consumer selecting it may then see an
// incomplete set rather than the full previous generation. The barrier
// guarantees a single-generation view, not a rollback of object contents.
func (a *ClientApplier) Apply(ctx context.Context, rs *RenderSet) error {
	generation, err := GenerationID(rs)
	if err != nil {
		return &ApplyError{Phase: PhasePrepare, Err: err}
	}
	previous, err := AuthoritativeGeneration(ctx, a.Client, rs.Namespace)
	if err != nil {
		return &ApplyError{Phase: PhasePrepare, Generation: generation, Err: err}
	}
	fail := func(phase string, mutations []Mutation, err error) error {
		return &ApplyError{
			Phase:         phase,
			Generation:    generation,
			Authoritative: previous,
			Mutations:     mutations,
			Err:           err,
		}
	}

	// Capture desired identity keys up front: a client Create/Update clears an
	// object's in-memory TypeMeta, so objKey must be read before upserting.
	objects := make([]client.Object, 0, len(rs.Objects))
	keys := make([]string, 0, len(rs.Objects))
	desired := make(map[string]bool, len(rs.Objects))
	for _, obj := range rs.Objects {
		stamped, err := stampGeneration(obj, generation)
		if err != nil {
			return fail(PhasePrepare, nil, err)
		}
		objects = append(objects, stamped)
		keys = append(keys, objKey(stamped))
		desired[objKey(stamped)] = true
	}

	var mutations []Mutation
	for i, obj := range objects {
		op, err := a.upsert(ctx, obj)
		if err != nil {
			mutations = appendMutation(mutations, op, keys[i], mutationState(err))
			return fail(PhaseApply, mutations, fmt.Errorf("apply %s: %w", keys[i], err))
		}
		mutations = appendMutation(mutations, op, keys[i], MutationCommitted)
	}

	if err := a.validateGeneration(ctx, rs.Objects, generation); err != nil {
		return fail(PhaseValidate, mutations, err)
	}

	op, err := a.publishGeneration(ctx, rs.Namespace, generation)
	if err != nil {
		mutations = appendMutation(mutations, op, generationRef(rs.Namespace), mutationState(err))
		return fail(PhaseSwitch, mutations, err)
	}
	mutations = appendMutation(mutations, op, generationRef(rs.Namespace), MutationCommitted)

	pruned, pruneErr := a.prune(ctx, rs.Namespace, desired)
	mutations = append(mutations, pruned...)
	if pruneErr != nil {
		// The new generation is already authoritative here, so a prune failure
		// leaves stale objects behind but never a mixed view.
		return &ApplyError{
			Phase:         PhasePrune,
			Generation:    generation,
			Authoritative: generation,
			Mutations:     mutations,
			Err:           pruneErr,
		}
	}
	if a.Log != nil {
		a.Log.Info("config-sync generation published",
			"namespace", rs.Namespace, "generation", generation, "objects", len(objects))
	}
	return nil
}

// upsert creates the object, or updates it in place if it already exists, and
// reports which operation was attempted so a failure can name the mutation. It
// reports OpNone when it fails before issuing any write, so a read failure is
// not over-reported as an attempted mutation.
func (a *ClientApplier) upsert(ctx context.Context, obj client.Object) (string, error) {
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return OpNone, fmt.Errorf("object %T is not a client.Object", obj)
	}
	err := a.Client.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		return OpCreate, a.Client.Create(ctx, obj)
	}
	if err != nil {
		return OpNone, err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return OpUpdate, a.Client.Update(ctx, obj)
}

// validateGeneration re-reads every desired object and asserts the cluster holds
// the complete generation, so the authoritative switch never points at a
// partially applied set.
func (a *ClientApplier) validateGeneration(ctx context.Context, objs []client.Object, generation string) error {
	for _, obj := range objs {
		key := objKey(obj)
		existing, ok := obj.DeepCopyObject().(client.Object)
		if !ok {
			return fmt.Errorf("object %T is not a client.Object", obj)
		}
		if err := a.Client.Get(ctx, client.ObjectKeyFromObject(obj), existing); err != nil {
			return fmt.Errorf("validate %s: %w", key, err)
		}
		if got := existing.GetLabels()[GenerationLabel]; got != generation {
			return fmt.Errorf("validate %s: generation %q, want %q", key, got, generation)
		}
	}
	return nil
}

// publishGeneration switches the single authoritative reference to generation.
func (a *ClientApplier) publishGeneration(ctx context.Context, namespace, generation string) (string, error) {
	var existing corev1.ConfigMap
	key := client.ObjectKey{Namespace: namespace, Name: GenerationConfigMapName}
	err := a.Client.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GenerationConfigMapName,
				Namespace: namespace,
				Labels:    map[string]string{ManagedByLabel: ManagedByValue},
			},
			Data: map[string]string{GenerationKey: generation},
		}
		if err := a.Client.Create(ctx, cm); err != nil {
			return OpCreate, fmt.Errorf("publish generation %s: %w", generation, err)
		}
		return OpCreate, nil
	}
	if err != nil {
		return OpNone, fmt.Errorf("read authoritative config generation: %w", err)
	}
	if existing.Data == nil {
		existing.Data = map[string]string{}
	}
	existing.Data[GenerationKey] = generation
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	existing.Labels[ManagedByLabel] = ManagedByValue
	if err := a.Client.Update(ctx, &existing); err != nil {
		return OpUpdate, fmt.Errorf("publish generation %s: %w", generation, err)
	}
	return OpUpdate, nil
}

// AuthoritativeGeneration returns the config generation consumers must select,
// or "" when none has been published yet.
func AuthoritativeGeneration(ctx context.Context, c client.Client, namespace string) (string, error) {
	var cm corev1.ConfigMap
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: GenerationConfigMapName}, &cm)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read authoritative config generation: %w", err)
	}
	return cm.Data[GenerationKey], nil
}

// GenerationSelector returns the label selector a consumer must use to read
// managed config. Selecting on the authoritative generation is what keeps a
// consumer from ever mixing objects across generations.
func GenerationSelector(ctx context.Context, c client.Client, namespace string) (client.MatchingLabels, error) {
	generation, err := AuthoritativeGeneration(ctx, c, namespace)
	if err != nil {
		return nil, err
	}
	if generation == "" {
		return nil, ErrNoAuthoritativeGeneration
	}
	return client.MatchingLabels{ManagedByLabel: ManagedByValue, GenerationLabel: generation}, nil
}

// GenerationID derives the immutable identity of a desired set. Identical config
// yields an identical generation, so re-applying is idempotent.
func GenerationID(rs *RenderSet) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "namespace=%s\n", rs.Namespace)
	for _, obj := range rs.Objects {
		data, err := json.Marshal(obj)
		if err != nil {
			return "", fmt.Errorf("identify generation for %s: %w", objKey(obj), err)
		}
		fmt.Fprintf(h, "%s\n%s\n", objKey(obj), data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// stampGeneration returns a copy of obj labelled with the generation, leaving
// the caller's RenderSet untouched.
func stampGeneration(obj client.Object, generation string) (client.Object, error) {
	stamped, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return nil, fmt.Errorf("object %T is not a client.Object", obj)
	}
	labels := stamped.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[GenerationLabel] = generation
	stamped.SetLabels(labels)
	return stamped, nil
}

func generationRef(namespace string) string {
	return "ConfigMap/" + namespace + "/" + GenerationConfigMapName
}

// prune deletes managed CRs in the namespace that are not in the desired set. It
// runs only after the new generation is authoritative, and reports the deletions
// it committed even when it fails partway.
func (a *ClientApplier) prune(ctx context.Context, namespace string, desired map[string]bool) ([]Mutation, error) {
	var mutations []Mutation
	lists := []client.ObjectList{
		&v1alpha1.GaggleList{}, &v1alpha1.GooberList{}, &v1alpha1.WorkflowList{}, &v1alpha1.ManifestList{},
	}
	for _, list := range lists {
		if err := a.Client.List(ctx, list,
			client.InNamespace(namespace),
			client.MatchingLabels{ManagedByLabel: ManagedByValue},
		); err != nil {
			return mutations, fmt.Errorf("list for prune: %w", err)
		}
		objs, err := metaItems(list)
		if err != nil {
			return mutations, err
		}
		for _, obj := range objs {
			key := objKey(obj)
			if desired[key] {
				continue
			}
			if err := a.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
				mutations = appendMutation(mutations, OpDelete, key, mutationState(err))
				return mutations, fmt.Errorf("prune %s: %w", key, err)
			}
			mutations = appendMutation(mutations, OpDelete, key, MutationCommitted)
			if a.Log != nil {
				a.Log.Info("config-sync pruned", "name", obj.GetName(), "namespace", obj.GetNamespace())
			}
		}
	}
	return mutations, nil
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
