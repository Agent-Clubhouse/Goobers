package gaggletemplate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/strictyaml"
)

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
var revisionPattern = regexp.MustCompile(`^([a-f0-9]{40}|[a-f0-9]{64})$`)

// Source identifies enrollment independently from the user's deployment repository.
type Source struct {
	SchemaVersion int    `json:"schemaVersion"`
	Repository    string `json:"repository"`
	Ref           string `json:"ref"`
	Directory     string `json:"directory"`
	Gaggle        string `json:"gaggle"`
	TokenEnv      string `json:"tokenEnv,omitempty"`
}

// Lock records accepted source identity and pristine bound content, not user edits.
type Lock struct {
	SchemaVersion int    `json:"schemaVersion"`
	Source        Source `json:"source"`
	Revision      string `json:"revision"`
	Digest        string `json:"digest"`
	Baseline      Tree   `json:"baseline"`
}

// Tracking pairs source settings with their validated, matching lock.
type Tracking struct {
	Source Source
	Lock   Lock
}

// Status is the cached notify-only result of checking an enrolled gaggle.
type Status struct {
	State           string    `json:"state"`
	Installed       string    `json:"installed"`
	Candidate       string    `json:"candidate,omitempty"`
	CandidateDigest string    `json:"candidateDigest,omitempty"`
	CheckedAt       time.Time `json:"checkedAt"`
	LastSuccess     time.Time `json:"lastSuccess"`
	Changes         []string  `json:"changes,omitempty"`
	Conflicts       []string  `json:"conflicts,omitempty"`
	Error           string    `json:"error,omitempty"`
	PendingBackprop bool      `json:"pendingBackprop"`
}

// Validate rejects unsupported enrollment versions, identities and package paths.
func (s Source) Validate() error {
	if s.SchemaVersion != 1 || s.Repository == "" || s.Ref == "" ||
		!safeRelative(s.Directory) || !namePattern.MatchString(s.Gaggle) || len(s.Gaggle) > 63 {
		return errors.New("template source requires schemaVersion: 1, repository, branch ref, relative directory and a valid gaggle name")
	}
	if strings.HasPrefix(s.Ref, "-") || strings.ContainsAny(s.Ref, "\r\n") {
		return errors.New("invalid template ref")
	}
	return nil
}

func metadataPath(root, name string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("gaggle directory %s must not be a link", root)
	}
	dir := filepath.Join(root, MetadataDir)
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("template metadata %s must be a real directory", dir)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// Load returns nil for unenrolled directories and rejects partial or corrupt metadata.
func Load(root string) (*Tracking, error) {
	path, err := metadataPath(root, "source.yaml")
	if err != nil {
		return nil, err
	}
	data, err := readBounded(path, 64<<10)
	if errors.Is(err, fs.ErrNotExist) {
		if _, statErr := os.Lstat(filepath.Dir(path)); errors.Is(statErr, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("incomplete template metadata: %w", err)
	}
	if err != nil {
		return nil, err
	}
	var source Source
	jsonData, err := strictyaml.YAMLToJSON(data)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&source); err != nil {
		return nil, err
	}
	if err := source.Validate(); err != nil {
		return nil, err
	}
	data, err = readBounded(filepath.Join(filepath.Dir(path), "lock.json"), 24<<20)
	if err != nil {
		return nil, err
	}
	var lock Lock
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, err
	}
	if err := lock.Baseline.Validate(); err != nil {
		return nil, err
	}
	if lock.SchemaVersion != 1 || lock.Source != source || !revisionPattern.MatchString(lock.Revision) ||
		lock.Digest != lock.Baseline.Digest() || lock.Baseline["gaggle.yaml"].Data == nil {
		return nil, errors.New("template lock is corrupt or source changed; restore source.yaml and lock.json from the same commit")
	}
	return &Tracking{Source: source, Lock: lock}, nil
}

// Save writes matching source settings and pristine ancestry into a staged gaggle.
func Save(root string, source Source, revision string, base Tree) error {
	if err := source.Validate(); err != nil {
		return err
	}
	if !revisionPattern.MatchString(revision) {
		return errors.New("template revision must be a full commit hash")
	}
	if err := base.Validate(); err != nil {
		return err
	}
	path, err := metadataPath(root, "source.yaml")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := yaml.Marshal(source)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return err
	}
	return writeJSON(filepath.Join(filepath.Dir(path), "lock.json"), Lock{
		SchemaVersion: 1, Source: source, Revision: revision, Digest: base.Digest(), Baseline: base,
	})
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".template-write-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// StatusPath locates runtime status outside the deployed configuration tree.
func StatusPath(instanceRoot, gaggle string) string {
	return filepath.Join(instanceRoot, "template-status", gaggle+".json")
}

// WriteStatus replaces one gaggle's cached check result atomically.
func WriteStatus(instanceRoot, gaggle string, status Status) error {
	if !namePattern.MatchString(gaggle) {
		return errors.New("invalid gaggle name")
	}
	path := StatusPath(instanceRoot, gaggle)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return writeJSON(path, status)
}

// ReadStatus reads bounded cached state without resolving the template source.
func ReadStatus(instanceRoot, gaggle string) (*Status, error) {
	if !namePattern.MatchString(gaggle) {
		return nil, errors.New("invalid gaggle name")
	}

	data, err := readBounded(StatusPath(instanceRoot, gaggle), 1<<20)
	if err != nil {
		return nil, err
	}
	var result Status
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// InventoryStatus is a filesystem-only read: API requests never fetch templates
// or scan the contents of a gaggle. Check failures remain explicit.
func InventoryStatus(instanceRoot, gaggle string) *Status {
	if !namePattern.MatchString(gaggle) {
		return &Status{State: "unknown", Error: "invalid gaggle name"}
	}
	metadata := filepath.Join(instanceRoot, "config", "gaggles", gaggle, MetadataDir)
	if _, err := os.Lstat(metadata); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return &Status{State: "unknown", Error: err.Error()}
	}
	status, err := ReadStatus(instanceRoot, gaggle)
	if errors.Is(err, fs.ErrNotExist) {
		return &Status{State: "unknown", Error: "template has not been checked yet"}
	}
	if err != nil {
		return &Status{State: "unknown", Error: err.Error()}
	}
	if info, err := os.Stat(filepath.Join(metadata, "lock.json")); err != nil {
		return &Status{State: "unknown", Error: err.Error()}
	} else if info.ModTime().After(status.CheckedAt) {
		return &Status{State: "unknown", Error: "template configuration changed since the last check"}
	}
	if !status.LastSuccess.IsZero() && time.Since(status.LastSuccess) > 24*time.Hour {
		status.State = "stale"
	}
	return status
}

// RecordDeployment saves the runtime user-source snapshot, not template ancestry.
func RecordDeployment(root string) error {
	tracking, err := Load(root)
	if err != nil || tracking == nil {
		return err
	}
	tree, err := ReadTree(root)
	if err != nil {
		return err
	}
	path, err := metadataPath(root, "deployed.json")
	if err != nil {
		return err
	}
	return writeJSON(path, tree)
}

// Deployment loads the bounded user-source baseline used to preserve runtime edits.
func Deployment(root string) (Tree, error) {
	path, err := metadataPath(root, "deployed.json")
	if err != nil {
		return nil, err
	}
	data, err := readBounded(path, 24<<20)
	if err != nil {
		return nil, fmt.Errorf("template deployment ancestry unavailable; refusing overwrite: %w", err)
	}
	var tree Tree
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, err
	}
	if tree["gaggle.yaml"].Data == nil {
		return nil, errors.New("template deployment ancestry is incomplete; refusing overwrite")
	}
	return tree, tree.Validate()
}

// GuardReplacement leaves legacy gaggles untouched. Managed runtime edits must
// already exist in the candidate source, or deployment is refused.
func GuardReplacement(currentConfig, candidateConfig string) error {
	entries, err := os.ReadDir(filepath.Join(currentConfig, "gaggles"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		current := filepath.Join(currentConfig, "gaggles", entry.Name())
		tracking, err := Load(current)
		if err != nil {
			return err
		}
		if tracking == nil {
			continue
		}
		deployed, err := Deployment(current)
		if err != nil {
			return err
		}
		runtime, err := ReadTree(current)
		if err != nil {
			return err
		}
		if deployed.Digest() == runtime.Digest() {
			continue
		}
		candidate, err := ReadTree(filepath.Join(candidateConfig, "gaggles", entry.Name()))
		if err != nil || !containsEdits(deployed, runtime, candidate) {
			return fmt.Errorf("gaggle %s has unpersisted runtime edits; run config templates backprop before deploying", entry.Name())
		}
	}
	return nil
}

func containsEdits(base, runtime, candidate Tree) bool {
	merged, conflicts, err := Merge(base, runtime, candidate)
	return err == nil && len(conflicts) == 0 && Equivalent(merged, candidate)
}

// RecordDeployments snapshots enrolled gaggles after their source is materialized.
func RecordDeployments(configDir string) error {
	entries, err := os.ReadDir(filepath.Join(configDir, "gaggles"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			if err := RecordDeployment(filepath.Join(configDir, "gaggles", entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
