package environmentresolver

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var requiredContractPaths = []string{
	"docs/ARCHITECTURE.md",
	"docs/requirements/README.md",
	"api/schemas/workflow.schema.json",
	"config-examples/manifest.yaml",
	"internal/capability/capability.go",
	"skills/goobers-environment-resolver/SKILL.md",
}

type fixtureDocument struct {
	Scenarios []fixtureScenario `json:"scenarios"`
}

type fixtureScenario struct {
	Name                string                 `json:"name"`
	Files               map[string]string      `json:"files"`
	GitRepositories     []fixtureGitRepository `json:"gitRepositories"`
	SourceContractRoots []string               `json:"sourceContractRoots"`
	InstalledToolkit    *fixtureToolkit        `json:"installedToolkit"`
	PathBinary          string                 `json:"pathBinary"`
	CLIOutputs          map[string]string      `json:"cliOutputs"`
	ProviderOutputs     fixtureProviderOutputs `json:"providerOutputs"`
	Runs                []fixtureRun           `json:"runs"`
}

type fixtureGitRepository struct {
	Root     string   `json:"root"`
	Identity string   `json:"identity"`
	Head     string   `json:"head"`
	Tags     []string `json:"tags"`
}

type fixtureToolkit struct {
	ConfigRoot  string       `json:"configRoot"`
	Version     string       `json:"version"`
	Commit      string       `json:"commit"`
	DSLVersions []dslVersion `json:"dslVersions"`
}

type fixtureProviderOutputs struct {
	Repository  string            `json:"repository"`
	ReleaseRef  string            `json:"releaseRef"`
	ReleaseTree string            `json:"releaseTree"`
	Targets     map[string]string `json:"targets"`
}

type fixtureRun struct {
	Name               string      `json:"name"`
	Start              string      `json:"start"`
	Instance           string      `json:"instance"`
	ConfigSource       string      `json:"configSource"`
	Binary             string      `json:"binary"`
	Source             string      `json:"source"`
	InstanceCandidates []string    `json:"instanceCandidates"`
	Want               fixtureWant `json:"want"`
}

type fixtureWant struct {
	CurrentRole        string   `json:"currentRole"`
	BinaryProvenance   string   `json:"binaryProvenance"`
	ContractKind       string   `json:"contractKind"`
	Instance           string   `json:"instance"`
	ConfigSource       string   `json:"configSource"`
	Targets            []string `json:"targets"`
	DiagnosticsContain []string `json:"diagnosticsContain"`
}

type dslVersion struct {
	Version   string `json:"version"`
	Lifecycle string `json:"lifecycle"`
}

type testEnvironment struct {
	root            string
	scenario        fixtureScenario
	gitRepositories []fixtureGitRepository
	cli             *fakeCLI
	provider        *fakeProvider
}

func loadFixtureDocument(t *testing.T) fixtureDocument {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "resolver-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document fixtureDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode resolver fixtures: %v", err)
	}
	return document
}

func materializeScenario(t *testing.T, scenario fixtureScenario) *testEnvironment {
	t.Helper()
	root := t.TempDir()
	for relative, contents := range scenario.Files {
		writeFixtureFile(t, root, relative, strings.ReplaceAll(contents, "$ROOT", filepath.ToSlash(root)))
	}

	repositories := make([]fixtureGitRepository, len(scenario.GitRepositories))
	for i, repository := range scenario.GitRepositories {
		repositories[i] = repository
		repositories[i].Root = fixturePath(root, repository.Root)
		writeFixtureFile(t, root, filepath.Join(repository.Root, ".git", "HEAD"), repository.Head+"\n")
	}
	for _, sourceRoot := range scenario.SourceContractRoots {
		for _, relative := range requiredContractPaths {
			writeFixtureFile(t, root, filepath.Join(sourceRoot, relative), "fixture contract: "+relative+"\n")
		}
	}
	if scenario.InstalledToolkit != nil {
		writeFixtureToolkit(t, root, *scenario.InstalledToolkit)
	}
	for relative := range scenario.Files {
		if filepath.Base(relative) == "goobers" && strings.Contains(filepath.ToSlash(relative), "/bin/") {
			if err := os.Chmod(fixturePath(root, relative), 0o755); err != nil {
				t.Fatalf("make fixture executable %s: %v", relative, err)
			}
		}
	}

	cliOutputs := make(map[string]string, len(scenario.CLIOutputs))
	for command, output := range scenario.CLIOutputs {
		cliOutputs[command] = strings.ReplaceAll(output, "$ROOT", filepath.ToSlash(root))
	}
	providerOutputs := scenario.ProviderOutputs
	providerOutputs.Repository = strings.ReplaceAll(providerOutputs.Repository, "$ROOT", filepath.ToSlash(root))
	providerOutputs.ReleaseRef = strings.ReplaceAll(providerOutputs.ReleaseRef, "$ROOT", filepath.ToSlash(root))
	providerOutputs.ReleaseTree = strings.ReplaceAll(providerOutputs.ReleaseTree, "$ROOT", filepath.ToSlash(root))
	providerOutputs.Targets = cloneExpandedMap(providerOutputs.Targets, root)

	return &testEnvironment{
		root:            root,
		scenario:        scenario,
		gitRepositories: repositories,
		cli:             &fakeCLI{outputs: cliOutputs},
		provider:        &fakeProvider{outputs: providerOutputs},
	}
}

func writeFixtureToolkit(t *testing.T, root string, toolkit fixtureToolkit) {
	t.Helper()
	configRoot := fixturePath(root, toolkit.ConfigRoot)
	productRoot := filepath.Join(configRoot, ".goobers", "agent-toolkit")
	release := toolkitRelease{
		SchemaVersion: "1",
		BundleVersion: "1",
		Producer: toolkitProducer{
			Version: toolkit.Version,
			Commit:  toolkit.Commit,
		},
		DSLVersions: toolkit.DSLVersions,
	}
	releaseJSON, err := json.MarshalIndent(release, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	releaseJSON = append(releaseJSON, '\n')

	assets := map[string][]byte{
		".goobers/agent-toolkit/release.json": releaseJSON,
	}
	for _, relative := range requiredContractPaths {
		assets[filepath.ToSlash(filepath.Join(".goobers", "agent-toolkit", relative))] = []byte("fixture contract: " + relative + "\n")
	}
	paths := make([]string, 0, len(assets))
	for path := range assets {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	manifest := toolkitManifest{
		SchemaVersion: "1",
		BundleVersion: "1",
		Producer:      release.Producer,
		DSLVersions:   release.DSLVersions,
	}
	for _, path := range paths {
		data := assets[path]
		writeFixtureFile(t, configRoot, path, string(data))
		info, err := os.Stat(filepath.Join(configRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("stat fixture toolkit asset %s: %v", path, err)
		}
		sum := sha256.Sum256(data)
		manifest.Assets = append(manifest.Assets, toolkitAsset{
			Path:   "payload/" + path,
			SHA256: fmt.Sprintf("%x", sum),
			Size:   int64(len(data)),
			Mode:   fmt.Sprintf("%04o", info.Mode().Perm()),
		})
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, productRoot, "manifest.json", string(append(manifestJSON, '\n')))
}

func writeFixtureFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := fixturePath(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create fixture directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write fixture file %s: %v", path, err)
	}
}

func fixturePath(root, relative string) string {
	if relative == "" {
		return ""
	}
	if filepath.IsAbs(relative) {
		return filepath.Clean(relative)
	}
	return filepath.Join(root, filepath.FromSlash(relative))
}

func cloneExpandedMap(source map[string]string, root string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = strings.ReplaceAll(value, "$ROOT", filepath.ToSlash(root))
	}
	return cloned
}

type fakeCLI struct {
	outputs map[string]string
	calls   []string
}

func (f *fakeCLI) supports(command string) bool {
	_, ok := f.outputs[command]
	return ok
}

func (f *fakeCLI) run(binary, command string, arguments ...string) ([]byte, error) {
	call := strings.TrimSpace(strings.Join(append([]string{binary, command}, arguments...), " "))
	f.calls = append(f.calls, call)
	output, ok := f.outputs[command]
	if !ok {
		return nil, fmt.Errorf("fake CLI has no output for %q", command)
	}
	return []byte(output), nil
}

type fakeProvider struct {
	outputs fixtureProviderOutputs
	calls   []string
}

func (f *fakeProvider) repository() ([]byte, error) {
	f.calls = append(f.calls, "repository Agent-Clubhouse/Goobers")
	if f.outputs.Repository == "" {
		return nil, fmt.Errorf("fake provider has no canonical repository output")
	}
	return []byte(f.outputs.Repository), nil
}

func (f *fakeProvider) releaseRef(version string) ([]byte, error) {
	f.calls = append(f.calls, "release-ref "+version)
	if f.outputs.ReleaseRef == "" {
		return nil, fmt.Errorf("fake provider has no release ref output")
	}
	return []byte(f.outputs.ReleaseRef), nil
}

func (f *fakeProvider) releaseTree(commit string) ([]byte, error) {
	f.calls = append(f.calls, "release-tree "+commit)
	if f.outputs.ReleaseTree == "" {
		return nil, fmt.Errorf("fake provider has no release tree output")
	}
	return []byte(f.outputs.ReleaseTree), nil
}

func (f *fakeProvider) target(identity string) ([]byte, error) {
	f.calls = append(f.calls, "target "+identity)
	output, ok := f.outputs.Targets[identity]
	if !ok {
		return nil, fmt.Errorf("fake provider has no target output for %s", identity)
	}
	return []byte(output), nil
}

type toolkitProducer struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type toolkitRelease struct {
	SchemaVersion string          `json:"schemaVersion"`
	BundleVersion string          `json:"bundleVersion"`
	Producer      toolkitProducer `json:"producer"`
	DSLVersions   []dslVersion    `json:"dslVersions"`
}

type toolkitManifest struct {
	SchemaVersion string          `json:"schemaVersion"`
	BundleVersion string          `json:"bundleVersion"`
	Producer      toolkitProducer `json:"producer"`
	DSLVersions   []dslVersion    `json:"dslVersions"`
	Assets        []toolkitAsset  `json:"assets"`
}

type toolkitAsset struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode"`
}
