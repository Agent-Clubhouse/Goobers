// Package configsync turns a config-as-code repo (YAML gaggle/goober/workflow
// definitions, CFG-002/003) into the validated set of Goobers CRs that the M9
// operator reconciles. It is the GitOps bridge: by default it renders the desired
// CR manifest set that ArgoCD applies (DEP-012); a direct Applier is available
// behind an interface. Invalid config is rejected up front with the M1 validator's
// field-level errors.
package configsync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
)

// DefaultNamespace is the control-plane namespace rendered CRs are placed in;
// the operator reconciles Gaggles from here.
const DefaultNamespace = "goobers-system"

// Provenance labels stamped on every rendered/applied object.
const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "goobers-config-sync"
	InstanceLabel  = "goobers.dev/instance"
	GaggleLabel    = "goobers.dev/gaggle"
)

// ErrInvalidConfig is returned by Load when the config repo fails validation.
// The accompanying *validate.Report carries the field-level issues.
var ErrInvalidConfig = errors.New("config repo failed validation")

// RenderSet is the desired set of Goobers CRs derived from a config repo, in a
// deterministic order. Objects carry their TypeMeta, target namespace, and
// provenance labels, ready to render to manifests or apply directly.
type RenderSet struct {
	Namespace string
	Manifest  *v1alpha1.Manifest
	Objects   []client.Object
}

// Loader validates and loads a config repo. Reuse a Loader across calls.
type Loader struct {
	// Namespace is where rendered CRs are placed (defaults to DefaultNamespace).
	Namespace string
	validator *validate.Validator
}

// NewLoader builds a Loader with the embedded schema validator (M1).
func NewLoader(namespace string) (*Loader, error) {
	v, err := validate.New()
	if err != nil {
		return nil, fmt.Errorf("init validator: %w", err)
	}
	if namespace == "" {
		namespace = DefaultNamespace
	}
	return &Loader{Namespace: namespace, validator: v}, nil
}

var docSep = regexp.MustCompile(`(?m)^---\s*$`)

// Load validates the config repo at root, then parses + renders its desired-state
// CRs. ignoreDirs are paths excluded from both validation and parsing — pass the
// render output directory so a config repo that contains its own rendered/ output
// (the GitOps layout ArgoCD watches) stays idempotent across repeated renders and
// does not re-ingest its own output.
//
// The *validate.Report is always returned (even on success, for warnings). If
// validation finds errors, Load returns ErrInvalidConfig and a nil RenderSet —
// callers reject the change and surface the report.
func (l *Loader) Load(root string, ignoreDirs ...string) (*RenderSet, *validate.Report, error) {
	src, cleanup, err := stageSource(root, ignoreDirs)
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()

	report, err := l.validator.ValidateDir(src)
	if err != nil {
		return nil, report, fmt.Errorf("validate %s: %w", root, err)
	}
	if report.HasErrors() {
		return nil, report, ErrInvalidConfig
	}

	docs, err := readDocs(src)
	if err != nil {
		return nil, report, err
	}

	set, err := l.assemble(docs)
	if err != nil {
		return nil, report, err
	}
	return set, report, nil
}

// stageSource returns a directory to load from that excludes ignoreDirs. When
// nothing is excluded it returns root directly (no copy). Otherwise it mirrors
// root into a temp dir, skipping the ignored subtrees, so the full validator
// (which resolves instruction files and cross-references) runs over exactly the
// source definitions. The returned cleanup removes any temp dir.
func stageSource(root string, ignoreDirs []string) (string, func(), error) {
	noop := func() {}
	skip := map[string]bool{}
	for _, d := range ignoreDirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return "", noop, fmt.Errorf("resolve ignore dir %q: %w", d, err)
		}
		skip[abs] = true
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", noop, fmt.Errorf("resolve root %q: %w", root, err)
	}
	// Only stage if an ignored dir actually lives under root.
	staged := false
	for abs := range skip {
		if abs != rootAbs && strings.HasPrefix(abs+string(filepath.Separator), rootAbs+string(filepath.Separator)) {
			staged = true
		}
	}
	if !staged {
		return root, noop, nil
	}

	tmp, err := os.MkdirTemp("", "configsync-src-")
	if err != nil {
		return "", noop, fmt.Errorf("stage source: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	err = filepath.WalkDir(rootAbs, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() && skip[path] {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(rootAbs, path)
		dst := filepath.Join(tmp, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		cleanup()
		return "", noop, fmt.Errorf("stage source: %w", err)
	}
	return tmp, cleanup, nil
}

// rawDoc is one parsed YAML document with its kind/name.
type rawDoc struct {
	kind string
	name string
	yaml []byte
}

type docMeta struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

// readDocs walks root and returns every YAML document with its kind/name.
func readDocs(root string) ([]rawDoc, error) {
	var docs []rawDoc
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, seg := range docSep.Split(string(raw), -1) {
			if strings.TrimSpace(seg) == "" {
				continue
			}
			var meta docMeta
			if err := yaml.Unmarshal([]byte(seg), &meta); err != nil || meta.Kind == "" {
				// Validation already reported malformed docs; skip here.
				continue
			}
			docs = append(docs, rawDoc{kind: meta.Kind, name: meta.Metadata.Name, yaml: []byte(seg)})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	return docs, nil
}

// assemble parses docs into typed CRs and reduces them to the manifest's desired
// state (only manifest-listed gaggles and the goobers/workflows bound to them).
func (l *Loader) assemble(docs []rawDoc) (*RenderSet, error) {
	var (
		manifest *v1alpha1.Manifest
		gaggles  []v1alpha1.Gaggle
		goobers  []v1alpha1.Goober
		flows    []v1alpha1.Workflow
	)
	for _, d := range docs {
		switch d.kind {
		case "Manifest":
			var m v1alpha1.Manifest
			if err := yaml.Unmarshal(d.yaml, &m); err != nil {
				return nil, fmt.Errorf("parse Manifest %s: %w", d.name, err)
			}
			if manifest != nil {
				return nil, fmt.Errorf("multiple Manifest documents (found %q and %q)", manifest.Name, m.Name)
			}
			manifest = &m
		case "Gaggle":
			var g v1alpha1.Gaggle
			if err := yaml.Unmarshal(d.yaml, &g); err != nil {
				return nil, fmt.Errorf("parse Gaggle %s: %w", d.name, err)
			}
			gaggles = append(gaggles, g)
		case "Goober":
			var g v1alpha1.Goober
			if err := yaml.Unmarshal(d.yaml, &g); err != nil {
				return nil, fmt.Errorf("parse Goober %s: %w", d.name, err)
			}
			goobers = append(goobers, g)
		case "Workflow":
			var w v1alpha1.Workflow
			if err := yaml.Unmarshal(d.yaml, &w); err != nil {
				return nil, fmt.Errorf("parse Workflow %s: %w", d.name, err)
			}
			flows = append(flows, w)
		}
	}
	if manifest == nil {
		return nil, errors.New("no Manifest document found in config repo")
	}

	included := map[string]bool{}
	for _, name := range manifest.Spec.Gaggles {
		included[name] = true
	}
	instance := manifest.Spec.Instance.Name

	set := &RenderSet{Namespace: l.Namespace, Manifest: manifest}
	l.stampManifest(manifest, instance)
	set.Objects = append(set.Objects, manifest)

	// Only manifest-listed gaggles (and their goobers/workflows) are desired state;
	// anything else is treated as removed and excluded from the render (prune).
	for i := range gaggles {
		if !included[gaggles[i].Name] {
			continue
		}
		l.stamp(&gaggles[i].TypeMeta, &gaggles[i].ObjectMeta, "Gaggle", instance, gaggles[i].Name)
		set.Objects = append(set.Objects, &gaggles[i])
	}
	for i := range goobers {
		if !included[goobers[i].Spec.Gaggle] {
			continue
		}
		l.stamp(&goobers[i].TypeMeta, &goobers[i].ObjectMeta, "Goober", instance, goobers[i].Spec.Gaggle)
		set.Objects = append(set.Objects, &goobers[i])
	}
	for i := range flows {
		if !included[flows[i].Spec.Gaggle] {
			continue
		}
		l.stamp(&flows[i].TypeMeta, &flows[i].ObjectMeta, "Workflow", instance, flows[i].Spec.Gaggle)
		set.Objects = append(set.Objects, &flows[i])
	}

	sortObjects(set.Objects)
	return set, nil
}

func (l *Loader) stampManifest(m *v1alpha1.Manifest, instance string) {
	l.stamp(&m.TypeMeta, &m.ObjectMeta, "Manifest", instance, "")
}

// stamp normalizes a CR for output: apiVersion/kind, target namespace, and
// provenance labels (managed-by, instance, owning gaggle).
func (l *Loader) stamp(tm *metav1.TypeMeta, om *metav1.ObjectMeta, kind, instance, gaggle string) {
	tm.APIVersion = v1alpha1.GroupVersion.String()
	tm.Kind = kind
	om.Namespace = l.Namespace
	if om.Labels == nil {
		om.Labels = map[string]string{}
	}
	om.Labels[ManagedByLabel] = ManagedByValue
	if instance != "" {
		om.Labels[InstanceLabel] = instance
	}
	if gaggle != "" {
		om.Labels[GaggleLabel] = gaggle
	}
}

// sortObjects orders objects deterministically by (kind, name) so renders are stable.
func sortObjects(objs []client.Object) {
	sort.SliceStable(objs, func(i, j int) bool {
		ki, kj := objs[i].GetObjectKind().GroupVersionKind().Kind, objs[j].GetObjectKind().GroupVersionKind().Kind
		if ki != kj {
			return kindOrder(ki) < kindOrder(kj)
		}
		return objs[i].GetName() < objs[j].GetName()
	})
}

// kindOrder applies a dependency-friendly ordering: Manifest, Gaggle, then workers.
func kindOrder(kind string) int {
	switch kind {
	case "Manifest":
		return 0
	case "Gaggle":
		return 1
	case "Goober":
		return 2
	case "Workflow":
		return 3
	default:
		return 4
	}
}
