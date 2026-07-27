package environmentresolver

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/agentkit"
	"sigs.k8s.io/yaml"
)

type resolverReport struct {
	CurrentRepository string           `json:"currentRepository"`
	CurrentRole       string           `json:"currentRole"`
	Executable        executableReport `json:"executable"`
	BinaryVersion     string           `json:"binaryVersion,omitempty"`
	BinaryCommit      string           `json:"binaryCommit,omitempty"`
	DSLVersions       []dslVersion     `json:"dslVersions,omitempty"`
	ConfigSource      string           `json:"configSource,omitempty"`
	Instance          string           `json:"instance,omitempty"`
	ActiveConfig      string           `json:"activeConfig,omitempty"`
	Contract          contractReport   `json:"contract"`
	Targets           []targetReport   `json:"targets,omitempty"`
	Diagnostics       []string         `json:"diagnostics,omitempty"`
}

type executableReport struct {
	Path       string `json:"path,omitempty"`
	Selection  string `json:"selection,omitempty"`
	Provenance string `json:"provenance"`
}

type contractReport struct {
	Kind      string            `json:"kind"`
	Root      string            `json:"root,omitempty"`
	Version   string            `json:"version,omitempty"`
	Commit    string            `json:"commit,omitempty"`
	Integrity string            `json:"integrity,omitempty"`
	Locations map[string]string `json:"locations,omitempty"`
}

type targetReport struct {
	Identity   string   `json:"identity"`
	Branch     string   `json:"branch,omitempty"`
	Access     string   `json:"access"`
	Root       string   `json:"root,omitempty"`
	Guidance   []string `json:"guidance,omitempty"`
	BuildOrCI  []string `json:"buildOrCI,omitempty"`
	Unresolved string   `json:"unresolved,omitempty"`
}

type resolverInputs struct {
	start              string
	instance           string
	configSource       string
	binary             string
	source             string
	instanceCandidates []string
}

type binaryIdentity struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type versionsOutput struct {
	DSLVersions []dslVersion `json:"dslVersions"`
}

type configOutput struct {
	WorkflowSource *workflowSource `json:"workflowSource"`
	Repos          []configRepo    `json:"repos"`
}

type workflowSource struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
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

type agentKitCheck struct {
	State                  string
	SourceBinaryVersion    string
	SourceBinaryCommit     string
	InstalledSourceVersion string
	InstalledSourceCommit  string
	UpdateAvailable        string
	ModifiedOwnedFiles     string
	MissingOwnedFiles      string
}

type remoteRepository struct {
	NameWithOwner string `json:"nameWithOwner"`
}

type remoteGitObject struct {
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

type remoteReleaseRef struct {
	Ref    string          `json:"ref"`
	Object remoteGitObject `json:"object"`
}

type remoteAnnotatedTag struct {
	SHA    string          `json:"sha"`
	Object remoteGitObject `json:"object"`
}

type remoteReleaseTree struct {
	SHA       string            `json:"sha"`
	Truncated bool              `json:"truncated"`
	Tree      []remoteTreeEntry `json:"tree"`
}

type remoteTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

type remoteCommit struct {
	SHA string `json:"sha"`
}

type remoteContent struct {
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

type remoteTarget struct {
	Identity string   `json:"identity"`
	Files    []string `json:"files"`
}

func resolveEnvironment(environment *testEnvironment, inputs resolverInputs) resolverReport {
	report := resolverReport{
		CurrentRole: "unresolved",
		Executable: executableReport{
			Provenance: "unresolved",
		},
		Contract: contractReport{Kind: "unresolved"},
	}
	currentRepository := environment.gitRepositoryForPath(inputs.start)
	if currentRepository != nil {
		report.CurrentRepository = currentRepository.Root
	}

	sourceRoot := inputs.source
	if sourceRoot == "" && currentRepository != nil && isGoobersSource(currentRepository.Root) {
		sourceRoot = currentRepository.Root
	}
	instanceRoot, instanceDiagnostic := selectInstance(inputs)
	if instanceDiagnostic != "" {
		report.Diagnostics = append(report.Diagnostics, instanceDiagnostic)
	}
	report.Instance = instanceRoot
	if instanceRoot != "" {
		report.ActiveConfig = filepath.Join(instanceRoot, "config")
	}

	configSource := inputs.configSource
	if configSource == "" && currentRepository != nil && isConfigSource(currentRepository.Root) {
		configSource = currentRepository.Root
	}

	binaryPath, binarySelection := selectBinary(inputs.binary, sourceRoot, environment)
	report.Executable.Path = binaryPath
	report.Executable.Selection = binarySelection
	var identity binaryIdentity
	if binaryPath == "" {
		report.Diagnostics = append(report.Diagnostics, "executable unresolved")
	} else {
		versionJSON, err := environment.cli.run(binaryPath, "version --json")
		if err != nil {
			report.Diagnostics = append(report.Diagnostics, "binary identity unresolved: "+err.Error())
		} else if err := json.Unmarshal(versionJSON, &identity); err != nil {
			report.Diagnostics = append(report.Diagnostics, "binary identity unresolved: invalid version JSON")
		} else {
			report.BinaryVersion = identity.Version
			report.BinaryCommit = identity.Commit
		}
		versionsJSON, err := environment.cli.run(binaryPath, "versions --json")
		if err != nil {
			report.Diagnostics = append(report.Diagnostics, "DSL support unresolved: "+err.Error())
		} else {
			var versions versionsOutput
			if err := json.Unmarshal(versionsJSON, &versions); err != nil {
				report.Diagnostics = append(report.Diagnostics, "DSL support unresolved: invalid versions JSON")
			} else {
				report.DSLVersions = versions.DSLVersions
			}
		}
	}

	var effectiveConfig configOutput
	if instanceRoot != "" && binaryPath != "" {
		configJSON, err := environment.cli.run(binaryPath, "config show --json", instanceRoot)
		if err != nil {
			report.Diagnostics = append(report.Diagnostics, "instance config unresolved: "+err.Error())
		} else if err := json.Unmarshal(configJSON, &effectiveConfig); err != nil {
			report.Diagnostics = append(report.Diagnostics, "instance config unresolved: invalid config JSON")
		} else if configSource == "" && effectiveConfig.WorkflowSource != nil {
			if filepath.IsAbs(effectiveConfig.WorkflowSource.Path) {
				configSource = filepath.Clean(effectiveConfig.WorkflowSource.Path)
			} else {
				report.Diagnostics = append(report.Diagnostics, "config source unresolved: relative workflowSource requires daemon working directory")
			}
		}
	}
	report.ConfigSource = configSource

	report.Contract = selectContract(environment, sourceRoot, configSource, binaryPath, identity, &report.Diagnostics)
	switch {
	case binaryPath == "":
		report.Executable.Provenance = "unresolved"
		if report.Contract.Kind == "installed-toolkit" {
			report.Diagnostics = append(report.Diagnostics, "ready without binary")
		}
	case report.Contract.Kind == "local-source" && filepath.Clean(binaryPath) == filepath.Join(sourceRoot, "bin", "goobers"):
		report.Executable.Provenance = "source-built"
	case report.Contract.Kind == "installed-toolkit" || report.Contract.Kind == "remote-release":
		report.Executable.Provenance = "installed-release"
	case binarySelection == "PATH":
		report.Executable.Provenance = "PATH-only"
	default:
		report.Executable.Provenance = "unresolved"
		report.Diagnostics = append(report.Diagnostics, "source binary provenance unresolved: checkout identity mismatch")
	}

	report.Targets = resolveTargets(environment, effectiveConfig.Repos, configSource, &report.Diagnostics)
	report.CurrentRole = classifyCurrentRole(inputs.start, currentRepository, instanceRoot, configSource, sourceRoot, report.Targets)
	sort.Strings(report.Diagnostics)
	return report
}

func selectInstance(inputs resolverInputs) (string, string) {
	if inputs.instance != "" {
		if isInstance(inputs.instance) {
			return inputs.instance, ""
		}
		return "", "explicit instance is missing required markers"
	}
	var candidates []string
	for _, candidate := range inputs.instanceCandidates {
		if isInstance(candidate) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) > 1 {
		return "", "ambiguous instance: multiple supplied candidates have instance markers"
	}
	if len(candidates) == 1 {
		return candidates[0], ""
	}
	for current := filepath.Clean(inputs.start); ; current = filepath.Dir(current) {
		if isInstance(current) {
			return current, ""
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return "", ""
}

func selectBinary(explicit, sourceRoot string, environment *testEnvironment) (string, string) {
	if explicit != "" {
		return explicit, "explicit"
	}
	if sourceRoot != "" {
		candidate := filepath.Join(sourceRoot, "bin", "goobers")
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, "source bin/goobers"
		}
	}
	if environment.scenario.PathBinary != "" {
		return fixturePath(environment.root, environment.scenario.PathBinary), "PATH"
	}
	return "", ""
}

func selectContract(
	environment *testEnvironment,
	sourceRoot, configSource, binaryPath string,
	identity binaryIdentity,
	diagnostics *[]string,
) contractReport {
	if sourceRoot != "" {
		if contract, ok := verifyLocalSource(environment, sourceRoot, identity); ok {
			return contract
		}
	}
	if configSource != "" {
		contract, exists, matches := verifyInstalledToolkit(environment, configSource, binaryPath, identity)
		if exists && !matches {
			*diagnostics = append(*diagnostics, "installed toolkit identity does not match binary")
		}
		if matches {
			return contract
		}
	}
	if identityIsKnown(identity) {
		if contract, ok := verifyRemoteRelease(environment.provider, identity); ok {
			return contract
		}
		*diagnostics = append(*diagnostics,
			fmt.Sprintf("known release %s has no exact verified contract source", identity.Version))
	}
	return contractReport{Kind: "unresolved"}
}

func verifyLocalSource(environment *testEnvironment, sourceRoot string, identity binaryIdentity) (contractReport, bool) {
	repository := environment.gitRepositoryAt(sourceRoot)
	if repository == nil || !hasContractPaths(sourceRoot) || hasTrackedContractSymlink(repository) {
		return contractReport{}, false
	}
	objects := append(append([]string(nil), repository.Objects...), repository.Head)
	commitMatches := commitsMatch(repository.Head, identity.Commit, objects)
	tagMatches := false
	if identity.Version != "" {
		for _, tag := range repository.Tags {
			if versionsMatch(tag, identity.Version) {
				tagMatches = true
				break
			}
		}
	}
	if !commitMatches || (len(repository.Tags) > 0 && !tagMatches) {
		return contractReport{}, false
	}
	return contractReport{
		Kind:      "local-source",
		Root:      sourceRoot,
		Version:   identity.Version,
		Commit:    repository.Head,
		Integrity: "tracked exact release fixture",
		Locations: contractLocations(sourceRoot),
	}, true
}

func verifyInstalledToolkit(
	environment *testEnvironment,
	configSource, binaryPath string,
	identity binaryIdentity,
) (contractReport, bool, bool) {
	toolkitRoot := filepath.Join(configSource, ".goobers", "agent-toolkit")
	if _, err := os.Stat(filepath.Join(toolkitRoot, "manifest.json")); err != nil {
		return contractReport{}, false, false
	}

	var producer toolkitProducer
	if binaryPath != "" && environment.cli.supports("agent-kit check") {
		checkOutput, err := environment.cli.run(binaryPath, "agent-kit check", configSource)
		if err != nil {
			return contractReport{}, true, false
		}
		check, ok := parseAgentKitCheck(checkOutput)
		if !ok || check.State != "current" || check.UpdateAvailable != "no" ||
			check.ModifiedOwnedFiles != "none" || check.MissingOwnedFiles != "none" {
			return contractReport{}, true, false
		}
		if !versionsMatch(check.SourceBinaryVersion, identity.Version) ||
			!commitsMatch(check.SourceBinaryCommit, identity.Commit, nil) ||
			!versionsMatch(check.InstalledSourceVersion, check.SourceBinaryVersion) ||
			!commitsMatch(check.InstalledSourceCommit, check.SourceBinaryCommit, nil) {
			return contractReport{}, true, false
		}
		producer = toolkitProducer{
			Version: check.InstalledSourceVersion,
			Commit:  check.InstalledSourceCommit,
		}
		release, ok := readToolkitRelease(toolkitRoot)
		if !ok || release.Producer != producer {
			return contractReport{}, true, false
		}
	} else {
		manifest, _, ok := verifyToolkitManifest(configSource)
		if !ok {
			return contractReport{}, true, false
		}
		producer = manifest.Producer
	}
	if binaryPath != "" && (!versionsMatch(producer.Version, identity.Version) ||
		!commitsMatch(producer.Commit, identity.Commit, nil)) {
		return contractReport{}, true, false
	}
	return contractReport{
		Kind:      "installed-toolkit",
		Root:      toolkitRoot,
		Version:   producer.Version,
		Commit:    producer.Commit,
		Integrity: "verified complete inventory",
		Locations: contractLocations(toolkitRoot),
	}, true, true
}

func parseAgentKitCheck(output []byte) (agentKitCheck, bool) {
	var check agentKitCheck
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		label, value, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(label) {
		case "state":
			check.State = value
		case "source binary version":
			check.SourceBinaryVersion = value
		case "source binary commit":
			check.SourceBinaryCommit = value
		case "installed source version":
			check.InstalledSourceVersion = value
		case "installed source commit":
			check.InstalledSourceCommit = value
		case "update available":
			check.UpdateAvailable = value
		case "modified owned files":
			check.ModifiedOwnedFiles = value
		case "missing owned files":
			check.MissingOwnedFiles = value
		}
	}
	if scanner.Err() != nil {
		return agentKitCheck{}, false
	}
	return check, check.State != "" &&
		check.SourceBinaryVersion != "" &&
		check.SourceBinaryCommit != "" &&
		check.InstalledSourceVersion != "" &&
		check.InstalledSourceCommit != "" &&
		check.UpdateAvailable != "" &&
		check.ModifiedOwnedFiles != "" &&
		check.MissingOwnedFiles != ""
}

func verifyToolkitManifest(configSource string) (toolkitManifest, toolkitRelease, bool) {
	toolkitRoot := filepath.Join(configSource, ".goobers", "agent-toolkit")
	manifestPath := filepath.Join(toolkitRoot, "manifest.json")
	if hasSymlinkComponent(configSource, manifestPath) {
		return toolkitManifest{}, toolkitRelease{}, false
	}
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode()&os.ModeSymlink != 0 {
		return toolkitManifest{}, toolkitRelease{}, false
	}
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return toolkitManifest{}, toolkitRelease{}, false
	}
	manifest, err := agentkit.DecodeManifest(manifestData)
	if err != nil {
		return toolkitManifest{}, toolkitRelease{}, false
	}
	inventory := make(map[string]bool, len(manifest.Assets))
	verified := make(map[string][]byte, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		if inventory[asset.Path] || !strings.HasPrefix(asset.Path, "payload/.goobers/agent-toolkit/") ||
			hasUnsafePath(asset.Path) {
			return toolkitManifest{}, toolkitRelease{}, false
		}
		inventory[asset.Path] = true
		installedPath := filepath.Join(configSource, filepath.FromSlash(strings.TrimPrefix(asset.Path, "payload/")))
		if hasSymlinkComponent(configSource, installedPath) {
			return toolkitManifest{}, toolkitRelease{}, false
		}
		info, err := os.Lstat(installedPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return toolkitManifest{}, toolkitRelease{}, false
		}
		data, err := os.ReadFile(installedPath)
		if err != nil {
			return toolkitManifest{}, toolkitRelease{}, false
		}
		sum := sha256.Sum256(data)
		if fmt.Sprintf("%x", sum) != asset.SHA256 || int64(len(data)) != asset.Size ||
			fmt.Sprintf("%04o", info.Mode().Perm()) != asset.Mode {
			return toolkitManifest{}, toolkitRelease{}, false
		}
		verified[asset.Path] = data
	}
	for _, relative := range append([]string{"release.json"}, requiredContractPaths...) {
		path := "payload/.goobers/agent-toolkit/" + relative
		if !inventory[path] {
			return toolkitManifest{}, toolkitRelease{}, false
		}
	}
	requirementsIndex := verified["payload/.goobers/agent-toolkit/docs/requirements/README.md"]
	if !requirementsInventoryComplete(requirementsIndex, inventory) {
		return toolkitManifest{}, toolkitRelease{}, false
	}
	var release toolkitRelease
	releaseData := verified["payload/.goobers/agent-toolkit/release.json"]
	if err := json.Unmarshal(releaseData, &release); err != nil ||
		release.SchemaVersion != agentkit.SchemaVersion ||
		release.BundleVersion != agentkit.BundleVersion ||
		release.Producer != manifest.Producer ||
		!dslVersionsEqual(release.DSLVersions, manifest.DSLVersions) {
		return toolkitManifest{}, toolkitRelease{}, false
	}
	return manifest, release, true
}

var markdownLinkPattern = regexp.MustCompile(`\]\(([^)#?]+\.md)(?:#[^)]*)?\)`)

func requirementsInventoryComplete(index []byte, inventory map[string]bool) bool {
	matches := markdownLinkPattern.FindAllSubmatch(index, -1)
	required := 0
	for _, match := range matches {
		relative := string(match[1])
		if strings.HasPrefix(relative, "../") || strings.Contains(relative, `\`) ||
			filepath.IsAbs(relative) {
			continue
		}
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return false
		}
		required++
		path := "payload/.goobers/agent-toolkit/docs/requirements/" + clean
		if !inventory[path] {
			return false
		}
	}
	return required > 0
}

func readToolkitRelease(toolkitRoot string) (toolkitRelease, bool) {
	data, err := os.ReadFile(filepath.Join(toolkitRoot, "release.json"))
	if err != nil {
		return toolkitRelease{}, false
	}
	var release toolkitRelease
	if err := json.Unmarshal(data, &release); err != nil {
		return toolkitRelease{}, false
	}
	return release, release.SchemaVersion == "1" && release.BundleVersion == "1"
}

func verifyRemoteRelease(provider *fakeProvider, identity binaryIdentity) (contractReport, bool) {
	repositoryJSON, err := provider.repository()
	if err != nil {
		return contractReport{}, false
	}
	var repository remoteRepository
	if err := json.Unmarshal(repositoryJSON, &repository); err != nil ||
		repository.NameWithOwner != "Agent-Clubhouse/Goobers" {
		return contractReport{}, false
	}
	refJSON, err := provider.releaseRef(identity.Version)
	if err != nil {
		return contractReport{}, false
	}
	var ref remoteReleaseRef
	if err := json.Unmarshal(refJSON, &ref); err != nil ||
		!strings.HasPrefix(ref.Ref, "refs/tags/") ||
		!versionsMatch(strings.TrimPrefix(ref.Ref, "refs/tags/"), identity.Version) {
		return contractReport{}, false
	}
	commit, ok := peelRemoteCommit(provider, ref.Object)
	if !ok {
		return contractReport{}, false
	}
	binaryCommit, ok := resolveProviderCommit(provider, identity.Commit)
	if !ok || !commitsMatch(commit, binaryCommit, nil) {
		return contractReport{}, false
	}
	treeJSON, err := provider.releaseTree(commit)
	if err != nil {
		return contractReport{}, false
	}
	var tree remoteReleaseTree
	if err := json.Unmarshal(treeJSON, &tree); err != nil ||
		tree.Truncated || !isFullGitObjectID(tree.SHA) {
		return contractReport{}, false
	}
	entries := make(map[string]remoteTreeEntry, len(tree.Tree))
	for _, entry := range tree.Tree {
		if _, duplicate := entries[entry.Path]; duplicate {
			return contractReport{}, false
		}
		entries[entry.Path] = entry
	}
	locations := make(map[string]string, len(requiredContractPaths))
	for _, path := range requiredContractPaths {
		entry, exists := entries[path]
		if !exists || entry.Type != "blob" || entry.Mode == "120000" || !isFullGitObjectID(entry.SHA) {
			return contractReport{}, false
		}
		contentJSON, err := provider.releaseContent(path, commit)
		if err != nil {
			return contractReport{}, false
		}
		var content remoteContent
		if err := json.Unmarshal(contentJSON, &content); err != nil ||
			content.Path != path || content.SHA != entry.SHA || content.Type != "file" ||
			content.Encoding != "base64" {
			return contractReport{}, false
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
		if err != nil || len(decoded) == 0 {
			return contractReport{}, false
		}
		locations[path] = "github:Agent-Clubhouse/Goobers@" + commit + "/" + path
	}
	return contractReport{
		Kind:      "remote-release",
		Root:      "github:Agent-Clubhouse/Goobers",
		Version:   strings.TrimPrefix(ref.Ref, "refs/tags/"),
		Commit:    commit,
		Integrity: "exact untruncated release tree and pinned contents",
		Locations: locations,
	}, true
}

func peelRemoteCommit(provider *fakeProvider, object remoteGitObject) (string, bool) {
	seen := make(map[string]bool)
	for object.Type == "tag" {
		if !isFullGitObjectID(object.SHA) || seen[object.SHA] {
			return "", false
		}
		seen[object.SHA] = true
		tagJSON, err := provider.releaseTag(object.SHA)
		if err != nil {
			return "", false
		}
		var tag remoteAnnotatedTag
		if err := json.Unmarshal(tagJSON, &tag); err != nil || tag.SHA != object.SHA {
			return "", false
		}
		object = tag.Object
	}
	if object.Type != "commit" || !isFullGitObjectID(object.SHA) {
		return "", false
	}
	return strings.ToLower(object.SHA), true
}

func resolveProviderCommit(provider *fakeProvider, commit string) (string, bool) {
	normalized, ok := normalizeGitObjectID(commit)
	if !ok {
		return "", false
	}
	if len(normalized) == 40 {
		return normalized, true
	}
	commitJSON, err := provider.releaseCommit(normalized)
	if err != nil {
		return "", false
	}
	var resolved remoteCommit
	if err := json.Unmarshal(commitJSON, &resolved); err != nil {
		return "", false
	}
	full, ok := normalizeGitObjectID(resolved.SHA)
	return full, ok && len(full) == 40 && strings.HasPrefix(full, normalized)
}

func resolveTargets(
	environment *testEnvironment,
	instanceRepos []configRepo,
	configSource string,
	diagnostics *[]string,
) []targetReport {
	repositories := append([]configRepo(nil), instanceRepos...)
	if configSource != "" {
		gagglePattern := filepath.Join(configSource, "gaggles", "*", "gaggle.yaml")
		gaggles, _ := filepath.Glob(gagglePattern)
		for _, path := range gaggles {
			data, err := os.ReadFile(path)
			if err != nil {
				*diagnostics = append(*diagnostics, "target config unreadable: "+path)
				continue
			}
			var gaggle gaggleConfig
			if err := yaml.Unmarshal(data, &gaggle); err != nil {
				*diagnostics = append(*diagnostics, "target config invalid: "+path)
				continue
			}
			repositories = append(repositories, gaggle.Spec.Project)
			repositories = append(repositories, gaggle.Spec.AdditionalRepos...)
		}
	}

	byIdentity := make(map[string]configRepo)
	for _, repository := range repositories {
		identity := repositoryIdentity(repository)
		if identity != "" {
			byIdentity[identity] = repository
		}
	}
	identities := make([]string, 0, len(byIdentity))
	for identity := range byIdentity {
		identities = append(identities, identity)
	}
	sort.Strings(identities)

	targets := make([]targetReport, 0, len(identities))
	for _, identity := range identities {
		repository := byIdentity[identity]
		target := targetReport{Identity: identity, Branch: repository.Branch}
		if local := environment.gitRepositoryByIdentity(identity); local != nil {
			target.Access = "local"
			target.Root = local.Root
			target.Guidance, target.BuildOrCI = inspectTargetFiles(local.Root, nil)
		} else {
			output, err := environment.provider.target(identity)
			if err != nil {
				target.Access = "unresolved"
				target.Unresolved = err.Error()
			} else {
				var remote remoteTarget
				if err := json.Unmarshal(output, &remote); err != nil || remote.Identity != identity {
					target.Access = "unresolved"
					target.Unresolved = "provider returned invalid target evidence"
				} else {
					target.Access = "provider-only"
					target.Guidance, target.BuildOrCI = inspectTargetFiles("", remote.Files)
				}
			}
		}
		targets = append(targets, target)
	}
	return targets
}

func inspectTargetFiles(root string, remoteFiles []string) ([]string, []string) {
	files := remoteFiles
	if root != "" {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() || strings.Contains(filepath.ToSlash(path), "/.git/") {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err == nil {
				files = append(files, filepath.ToSlash(relative))
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
		case path == ".github/workflows" || strings.HasPrefix(path, ".github/workflows/"),
			base == "Makefile", strings.HasPrefix(base, "Taskfile"), base == "Justfile",
			base == "go.mod", base == "package.json":
			build = append(build, path)
		}
	}
	sort.Strings(guidance)
	sort.Strings(build)
	return guidance, build
}

func classifyCurrentRole(
	start string,
	currentRepository *fixtureGitRepository,
	instanceRoot, configSource, sourceRoot string,
	targets []targetReport,
) string {
	if sourceRoot != "" && pathWithin(start, sourceRoot) {
		return "goobers-source"
	}
	if configSource != "" && pathWithin(start, configSource) && isConfigSource(configSource) {
		return "config-source"
	}
	if instanceRoot != "" && pathWithin(start, instanceRoot) {
		return "instance"
	}
	if currentRepository != nil {
		for _, target := range targets {
			if target.Identity == currentRepository.Identity {
				return "target"
			}
		}
	}
	return "unresolved"
}

func (environment *testEnvironment) gitRepositoryForPath(path string) *fixtureGitRepository {
	var selected *fixtureGitRepository
	for i := range environment.gitRepositories {
		repository := &environment.gitRepositories[i]
		if pathWithin(path, repository.Root) && (selected == nil || len(repository.Root) > len(selected.Root)) {
			selected = repository
		}
	}
	return selected
}

func (environment *testEnvironment) gitRepositoryAt(root string) *fixtureGitRepository {
	for i := range environment.gitRepositories {
		if filepath.Clean(environment.gitRepositories[i].Root) == filepath.Clean(root) {
			return &environment.gitRepositories[i]
		}
	}
	return nil
}

func (environment *testEnvironment) gitRepositoryByIdentity(identity string) *fixtureGitRepository {
	for i := range environment.gitRepositories {
		if environment.gitRepositories[i].Identity == identity {
			return &environment.gitRepositories[i]
		}
	}
	return nil
}

func isInstance(root string) bool {
	return regularFile(filepath.Join(root, "instance.yaml")) && directory(filepath.Join(root, "config"))
}

func isConfigSource(root string) bool {
	return regularFile(filepath.Join(root, "manifest.yaml")) && directory(filepath.Join(root, "gaggles"))
}

func isGoobersSource(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	return err == nil &&
		strings.Contains(string(data), "module github.com/goobers/goobers") &&
		directory(filepath.Join(root, "cmd", "goobers")) &&
		regularFile(filepath.Join(root, "docs", "ARCHITECTURE.md"))
}

func hasContractPaths(root string) bool {
	for _, relative := range requiredContractPaths {
		if !regularFile(filepath.Join(root, filepath.FromSlash(relative))) {
			return false
		}
	}
	return true
}

func contractLocations(root string) map[string]string {
	locations := make(map[string]string, len(requiredContractPaths))
	for _, relative := range requiredContractPaths {
		locations[relative] = filepath.Join(root, filepath.FromSlash(relative))
	}
	return locations
}

func regularFile(path string) bool {
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

func hasSymlinkComponent(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return true
	}
	current := root
	parts := []string{"."}
	if relative != "." {
		parts = append(parts, strings.Split(relative, string(filepath.Separator))...)
	}
	for _, part := range parts {
		if part != "." {
			current = filepath.Join(current, part)
		}
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

func hasTrackedContractSymlink(repository *fixtureGitRepository) bool {
	for _, tracked := range repository.TrackedSymlinks {
		tracked = filepath.ToSlash(filepath.Clean(filepath.FromSlash(tracked)))
		for _, root := range []string{"docs", "api/schemas", "config-examples", "internal/capability", "skills"} {
			if tracked == root || strings.HasPrefix(tracked, root+"/") {
				return true
			}
		}
	}
	return false
}

func identityIsKnown(identity binaryIdentity) bool {
	version := strings.TrimSpace(strings.ToLower(identity.Version))
	commit := strings.TrimSpace(strings.ToLower(identity.Commit))
	return version != "" && version != "dev" && version != "unknown" &&
		commit != "" && commit != "none" && commit != "unknown"
}

func versionsMatch(left, right string) bool {
	normalize := func(value string) string {
		return strings.TrimPrefix(strings.TrimSpace(value), "v")
	}
	return normalize(left) != "" && normalize(left) == normalize(right)
}

func commitsMatch(left, right string, objects []string) bool {
	left, leftOK := resolveGitObjectID(left, objects)
	right, rightOK := resolveGitObjectID(right, objects)
	return leftOK && rightOK && left == right
}

func resolveGitObjectID(value string, objects []string) (string, bool) {
	value, ok := normalizeGitObjectID(value)
	if !ok {
		return "", false
	}
	if len(value) == 40 {
		return value, true
	}
	if len(value) < 4 {
		return "", false
	}
	matches := make(map[string]bool)
	for _, object := range objects {
		object, ok := normalizeGitObjectID(object)
		if ok && len(object) == 40 && strings.HasPrefix(object, value) {
			matches[object] = true
		}
	}
	if len(matches) != 1 {
		return "", false
	}
	for match := range matches {
		return match, true
	}
	return "", false
}

func isFullGitObjectID(value string) bool {
	value, ok := normalizeGitObjectID(value)
	return ok && len(value) == 40
}

func normalizeGitObjectID(value string) (string, bool) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || len(value) > 40 {
		return "", false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return "", false
		}
	}
	return value, true
}

func repositoryIdentity(repository configRepo) string {
	if repository.Provider == "" || repository.Owner == "" || repository.Name == "" {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(repository.Provider))
	owner := strings.ToLower(strings.TrimSpace(repository.Owner))
	project := strings.ToLower(strings.TrimSpace(repository.Project))
	name := strings.ToLower(strings.TrimSpace(repository.Name))
	switch provider {
	case "github":
		if project != "" {
			return ""
		}
		return strings.Join([]string{provider, owner, name}, "/")
	case "ado":
		if project == "" {
			return ""
		}
		return strings.Join([]string{provider, owner, project, name}, "/")
	default:
		return ""
	}
}

func repositoryIdentityFromRemoteURLs(remoteURLs []string) (string, bool) {
	identities := make(map[string]bool)
	for _, remoteURL := range remoteURLs {
		identity, ok := repositoryIdentityFromRemoteURL(remoteURL)
		if !ok {
			continue
		}
		identities[identity] = true
	}
	if len(identities) != 1 {
		return "", false
	}
	for identity := range identities {
		return identity, true
	}
	return "", false
}

func repositoryIdentityFromRemoteURL(remoteURL string) (string, bool) {
	value := strings.TrimSpace(remoteURL)
	if value == "" {
		return "", false
	}

	var host, repositoryPath string
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", false
		}
		host = parsed.Hostname()
		repositoryPath = parsed.EscapedPath()
		if unescaped, err := url.PathUnescape(repositoryPath); err == nil {
			repositoryPath = unescaped
		}
	} else {
		before, after, found := strings.Cut(value, ":")
		if !found || strings.Contains(before, "/") {
			return "", false
		}
		if _, hostname, found := strings.Cut(before, "@"); found {
			host = hostname
		} else {
			host = before
		}
		repositoryPath = after
	}

	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	repositoryPath = strings.Trim(strings.TrimSuffix(repositoryPath, ".git"), "/")
	segments := strings.Split(repositoryPath, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", false
		}
	}
	switch {
	case host == "github.com" && len(segments) == 2:
		return strings.ToLower(strings.Join([]string{"github", segments[0], segments[1]}, "/")), true
	case host == "dev.azure.com" && len(segments) == 4 && strings.EqualFold(segments[2], "_git"):
		return strings.ToLower(strings.Join([]string{"ado", segments[0], segments[1], segments[3]}, "/")), true
	case strings.HasSuffix(host, ".visualstudio.com") && len(segments) == 3 &&
		strings.EqualFold(segments[1], "_git"):
		owner := strings.TrimSuffix(host, ".visualstudio.com")
		return strings.ToLower(strings.Join([]string{"ado", owner, segments[0], segments[2]}, "/")), true
	case host == "ssh.dev.azure.com" && len(segments) == 4 && strings.EqualFold(segments[0], "v3"):
		return strings.ToLower(strings.Join([]string{"ado", segments[1], segments[2], segments[3]}, "/")), true
	default:
		return "", false
	}
}

func containsAll(have, want []string) bool {
	set := make(map[string]bool, len(have))
	for _, item := range have {
		set[item] = true
	}
	for _, item := range want {
		if !set[item] {
			return false
		}
	}
	return true
}

func hasUnsafePath(path string) bool {
	if filepath.IsAbs(path) || strings.Contains(path, `\`) {
		return true
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

func dslVersionsEqual(left, right []dslVersion) bool {
	return reflect.DeepEqual(left, right)
}
