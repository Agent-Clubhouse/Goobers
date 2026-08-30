package environmentresolver

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/agentkit"
	"github.com/goobers/goobers/internal/supportmatrix"
)

var contractRoots = []string{"docs", "api/schemas", "config-examples", "internal/capability", "skills"}

var contractPaths = func() []string {
	bundle, err := agentkit.Build(os.DirFS(filepath.Join("..", "..")), "fixture", "fixture")
	if err != nil {
		panic(err)
	}
	var paths []string
	for _, asset := range bundle.Manifest.Assets {
		relative, ok := strings.CutPrefix(asset.Path, agentkit.ProductRoot+"/")
		if ok && slices.ContainsFunc(contractRoots, func(root string) bool {
			return relative == root || strings.HasPrefix(relative, root+"/")
		}) {
			paths = append(paths, relative)
		}
	}
	return paths
}()

type fixtureDocument struct {
	Scenarios []fixtureScenario `json:"scenarios"`
}

type fixtureScenario struct {
	Name                string                `json:"name"`
	Files               map[string]string     `json:"files"`
	Repositories        []fixtureRepository   `json:"repositories"`
	SourceContractRoots []string              `json:"sourceContractRoots"`
	Toolkit             *fixtureToolkit       `json:"toolkit"`
	PathBinary          string                `json:"pathBinary"`
	CLI                 fixtureCLIOutput      `json:"cli"`
	Provider            fixtureProviderOutput `json:"provider"`
	Runs                []fixtureRun          `json:"runs"`
}

type fixtureRepository struct {
	Root            string                   `json:"root"`
	RemoteURLs      []string                 `json:"remoteUrls"`
	Head            string                   `json:"head"`
	Tags            []string                 `json:"tags"`
	Objects         []string                 `json:"objects"`
	Refs            map[string]fixtureGitRef `json:"refs"`
	TrackedSymlinks []string                 `json:"trackedSymlinks"`
	Identity        string                   `json:"-"`
}

type fixtureGitRef struct {
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

type fixtureToolkit struct {
	ConfigRoot      string `json:"configRoot"`
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	RemoveInventory string `json:"removeInventory"`
}

type fixtureCLIOutput struct {
	Version       string        `json:"version"`
	Commit        string        `json:"commit"`
	DSLMode       string        `json:"dslMode"`
	Config        *configOutput `json:"config"`
	AgentKitCheck string        `json:"agentKitCheck"`
}

type fixtureProviderOutput struct {
	Release    *remoteRelease      `json:"release"`
	ConfigRefs map[string]string   `json:"configRefs"`
	Targets    map[string][]string `json:"targets"`
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
	ActiveConfig       string   `json:"activeConfig"`
	ConfigSource       string   `json:"configSource"`
	ConfigSourceKind   string   `json:"configSourceKind"`
	ConfigSourceRef    string   `json:"configSourceRef"`
	ConfigSourceCommit string   `json:"configSourceCommit"`
	Targets            []string `json:"targets"`
	DiagnosticsContain []string `json:"diagnosticsContain"`
}

type resolverInputs struct {
	start, instance, configSource, binary, source string
	instanceCandidates                            []string
}

type resolverReport struct {
	CurrentRepository  string                  `json:"currentRepository,omitempty"`
	CurrentRole        string                  `json:"currentRole"`
	Executable         executableReport        `json:"executable"`
	BinaryVersion      string                  `json:"binaryVersion,omitempty"`
	BinaryCommit       string                  `json:"binaryCommit,omitempty"`
	DSLVersions        []supportmatrix.Version `json:"dslVersions,omitempty"`
	ConfigSource       string                  `json:"configSource,omitempty"`
	ConfigSourceKind   string                  `json:"configSourceKind,omitempty"`
	ConfigSourceRef    string                  `json:"configSourceRef,omitempty"`
	ConfigSourceCommit string                  `json:"configSourceCommit,omitempty"`
	Instance           string                  `json:"instance,omitempty"`
	ActiveConfig       string                  `json:"activeConfig,omitempty"`
	Contract           contractReport          `json:"contract"`
	Targets            []targetReport          `json:"targets,omitempty"`
	Diagnostics        []string                `json:"diagnostics,omitempty"`
}

type executableReport struct {
	Path, Selection, Provenance string
}

type contractReport struct {
	Kind, Root, Version, Commit, Integrity string
	Locations                              map[string]string
}

type targetReport struct {
	Identity, Branch, Access, Root, Unresolved string
	Guidance, BuildOrCI                        []string
}

type binaryIdentity struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type versionsOutput struct {
	DSLVersions []supportmatrix.Version `json:"dslVersions"`
}

type configOutput struct {
	WorkflowSource *workflowSource `json:"workflowSource"`
	Repos          []configRepo    `json:"repos"`
}

type workflowSource struct {
	Kind, Path, URL, Ref string
}

type configRepo struct {
	Provider string `json:"provider" yaml:"provider"`
	Owner    string `json:"owner" yaml:"owner"`
	Project  string `json:"project" yaml:"project"`
	Name     string `json:"name" yaml:"name"`
	Branch   string `json:"branch" yaml:"branch"`
}

type gaggleConfig struct {
	Spec struct {
		Project         configRepo   `json:"project"`
		AdditionalRepos []configRepo `json:"additionalRepos"`
	} `json:"spec"`
}

type remoteRelease struct {
	Version, Commit, TagObject, AnnotatedCommit string
	TreeObject, ContentObject, Content          string
	Objects                                     []string
	Files                                       []string
}

type remoteContent struct {
	Path, SHA, Content string
}

type remoteTarget struct {
	Identity string   `json:"identity"`
	Files    []string `json:"files"`
}

type testEnvironment struct {
	root         string
	scenario     fixtureScenario
	repositories []fixtureRepository
	cli          *fakeCLI
	provider     *fakeProvider
}

func TestResolverScriptedFixtures(t *testing.T) {
	const (
		credentialLocator = "RESOLVER_FIXTURE_TOKEN"
		ambientSecret     = "TOP_SECRET_MUST_NOT_APPEAR"
		remoteSecret      = "REMOTE_SECRET_MUST_NOT_APPEAR"
	)
	t.Setenv(credentialLocator, ambientSecret)
	document := loadFixtures(t)
	for _, scenario := range document.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			environment := materializeScenario(t, scenario)
			for _, run := range scenario.Runs {
				t.Run(run.Name, func(t *testing.T) {
					environment.cli.calls = nil
					environment.provider.calls = nil
					report := resolveEnvironment(environment, resolverInputs{
						start:              fixturePath(environment.root, run.Start),
						instance:           fixturePath(environment.root, run.Instance),
						configSource:       fixturePath(environment.root, run.ConfigSource),
						binary:             fixturePath(environment.root, run.Binary),
						source:             fixturePath(environment.root, run.Source),
						instanceCandidates: fixturePaths(environment.root, run.InstanceCandidates),
					})
					assertReport(t, environment.root, run.Want, report)
					assertReadOnly(t, environment.cli.calls, environment.provider.calls)
					data, err := json.Marshal(report)
					if err != nil {
						t.Fatal(err)
					}
					for _, secret := range []string{credentialLocator, ambientSecret, remoteSecret, "oauth2:"} {
						if strings.Contains(string(data), secret) {
							t.Fatalf("report contains credential material %q: %s", secret, data)
						}
					}
				})
			}
		})
	}
}

func TestResolverFixturesCoverAcceptanceMatrix(t *testing.T) {
	document := loadFixtures(t)
	scenarios, runs := map[string]bool{}, map[string]bool{}
	for _, scenario := range document.Scenarios {
		scenarios[scenario.Name] = true
		for _, run := range scenario.Runs {
			runs[run.Name] = true
		}
	}
	for _, name := range []string{
		"config repository", "initialized instance", "target checkout",
		"Goobers source checkout", "unrelated parent", "manual manifest verification",
		"refuse toolkit and remote mismatch", "reject matching tag with conflicting commit",
		"reject tracked contract symlink", "reject ambiguous abbreviated commit",
		"local Git config source", "remote Git config source",
		"remote Git config source without provider access", "default instance config source",
		"reject installed toolkit DSL mismatch", "reject incomplete contract inventory",
	} {
		if !runs[name] {
			t.Errorf("fixture suite is missing run %q", name)
		}
	}
	for _, name := range []string{
		"starting directories and multiple targets", "matching remote release",
		"matching annotated remote release", "ambiguous instance", "intact toolkit without binary",
		"known version mismatch", "conflicting local release identities",
		"tracked symlink local source", "ambiguous abbreviated commit", "local Git config source",
		"remote Git config source", "remote Git config source without provider access",
		"default instance config source", "installed toolkit DSL mismatch",
		"incomplete contract inventory",
	} {
		if !scenarios[name] {
			t.Errorf("fixture suite is missing scenario %q", name)
		}
	}
}

func TestResolverSkillMatchesFixtureContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "skills", "goobers-environment-resolver", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	skill := string(data)
	assertInOrder(t, skill,
		"### 1. Record the environment locations", "version --json", "versions --json",
		"config show --json", "### 2. Establish an exact release identity",
		"### 3. Select and verify one contract source", "#### Matching local source checkout",
		"#### Matching installed toolkit", "#### Matching remote release ref",
		"### 4. Retain target-repository evidence", "## Required report",
	)
	for _, directive := range []string{
		"entire sanitized provider key", "`dslVersions[]`", "`agent-kit check` has no JSON mode",
		"an exact tag match never overrides", "Resolve an abbreviated commit", "reject mode `120000`",
		"Never query or link to `main`", "`spec.additionalRepos`", "capture both stdout and stderr",
		"`kind: git` with `path`", "`kind: git` with `url`",
		"`git/<lowercase-host[:port]>/<repository-path>`", "`<instance>/config` is both the",
	} {
		if !strings.Contains(skill, directive) {
			t.Errorf("skill is missing fixture-backed directive %q", directive)
		}
	}
}

func TestRepositoryIdentitySanitizesCapturedRemotes(t *testing.T) {
	for remote, want := range map[string]string{
		"https://token@github.com/Acme/Web.git":        "github/acme/web",
		"git@github.com:Acme/Web.git":                  "github/acme/web",
		"https://dev.azure.com/Acme/Platform/_git/Web": "ado/acme/platform/web",
		"git@ssh.dev.azure.com:v3/Acme/Platform/Web":   "ado/acme/platform/web",
	} {
		if got, ok := repositoryIdentity([]string{remote}); !ok || got != want {
			t.Errorf("identity for credential-bearing remote = %q, %t; want %q", got, ok, want)
		}
	}
	if _, ok := repositoryIdentity([]string{
		"https://github.com/acme/one.git",
		"https://github.com/acme/two.git",
	}); ok {
		t.Fatal("competing remote identities were accepted")
	}
}

func TestContractInventoryIncludesCompleteTrees(t *testing.T) {
	for _, path := range []string{"api/schemas/gaggle.schema.json", "api/schemas/goober.schema.json", "skills/goobers-dsl-author/references/dsl-reference.md"} {
		if !slices.Contains(contractPaths, path) {
			t.Errorf("contract inventory is missing %q", path)
		}
	}
}

func TestRemoteReleaseRejectsMismatchedContentObject(t *testing.T) {
	scenario := loadFixtures(t).Scenarios[1]
	environment := materializeScenario(t, scenario)
	identity := binaryIdentity{Version: scenario.CLI.Version, Commit: scenario.CLI.Commit}
	files := environment.provider.output.Release.Files
	environment.provider.output.Release.Files = files[1:]
	if _, ok := verifyRemoteRelease(environment.provider, identity); ok {
		t.Fatal("remote release with incomplete contract inventory was accepted")
	}
	environment.provider.output.Release.Files = files
	environment.provider.output.Release.ContentObject = strings.Repeat("d", 40)
	if _, ok := verifyRemoteRelease(environment.provider, identity); ok {
		t.Fatal("remote release accepted content from a different Git object")
	}
}

func TestVerifyToolkitRejectsSymlinkedTrustRoots(t *testing.T) {
	root := t.TempDir()
	writeToolkit(t, root, fixtureToolkit{
		ConfigRoot: "external", Version: "v1.2.3", Commit: strings.Repeat("a", 40),
	})
	for _, test := range []struct{ name, target, link string }{
		{"toolkit root", "external/.goobers/agent-toolkit", ".goobers/agent-toolkit"},
		{"intermediate component", "external/.goobers", ".goobers"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := filepath.Join(root, strings.ReplaceAll(test.name, " ", "-"))
			link := filepath.Join(config, filepath.FromSlash(test.link))
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(fixturePath(root, test.target), link); err != nil {
				t.Skipf("symlinks unsupported: %v", err)
			}
			if _, ok := verifyToolkit(config, binaryIdentity{}, nil, false); ok {
				t.Fatal("toolkit outside the config trust root was accepted through a symlink")
			}
		})
	}
}

func resolveEnvironment(environment *testEnvironment, inputs resolverInputs) resolverReport {
	report := resolverReport{
		CurrentRole: "unresolved",
		Executable:  executableReport{Provenance: "unresolved"},
		Contract:    contractReport{Kind: "unresolved"},
	}
	current := environment.repositoryForPath(inputs.start)
	if current != nil {
		report.CurrentRepository = current.Root
	}
	source := inputs.source
	if source == "" && current != nil && isGoobersSource(current.Root) {
		source = current.Root
	}
	report.Instance, report.Diagnostics = selectInstance(inputs)
	if report.Instance != "" {
		report.ActiveConfig = filepath.Join(report.Instance, "config")
	}
	configExplicit := inputs.configSource != ""
	report.ConfigSource = inputs.configSource
	if report.ConfigSource == "" && current != nil && isConfigSource(current.Root) {
		report.ConfigSource = current.Root
	}
	if report.ConfigSource != "" {
		report.ConfigSourceKind = "local-dir"
	}

	report.Executable.Path, report.Executable.Selection = selectBinary(inputs.binary, source, environment)
	var identity binaryIdentity
	if report.Executable.Path == "" {
		report.Diagnostics = append(report.Diagnostics, "executable unresolved")
	} else {
		versionData, err := environment.cli.run("version --json")
		if err != nil || json.Unmarshal(versionData, &identity) != nil {
			report.Diagnostics = append(report.Diagnostics, "binary identity unresolved")
		} else {
			report.BinaryVersion, report.BinaryCommit = identity.Version, identity.Commit
		}
		versionsData, err := environment.cli.run("versions --json")
		var versions versionsOutput
		if err != nil || json.Unmarshal(versionsData, &versions) != nil {
			report.Diagnostics = append(report.Diagnostics, "DSL support unresolved")
		} else {
			report.DSLVersions = versions.DSLVersions
		}
	}

	var effective configOutput
	if report.Instance != "" && report.Executable.Path != "" {
		configData, err := environment.cli.run("config show --json")
		if err != nil || json.Unmarshal(configData, &effective) != nil {
			report.Diagnostics = append(report.Diagnostics, "instance config unresolved")
		} else if !configExplicit {
			resolveConfigSource(environment, effective.WorkflowSource, &report)
		}
	}
	localConfig := report.ConfigSource
	if report.ConfigSourceKind == "git" && !filepath.IsAbs(localConfig) {
		localConfig = ""
	}
	report.Contract = selectContract(environment, source, localConfig, identity, report.DSLVersions)
	switch {
	case report.Executable.Path == "":
		if report.Contract.Kind == "installed-toolkit" {
			report.Diagnostics = append(report.Diagnostics, "ready without binary")
		}
	case report.Contract.Kind == "local-source" &&
		filepath.Clean(report.Executable.Path) == filepath.Join(source, "bin", "goobers"):
		report.Executable.Provenance = "source-built"
	case report.Contract.Kind == "installed-toolkit" || report.Contract.Kind == "remote-release":
		report.Executable.Provenance = "installed-release"
	case report.Executable.Selection == "PATH":
		report.Executable.Provenance = "PATH-only"
	}
	structuredConfig := localConfig
	if isConfigSource(report.ActiveConfig) {
		structuredConfig = report.ActiveConfig
	}
	report.Targets = resolveTargets(environment, effective.Repos, structuredConfig)
	report.CurrentRole = classify(inputs.start, current, report.Instance, localConfig, source, report.Targets)
	if report.Contract.Kind == "unresolved" && knownIdentity(identity) {
		report.Diagnostics = append(report.Diagnostics, "known release has no exact verified contract source")
	}
	sort.Strings(report.Diagnostics)
	return report
}

func resolveConfigSource(environment *testEnvironment, source *workflowSource, report *resolverReport) {
	if source == nil {
		report.ConfigSource, report.ConfigSourceKind = report.ActiveConfig, "local-dir"
		return
	}
	switch source.Kind {
	case "local-dir":
		if source.Path != "" && filepath.IsAbs(source.Path) && source.URL == "" && source.Ref == "" {
			report.ConfigSource, report.ConfigSourceKind = filepath.Clean(source.Path), source.Kind
			return
		}
	case "git":
		if (source.Path == "") == (source.URL == "") {
			break
		}
		report.ConfigSourceKind, report.ConfigSourceRef = source.Kind, source.Ref
		if report.ConfigSourceRef == "" {
			report.ConfigSourceRef = "main"
		}
		if source.Path != "" {
			if filepath.IsAbs(source.Path) {
				repository := environment.repositoryAt(source.Path)
				ref, ok := repositoryRef(repository, report.ConfigSourceRef)
				if ok {
					report.ConfigSource = filepath.Clean(source.Path)
					report.ConfigSourceCommit = ref
					return
				}
			}
		} else if identity, ok := remoteConfigIdentity(source.URL); ok {
			report.ConfigSource = identity
			if commit, ok := environment.provider.configRef(identity, report.ConfigSourceRef); ok {
				report.ConfigSourceCommit = commit
				return
			}
			report.Diagnostics = append(report.Diagnostics, "config source commit unresolved: ref is inaccessible")
			return
		}
	}
	report.ConfigSource, report.ConfigSourceKind, report.ConfigSourceRef = "", "", ""
	report.Diagnostics = append(report.Diagnostics, "config source unresolved")
}

func selectInstance(inputs resolverInputs) (string, []string) {
	if inputs.instance != "" {
		if isInstance(inputs.instance) {
			return inputs.instance, nil
		}
		return "", []string{"explicit instance is missing required markers"}
	}
	var candidates []string
	for _, candidate := range inputs.instanceCandidates {
		if isInstance(candidate) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) > 1 {
		return "", []string{"ambiguous instance"}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	for current := filepath.Clean(inputs.start); ; current = filepath.Dir(current) {
		if isInstance(current) {
			return current, nil
		}
		if filepath.Dir(current) == current {
			return "", nil
		}
	}
}

func selectBinary(explicit, source string, environment *testEnvironment) (string, string) {
	if explicit != "" {
		return explicit, "explicit"
	}
	candidate := filepath.Join(source, "bin", "goobers")
	if source != "" && executable(candidate) {
		return candidate, "source bin/goobers"
	}
	if environment.scenario.PathBinary != "" {
		return fixturePath(environment.root, environment.scenario.PathBinary), "PATH"
	}
	return "", ""
}

func selectContract(
	environment *testEnvironment,
	source, config string,
	identity binaryIdentity,
	dsl []supportmatrix.Version,
) contractReport {
	if contract, ok := verifyLocalSource(environment, source, identity); ok {
		return contract
	}
	toolkitCheckPassed := true
	if _, available := environment.cli.outputs["agent-kit check"]; available {
		output, err := environment.cli.run("agent-kit check")
		toolkitCheckPassed = err == nil && agentKitCheckCurrent(output, identity)
	}
	if toolkitCheckPassed {
		if contract, ok := verifyToolkit(config, identity, dsl, environment.ExecutableAvailable()); ok {
			return contract
		}
	}
	if contract, ok := verifyRemoteRelease(environment.provider, identity); ok {
		return contract
	}
	return contractReport{Kind: "unresolved"}
}

func verifyLocalSource(
	environment *testEnvironment,
	root string,
	identity binaryIdentity,
) (contractReport, bool) {
	repository := environment.repositoryAt(root)
	if repository == nil || !hasContractFiles(root) || hasTrackedSymlink(repository) {
		return contractReport{}, false
	}
	objects := append(append([]string(nil), repository.Objects...), repository.Head)
	if !commitsMatch(repository.Head, identity.Commit, objects) {
		return contractReport{}, false
	}
	if len(repository.Tags) > 0 && !slices.ContainsFunc(repository.Tags, func(tag string) bool {
		return versionsMatch(tag, identity.Version)
	}) {
		return contractReport{}, false
	}
	return newContract("local-source", root, identity.Version, repository.Head), true
}

func verifyToolkit(
	config string,
	identity binaryIdentity,
	dsl []supportmatrix.Version,
	binaryPresent bool,
) (contractReport, bool) {
	if config == "" {
		return contractReport{}, false
	}
	root := filepath.Join(config, agentkit.InstalledRoot)
	if !secureDirectoryUnder(config, root) {
		return contractReport{}, false
	}
	manifestData, _, ok := secureFileUnder(config, filepath.Join(root, "manifest.json"))
	if !ok {
		return contractReport{}, false
	}
	manifest, err := agentkit.DecodeManifest(manifestData)
	if err != nil {
		return contractReport{}, false
	}
	inventory, contents := map[string]bool{}, map[string][]byte{}
	for _, asset := range manifest.Assets {
		installed, err := agentkit.InstalledAssetPath(asset.Path)
		if err != nil || inventory[asset.Path] {
			return contractReport{}, false
		}
		data, info, ok := secureFileUnder(config, filepath.Join(config, filepath.FromSlash(installed)))
		if !ok {
			return contractReport{}, false
		}
		sum := sha256.Sum256(data)
		if fmt.Sprintf("%x", sum) != asset.SHA256 ||
			int64(len(data)) != asset.Size || !modeMatches(info.Mode(), asset.Mode) {
			return contractReport{}, false
		}
		inventory[asset.Path], contents[asset.Path] = true, data
	}
	required := append([]string{"release.json"}, contractPaths...)
	for _, path := range required {
		if !inventory[agentkit.ProductRoot+"/"+path] {
			return contractReport{}, false
		}
	}
	for _, adapter := range manifest.Adapters {
		if !inventory[adapter.Path] {
			return contractReport{}, false
		}
		for _, skill := range adapter.Skills {
			if !inventory[agentkit.ProductRoot+"/skills/"+skill+"/SKILL.md"] {
				return contractReport{}, false
			}
		}
	}
	if !requirementsComplete(contents[agentkit.ProductRoot+"/docs/requirements/README.md"], inventory) {
		return contractReport{}, false
	}
	var release agentkit.Release
	if json.Unmarshal(contents[agentkit.ProductRoot+"/release.json"], &release) != nil ||
		release.Producer != manifest.Producer || !reflect.DeepEqual(release.DSLVersions, manifest.DSLVersions) {
		return contractReport{}, false
	}
	if binaryPresent && (!versionsMatch(release.Producer.Version, identity.Version) ||
		!commitsMatch(release.Producer.Commit, identity.Commit, nil) ||
		!reflect.DeepEqual(release.DSLVersions, dsl)) {
		return contractReport{}, false
	}
	return newContract("installed-toolkit", root, release.Producer.Version, release.Producer.Commit), true
}

var markdownLink = regexp.MustCompile(`\]\(([^)#?]+\.md)(?:#[^)]*)?\)`)

func requirementsComplete(index []byte, inventory map[string]bool) bool {
	matches := markdownLink.FindAllSubmatch(index, -1)
	required := 0
	for _, match := range matches {
		relative := filepath.ToSlash(filepath.Clean(string(match[1])))
		if strings.HasPrefix(relative, "../") {
			continue
		}
		required++
		if !inventory[agentkit.ProductRoot+"/docs/requirements/"+relative] {
			return false
		}
	}
	return required > 0
}

func verifyRemoteRelease(provider *fakeProvider, identity binaryIdentity) (contractReport, bool) {
	release, ok := provider.release(identity.Version)
	if !ok || !versionsMatch(release.Version, identity.Version) {
		return contractReport{}, false
	}
	commit := release.Commit
	if release.TagObject != "" {
		provider.calls = append(provider.calls, "release-tag "+release.TagObject)
		commit = release.AnnotatedCommit
	}
	binaryCommit, ok := resolveObject(identity.Commit, release.Objects)
	if !ok || !commitsMatch(commit, binaryCommit, nil) {
		return contractReport{}, false
	}
	for _, path := range contractPaths {
		if !slices.Contains(release.Files, path) {
			return contractReport{}, false
		}
		content, ok := provider.releaseContent(path, commit)
		if !ok || content.Path != path || content.SHA != release.TreeObject || content.Content == "" {
			return contractReport{}, false
		}
	}
	return newContract("remote-release", "github:Agent-Clubhouse/Goobers", release.Version, commit), true
}

func resolveTargets(
	environment *testEnvironment,
	repositories []configRepo,
	configRoot string,
) []targetReport {
	if configRoot != "" {
		paths, _ := filepath.Glob(filepath.Join(configRoot, "gaggles", "*", "gaggle.yaml"))
		for _, path := range paths {
			data, err := os.ReadFile(path)
			var gaggle gaggleConfig
			if err == nil && yaml.Unmarshal(data, &gaggle) == nil {
				repositories = append(repositories, gaggle.Spec.Project)
				repositories = append(repositories, gaggle.Spec.AdditionalRepos...)
			}
		}
	}
	byIdentity := map[string]configRepo{}
	for _, repository := range repositories {
		if identity := configuredIdentity(repository); identity != "" {
			byIdentity[identity] = repository
		}
	}
	identities := make([]string, 0, len(byIdentity))
	for identity := range byIdentity {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	var targets []targetReport
	for _, identity := range identities {
		target := targetReport{Identity: identity, Branch: byIdentity[identity].Branch}
		if repository := environment.repositoryByIdentity(identity); repository != nil {
			target.Access, target.Root = "local", repository.Root
			target.Guidance, target.BuildOrCI = inspectTarget(repository.Root, nil)
		} else if files, ok := environment.provider.target(identity); ok {
			target.Access = "provider-only"
			target.Guidance, target.BuildOrCI = inspectTarget("", files)
		} else {
			target.Access, target.Unresolved = "unresolved", "target access unavailable"
		}
		targets = append(targets, target)
	}
	return targets
}

func inspectTarget(root string, remoteFiles []string) ([]string, []string) {
	files := append([]string(nil), remoteFiles...)
	if root != "" {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && !strings.Contains(filepath.ToSlash(path), "/.git/") {
				if relative, relErr := filepath.Rel(root, path); relErr == nil {
					files = append(files, filepath.ToSlash(relative))
				}
			}
			return nil
		})
	}
	var guidance, build []string
	for _, path := range files {
		base := filepath.Base(path)
		switch {
		case strings.EqualFold(base, "README.md"), base == "AGENTS.md", base == "CLAUDE.md",
			path == ".github/copilot-instructions.md":
			guidance = append(guidance, path)
		case strings.HasPrefix(path, ".github/workflows/"), base == "Makefile",
			strings.HasPrefix(base, "Taskfile"), base == "Justfile", base == "go.mod",
			base == "package.json":
			build = append(build, path)
		}
	}
	sort.Strings(guidance)
	sort.Strings(build)
	return guidance, build
}

func classify(
	start string,
	current *fixtureRepository,
	instance, config, source string,
	targets []targetReport,
) string {
	switch {
	case pathWithin(start, source):
		return "goobers-source"
	case pathWithin(start, config) && isConfigSource(config):
		return "config-source"
	case pathWithin(start, instance):
		return "instance"
	case current != nil && slices.ContainsFunc(targets, func(target targetReport) bool {
		return target.Identity == current.Identity
	}):
		return "target"
	default:
		return "unresolved"
	}
}

func loadFixtures(t *testing.T) fixtureDocument {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "resolver-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document fixtureDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func materializeScenario(t *testing.T, scenario fixtureScenario) *testEnvironment {
	t.Helper()
	root := t.TempDir()
	for path, data := range scenario.Files {
		writeFixture(t, root, path, strings.ReplaceAll(data, "$ROOT", filepath.ToSlash(root)))
	}
	repositories := append([]fixtureRepository(nil), scenario.Repositories...)
	for index := range repositories {
		repositories[index].Root = fixturePath(root, repositories[index].Root)
		identity, ok := repositoryIdentity(repositories[index].RemoteURLs)
		if len(repositories[index].RemoteURLs) > 0 && !ok {
			t.Fatalf("fixture repository %s has unresolved identity", repositories[index].Root)
		}
		repositories[index].Identity = identity
		writeFixture(t, root, filepath.Join(scenario.Repositories[index].Root, ".git", "HEAD"),
			repositories[index].Head+"\n")
	}
	for _, contractRoot := range scenario.SourceContractRoots {
		for _, path := range contractPaths {
			writeFixture(t, root, filepath.Join(contractRoot, path), "fixture contract: "+path+"\n")
		}
	}
	if scenario.Toolkit != nil {
		writeToolkit(t, root, *scenario.Toolkit)
	}
	for path := range scenario.Files {
		if filepath.Base(path) == "goobers" {
			if err := os.Chmod(fixturePath(root, path), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if scenario.CLI.Config != nil {
		data, err := json.Marshal(scenario.CLI.Config)
		if err != nil {
			t.Fatal(err)
		}
		var expanded configOutput
		if err := json.Unmarshal([]byte(strings.ReplaceAll(string(data), "$ROOT", filepath.ToSlash(root))), &expanded); err != nil {
			t.Fatal(err)
		}
		scenario.CLI.Config = &expanded
	}
	if scenario.Provider.Release != nil && len(scenario.Provider.Release.Files) == 0 {
		scenario.Provider.Release.Files = append([]string(nil), contractPaths...)
	}
	if scenario.Provider.Release != nil && scenario.Provider.Release.TreeObject == "" {
		scenario.Provider.Release.TreeObject = strings.Repeat("c", 40)
		scenario.Provider.Release.ContentObject = strings.Repeat("c", 40)
		scenario.Provider.Release.Content = "fixture contract"
	}
	return &testEnvironment{
		root:         root,
		scenario:     scenario,
		repositories: repositories,
		cli:          newFakeCLI(scenario.CLI),
		provider:     &fakeProvider{output: scenario.Provider},
	}
}

func writeToolkit(t *testing.T, root string, fixture fixtureToolkit) {
	t.Helper()
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := agentkit.Build(os.DirFS(repositoryRoot), fixture.Version, fixture.Commit)
	if err != nil {
		t.Fatal(err)
	}
	configRoot := fixturePath(root, fixture.ConfigRoot)
	for path, file := range bundle.Files {
		writeFixture(t, configRoot, path, string(file.Data))
		if err := os.Chmod(filepath.Join(configRoot, filepath.FromSlash(path)), file.Mode); err != nil {
			t.Fatal(err)
		}
	}
	manifest := bundle.Manifest
	if fixture.RemoveInventory != "" {
		assets := manifest.Assets[:0]
		for _, asset := range manifest.Assets {
			if asset.Path != fixture.RemoveInventory {
				assets = append(assets, asset)
			}
		}
		if len(assets) == len(manifest.Assets) {
			t.Fatalf("fixture inventory does not contain %s", fixture.RemoveInventory)
		}
		manifest.Assets = assets
		installed, err := agentkit.InstalledAssetPath(fixture.RemoveInventory)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(configRoot, filepath.FromSlash(installed))); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, configRoot, agentkit.InstalledManifestPath, string(append(data, '\n')))
}

func writeFixture(t *testing.T, root, relative, data string) {
	t.Helper()
	path := fixturePath(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fakeCLI struct {
	outputs map[string][]byte
	calls   []string
}

func newFakeCLI(output fixtureCLIOutput) *fakeCLI {
	cli := &fakeCLI{outputs: map[string][]byte{}}
	if output.Version != "" {
		cli.outputs["version --json"], _ = json.Marshal(binaryIdentity{
			Version: output.Version,
			Commit:  output.Commit,
		})
		dsl := supportmatrix.GetDSL().Versions()
		if output.DSLMode == "mismatch" {
			dsl = append([]supportmatrix.Version(nil), dsl...)
			// Simulate a live binary whose reported DSL surface diverges from
			// the toolkit's recorded release.json: flip a currently-SUPPORTED
			// version to unsupported. Flipping dsl[0] no longer works — in
			// Versions()' ascending order dsl[0] is 1.4, which now ships
			// unsupported (issue #3507), so re-marking it unsupported is a
			// no-op that leaves dsl equal to the recorded surface and produces
			// no mismatch for verifyToolkit to detect.
			for i := range dsl {
				if dsl[i].Level == supportmatrix.LevelSupported {
					dsl[i].Level = supportmatrix.LevelUnsupported
					break
				}
			}
		}
		cli.outputs["versions --json"], _ = json.Marshal(versionsOutput{DSLVersions: dsl})
	}
	if output.Config != nil {
		cli.outputs["config show --json"], _ = json.Marshal(output.Config)
	}
	if output.AgentKitCheck != "" {
		cli.outputs["agent-kit check"] = []byte(output.AgentKitCheck)
	}
	return cli
}

func (f *fakeCLI) run(command string) ([]byte, error) {
	f.calls = append(f.calls, command)
	output, ok := f.outputs[command]
	if !ok {
		return nil, fmt.Errorf("fake CLI has no output for %q", command)
	}
	return output, nil
}

type fakeProvider struct {
	output fixtureProviderOutput
	calls  []string
}

func (f *fakeProvider) release(version string) (remoteRelease, bool) {
	f.calls = append(f.calls, "release-ref "+version)
	returnValue := f.output.Release
	return dereference(returnValue), returnValue != nil
}

func (f *fakeProvider) configRef(identity, ref string) (string, bool) {
	f.calls = append(f.calls, "config-ref "+identity+" "+ref)
	commit, ok := f.output.ConfigRefs[identity+"@"+ref]
	return commit, ok && isFullObject(commit)
}

func (f *fakeProvider) releaseContent(path, commit string) (remoteContent, bool) {
	f.calls = append(f.calls, "release-content "+commit+" "+path)
	release := f.output.Release
	if release == nil {
		return remoteContent{}, false
	}
	return remoteContent{Path: path, SHA: release.ContentObject, Content: release.Content},
		isFullObject(release.TreeObject) && isFullObject(release.ContentObject)
}

func (f *fakeProvider) target(identity string) ([]string, bool) {
	f.calls = append(f.calls, "target "+identity)
	files, ok := f.output.Targets[identity]
	data, _ := json.Marshal(remoteTarget{Identity: identity, Files: files})
	var decoded remoteTarget
	return decoded.Files, ok && json.Unmarshal(data, &decoded) == nil && decoded.Identity == identity
}

func (environment *testEnvironment) ExecutableAvailable() bool {
	return len(environment.cli.outputs["version --json"]) > 0
}

func (environment *testEnvironment) repositoryForPath(path string) *fixtureRepository {
	var selected *fixtureRepository
	for index := range environment.repositories {
		repository := &environment.repositories[index]
		if pathWithin(path, repository.Root) &&
			(selected == nil || len(repository.Root) > len(selected.Root)) {
			selected = repository
		}
	}
	return selected
}

func (environment *testEnvironment) repositoryAt(root string) *fixtureRepository {
	for index := range environment.repositories {
		if filepath.Clean(environment.repositories[index].Root) == filepath.Clean(root) {
			return &environment.repositories[index]
		}
	}
	return nil
}

func (environment *testEnvironment) repositoryByIdentity(identity string) *fixtureRepository {
	for index := range environment.repositories {
		if environment.repositories[index].Identity == identity {
			return &environment.repositories[index]
		}
	}
	return nil
}

func repositoryIdentity(remotes []string) (string, bool) {
	identities := map[string]bool{}
	for _, remote := range remotes {
		if identity, ok := providerIdentity(remote); ok {
			identities[identity] = true
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

func providerIdentity(remote string) (string, bool) {
	value := strings.TrimSpace(remote)
	var host, path string
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", false
		}
		host, path = parsed.Hostname(), parsed.Path
	} else {
		before, after, ok := strings.Cut(value, ":")
		if !ok {
			return "", false
		}
		_, host, _ = strings.Cut(before, "@")
		path = after
	}
	host = strings.ToLower(host)
	parts := strings.Split(strings.Trim(strings.TrimSuffix(path, ".git"), "/"), "/")
	switch {
	case host == "github.com" && len(parts) == 2:
		return strings.ToLower("github/" + parts[0] + "/" + parts[1]), true
	case host == "dev.azure.com" && len(parts) == 4 && strings.EqualFold(parts[2], "_git"):
		return strings.ToLower("ado/" + parts[0] + "/" + parts[1] + "/" + parts[3]), true
	case host == "ssh.dev.azure.com" && len(parts) == 4 && strings.EqualFold(parts[0], "v3"):
		return strings.ToLower("ado/" + parts[1] + "/" + parts[2] + "/" + parts[3]), true
	default:
		return "", false
	}
}

func remoteConfigIdentity(rawURL string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	if identity, ok := providerIdentity(rawURL); ok {
		return identity, true
	}
	path := strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/")
	if path == "" || strings.Contains(path, "..") {
		return "", false
	}
	return "git/" + strings.ToLower(parsed.Host) + "/" + path, true
}

func repositoryRef(repository *fixtureRepository, name string) (string, bool) {
	if repository == nil {
		return "", false
	}
	ref, ok := repository.Refs[name]
	return strings.ToLower(ref.SHA), ok && ref.Type == "commit" && isFullObject(ref.SHA)
}

func configuredIdentity(repository configRepo) string {
	provider := strings.ToLower(strings.TrimSpace(repository.Provider))
	owner := strings.ToLower(strings.TrimSpace(repository.Owner))
	project := strings.ToLower(strings.TrimSpace(repository.Project))
	name := strings.ToLower(strings.TrimSpace(repository.Name))
	switch {
	case provider == "github" && owner != "" && name != "" && project == "":
		return strings.Join([]string{provider, owner, name}, "/")
	case provider == "ado" && owner != "" && project != "" && name != "":
		return strings.Join([]string{provider, owner, project, name}, "/")
	default:
		return ""
	}
}

func commitsMatch(left, right string, objects []string) bool {
	left, leftOK := resolveObject(left, objects)
	right, rightOK := resolveObject(right, objects)
	return leftOK && rightOK && left == right
}

func resolveObject(value string, objects []string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) == 40 && isHex(value) {
		return value, true
	}
	if len(value) < 4 || !isHex(value) {
		return "", false
	}
	var matches []string
	for _, object := range objects {
		object = strings.ToLower(object)
		if isFullObject(object) && strings.HasPrefix(object, value) && !slices.Contains(matches, object) {
			matches = append(matches, object)
		}
	}
	if len(matches) != 1 {
		return "", false
	}
	return matches[0], true
}

func isFullObject(value string) bool {
	return len(value) == 40 && isHex(value)
}

func isHex(value string) bool {
	return !strings.ContainsFunc(value, func(character rune) bool {
		return !strings.ContainsRune("0123456789abcdef", character)
	})
}

func versionsMatch(left, right string) bool {
	left = strings.TrimPrefix(strings.TrimSpace(left), "v")
	right = strings.TrimPrefix(strings.TrimSpace(right), "v")
	return left != "" && left == right
}

func knownIdentity(identity binaryIdentity) bool {
	return identity.Version != "" && identity.Version != "dev" &&
		identity.Commit != "" && identity.Commit != "unknown"
}

func newContract(kind, root, version, commit string) contractReport {
	locations := make(map[string]string, len(contractPaths))
	for _, path := range contractPaths {
		locations[path] = filepath.Join(root, filepath.FromSlash(path))
	}
	return contractReport{
		Kind: kind, Root: root, Version: version, Commit: commit,
		Integrity: "fixture verified", Locations: locations,
	}
}

func hasTrackedSymlink(repository *fixtureRepository) bool {
	for _, path := range repository.TrackedSymlinks {
		for _, root := range contractRoots {
			if path == root || strings.HasPrefix(path, root+"/") {
				return true
			}
		}
	}
	return false
}

func hasContractFiles(root string) bool {
	for _, path := range contractPaths {
		if _, ok := secureFile(filepath.Join(root, filepath.FromSlash(path))); !ok {
			return false
		}
	}
	return root != ""
}

func secureFile(path string) ([]byte, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false
	}
	data, err := os.ReadFile(path)
	return data, err == nil
}

func secureDirectoryUnder(root, path string) bool {
	info, ok := securePathUnder(root, path)
	return ok && info.IsDir()
}

func secureFileUnder(root, path string) ([]byte, fs.FileInfo, bool) {
	info, ok := securePathUnder(root, path)
	if !ok || !info.Mode().IsRegular() {
		return nil, nil, false
	}
	data, err := os.ReadFile(path)
	return data, info, err == nil
}

func securePathUnder(root, path string) (fs.FileInfo, bool) {
	root, rootErr := filepath.Abs(root)
	path, pathErr := filepath.Abs(path)
	if rootErr != nil || pathErr != nil || !pathWithin(path, root) {
		return nil, false
	}
	var target fs.FileInfo
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || current != path && !info.IsDir() {
			return nil, false
		}
		if current == path {
			target = info
		}
		if current == root {
			break
		}
		if filepath.Dir(current) == current {
			return nil, false
		}
	}
	canonicalRoot, rootErr := filepath.EvalSymlinks(root)
	canonicalPath, pathErr := filepath.EvalSymlinks(path)
	return target, rootErr == nil && pathErr == nil && pathWithin(canonicalPath, canonicalRoot)
}

func isInstance(root string) bool {
	return regular(filepath.Join(root, "instance.yaml")) && directory(filepath.Join(root, "config"))
}

func isConfigSource(root string) bool {
	return regular(filepath.Join(root, "manifest.yaml")) && directory(filepath.Join(root, "gaggles"))
}

func isGoobersSource(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	return err == nil && strings.Contains(string(data), "module github.com/goobers/goobers") &&
		directory(filepath.Join(root, "cmd", "goobers")) &&
		regular(filepath.Join(root, "docs", "ARCHITECTURE.md"))
}

// modeMatches reports whether a file's on-disk mode matches a manifest
// asset's recorded "%04o" mode string. Windows has no POSIX permission
// bits — os.Chmod there only toggles the read-only attribute — so a mode
// captured while building the toolkit bundle (via agentkit.Build, itself
// reading os.Stat on this same platform) can never be read back
// byte-for-byte once it round-trips through a fresh os.Chmod/os.Stat pair.
// Skipping the comparison on Windows mirrors internal/agentkit's
// requiredModeMatches, which short-circuits to true for the identical
// reason (see internal/agentkit/repository.go).
func modeMatches(actual fs.FileMode, want string) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	return fmt.Sprintf("%04o", actual.Perm()) == want
}

func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		// Windows has no POSIX executable bit — os.Chmod on this platform
		// only ever toggles the read-only attribute, so Perm()&0o111 is
		// always 0 regardless of what the fixture intended (mirrors
		// internal/agentkit's requiredModeMatches, which short-circuits the
		// same way for the same reason). A regular file the fixture placed
		// at a "bin/goobers"-shaped path is the strongest signal available
		// on this platform.
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

func regular(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}

func directory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func pathWithin(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
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

func fixturePaths(root string, paths []string) []string {
	result := make([]string, len(paths))
	for index, path := range paths {
		result[index] = fixturePath(root, path)
	}
	return result
}

func dereference[T any](value *T) T {
	if value == nil {
		var zero T
		return zero
	}
	return *value
}

var acceptanceTargetEvidence = map[string]struct {
	guidance, build []string
}{
	"github/acme/web": {
		guidance: []string{"AGENTS.md", "README.md"},
		build:    []string{"package.json"},
	},
	"github/acme/api": {
		guidance: []string{"AGENTS.md", "README.md"},
		build:    []string{".github/workflows/ci.yml", "go.mod"},
	},
}

func assertReport(t *testing.T, root string, want fixtureWant, got resolverReport) {
	t.Helper()
	if got.CurrentRole != want.CurrentRole || got.Executable.Provenance != want.BinaryProvenance ||
		got.Contract.Kind != want.ContractKind {
		t.Errorf("role/provenance/contract = %q/%q/%q, want %q/%q/%q; diagnostics=%v",
			got.CurrentRole, got.Executable.Provenance, got.Contract.Kind,
			want.CurrentRole, want.BinaryProvenance, want.ContractKind, got.Diagnostics)
	}
	assertPath(t, root, "instance", want.Instance, got.Instance)
	wantActiveConfig := want.ActiveConfig
	if wantActiveConfig == "" && want.Instance != "" {
		wantActiveConfig = filepath.Join(want.Instance, "config")
	}
	assertPath(t, root, "active config", wantActiveConfig, got.ActiveConfig)
	assertPath(t, root, "config source", want.ConfigSource, got.ConfigSource)
	wantKind := want.ConfigSourceKind
	if wantKind == "" && want.ConfigSource != "" {
		wantKind = "local-dir"
	}
	if got.ConfigSourceKind != wantKind || got.ConfigSourceRef != want.ConfigSourceRef ||
		got.ConfigSourceCommit != want.ConfigSourceCommit {
		t.Errorf("config evidence = %q/%q/%q, want %q/%q/%q",
			got.ConfigSourceKind, got.ConfigSourceRef, got.ConfigSourceCommit,
			wantKind, want.ConfigSourceRef, want.ConfigSourceCommit)
	}
	if got.Executable.Path != "" &&
		(got.BinaryVersion == "" || got.BinaryCommit == "" || len(got.DSLVersions) == 0) {
		t.Error("selected binary has incomplete version, commit, or DSL evidence")
	}
	if got.Contract.Kind != "unresolved" && len(got.Contract.Locations) != len(contractPaths) {
		t.Errorf("contract locations = %d, want %d", len(got.Contract.Locations), len(contractPaths))
	}
	var identities []string
	for _, target := range got.Targets {
		identities = append(identities, target.Identity)
		if target.Access == "unresolved" || len(target.Guidance) == 0 || len(target.BuildOrCI) == 0 {
			t.Errorf("target evidence is incomplete: %+v", target)
		}
		if want, ok := acceptanceTargetEvidence[target.Identity]; ok &&
			(!slices.Equal(target.Guidance, want.guidance) || !slices.Equal(target.BuildOrCI, want.build)) {
			t.Errorf("target %s guidance/build = %v/%v, want %v/%v",
				target.Identity, target.Guidance, target.BuildOrCI, want.guidance, want.build)
		}
	}

	if !slices.Equal(identities, want.Targets) {
		t.Errorf("targets = %v, want %v", identities, want.Targets)
	}
	for _, text := range want.DiagnosticsContain {
		if !slices.ContainsFunc(got.Diagnostics, func(diagnostic string) bool {
			return strings.Contains(diagnostic, text)
		}) {
			t.Errorf("diagnostics %v do not contain %q", got.Diagnostics, text)
		}
	}
}

func assertPath(t *testing.T, root, name, want, got string) {
	t.Helper()
	want = strings.ReplaceAll(want, "$ROOT", filepath.ToSlash(root))
	if want != "" {
		want = filepath.Clean(filepath.FromSlash(want))
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func assertReadOnly(t *testing.T, cliCalls, providerCalls []string) {
	t.Helper()
	for _, call := range cliCalls {
		if !slices.Contains([]string{
			"version --json", "versions --json", "config show --json", "agent-kit check",
		}, call) {
			t.Errorf("fixture invoked non-read-only CLI command %q", call)
		}
	}
	for _, call := range providerCalls {
		if !strings.HasPrefix(call, "release-ref ") && !strings.HasPrefix(call, "release-tag ") &&
			!strings.HasPrefix(call, "release-content ") && !strings.HasPrefix(call, "config-ref ") &&
			!strings.HasPrefix(call, "target ") {
			t.Errorf("fixture invoked non-read-only provider operation %q", call)
		}
		if strings.HasPrefix(call, "release-") && strings.Contains(call, " main") {
			t.Errorf("known release fixture fell back to main: %q", call)
		}
	}
}

func assertInOrder(t *testing.T, text string, required ...string) {
	t.Helper()
	offset := 0
	for _, fragment := range required {
		index := strings.Index(text[offset:], fragment)
		if index < 0 {
			t.Fatalf("text does not contain %q after byte %d", fragment, offset)
		}
		offset += index + len(fragment)
	}
}

func parseAgentKitCheck(data []byte) map[string]string {
	values := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		if key, value, ok := strings.Cut(scanner.Text(), ":"); ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return values
}

func agentKitCheckCurrent(data []byte, identity binaryIdentity) bool {
	values := parseAgentKitCheck(data)
	return values["state"] == "current" &&
		versionsMatch(values["source binary version"], identity.Version) &&
		commitsMatch(values["source binary commit"], identity.Commit, nil) &&
		versionsMatch(values["installed source version"], identity.Version) &&
		commitsMatch(values["installed source commit"], identity.Commit, nil) &&
		values["update available"] == "no" &&
		values["modified owned files"] == "none" &&
		values["missing owned files"] == "none"
}
