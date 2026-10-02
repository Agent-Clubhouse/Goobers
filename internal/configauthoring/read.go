// Package configauthoring exposes browser-safe reads over configuration sources.
package configauthoring

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/configsource"
	"github.com/goobers/goobers/internal/configsync"
	"github.com/goobers/goobers/internal/yamldoc"
)

const (
	localSourceID = "source:local"
	apiVersion    = "v1"
)

var (
	ErrSourceNotFound   = errors.New("configuration source not found")
	ErrDocumentNotFound = errors.New("configuration document not found")
)

// Reader is the configuration-source read plane consumed by the HTTP adapter.
type Reader interface {
	Sources(context.Context) (apicontract.ConfigSourcePage, error)
	Documents(context.Context, string) (apicontract.ConfigDocumentPage, error)
	Document(context.Context, string, string) (apicontract.ConfigDocument, error)
}

// LocalReader adapts one canonical local configuration directory without
// exposing its host path.
type LocalReader struct {
	source   configsource.LocalDirSource
	kind     apicontract.ConfigSourceKind
	writable bool
	loader   *configsync.Loader
}

// NewReader constructs an adapter over a resolved source snapshot.
func NewReader(root string, kind apicontract.ConfigSourceKind, writable bool) (*LocalReader, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("configuration source root is required")
	}
	switch kind {
	case apicontract.ConfigSourceLocal, apicontract.ConfigSourceGit, apicontract.ConfigSourceProvider:
	default:
		return nil, fmt.Errorf("unsupported configuration source kind %q", kind)
	}
	loader, err := configsync.NewLoader("")
	if err != nil {
		return nil, err
	}
	return &LocalReader{
		source:   configsource.LocalDirSource{Path: root},
		kind:     kind,
		writable: writable,
		loader:   loader,
	}, nil
}

func (r *LocalReader) Sources(ctx context.Context) (apicontract.ConfigSourcePage, error) {
	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return apicontract.ConfigSourcePage{}, err
	}
	return apicontract.ConfigSourcePage{
		APIVersion:    apiVersion,
		SchemaVersion: apicontract.AuthoringSchemaVersion,
		Items:         []apicontract.ConfigSourceDescriptor{r.descriptor(snapshot.revision)},
	}, nil
}

func (r *LocalReader) Documents(ctx context.Context, sourceID string) (apicontract.ConfigDocumentPage, error) {
	if sourceID != localSourceID {
		return apicontract.ConfigDocumentPage{}, ErrSourceNotFound
	}
	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return apicontract.ConfigDocumentPage{}, err
	}
	items := make([]apicontract.ConfigDocumentDescriptor, 0, len(snapshot.documents))
	for _, document := range snapshot.documents {
		items = append(items, document.descriptor)
	}
	return apicontract.ConfigDocumentPage{
		APIVersion:    apiVersion,
		SchemaVersion: apicontract.AuthoringSchemaVersion,
		SourceID:      localSourceID,
		Revision:      snapshot.revision,
		Items:         items,
	}, nil
}

func (r *LocalReader) Document(ctx context.Context, sourceID, logicalPath string) (apicontract.ConfigDocument, error) {
	if sourceID != localSourceID {
		return apicontract.ConfigDocument{}, ErrSourceNotFound
	}
	if !safeLogicalPath(logicalPath) {
		return apicontract.ConfigDocument{}, ErrDocumentNotFound
	}
	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return apicontract.ConfigDocument{}, err
	}
	index := sort.Search(len(snapshot.documents), func(i int) bool {
		return snapshot.documents[i].descriptor.Path >= logicalPath
	})
	if index == len(snapshot.documents) || snapshot.documents[index].descriptor.Path != logicalPath {
		return apicontract.ConfigDocument{}, ErrDocumentNotFound
	}
	document := snapshot.documents[index]
	return apicontract.ConfigDocument{
		APIVersion:    apiVersion,
		SchemaVersion: apicontract.AuthoringSchemaVersion,
		SourceID:      localSourceID,
		Revision:      snapshot.revision,
		Document:      document.descriptor,
		Content:       string(document.content),
		Diagnostics:   diagnosticsForPath(snapshot.diagnostics, logicalPath),
	}, nil
}

func (r *LocalReader) descriptor(revision string) apicontract.ConfigSourceDescriptor {
	displayName := "Local configuration"
	if r.kind == apicontract.ConfigSourceGit {
		displayName = "Git configuration"
	} else if r.kind == apicontract.ConfigSourceProvider {
		displayName = "Managed configuration"
	}
	return apicontract.ConfigSourceDescriptor{
		ID:          localSourceID,
		DisplayName: displayName,
		Kind:        r.kind,
		Revision:    revision,
		Capabilities: apicontract.ConfigSourceCapabilities{
			Read:        true,
			Validate:    true,
			DirectWrite: r.writable,
		},
	}
}

type sourceDocument struct {
	descriptor apicontract.ConfigDocumentDescriptor
	content    []byte
	staging    string
}

type sourceSnapshot struct {
	revision    string
	documents   []sourceDocument
	diagnostics []apicontract.ConfigDiagnostic
}

func (r *LocalReader) snapshot(ctx context.Context) (sourceSnapshot, error) {
	root, err := r.source.Resolve(ctx)
	if err != nil {
		return sourceSnapshot{}, fmt.Errorf("resolve configuration source: %w", err)
	}
	documents, err := readDocuments(root, r.writable)
	if err != nil {
		return sourceSnapshot{}, err
	}
	revision := digestDocuments(documents)
	stagedRoot, cleanup, err := stageDocuments(documents)
	if err != nil {
		return sourceSnapshot{}, err
	}
	defer cleanup()
	_, report, loadErr := r.loader.LoadSource(ctx, configsource.LocalDirSource{Path: stagedRoot})
	if loadErr != nil && !errors.Is(loadErr, configsync.ErrInvalidConfig) {
		return sourceSnapshot{}, fmt.Errorf("validate configuration source: %w", loadErr)
	}
	return sourceSnapshot{
		revision:    revision,
		documents:   documents,
		diagnostics: projectDiagnostics(report),
	}, nil
}

func stageDocuments(documents []sourceDocument) (string, func(), error) {
	root, err := os.MkdirTemp("", "config-authoring-read-")
	if err != nil {
		return "", func() {}, fmt.Errorf("stage configuration source: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	for _, document := range documents {
		target := filepath.Join(root, filepath.FromSlash(document.staging))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("stage configuration source: %w", err)
		}
		if err := os.WriteFile(target, document.content, 0o600); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("stage configuration source: %w", err)
		}
	}
	return filepath.Join(root, "config"), cleanup, nil
}

func readDocuments(root string, writable bool) ([]sourceDocument, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve configuration root: %w", err)
	}
	var documents []sourceDocument
	if err := appendDocuments(&documents, root, "", "config", writable); err != nil {
		return nil, err
	}
	for _, name := range []string{"goobers", "skills"} {
		shared := filepath.Join(filepath.Dir(root), name)
		info, err := os.Lstat(shared)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect shared configuration source: %w", err)
		}
		if !info.IsDir() {
			continue
		}
		if err := appendDocuments(&documents, shared, name, name, writable); err != nil {
			return nil, err
		}
	}
	sort.Slice(documents, func(i, j int) bool {
		return documents[i].descriptor.Path < documents[j].descriptor.Path
	})
	for i := 1; i < len(documents); i++ {
		if documents[i-1].descriptor.Path == documents[i].descriptor.Path {
			return nil, fmt.Errorf("configuration source contains duplicate logical path %q", documents[i].descriptor.Path)
		}
	}
	return documents, nil
}

func appendDocuments(documents *[]sourceDocument, root, logicalPrefix, stagingPrefix string, writable bool) error {
	confined, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open configuration source: %w", err)
	}
	defer func() { _ = confined.Close() }()
	err = filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == root {
			return nil
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		logicalPath := path.Join(logicalPrefix, filepath.ToSlash(relative))
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if hiddenSegment(logicalPath) || sensitiveSegment(logicalPath) {
				return filepath.SkipDir
			}
			return nil
		}
		if !safeLogicalPath(logicalPath) {
			return nil
		}
		content, err := confined.ReadFile(relative)
		if err != nil {
			return err
		}
		*documents = append(*documents, sourceDocument{
			descriptor: apicontract.ConfigDocumentDescriptor{
				Path:       logicalPath,
				MediaType:  mediaType(logicalPath),
				ETag:       digest(content),
				Editable:   writable,
				Definition: documentReference(logicalPath, content),
			},
			content: content,
			staging: path.Join(stagingPrefix, filepath.ToSlash(relative)),
		})
		return nil
	})
	if err != nil {
		return fmt.Errorf("enumerate configuration source: %w", err)
	}
	return nil
}

func safeLogicalPath(logicalPath string) bool {
	if logicalPath == "" || logicalPath != filepath.ToSlash(logicalPath) ||
		path.IsAbs(logicalPath) || path.Clean(logicalPath) != logicalPath {
		return false
	}
	for _, segment := range strings.Split(logicalPath, "/") {
		if segment == "" || segment == "." || segment == ".." ||
			strings.HasPrefix(segment, ".") || sensitiveSegment(segment) {
			return false
		}
	}
	extension := strings.ToLower(path.Ext(logicalPath))
	if extension != ".yaml" && extension != ".yml" && extension != ".md" {
		return false
	}
	return true
}

func sensitiveSegment(logicalPath string) bool {
	name := strings.ToLower(path.Base(logicalPath))
	for _, marker := range []string{"credential", "password", "secret", "token"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func hiddenSegment(logicalPath string) bool {
	for _, segment := range strings.Split(logicalPath, "/") {
		if strings.HasPrefix(segment, ".") {
			return true
		}
	}
	return false
}

func mediaType(logicalPath string) string {
	if strings.EqualFold(path.Ext(logicalPath), ".md") {
		return "text/markdown"
	}
	return "application/yaml"
}

func digestDocuments(documents []sourceDocument) string {
	hash := sha256.New()
	for _, document := range documents {
		_, _ = hash.Write([]byte(document.descriptor.Path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(document.content)
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func documentReference(logicalPath string, content []byte) *apicontract.ConfigDefinitionReference {
	if reference := definitionReference(content); reference != nil {
		return reference
	}
	if strings.EqualFold(path.Ext(logicalPath), ".md") {
		return &apicontract.ConfigDefinitionReference{
			Kind: apicontract.ConfigDocumentSupport,
			Name: logicalPath,
		}
	}
	return nil
}

func definitionReference(content []byte) *apicontract.ConfigDefinitionReference {
	documents := yamldoc.SplitDocuments(content)
	if len(documents) != 1 || documents[0].Meta.Kind == "" || documents[0].Meta.Name == "" {
		return nil
	}
	kind, ok := definitionKind(documents[0].Meta.Kind)
	if !ok {
		return nil
	}
	var identity struct {
		Spec struct {
			Gaggle string `json:"gaggle"`
		} `json:"spec"`
	}
	_ = yaml.Unmarshal(documents[0].Content, &identity)
	return &apicontract.ConfigDefinitionReference{
		Kind:   kind,
		Name:   documents[0].Meta.Name,
		Gaggle: identity.Spec.Gaggle,
	}
}

func definitionKind(kind string) (apicontract.ConfigDocumentKind, bool) {
	switch kind {
	case "Manifest":
		return apicontract.ConfigDocumentManifest, true
	case "Instance":
		return apicontract.ConfigDocumentInstance, true
	case "Gaggle":
		return apicontract.ConfigDocumentGaggle, true
	case "Workflow":
		return apicontract.ConfigDocumentWorkflow, true
	case "Goober":
		return apicontract.ConfigDocumentGoober, true
	default:
		return "", false
	}
}

func projectDiagnostics(report *validate.Report) []apicontract.ConfigDiagnostic {
	if report == nil {
		return []apicontract.ConfigDiagnostic{}
	}
	diagnostics := make([]apicontract.ConfigDiagnostic, 0, len(report.Issues))
	for _, issue := range report.Issues {
		severity := apicontract.ConfigDiagnosticWarning
		if issue.Severity == validate.Error {
			severity = apicontract.ConfigDiagnosticError
		}
		diagnostic := apicontract.ConfigDiagnostic{
			Code:     string(issue.Code),
			Severity: severity,
			Message:  issue.Message,
			Scope:    issue.Scope(),
		}
		if issue.File != "" {
			logicalPath := filepath.ToSlash(issue.File)
			logicalPath = strings.TrimPrefix(logicalPath, "../")
			diagnostic.Location = &apicontract.ConfigDiagnosticLocation{
				Path:   logicalPath,
				Line:   issue.Line,
				Column: issue.Col,
			}
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	return diagnostics
}

func diagnosticsForPath(diagnostics []apicontract.ConfigDiagnostic, logicalPath string) []apicontract.ConfigDiagnostic {
	filtered := make([]apicontract.ConfigDiagnostic, 0)
	for _, diagnostic := range diagnostics {
		if diagnostic.Location != nil && diagnostic.Location.Path == logicalPath {
			filtered = append(filtered, diagnostic)
		}
	}
	return filtered
}
