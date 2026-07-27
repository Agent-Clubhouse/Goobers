package environmentresolver

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/agentkit"
	"github.com/goobers/goobers/internal/supportmatrix"
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
	Root            string                   `json:"root"`
	RemoteURLs      []string                 `json:"remoteUrls"`
	Head            string                   `json:"head"`
	Objects         []string                 `json:"objects"`
	Tags            []string                 `json:"tags"`
	Refs            map[string]fixtureGitRef `json:"refs"`
	TrackedSymlinks []string                 `json:"trackedSymlinks"`
	Identity        string                   `json:"-"`
}

type fixtureGitRef struct {
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

type fixtureToolkit struct {
	ConfigRoot string `json:"configRoot"`
	Version    string `json:"version"`
	Commit     string `json:"commit"`
}

type fixtureProviderOutputs struct {
	Repository      string            `json:"repository"`
	ReleaseRef      string            `json:"releaseRef"`
	ReleaseTags     map[string]string `json:"releaseTags"`
	ReleaseTree     string            `json:"releaseTree"`
	ReleaseContents map[string]string `json:"releaseContents"`
	CommitObjects   []string          `json:"commitObjects"`
	ConfigRefs      map[string]string `json:"configRefs"`
	Targets         map[string]string `json:"targets"`
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
	ConfigSourceKind   string   `json:"configSourceKind"`
	ConfigSourceRef    string   `json:"configSourceRef"`
	ConfigSourceCommit string   `json:"configSourceCommit"`
	Targets            []string `json:"targets"`
	DiagnosticsContain []string `json:"diagnosticsContain"`
}

type dslVersion = supportmatrix.Version

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
		if len(repository.RemoteURLs) > 0 {
			identity, ok := repositoryIdentityFromCapturedRemotes(
				fixtureGitCommandRunner{urls: repository.RemoteURLs},
				repositories[i].Root,
			)
			if !ok {
				t.Fatalf("derive fixture repository identity for %s", repository.Root)
			}
			repositories[i].Identity = identity
		}
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
	providerOutputs.ReleaseTags = cloneExpandedMap(providerOutputs.ReleaseTags, root)
	providerOutputs.ReleaseTree = strings.ReplaceAll(providerOutputs.ReleaseTree, "$ROOT", filepath.ToSlash(root))
	providerOutputs.ReleaseContents = cloneExpandedMap(providerOutputs.ReleaseContents, root)
	providerOutputs.CommitObjects = append([]string(nil), providerOutputs.CommitObjects...)
	providerOutputs.ConfigRefs = cloneExpandedMap(providerOutputs.ConfigRefs, root)
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
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := agentkit.Build(os.DirFS(repositoryRoot), toolkit.Version, toolkit.Commit)
	if err != nil {
		t.Fatalf("build fixture toolkit: %v", err)
	}
	for path, file := range bundle.Files {
		writeFixtureFile(t, configRoot, path, string(file.Data))
		if err := os.Chmod(filepath.Join(configRoot, filepath.FromSlash(path)), file.Mode); err != nil {
			t.Fatalf("set fixture toolkit mode for %s: %v", path, err)
		}
	}
	if _, err := agentkit.DecodeManifest(bundle.ManifestJSON); err != nil {
		t.Fatalf("validate fixture toolkit manifest: %v", err)
	}
	writeFixtureFile(t, configRoot, agentkit.InstalledManifestPath, string(bundle.ManifestJSON))
}

func TestFallbackRejectsSchemaValidIncompleteRequirementInventory(t *testing.T) {
	root := t.TempDir()
	writeFixtureToolkit(t, root, fixtureToolkit{
		ConfigRoot: ".",
		Version:    "v1.2.3",
		Commit:     "abc1230000000000000000000000000000000000",
	})
	if _, _, ok := verifyToolkitManifest(root); !ok {
		t.Fatal("canonical fixture toolkit did not verify")
	}

	manifestPath := filepath.Join(root, agentkit.InstalledManifestPath)
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := agentkit.DecodeManifest(manifestData)
	if err != nil {
		t.Fatal(err)
	}
	const removed = "payload/.goobers/agent-toolkit/docs/requirements/workflow.md"
	assets := manifest.Assets[:0]
	for _, asset := range manifest.Assets {
		if asset.Path != removed {
			assets = append(assets, asset)
		}
	}
	if len(assets) == len(manifest.Assets) {
		t.Fatalf("canonical fixture does not contain %s", removed)
	}
	manifest.Assets = assets
	manifestData, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	manifestData = append(manifestData, '\n')
	if _, err := agentkit.DecodeManifest(manifestData); err != nil {
		t.Fatalf("incomplete fixture must remain schema-valid: %v", err)
	}
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		t.Fatal(err)
	}
	installed, err := agentkit.InstalledAssetPath(removed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(installed))); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := verifyToolkitManifest(root); ok {
		t.Fatal("fallback verifier accepted an incomplete requirements inventory")
	}
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

func (f *fakeProvider) releaseTag(object string) ([]byte, error) {
	f.calls = append(f.calls, "release-tag "+object)
	output, ok := f.outputs.ReleaseTags[object]
	if !ok {
		return nil, fmt.Errorf("fake provider has no annotated tag output for %s", object)
	}
	return []byte(output), nil
}

func (f *fakeProvider) releaseCommit(commit string) ([]byte, error) {
	f.calls = append(f.calls, "release-commit "+commit)
	resolved, ok := resolveGitObjectID(commit, f.outputs.CommitObjects)
	if !ok {
		return nil, fmt.Errorf("fake provider cannot resolve commit %s uniquely", commit)
	}
	return json.Marshal(remoteCommit{SHA: resolved})
}

func (f *fakeProvider) releaseTree(commit string) ([]byte, error) {
	f.calls = append(f.calls, "release-tree "+commit)
	if f.outputs.ReleaseTree == "" {
		return nil, fmt.Errorf("fake provider has no release tree output")
	}
	return []byte(f.outputs.ReleaseTree), nil
}

func (f *fakeProvider) releaseContent(path, commit string) ([]byte, error) {
	f.calls = append(f.calls, "release-content "+commit+" "+path)
	output, ok := f.outputs.ReleaseContents[path]
	if !ok {
		return nil, fmt.Errorf("fake provider has no release content output for %s", path)
	}
	return []byte(output), nil
}

func (f *fakeProvider) configRef(identity, ref string) ([]byte, error) {
	f.calls = append(f.calls, "config-ref "+identity+" "+ref)
	output, ok := f.outputs.ConfigRefs[identity+"@"+ref]
	if !ok {
		return nil, fmt.Errorf("fake provider has no config ref output")
	}
	return []byte(output), nil
}

func (f *fakeProvider) target(identity string) ([]byte, error) {
	f.calls = append(f.calls, "target "+identity)
	output, ok := f.outputs.Targets[identity]
	if !ok {
		return nil, fmt.Errorf("fake provider has no target output for %s", identity)
	}
	return []byte(output), nil
}

type gitCommandRunner interface {
	runGit(root string, arguments ...string) (stdout, stderr []byte, err error)
}

type fixtureGitCommandRunner struct {
	urls   []string
	stderr []byte
}

func (r fixtureGitCommandRunner) runGit(_ string, arguments ...string) ([]byte, []byte, error) {
	switch strings.Join(arguments, " ") {
	case "remote":
		return []byte("origin\n"), r.stderr, nil
	case "remote get-url --all origin":
		return []byte(strings.Join(r.urls, "\n") + "\n"), r.stderr, nil
	default:
		return nil, r.stderr, fmt.Errorf("unsupported fixture Git command")
	}
}

func repositoryIdentityFromCapturedRemotes(runner gitCommandRunner, root string) (string, bool) {
	namesOutput, _, err := runner.runGit(root, "remote")
	if err != nil {
		return "", false
	}
	identities := make(map[string]bool)
	for _, name := range strings.Fields(string(namesOutput)) {
		urlsOutput, _, err := runner.runGit(root, "remote", "get-url", "--all", name)
		if err != nil {
			return "", false
		}
		for _, remoteURL := range strings.Split(strings.TrimSpace(string(urlsOutput)), "\n") {
			if identity, ok := repositoryIdentityFromRemoteURL(remoteURL); ok {
				identities[identity] = true
			}
		}
	}
	if len(identities) != 1 {
		return "", false
	}
	for identity := range identities {
		return identity, true
	}
	return "", false
}

func writeRepositoryIdentity(runner gitCommandRunner, root string, output io.Writer) bool {
	identity, ok := repositoryIdentityFromCapturedRemotes(runner, root)
	if !ok {
		return false
	}
	_, err := fmt.Fprintln(output, identity)
	return err == nil
}

type toolkitProducer = agentkit.Producer
type toolkitRelease = agentkit.Release
type toolkitManifest = agentkit.Manifest
