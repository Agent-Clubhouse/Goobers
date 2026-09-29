// Package gagglebundle exports and imports sanitized, portable gaggle definitions.
package gagglebundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretpattern"
	"github.com/goobers/goobers/internal/version"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// ErrInvalidBundle indicates that a bundle violates its schema or safety contract.
var ErrInvalidBundle = errors.New("invalid gaggle bundle")

// ErrGaggleNotFound indicates that the requested source gaggle does not exist.
var ErrGaggleNotFound = errors.New("gaggle not found")

// ErrNameConflict indicates that the destination gaggle name already exists.
var ErrNameConflict = errors.New("gaggle name conflict")

// ErrRepositoryAuthorization indicates that the destination lacks a required repository authorization.
var ErrRepositoryAuthorization = errors.New("destination repository authorization is required")

var prepareConfigDirSwap = instance.PrepareConfigDirSwap

const (
	maxCompanionFiles        = 256
	maxCompanionFileBytes    = 1 << 20
	maxCompanionEncodedBytes = ((maxCompanionFileBytes + 2) / 3) * 4
	maxCompanionTotalBytes   = 8 << 20
)

var (
	companionSecretPatterns = secretpattern.NewScrubber()
	credentialAssignment    = regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?key|auth(?:entication|orization)?|client[_-]?secret|credential|passwd|password|private[_-]?key|secret|token)\b\s*[:=]\s*(?:"([^"\r\n]{8,})"|'([^'\r\n]{8,})'|([^\s"',;#]{8,}))`)
	credentialURI           = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/\s:@]+:([^@\s/]+)@`)
	windowsLocalPath        = regexp.MustCompile(`(?i)(?:\b[A-Z]:[\\/]|\\\\[^\\\s]+\\[^\\\s]+)`)
	unixLocalPath           = regexp.MustCompile(`(?:^|[\s"'=(])/(?:home|Users|private|tmp|var|etc|opt|srv|root|mnt)/[^\s"'<>]+`)
)

var sanitizedFields = []string{
	"definition.gaggle.metadata.runtime",
	"definition.gaggle.spec.selfIdentity",
	"definition.gaggle.spec.isolation",
	"definition.gaggle.spec.outboxMirrorPath",
	"definition.gaggle.spec.workcopies",
	"definition.gaggle.spec.*.connectionRef",
	"definition.workflows[].metadata.runtime",
	"definition.workflows[].spec.outboxMirrorPath",
	"definition.workflows[].spec.tasks[].outboxMirrorPath",
	"definition.goobers[].metadata.runtime",
}

// Export returns a deterministic portable definition plus timestamped
// provenance. The digest commits only to Definition.
func Export(configDir, name string, now time.Time) (apiv1.GaggleBundle, error) {
	set, report, err := instance.LoadConfigDir(configDir)
	if err != nil {
		return apiv1.GaggleBundle{}, fmt.Errorf("load source configuration: %w (%s)", err, reportSummary(report))
	}
	name = strings.TrimSpace(name)
	var source *apiv1.Gaggle
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == name {
			source = &set.Gaggles[i]
			break
		}
	}
	if source == nil {
		return apiv1.GaggleBundle{}, fmt.Errorf("%w: %q", ErrGaggleNotFound, name)
	}

	gaggle := sanitizeGaggle(*source)
	var workflows []apiv1.Workflow
	for _, workflow := range set.Workflows {
		if workflow.Spec.Gaggle == name {
			for _, task := range workflow.Spec.Tasks {
				if task.Run != nil && len(task.Run.Env) != 0 {
					return apiv1.GaggleBundle{}, fmt.Errorf(
						"%w: workflow %q task %q declares run.env values; portable bundles refuse environment values rather than exporting or silently dropping them",
						ErrInvalidBundle, workflow.Name, task.Name,
					)
				}
			}
			workflows = append(workflows, sanitizeWorkflow(workflow))
		}
	}
	referencedGoobers := workflowGooberReferences(workflows)
	var goobers []apiv1.Goober
	for _, goober := range set.Goobers {
		if goober.Spec.Gaggle == name || (goober.Spec.Gaggle == "" && referencedGoobers[goober.Name]) {
			if len(goober.Spec.HarnessOptions) != 0 {
				return apiv1.GaggleBundle{}, fmt.Errorf(
					"%w: goober %q declares opaque harnessOptions; portable export cannot prove opaque values exclude credentials, environment values, or local paths",
					ErrInvalidBundle, goober.Name,
				)
			}
			portable := sanitizeGoober(goober)
			portable.Spec.Gaggle = name
			goobers = append(goobers, portable)
		}
	}
	sort.Slice(workflows, func(i, j int) bool { return workflows[i].Name < workflows[j].Name })
	sort.Slice(goobers, func(i, j int) bool { return goobers[i].Name < goobers[j].Name })

	files, err := collectFiles(configDir, set, name, goobers)
	if err != nil {
		return apiv1.GaggleBundle{}, err
	}
	repositories := portableRepositories(gaggle.Spec)
	definition := apiv1.GaggleBundleDefinition{
		Gaggle: gaggle, Workflows: workflows, Goobers: goobers,
		Files: files, Repositories: repositories,
	}
	if err := validateStructuredCredentials(definition); err != nil {
		return apiv1.GaggleBundle{}, fmt.Errorf("%w: %w", ErrInvalidBundle, err)
	}
	if path, value := firstAbsoluteString(definition); path != "" {
		return apiv1.GaggleBundle{}, fmt.Errorf("%w: %s contains non-portable absolute path %q", ErrInvalidBundle, path, value)
	}
	digest, err := DefinitionDigest(definition)
	if err != nil {
		return apiv1.GaggleBundle{}, err
	}
	build := version.Get()
	sourceVersion := gaggle.APIVersion
	if sourceVersion == "" {
		sourceVersion = "goobers.dev/v1alpha1"
	}
	bundle := apiv1.GaggleBundle{
		APIVersion:    apiv1.GaggleBundleAPIVersion,
		Kind:          apiv1.GaggleBundleKind,
		SchemaVersion: apiv1.GaggleBundleSchemaVersion,
		Source:        apiv1.GaggleBundleSource{Name: name, APIVersion: sourceVersion, Digest: digest},
		ExportedAt:    now.UTC(),
		Provenance: apiv1.GaggleBundleProvenance{
			Exporter: "goobers", ExporterVersion: build.Version, ExporterCommit: build.Commit,
			SanitizedFields: append([]string(nil), sanitizedFields...),
		},
		Definition: definition,
		Digest:     digest,
	}
	if err := Validate(bundle); err != nil {
		return apiv1.GaggleBundle{}, fmt.Errorf("validate exported gaggle bundle: %w", err)
	}
	return bundle, nil
}

// DefinitionDigest returns the stable SHA-256 of the sanitized declarative
// definition. JSON map keys are sorted by encoding/json.
func DefinitionDigest(definition apiv1.GaggleBundleDefinition) (string, error) {
	data, err := json.Marshal(definition)
	if err != nil {
		return "", fmt.Errorf("marshal portable gaggle definition: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Validate verifies the complete bundle before any destination mutation.
func Validate(bundle apiv1.GaggleBundle) error {
	switch {
	case bundle.APIVersion != apiv1.GaggleBundleAPIVersion:
		return fmt.Errorf("%w: apiVersion must be %q", ErrInvalidBundle, apiv1.GaggleBundleAPIVersion)
	case bundle.Kind != apiv1.GaggleBundleKind:
		return fmt.Errorf("%w: kind must be %q", ErrInvalidBundle, apiv1.GaggleBundleKind)
	case bundle.SchemaVersion != apiv1.GaggleBundleSchemaVersion:
		return fmt.Errorf("%w: unsupported schemaVersion %d", ErrInvalidBundle, bundle.SchemaVersion)
	case strings.TrimSpace(bundle.Source.Name) == "":
		return fmt.Errorf("%w: source.name is required", ErrInvalidBundle)
	case bundle.Source.APIVersion == "":
		return fmt.Errorf("%w: source.apiVersion is required", ErrInvalidBundle)
	case bundle.ExportedAt.IsZero():
		return fmt.Errorf("%w: exportedAt is required", ErrInvalidBundle)
	case bundle.Provenance.Exporter != "goobers":
		return fmt.Errorf("%w: provenance.exporter must be %q", ErrInvalidBundle, "goobers")
	case strings.TrimSpace(bundle.Provenance.ExporterVersion) == "":
		return fmt.Errorf("%w: provenance.exporterVersion is required", ErrInvalidBundle)
	case bundle.Definition.Gaggle.Name == "":
		return fmt.Errorf("%w: definition.gaggle.metadata.name is required", ErrInvalidBundle)
	case bundle.Definition.Gaggle.Name != bundle.Source.Name:
		return fmt.Errorf("%w: source.name %q does not match definition gaggle %q", ErrInvalidBundle, bundle.Source.Name, bundle.Definition.Gaggle.Name)
	}
	if err := validateRetainedEnvelope(bundle); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBundle, err)
	}
	if err := validateReferences(bundle.Definition); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBundle, err)
	}
	if err := validateSanitizedDefinition(bundle.Definition); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBundle, err)
	}
	if err := validateFiles(bundle.Definition); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBundle, err)
	}
	digest, err := DefinitionDigest(bundle.Definition)
	if err != nil {
		return err
	}
	if bundle.Digest != digest || bundle.Source.Digest != digest {
		return fmt.Errorf("%w: digest mismatch: computed %s", ErrInvalidBundle, digest)
	}
	return nil
}

func validateRetainedEnvelope(bundle apiv1.GaggleBundle) error {
	if !reflect.DeepEqual(bundle.Provenance.SanitizedFields, sanitizedFields) {
		return errors.New("provenance.sanitizedFields does not match the current bundle contract")
	}
	retained := struct {
		Source     apiv1.GaggleBundleSource     `json:"source"`
		ExportedAt time.Time                    `json:"exportedAt"`
		Provenance apiv1.GaggleBundleProvenance `json:"provenance"`
	}{
		Source: bundle.Source, ExportedAt: bundle.ExportedAt, Provenance: bundle.Provenance,
	}
	data, err := json.Marshal(retained)
	if err != nil {
		return fmt.Errorf("encode retained envelope for validation: %w", err)
	}
	if reason := credentialContentReason(data); reason != "" {
		return fmt.Errorf("retained envelope %s", reason)
	}
	if path, value := firstAbsoluteString(retained); path != "" {
		return fmt.Errorf("retained envelope %s contains non-portable absolute path %q", path, value)
	}
	return nil
}

// PrepareImport validates and stages a destination gaggle, then atomically
// installs the complete candidate config tree. The caller must Commit or
// Rollback the returned swap.
func PrepareImport(layout instance.Layout, target string, bundle apiv1.GaggleBundle) (*instance.PreparedConfigSwap, error) {
	if err := Validate(bundle); err != nil {
		return nil, err
	}
	target = strings.TrimSpace(target)
	if !portableName(target) {
		return nil, fmt.Errorf("%w: destination name %q must use lowercase letters, digits, and interior hyphens", ErrInvalidBundle, target)
	}
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		return nil, fmt.Errorf("load destination configuration: %w (%s)", err, reportSummary(report))
	}
	for _, gaggle := range set.Gaggles {
		if gaggle.Name == target {
			return nil, fmt.Errorf("%w: gaggle %q already exists", ErrNameConflict, target)
		}
	}
	existingWorkflows := make(map[string]bool, len(set.Workflows))
	for _, workflow := range set.Workflows {
		existingWorkflows[workflow.Name] = true
	}
	for _, workflow := range bundle.Definition.Workflows {
		if existingWorkflows[workflow.Name] {
			return nil, fmt.Errorf("%w: workflow %q already exists", ErrNameConflict, workflow.Name)
		}
	}
	existingGoobers := make(map[string]bool, len(set.Goobers))
	for _, goober := range set.Goobers {
		existingGoobers[goober.Name] = true
	}
	for _, goober := range bundle.Definition.Goobers {
		if existingGoobers[goober.Name] {
			return nil, fmt.Errorf("%w: goober %q already exists", ErrNameConflict, goober.Name)
		}
	}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load destination instance authorization: %w", err)
	}
	if err := requireRepositoryAuthorization(cfg, bundle.Definition.Repositories); err != nil {
		return nil, err
	}

	stagingRoot, err := os.MkdirTemp(layout.Root, ".gaggle-import-")
	if err != nil {
		return nil, fmt.Errorf("create import staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingRoot) }()
	stagedConfig := filepath.Join(stagingRoot, instance.ConfigDirName)
	if err := copyTree(layout.ConfigDir(), stagedConfig); err != nil {
		return nil, fmt.Errorf("stage destination configuration: %w", err)
	}
	if err := materialize(stagedConfig, target, bundle); err != nil {
		return nil, err
	}
	if _, stagedReport, err := instance.LoadConfigDir(stagedConfig); err != nil {
		return nil, fmt.Errorf("%w: imported configuration failed validation: %w (%s)", ErrInvalidBundle, err, reportSummary(stagedReport))
	}
	swap, err := prepareConfigDirSwap(layout, stagedConfig)
	if err != nil {
		return nil, fmt.Errorf("install imported gaggle: %w", err)
	}
	return swap, nil
}

func sanitizeGaggle(source apiv1.Gaggle) apiv1.Gaggle {
	g := *source.DeepCopy()
	g.TypeMeta = metav1.TypeMeta{APIVersion: "goobers.dev/v1alpha1", Kind: "Gaggle"}
	g.ObjectMeta = portableMetadata(g.Name, g.Labels, g.Annotations)
	g.Status = apiv1.GaggleStatus{}
	g.Spec.SelfIdentity = ""
	g.Spec.Isolation = apiv1.GaggleIsolation{Namespace: "gaggle-portable"}
	g.Spec.OutboxMirrorPath = ""
	g.Spec.Workcopies = nil
	clearRepoConnection(&g.Spec.Project)
	for i := range g.Spec.AdditionalRepos {
		clearRepoConnection(&g.Spec.AdditionalRepos[i])
	}
	g.Spec.Backlog.ConnectionRef = ""
	for i := range g.Spec.Siblings {
		clearRepoConnection(&g.Spec.Siblings[i].Project)
	}
	return g
}

func sanitizeWorkflow(source apiv1.Workflow) apiv1.Workflow {
	w := *source.DeepCopy()
	w.TypeMeta = metav1.TypeMeta{APIVersion: "goobers.dev/v1alpha1", Kind: "Workflow"}
	w.ObjectMeta = portableMetadata(w.Name, w.Labels, w.Annotations)
	w.Spec.OutboxMirrorPath = ""
	for i := range w.Spec.Tasks {
		w.Spec.Tasks[i].OutboxMirrorPath = ""
		if w.Spec.Tasks[i].Run != nil {
			w.Spec.Tasks[i].Run.Env = nil
		}
	}
	return w
}

func sanitizeGoober(source apiv1.Goober) apiv1.Goober {
	g := *source.DeepCopy()
	g.TypeMeta = metav1.TypeMeta{APIVersion: "goobers.dev/v1alpha1", Kind: "Goober"}
	g.ObjectMeta = portableMetadata(g.Name, g.Labels, g.Annotations)
	return g
}

func portableMetadata(name string, labels, annotations map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Labels:      cloneStrings(labels),
		Annotations: cloneStrings(annotations),
	}
}

func cloneStrings(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func clearRepoConnection(repo *apiv1.RepoRef) {
	repo.ConnectionRef = ""
}

func portableRepositories(spec apiv1.GaggleSpec) []apiv1.RepoRef {
	repositories := append([]apiv1.RepoRef{spec.Project}, spec.AdditionalRepos...)
	if backlogRepo, ok := portableBacklogRepository(spec.Project, spec.Backlog); ok {
		repositories = append(repositories, backlogRepo)
	}
	for _, sibling := range spec.Siblings {
		repositories = append(repositories, sibling.Project)
	}
	for i := range repositories {
		clearRepoConnection(&repositories[i])
	}
	sort.Slice(repositories, func(i, j int) bool { return repositoryKey(repositories[i]) < repositoryKey(repositories[j]) })
	return repositories
}

func portableBacklogRepository(project apiv1.RepoRef, backlog apiv1.BacklogRef) (apiv1.RepoRef, bool) {
	if project.Provider != apiv1.ProviderADO ||
		(backlog.Provider != apiv1.ProviderGitHub && backlog.Provider != apiv1.ProviderGitea) {
		return apiv1.RepoRef{}, false
	}
	owner, name, ok := strings.Cut(backlog.Project, "/")
	if !ok || owner == "" || name == "" {
		return apiv1.RepoRef{}, false
	}
	return apiv1.RepoRef{
		Provider: backlog.Provider,
		BaseURL:  backlog.BaseURL,
		Owner:    owner,
		Name:     name,
	}, true
}

func validateReferences(definition apiv1.GaggleBundleDefinition) error {
	gaggle := definition.Gaggle.Name
	workflows := make(map[string]bool, len(definition.Workflows))
	for _, workflow := range definition.Workflows {
		if !portableName(workflow.Name) {
			return fmt.Errorf("workflow name %q must use lowercase letters, digits, and interior hyphens", workflow.Name)
		}
		if workflow.Spec.Gaggle != gaggle {
			return fmt.Errorf("workflow %q references gaggle %q, want %q", workflow.Name, workflow.Spec.Gaggle, gaggle)
		}
		if workflows[workflow.Name] {
			return fmt.Errorf("duplicate workflow %q", workflow.Name)
		}
		workflows[workflow.Name] = true
	}
	goobers := make(map[string]bool, len(definition.Goobers))
	for _, goober := range definition.Goobers {
		if !portableName(goober.Name) {
			return fmt.Errorf("goober name %q must use lowercase letters, digits, and interior hyphens", goober.Name)
		}
		if goober.Spec.Instructions != "" && !portableRelativePath(goober.Spec.Instructions) {
			return fmt.Errorf("goober %q instructions path %q must be a canonical relative slash-separated path", goober.Name, goober.Spec.Instructions)
		}
		if goober.Spec.Gaggle != gaggle {
			return fmt.Errorf("goober %q references gaggle %q, want %q", goober.Name, goober.Spec.Gaggle, gaggle)
		}
		if goobers[goober.Name] {
			return fmt.Errorf("duplicate goober %q", goober.Name)
		}
		goobers[goober.Name] = true
		for _, workflow := range goober.Spec.Workflows {
			if !workflows[workflow] {
				return fmt.Errorf("goober %q references missing workflow %q", goober.Name, workflow)
			}
		}
	}
	for _, workflow := range definition.Workflows {
		for goober := range workflowGooberReferences([]apiv1.Workflow{workflow}) {
			if !goobers[goober] {
				return fmt.Errorf("workflow %q references missing goober %q", workflow.Name, goober)
			}
		}
	}
	return nil
}

func workflowGooberReferences(workflows []apiv1.Workflow) map[string]bool {
	references := map[string]bool{}
	for _, workflow := range workflows {
		for _, task := range workflow.Spec.Tasks {
			if task.Goober != "" {
				references[task.Goober] = true
			}
		}
		for _, gate := range workflow.Spec.Gates {
			if gate.Agentic != nil && gate.Agentic.Goober != "" {
				references[gate.Agentic.Goober] = true
			}
		}
	}
	return references
}

func validateSanitizedDefinition(definition apiv1.GaggleBundleDefinition) error {
	gaggle := definition.Gaggle
	if !reflect.DeepEqual(gaggle.Status, apiv1.GaggleStatus{}) {
		return errors.New("gaggle runtime status is forbidden")
	}
	if gaggle.Spec.SelfIdentity != "" || gaggle.Spec.OutboxMirrorPath != "" || gaggle.Spec.Workcopies != nil {
		return errors.New("gaggle contains destination-local identity or absolute-path settings")
	}
	if gaggle.Spec.Isolation.Namespace != "gaggle-portable" || gaggle.Spec.Isolation.IdentityRef != "" {
		return errors.New("gaggle isolation must be the portable placeholder without an identity reference")
	}
	if err := validatePortableMetadata("gaggle", gaggle.Labels, gaggle.Annotations); err != nil {
		return err
	}
	if err := validateNoConnectionRefs(gaggle.Spec); err != nil {
		return err
	}
	for _, workflow := range definition.Workflows {
		if err := validatePortableMetadata("workflow "+workflow.Name, workflow.Labels, workflow.Annotations); err != nil {
			return err
		}
		if workflow.Spec.OutboxMirrorPath != "" {
			return fmt.Errorf("workflow %q contains an outbox mirror path", workflow.Name)
		}
		for _, task := range workflow.Spec.Tasks {
			if task.OutboxMirrorPath != "" {
				return fmt.Errorf("workflow %q task %q contains an outbox mirror path", workflow.Name, task.Name)
			}
			if task.Run != nil && len(task.Run.Env) != 0 {
				return fmt.Errorf("workflow %q task %q contains environment values", workflow.Name, task.Name)
			}
		}
	}
	for _, goober := range definition.Goobers {
		if err := validatePortableMetadata("goober "+goober.Name, goober.Labels, goober.Annotations); err != nil {
			return err
		}
		if len(goober.Spec.HarnessOptions) != 0 {
			return fmt.Errorf("goober %q contains opaque harnessOptions", goober.Name)
		}
	}
	if err := validateStructuredCredentials(definition); err != nil {
		return err
	}
	if path, value := firstAbsoluteString(definition); path != "" {
		return fmt.Errorf("%s contains non-portable absolute path %q", path, value)
	}
	expectedRepositories := portableRepositories(gaggle.Spec)
	if !reflect.DeepEqual(definition.Repositories, expectedRepositories) {
		return errors.New("repositories must exactly match the sorted credential-free project and additionalRepos identities")
	}
	return nil
}

func validateNoConnectionRefs(spec apiv1.GaggleSpec) error {
	if spec.Project.ConnectionRef != "" || spec.Backlog.ConnectionRef != "" {
		return errors.New("repository and backlog connection references are forbidden")
	}
	for _, repo := range spec.AdditionalRepos {
		if repo.ConnectionRef != "" {
			return errors.New("repository connection references are forbidden")
		}
	}
	for _, sibling := range spec.Siblings {
		if sibling.Project.ConnectionRef != "" {
			return errors.New("sibling repository connection references are forbidden")
		}
	}
	return nil
}

func validatePortableMetadata(subject string, labels, annotations map[string]string) error {
	metadata := struct {
		Labels      map[string]string `json:"labels,omitempty"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}{Labels: labels, Annotations: annotations}
	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode %s metadata: %w", subject, err)
	}
	if reason := credentialContentReason(data); reason != "" {
		return fmt.Errorf("%s metadata %s", subject, reason)
	}
	if path, value := firstAbsoluteString(metadata); path != "" {
		return fmt.Errorf("%s metadata %s contains non-portable absolute path %q", subject, path, value)
	}
	return nil
}

func collectFiles(configDir string, set *instance.ConfigSet, gaggle string, goobers []apiv1.Goober) ([]apiv1.GaggleBundleFile, error) {
	paths, err := collectCompanionPaths(configDir, set, gaggle, goobers)
	if err != nil {
		return nil, err
	}
	if len(paths) > maxCompanionFiles {
		return nil, fmt.Errorf("%w: companion file count %d exceeds limit %d", ErrInvalidBundle, len(paths), maxCompanionFiles)
	}
	return encodeCompanionFiles(paths)
}

func collectCompanionPaths(configDir string, set *instance.ConfigSet, gaggle string, goobers []apiv1.Goober) (map[string]string, error) {
	root := filepath.Join(configDir, "gaggles", gaggle)
	paths := map[string]string{}
	for _, goober := range goobers {
		source, ok := set.GooberSource(goober.Name)
		if !ok {
			return nil, fmt.Errorf("resolve goober %q source", goober.Name)
		}
		sourceDir := filepath.Dir(filepath.Join(configDir, filepath.FromSlash(source)))
		if goober.Spec.Instructions != "" {
			if !portableRelativePath(goober.Spec.Instructions) {
				return nil, fmt.Errorf("goober %q instructions path %q is not contained", goober.Name, goober.Spec.Instructions)
			}
			instructions := filepath.Clean(filepath.FromSlash(goober.Spec.Instructions))
			portable := filepath.ToSlash(filepath.Join("goobers", goober.Name, instructions))
			paths[portable] = filepath.Join(sourceDir, instructions)
		}
		for _, skill := range goober.Spec.Skills {
			if err := collectSkillPaths(configDir, root, goober.Name, skill, paths); err != nil {
				return nil, err
			}
		}
	}
	return paths, nil
}

func collectSkillPaths(configDir, gaggleRoot, gooberName, skill string, paths map[string]string) error {
	if skill == "" || strings.ContainsAny(skill, `/\`) || skill == "." || skill == ".." {
		return fmt.Errorf("goober %q has non-portable skill name %q", gooberName, skill)
	}
	skillDir := filepath.Join(gaggleRoot, "skills", skill)
	info, err := os.Stat(skillDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect gaggle skill package %q: %w", skill, err)
	}
	if err != nil || !info.IsDir() {
		skillDir = filepath.Join(filepath.Dir(configDir), "skills", skill)
	}
	info, err = os.Stat(skillDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect shared skill package %q: %w", skill, err)
	}
	if err != nil || !info.IsDir() {
		return fmt.Errorf("goober %q references missing skill package %q", gooberName, skill)
	}
	return filepath.WalkDir(skillDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("referenced skill path %s is a symlink", path)
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(skillDir, path)
		if err != nil {
			return err
		}
		portable := filepath.ToSlash(filepath.Join("skills", skill, rel))
		paths[portable] = path
		return nil
	})
}

func encodeCompanionFiles(paths map[string]string) ([]apiv1.GaggleBundleFile, error) {
	var files []apiv1.GaggleBundleFile
	totalBytes := int64(0)
	for rel, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("read referenced file %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("referenced file %s is not a regular file", path)
		}
		if info.Size() > maxCompanionFileBytes {
			return nil, invalidCompanionFile(rel, fmt.Sprintf("exceeds the %d-byte per-file limit", maxCompanionFileBytes))
		}
		if totalBytes+info.Size() > maxCompanionTotalBytes {
			return nil, fmt.Errorf("%w: companion files exceed the %d-byte aggregate limit", ErrInvalidBundle, maxCompanionTotalBytes)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read referenced file %s: %w", path, err)
		}
		if len(data) > maxCompanionFileBytes {
			return nil, invalidCompanionFile(rel, fmt.Sprintf("exceeds the %d-byte per-file limit", maxCompanionFileBytes))
		}
		totalBytes += int64(len(data))
		if totalBytes > maxCompanionTotalBytes {
			return nil, fmt.Errorf("%w: companion files exceed the %d-byte aggregate limit", ErrInvalidBundle, maxCompanionTotalBytes)
		}
		if err := validateCompanionContent(rel, data); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		files = append(files, apiv1.GaggleBundleFile{
			Path: rel, ContentBase64: base64.StdEncoding.EncodeToString(data),
			SHA256: "sha256:" + hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func decodeAndValidateFile(file apiv1.GaggleBundleFile) ([]byte, error) {
	if file.Path == "" || filepath.IsAbs(file.Path) || strings.Contains(file.Path, `\`) {
		return nil, fmt.Errorf("file path %q must be a relative slash-separated path", file.Path)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(file.Path)))
	if clean != file.Path || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return nil, fmt.Errorf("file path %q is not contained", file.Path)
	}
	if len(file.ContentBase64) > maxCompanionEncodedBytes {
		return nil, invalidCompanionFile(file.Path, fmt.Sprintf("exceeds the %d-byte per-file limit", maxCompanionFileBytes))
	}
	data, err := base64.StdEncoding.DecodeString(file.ContentBase64)
	if err != nil {
		return nil, fmt.Errorf("file %q contentBase64 is invalid: %w", file.Path, err)
	}
	if len(data) > maxCompanionFileBytes {
		return nil, invalidCompanionFile(file.Path, fmt.Sprintf("exceeds the %d-byte per-file limit", maxCompanionFileBytes))
	}
	if err := validateCompanionContent(file.Path, data); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	want := "sha256:" + hex.EncodeToString(sum[:])
	if file.SHA256 != want {
		return nil, fmt.Errorf("file %q digest mismatch", file.Path)
	}
	return data, nil
}

func validateFiles(definition apiv1.GaggleBundleDefinition) error {
	if len(definition.Files) > maxCompanionFiles {
		return fmt.Errorf("companion file count %d exceeds limit %d", len(definition.Files), maxCompanionFiles)
	}
	seen := map[string]bool{}
	totalBytes := 0
	for _, file := range definition.Files {
		data, err := decodeAndValidateFile(file)
		if err != nil {
			return err
		}
		totalBytes += len(data)
		if totalBytes > maxCompanionTotalBytes {
			return fmt.Errorf("companion files exceed the %d-byte aggregate limit", maxCompanionTotalBytes)
		}
		if seen[file.Path] {
			return fmt.Errorf("duplicate file path %q", file.Path)
		}
		seen[file.Path] = true
		if !isReferencedCompanionPath(definition.Goobers, file.Path) {
			return fmt.Errorf("file path %q is not a referenced Goober instruction or gaggle skill file", file.Path)
		}
	}
	for _, goober := range definition.Goobers {
		if goober.Spec.Instructions != "" {
			path := filepath.ToSlash(filepath.Clean(filepath.Join("goobers", goober.Name, filepath.FromSlash(goober.Spec.Instructions))))
			if !seen[path] {
				return fmt.Errorf("goober %q instruction file %q is missing", goober.Name, path)
			}
		}
		for _, skill := range goober.Spec.Skills {
			prefix := "skills/" + skill + "/"
			found := false
			for path := range seen {
				if strings.HasPrefix(path, prefix) && len(path) > len(prefix) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("goober %q skill package %q has no bundled files", goober.Name, skill)
			}
		}
	}
	return nil
}

func validateCompanionContent(path string, data []byte) error {
	if !utf8.Valid(data) {
		return invalidCompanionFile(path, "must be valid UTF-8 text")
	}
	for _, b := range data {
		if b == 0 || (b < 0x20 && b != '\n' && b != '\r' && b != '\t') {
			return invalidCompanionFile(path, "contains binary control bytes")
		}
	}
	if reason := credentialContentReason(data); reason != "" {
		return invalidCompanionFile(path, reason)
	}
	text := string(data)
	if windowsLocalPath.MatchString(text) || unixLocalPath.MatchString(text) {
		return invalidCompanionFile(path, "contains a host-local absolute path")
	}
	return nil
}

func validateStructuredCredentials(definition apiv1.GaggleBundleDefinition) error {
	structured := struct {
		Gaggle       apiv1.Gaggle     `json:"gaggle"`
		Workflows    []apiv1.Workflow `json:"workflows"`
		Goobers      []apiv1.Goober   `json:"goobers"`
		Repositories []apiv1.RepoRef  `json:"repositories"`
	}{
		Gaggle:       definition.Gaggle,
		Workflows:    definition.Workflows,
		Goobers:      definition.Goobers,
		Repositories: definition.Repositories,
	}
	data, err := json.Marshal(structured)
	if err != nil {
		return fmt.Errorf("encode structured definition for credential validation: %w", err)
	}
	if reason := credentialContentReason(data); reason != "" {
		return fmt.Errorf("structured definition %s", reason)
	}
	return nil
}

func credentialContentReason(data []byte) string {
	if !bytes.Equal(companionSecretPatterns.Scrub(data), data) {
		return "contains a provider credential or private key"
	}
	text := string(data)
	for _, match := range credentialAssignment.FindAllStringSubmatch(text, -1) {
		if value := firstNonEmptyCapture(match); value != "" && !portablePlaceholder(value) {
			return "contains a credential-like assignment"
		}
	}
	for _, match := range credentialURI.FindAllStringSubmatch(text, -1) {
		if len(match) > 1 && !portablePlaceholder(match[1]) {
			return "contains a credential-bearing URI"
		}
	}
	return ""
}

func firstNonEmptyCapture(match []string) string {
	for _, value := range match[1:] {
		if value != "" {
			return value
		}
	}
	return ""
}

func portablePlaceholder(value string) bool {
	trimmed := strings.Trim(strings.TrimSpace(value), `"'`)
	lower := strings.ToLower(trimmed)
	return (strings.HasPrefix(trimmed, "${") && strings.HasSuffix(trimmed, "}")) ||
		(strings.HasPrefix(trimmed, "{{") && strings.HasSuffix(trimmed, "}}")) ||
		(strings.HasPrefix(trimmed, "<") && strings.HasSuffix(trimmed, ">")) ||
		strings.HasPrefix(lower, "your_") || strings.HasPrefix(lower, "your-") ||
		strings.Contains(lower, "placeholder") || strings.Contains(lower, "replace_me") ||
		strings.Contains(lower, "replace-me")
}

func invalidCompanionFile(path, reason string) error {
	return fmt.Errorf("%w: companion file %q %s", ErrInvalidBundle, path, reason)
}

func isReferencedCompanionPath(goobers []apiv1.Goober, path string) bool {
	for _, goober := range goobers {
		if goober.Spec.Instructions != "" {
			instructions := filepath.ToSlash(filepath.Clean(filepath.Join("goobers", goober.Name, filepath.FromSlash(goober.Spec.Instructions))))
			if instructions == path {
				return true
			}
		}
		for _, skill := range goober.Spec.Skills {
			prefix := "skills/" + skill + "/"
			if strings.HasPrefix(path, prefix) && len(path) > len(prefix) {
				return true
			}
		}
	}
	return false
}

func requireRepositoryAuthorization(cfg *instance.Config, required []apiv1.RepoRef) error {
	configured := map[string]bool{}
	for _, repo := range cfg.Repos {
		configured[repositoryKey(apiv1.RepoRef{
			Provider: apiv1.Provider(repo.Provider), BaseURL: repo.BaseURL, Owner: repo.Owner,
			Project: repo.Project, Name: repo.Name,
		})] = true
	}
	for _, repo := range required {
		if !configured[repositoryKey(repo)] {
			return fmt.Errorf("%w for %s", ErrRepositoryAuthorization, repositoryKey(repo))
		}
	}
	return nil
}

func repositoryKey(repo apiv1.RepoRef) string {
	return strings.Join([]string{string(repo.Provider), repo.BaseURL, repo.Owner, repo.Project, repo.Name}, "|")
}

func materialize(configDir, target string, bundle apiv1.GaggleBundle) error {
	gaggleDir := filepath.Join(configDir, "gaggles", target)
	if _, err := os.Lstat(gaggleDir); err == nil {
		return fmt.Errorf("%w: destination directory %s exists", ErrNameConflict, gaggleDir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect destination gaggle directory: %w", err)
	}
	if err := os.MkdirAll(gaggleDir, 0o755); err != nil {
		return fmt.Errorf("create destination gaggle directory: %w", err)
	}
	gaggle := *bundle.Definition.Gaggle.DeepCopy()
	gaggle.Name = target
	gaggle.Spec.Isolation = apiv1.GaggleIsolation{Namespace: "gaggle-" + target}
	gaggleDocument := struct {
		metav1.TypeMeta   `json:",inline" yaml:",inline"`
		metav1.ObjectMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`
		Spec              apiv1.GaggleSpec `json:"spec" yaml:"spec"`
	}{TypeMeta: gaggle.TypeMeta, ObjectMeta: gaggle.ObjectMeta, Spec: gaggle.Spec}
	if err := writeYAML(filepath.Join(gaggleDir, "gaggle.yaml"), gaggleDocument); err != nil {
		return err
	}
	for _, workflow := range bundle.Definition.Workflows {
		copy := *workflow.DeepCopy()
		copy.Spec.Gaggle = target
		if err := writeYAML(filepath.Join(gaggleDir, "workflows", copy.Name+".yaml"), copy); err != nil {
			return err
		}
	}
	for _, goober := range bundle.Definition.Goobers {
		copy := *goober.DeepCopy()
		copy.Spec.Gaggle = target
		if err := writeYAML(filepath.Join(gaggleDir, "goobers", copy.Name, "goober.yaml"), copy); err != nil {
			return err
		}
	}
	for _, file := range bundle.Definition.Files {
		data, _ := base64.StdEncoding.DecodeString(file.ContentBase64)
		path := filepath.Join(gaggleDir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create imported file directory: %w", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write imported file %s: %w", file.Path, err)
		}
	}
	provenance := struct {
		Source     apiv1.GaggleBundleSource     `json:"source"`
		ExportedAt time.Time                    `json:"exportedAt"`
		ImportedAt time.Time                    `json:"importedAt"`
		Provenance apiv1.GaggleBundleProvenance `json:"provenance"`
	}{
		Source: bundle.Source, ExportedAt: bundle.ExportedAt,
		ImportedAt: time.Now().UTC(), Provenance: bundle.Provenance,
	}
	provenanceData, err := json.MarshalIndent(provenance, "", "  ")
	if err != nil {
		return fmt.Errorf("encode imported bundle provenance: %w", err)
	}
	provenanceData = append(provenanceData, '\n')
	if err := os.WriteFile(filepath.Join(gaggleDir, "bundle-source.json"), provenanceData, 0o644); err != nil {
		return fmt.Errorf("write imported bundle provenance: %w", err)
	}
	manifestPath := filepath.Join(configDir, "manifest.yaml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read destination manifest: %w", err)
	}
	var manifest apiv1.Manifest
	if err := yaml.UnmarshalStrict(raw, &manifest); err != nil {
		return fmt.Errorf("parse destination manifest: %w", err)
	}
	manifest.Spec.Gaggles = append(manifest.Spec.Gaggles, target)
	return writeYAML(manifestPath, manifest)
}

func writeYAML(path string, value any) error {
	data, err := yaml.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s parent: %w", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("configuration path %s is a symlink", path)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("configuration path %s is not a regular file", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if info, infoErr := entry.Info(); infoErr == nil {
			mode = info.Mode().Perm()
		}
		return os.WriteFile(target, data, mode)
	})
}

func portableName(name string) bool {
	if name == "" || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func portableRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, `\`) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return clean == path && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func firstAbsoluteString(value any) (string, string) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", ""
	}
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return "", ""
	}
	var walk func(any, string) (string, string)
	walk = func(current any, path string) (string, string) {
		switch typed := current.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if found, text := walk(typed[key], path+"."+key); found != "" {
					return found, text
				}
			}
		case []any:
			for i, item := range typed {
				if found, text := walk(item, fmt.Sprintf("%s[%d]", path, i)); found != "" {
					return found, text
				}
			}
		case string:
			if absolutePortablePath(typed) {
				return strings.TrimPrefix(path, "."), typed
			}
		}
		return "", ""
	}
	return walk(decoded, "")
}

func absolutePortablePath(value string) bool {
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\\`) {
		return true
	}
	return len(value) >= 3 &&
		((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) &&
		value[1] == ':' && (value[2] == '\\' || value[2] == '/')
}

func reportSummary(report *validate.Report) string {
	if report == nil {
		return "no validation report"
	}
	var issues []string
	for _, issue := range report.Issues {
		if issue.Severity == validate.Error {
			issues = append(issues, issue.String())
			if len(issues) == 3 {
				break
			}
		}
	}
	if len(issues) != 0 {
		return strings.Join(issues, "; ")
	}
	return "no validation errors"
}

// MarshalJSON emits deterministic, indented bundle JSON for CLI/file export.
func MarshalJSON(bundle apiv1.GaggleBundle) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(bundle); err != nil {
		return nil, fmt.Errorf("encode gaggle bundle: %w", err)
	}
	return out.Bytes(), nil
}
