package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	configexamples "github.com/goobers/goobers/config-examples"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/runnercap"
)

const (
	// GuidedWorkflowImplementation identifies the canonical implementation workflow.
	GuidedWorkflowImplementation = "implementation"
	// GuidedWorkflowBacklogCuration identifies the canonical backlog-curation workflow.
	GuidedWorkflowBacklogCuration = "backlog-curation"
	// GuidedWorkflowWorkNomination identifies the canonical work-nomination workflow.
	GuidedWorkflowWorkNomination   = "work-nomination"
	guidedExampleGaggle            = "acme-web"
	guidedExampleDisplayName       = "Acme Web"
	guidedExampleRoot              = "gaggles/" + guidedExampleGaggle
	guidedRepositoryConnectionName = "github-main"
	guidedBacklogConnectionName    = "github-backlog"

	// GuidedSourceInstanceFile is the secret-free instance template stored in a
	// checked-in config source tree.
	GuidedSourceInstanceFile = "instance.yaml.example"
)

var guidedWorkflowOrder = []string{
	GuidedWorkflowImplementation,
	GuidedWorkflowBacklogCuration,
	GuidedWorkflowWorkNomination,
}

var guidedWorkflowGoobers = map[string][]string{
	GuidedWorkflowImplementation:  {"implementer", "reviewer"},
	GuidedWorkflowBacklogCuration: {"curator"},
	GuidedWorkflowWorkNomination:  {"nominator"},
}

// GuidedWorkflowNames returns the canonical workflows available during guided
// initialization in their display order.
func GuidedWorkflowNames() []string {
	return append([]string(nil), guidedWorkflowOrder...)
}

// GuidedOptions describes one guided first-run instance.
type GuidedOptions struct {
	GaggleName           string
	DisplayName          string
	RepoOwner            string
	RepoName             string
	RepoBranch           string
	RepoTokenEnv         string
	WorkTrackingTokenEnv string
	PullRequestTokenEnv  string
	RepoPushTokenEnv     string
	// Harness selects the agent harness every generated agentic goober uses
	// (apiv1.HarnessCopilot or apiv1.HarnessClaudeCode). Empty defaults to
	// copilot (normalizeGuidedOptions), preserving prior guided-init
	// behavior byte-for-byte for callers that don't set it (#2777).
	Harness              string
	CopilotTokenEnv      string
	ClaudeTokenEnv       string
	Workflows            []string
	CICommand            []string
	RequiredCapabilities []string
}

// SeedGuidedConfigSource applies prompt-selected guided configuration through
// the same idempotent, no-clobber source seeder as the non-interactive action.
func SeedGuidedConfigSource(root string, opts GuidedOptions) (*ConfigSourceSeedResult, error) {
	opts = normalizeGuidedOptions(opts)
	if err := validateGuidedOptions(opts); err != nil {
		return nil, err
	}
	config, err := marshalConfig(guidedConfig(opts))
	if err != nil {
		return nil, err
	}
	files := []configSeedFile{{
		path: GuidedSourceInstanceFile,
		data: config,
	}}
	definitions, err := guidedConfigFiles(opts)
	if err != nil {
		return nil, err
	}
	files = append(files, definitions...)
	return seedConfigSource(root, files, "guided template")
}

// CheckGuidedSourceTarget rejects any populated path so guided setup never
// replaces files in an existing config source.
func CheckGuidedSourceTarget(root string) error {
	info, err := os.Stat(root)
	if err == nil && !info.IsDir() {
		return fmt.Errorf("config source path %s is not a directory", root)
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect config source %s: %w", root, err)
	}
	populated, err := dirHasFiles(root)
	if err != nil {
		return fmt.Errorf("inspect config source %s: %w", root, err)
	}
	if populated {
		return fmt.Errorf("config source %s already contains files; refusing to overwrite it", root)
	}
	return nil
}

// LoadGuidedSourceConfig loads the secret-free instance template from a
// checked-in config source tree.
func LoadGuidedSourceConfig(root string) (*Config, error) {
	return LoadConfig(filepath.Join(root, GuidedSourceInstanceFile))
}

// InitGuidedFromSource materializes a validated config source into a fresh
// runtime instance while retaining the source location in instance.yaml.
func InitGuidedFromSource(root, sourceRoot string, cfg *Config) (*InitResult, error) {
	if cfg == nil {
		return nil, fmt.Errorf("guided source instance config is required")
	}
	if err := CheckGuidedInitTarget(root); err != nil {
		return nil, err
	}
	if err := CheckGuidedSourceInstancePaths(root, sourceRoot); err != nil {
		return nil, err
	}
	sourceAbs, err := filepath.Abs(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve config source: %w", err)
	}

	runtimeConfig := *cfg
	runtimeConfig.WorkflowSource = &WorkflowSource{
		Kind: WorkflowSourceKindLocalDir,
		Path: sourceAbs,
	}
	if err := runtimeConfig.Validate(); err != nil {
		return nil, fmt.Errorf("guided source instance config: %w", err)
	}
	return initWithSeed(root, &runtimeConfig, func(dir string) error {
		return copyGuidedSourceDefinitions(dir, sourceAbs)
	})
}

// MaterializeWorkflowSource replaces an instance's declarative configuration
// with the validated local source recorded in instance.yaml.
func MaterializeWorkflowSource(root string) (string, error) {
	layout := NewLayout(root)
	runtimeConfig, err := LoadConfig(layout.ConfigFile())
	if err != nil {
		return "", err
	}
	if runtimeConfig.WorkflowSource == nil {
		return "", fmt.Errorf("instance has no workflowSource; run guided setup or configure a local source")
	}
	source := *runtimeConfig.WorkflowSource
	if source.Kind != WorkflowSourceKindLocalDir {
		return "", fmt.Errorf("materialize workflowSource: kind %q is not supported; use %q", source.Kind, WorkflowSourceKindLocalDir)
	}
	if err := CheckGuidedSourceInstancePaths(root, source.Path); err != nil {
		return "", err
	}
	sourceRoot, err := filepath.Abs(source.Path)
	if err != nil {
		return "", fmt.Errorf("resolve config source: %w", err)
	}
	sourceConfig, err := LoadGuidedSourceConfig(sourceRoot)
	if err != nil {
		return "", fmt.Errorf("load config source instance template: %w", err)
	}
	materializedConfig := *sourceConfig
	materializedConfig.WorkflowSource = &source
	if err := materializedConfig.Validate(); err != nil {
		return "", fmt.Errorf("config source instance template: %w", err)
	}
	if _, report, err := LoadConfigDir(sourceRoot); err != nil {
		return "", fmt.Errorf("load config source definitions: %w (report: %+v)", err, report)
	}

	stagingRoot, err := os.MkdirTemp(root, ".config-materialize-")
	if err != nil {
		return "", fmt.Errorf("create config materialization staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingRoot) }()

	if err := WriteConfig(filepath.Join(stagingRoot, ConfigFileName), &materializedConfig); err != nil {
		return "", err
	}
	stagedConfigDir := filepath.Join(stagingRoot, ConfigDirName)
	if err := copyGuidedSourceDefinitions(stagedConfigDir, sourceRoot); err != nil {
		return "", fmt.Errorf("stage config source definitions: %w", err)
	}
	if _, report, err := LoadConfigDir(stagedConfigDir); err != nil {
		return "", fmt.Errorf("validate staged config source definitions: %w (report: %+v)", err, report)
	}
	if err := installMaterializedConfig(layout, stagingRoot); err != nil {
		return "", err
	}
	return sourceRoot, nil
}

func installMaterializedConfig(layout Layout, stagingRoot string) error {
	backupRoot, err := os.MkdirTemp(layout.Root, ".config-materialize-backup-")
	if err != nil {
		return fmt.Errorf("create config materialization backup directory: %w", err)
	}
	backupConfigFile := filepath.Join(backupRoot, ConfigFileName)
	backupConfigDir := filepath.Join(backupRoot, ConfigDirName)

	if err := os.Rename(layout.ConfigFile(), backupConfigFile); err != nil {
		_ = os.RemoveAll(backupRoot)
		return fmt.Errorf("back up %s: %w", ConfigFileName, err)
	}
	if err := os.Rename(layout.ConfigDir(), backupConfigDir); err != nil {
		rollbackErr := wrapMaterializeRollbackError(os.Rename(backupConfigFile, layout.ConfigFile()))
		return errors.Join(
			fmt.Errorf("back up %s: %w", ConfigDirName, err),
			rollbackErr,
			removeMaterializeBackupAfterRollback(backupRoot, rollbackErr),
		)
	}

	stagedConfigFile := filepath.Join(stagingRoot, ConfigFileName)
	if err := os.Rename(stagedConfigFile, layout.ConfigFile()); err != nil {
		rollbackErr := rollbackMaterializedConfig(layout, backupConfigFile, backupConfigDir)
		return errors.Join(
			fmt.Errorf("install %s: %w", ConfigFileName, err),
			rollbackErr,
			removeMaterializeBackupAfterRollback(backupRoot, rollbackErr),
		)
	}
	if err := os.Rename(filepath.Join(stagingRoot, ConfigDirName), layout.ConfigDir()); err != nil {
		rollbackErr := rollbackMaterializedConfig(layout, backupConfigFile, backupConfigDir)
		return errors.Join(
			fmt.Errorf("install %s: %w", ConfigDirName, err),
			rollbackErr,
			removeMaterializeBackupAfterRollback(backupRoot, rollbackErr),
		)
	}
	if err := os.RemoveAll(backupRoot); err != nil {
		return fmt.Errorf("remove config materialization backup %s: %w", backupRoot, err)
	}
	return nil
}

func rollbackMaterializedConfig(layout Layout, backupConfigFile, backupConfigDir string) error {
	var rollbackErrors []error
	if err := os.Remove(layout.ConfigFile()); err != nil && !os.IsNotExist(err) {
		rollbackErrors = append(rollbackErrors, err)
	}
	if err := os.RemoveAll(layout.ConfigDir()); err != nil {
		rollbackErrors = append(rollbackErrors, err)
	}
	if err := os.Rename(backupConfigFile, layout.ConfigFile()); err != nil {
		rollbackErrors = append(rollbackErrors, err)
	}
	if err := os.Rename(backupConfigDir, layout.ConfigDir()); err != nil {
		rollbackErrors = append(rollbackErrors, err)
	}
	if err := errors.Join(rollbackErrors...); err != nil {
		return fmt.Errorf("roll back config materialization: %w", err)
	}
	return nil
}

func wrapMaterializeRollbackError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("roll back config materialization: %w", err)
}

func removeMaterializeBackup(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove config materialization backup %s: %w", path, err)
	}
	return nil
}

func removeMaterializeBackupAfterRollback(path string, rollbackErr error) error {
	if rollbackErr != nil {
		return nil
	}
	return removeMaterializeBackup(path)
}

// CheckGuidedSourceInstancePaths requires the desired-state source and runtime
// instance to be disjoint directory trees.
func CheckGuidedSourceInstancePaths(root, sourceRoot string) error {
	sourceAbs, err := canonicalGuidedPath(sourceRoot)
	if err != nil {
		return fmt.Errorf("resolve config source: %w", err)
	}
	rootAbs, err := canonicalGuidedPath(root)
	if err != nil {
		return fmt.Errorf("resolve instance root: %w", err)
	}
	if pathsOverlap(sourceAbs, rootAbs) {
		return fmt.Errorf("config source %s and instance root %s must be separate paths", sourceAbs, rootAbs)
	}
	return nil
}

func canonicalGuidedPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := absolute
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func pathsOverlap(first, second string) bool {
	contains := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
	}
	return contains(first, second) || contains(second, first)
}

func copyGuidedSourceDefinitions(destination, source string) error {
	for _, name := range []string{"manifest.yaml", "gaggles"} {
		if err := copyGuidedSourcePath(destination, source, name); err != nil {
			return err
		}
	}
	return nil
}

func copyGuidedSourcePath(destination, source, name string) error {
	sourcePath := filepath.Join(source, name)
	return filepath.WalkDir(sourcePath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("config source path %s is a symlink; refusing to materialize it", path)
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
			return fmt.Errorf("config source path %s is not a regular file", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		return nil
	})
}

// CheckGuidedInitTarget rejects configuration that guided setup cannot safely
// replace. Other scaffold directories may already exist.
func CheckGuidedInitTarget(root string) error {
	layout := NewLayout(root)
	if _, err := os.Stat(layout.ConfigFile()); err == nil {
		events, readErr := journal.ReadInstanceLog(layout.SchedulerDir())
		if readErr != nil {
			return fmt.Errorf("inspect guided init completion journal: %w", readErr)
		}
		if !slices.ContainsFunc(events, func(event journal.Event) bool {
			return event.Type == journal.EventInitCompleted
		}) {
			abs := absPath(root)
			quoted := strconv.Quote(abs)
			return targetConflictf("guided setup requires an unconfigured target: %s already exists in %s, but its instance journal has no %s marker; this may be an incomplete guided setup. To replace it, delete %s and rerun `goobers init --guided %s`. To recover the existing setup, run `goobers validate %s` and then `goobers config materialize %s`", ConfigFileName, abs, journal.EventInitCompleted, quoted, quoted, quoted, quoted)
		}
		return targetConflictf("guided setup requires an unconfigured target: %s already exists in %s; choose an empty path, e.g. `goobers init --guided ./my-instance`", ConfigFileName, absPath(root))
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s: %w", ConfigFileName, err)
	}

	populated, err := dirHasFiles(layout.ConfigDir())
	if err != nil {
		return fmt.Errorf("inspect %s: %w", ConfigDirName, err)
	}
	if populated {
		detail := ""
		if hasManifest, probeErr := configDirHasManifest(layout.ConfigDir()); probeErr == nil && !hasManifest {
			detail = ", and none is Goobers config (no `kind: Manifest` document) — the target looks like an unrelated project, for example a Goobers source checkout whose config/ holds CRD manifests"
		}
		return targetConflictf("guided setup requires an unconfigured target: %s in %s already contains files%s; choose an empty path, e.g. `goobers init --guided ./my-instance`", ConfigDirName, absPath(root), detail)
	}
	return nil
}

func normalizeGuidedOptions(opts GuidedOptions) GuidedOptions {
	opts.GaggleName = strings.TrimSpace(opts.GaggleName)
	opts.DisplayName = strings.TrimSpace(opts.DisplayName)
	opts.RepoOwner = strings.TrimSpace(opts.RepoOwner)
	opts.RepoName = strings.TrimSpace(opts.RepoName)
	opts.RepoBranch = strings.TrimSpace(opts.RepoBranch)
	opts.RepoTokenEnv = strings.TrimSpace(opts.RepoTokenEnv)
	opts.WorkTrackingTokenEnv = strings.TrimSpace(opts.WorkTrackingTokenEnv)
	opts.PullRequestTokenEnv = strings.TrimSpace(opts.PullRequestTokenEnv)
	opts.RepoPushTokenEnv = strings.TrimSpace(opts.RepoPushTokenEnv)
	opts.CopilotTokenEnv = strings.TrimSpace(opts.CopilotTokenEnv)
	opts.ClaudeTokenEnv = strings.TrimSpace(opts.ClaudeTokenEnv)
	opts.Harness = strings.TrimSpace(opts.Harness)
	if opts.Harness == "" {
		opts.Harness = string(apiv1.HarnessCopilot)
	}
	if opts.DisplayName == "" {
		opts.DisplayName = opts.RepoOwner + "/" + opts.RepoName
	}
	if opts.RepoBranch == "" {
		opts.RepoBranch = "main"
	}
	return opts
}

func validateGuidedOptions(opts GuidedOptions) error {
	for label, value := range map[string]string{
		"gaggle name":       opts.GaggleName,
		"repository owner":  opts.RepoOwner,
		"repository name":   opts.RepoName,
		"repository branch": opts.RepoBranch,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if !guidedObjectName(opts.GaggleName) {
		return fmt.Errorf("gaggle name %q must contain lowercase letters, numbers, or hyphens and start and end with a letter or number", opts.GaggleName)
	}
	switch apiv1.Harness(opts.Harness) {
	case apiv1.HarnessCopilot, apiv1.HarnessClaudeCode:
	default:
		return fmt.Errorf("harness must be %q or %q", apiv1.HarnessCopilot, apiv1.HarnessClaudeCode)
	}
	if len(opts.Workflows) == 0 {
		return fmt.Errorf("select at least one workflow")
	}
	seen := make(map[string]bool, len(opts.Workflows))
	for _, name := range opts.Workflows {
		if _, ok := guidedWorkflowGoobers[name]; !ok {
			return fmt.Errorf("unknown guided workflow %q", name)
		}
		if seen[name] {
			return fmt.Errorf("guided workflow %q selected more than once", name)
		}
		seen[name] = true
	}
	if seen[GuidedWorkflowImplementation] {
		if len(opts.CICommand) == 0 {
			return fmt.Errorf("local CI command is required when selecting the implementation workflow")
		}
		for _, arg := range opts.CICommand {
			if strings.TrimSpace(arg) == "" {
				return fmt.Errorf("local CI command arguments must not be empty")
			}
		}
		if len(opts.RequiredCapabilities) == 0 {
			return fmt.Errorf("at least one required toolchain capability is required when selecting the implementation workflow")
		}
		for i, required := range opts.RequiredCapabilities {
			if err := runnercap.ValidateToken(required); err != nil {
				return fmt.Errorf("required toolchain capability %d: %w", i, err)
			}
		}
	} else if len(opts.CICommand) > 0 || len(opts.RequiredCapabilities) > 0 {
		return fmt.Errorf("local CI command and required toolchain capabilities require the implementation workflow")
	}
	type guidedTokenEnv struct {
		label string
		value string
	}
	tokenEnvs := []guidedTokenEnv{
		{label: "repository token environment variable", value: opts.RepoTokenEnv},
		{label: "work-tracking token environment variable", value: opts.WorkTrackingTokenEnv},
	}
	if seen[GuidedWorkflowImplementation] || seen[GuidedWorkflowBacklogCuration] {
		tokenEnvs = append(tokenEnvs, guidedTokenEnv{
			label: "pull-request token environment variable",
			value: opts.PullRequestTokenEnv,
		})
	}
	if seen[GuidedWorkflowImplementation] {
		tokenEnvs = append(tokenEnvs, guidedTokenEnv{
			label: "repository push token environment variable",
			value: opts.RepoPushTokenEnv,
		})
	}
	if opts.CopilotTokenEnv != "" {
		tokenEnvs = append(tokenEnvs, guidedTokenEnv{
			label: "Copilot token environment variable",
			value: opts.CopilotTokenEnv,
		})
	}
	if opts.ClaudeTokenEnv != "" {
		tokenEnvs = append(tokenEnvs, guidedTokenEnv{
			label: "Claude Code token environment variable",
			value: opts.ClaudeTokenEnv,
		})
	}
	for _, tokenEnv := range tokenEnvs {
		if !ValidGuidedTokenEnvName(tokenEnv.value) {
			return fmt.Errorf("%s must name a valid environment variable; do not provide a token value", tokenEnv.label)
		}
	}
	return nil
}

func guidedObjectName(name string) bool {
	if name == "" || len(name) > 54 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// ValidGuidedTokenEnvName reports whether value is a valid environment variable
// name and not a GitHub token value accidentally pasted into the prompt.
func ValidGuidedTokenEnvName(value string) bool {
	if !procenv.ValidName(value) {
		return false
	}
	for _, prefix := range []string{"github_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_"} {
		if strings.HasPrefix(value, prefix) {
			return false
		}
	}
	return true
}

func guidedConfig(opts GuidedOptions) *Config {
	credentials := []CredentialGrant{{
		Capability: string(capability.GitHubIssuesWrite),
		Token:      TokenRef{Env: opts.WorkTrackingTokenEnv},
	}}
	if slices.Contains(opts.Workflows, GuidedWorkflowImplementation) ||
		slices.Contains(opts.Workflows, GuidedWorkflowBacklogCuration) {
		credentials = append(credentials, CredentialGrant{
			Capability: string(capability.ProviderPRWrite),
			Token:      TokenRef{Env: opts.PullRequestTokenEnv},
		})
	}
	if slices.Contains(opts.Workflows, GuidedWorkflowImplementation) {
		credentials = append(credentials, CredentialGrant{
			Capability: string(capability.RepoPush),
			Token:      TokenRef{Env: opts.RepoPushTokenEnv},
		})
	}
	if opts.CopilotTokenEnv != "" {
		credentials = append(credentials, CredentialGrant{
			Capability: string(capability.AgentModel),
			Token:      TokenRef{Env: opts.CopilotTokenEnv},
		})
	}
	if opts.ClaudeTokenEnv != "" {
		credentials = append(credentials, CredentialGrant{
			Capability: string(capability.AgentModel),
			Token:      TokenRef{Env: opts.ClaudeTokenEnv},
		})
	}
	cfg := &Config{
		APIVersion: ConfigAPIVersion,
		Kind:       ConfigKind,
		Repos: []RepoRef{{
			Provider: "github",
			Owner:    opts.RepoOwner,
			Name:     opts.RepoName,
			Token:    TokenRef{Env: opts.RepoTokenEnv},
		}},
		Credentials:   credentials,
		RunConditions: RunConditions{MaxParallelRuns: 1},
		Runner:        RunnerConfig{Capabilities: append([]string(nil), opts.RequiredCapabilities...)},
	}
	return cfg
}

func guidedConfigFiles(opts GuidedOptions) ([]configSeedFile, error) {
	files := []configSeedFile{
		{path: "manifest.yaml", data: guidedManifest(opts)},
		{
			path: filepath.ToSlash(filepath.Join("gaggles", opts.GaggleName, "gaggle.yaml")),
			data: guidedGaggle(opts),
		},
	}
	selected := make(map[string]bool, len(opts.Workflows))
	goobers := make(map[string]bool)
	for _, workflow := range opts.Workflows {
		selected[workflow] = true
		for _, goober := range guidedWorkflowGoobers[workflow] {
			goobers[goober] = true
		}
		file, err := guidedWorkflowFile(workflow, opts)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	names := make([]string, 0, len(goobers))
	for name := range goobers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		gooberFiles, err := guidedGooberFiles(name, selected, opts)
		if err != nil {
			return nil, err
		}
		files = append(files, gooberFiles...)
	}
	return files, nil
}

func guidedWorkflowFile(name string, opts GuidedOptions) (configSeedFile, error) {
	source := guidedExampleRoot + "/workflows/" + name + ".yaml"
	data, err := configexamples.Files.ReadFile(source)
	if err != nil {
		return configSeedFile{}, err
	}
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return configSeedFile{}, fmt.Errorf("decode canonical workflow %s: %w", name, err)
	}
	workflow.Spec.Gaggle = opts.GaggleName
	for i := range workflow.Spec.Tasks {
		task := &workflow.Spec.Tasks[i]
		if task.Type == apiv1.TaskAgentic {
			task.Capabilities = prependCapability(task.Capabilities, string(capability.AgentModel))
		}
		// Template the operator's answered CI command into the generated
		// local-ci stage instead of leaving the source example's literal
		// command on disk: gaggle.yaml's ciCommand (ApplyGaggleCICommand) wins
		// at runtime either way, but a mismatched on-disk command reads as a
		// lie to anyone who answered something other than the example's
		// default and later hand-edits this file (#2173).
		if task.Type == apiv1.TaskDeterministic && task.Name == LocalCIStageName && task.Run != nil && len(opts.CICommand) > 0 {
			task.Run.Command = append([]string(nil), opts.CICommand...)
			task.Run.Script = ""
		}
	}
	data, err = yaml.Marshal(workflow)
	if err != nil {
		return configSeedFile{}, fmt.Errorf("encode guided workflow %s: %w", name, err)
	}
	data = withLocalCICommandComment(data, workflow.Spec.Tasks)
	return configSeedFile{
		path: filepath.ToSlash(filepath.Join("gaggles", opts.GaggleName, "workflows", name+".yaml")),
		data: data,
	}, nil
}

// withLocalCICommandComment inserts an explanatory comment directly above the
// marshaled workflow's `tasks:` list when it contains a `local-ci` stage
// (#2071): the ciCommand<->local-ci relationship (MGV-1/#1009) was previously
// documented only in the CRD schema, CONTRIBUTING.md, and hand-written
// config-examples — none of which a user looking at a generated
// implementation.yaml is looking at. yaml.Marshal (sigs.k8s.io/yaml, which
// round-trips through encoding/json) never emits comments and always
// alphabetizes struct fields, so per-task comment placement is not reliably
// locatable; anchoring on the top-level `tasks:` key is exact regardless of
// field order because that key name is fixed and appears exactly once.
func withLocalCICommandComment(data []byte, tasks []apiv1.Task) []byte {
	hasLocalCI := false
	for _, task := range tasks {
		if task.Name == LocalCIStageName {
			hasLocalCI = true
			break
		}
	}
	if !hasLocalCI {
		return data
	}
	marker := []byte("\n  tasks:\n")
	idx := bytes.Index(data, marker)
	if idx < 0 {
		return data
	}
	comment := []byte("  # The \"local-ci\" stage below runs this gaggle's ciCommand (gaggle.yaml)\n" +
		"  # in place of the command it declares here — see MGV-1/#1009.\n")
	out := make([]byte, 0, len(data)+len(comment))
	out = append(out, data[:idx+1]...)
	out = append(out, comment...)
	out = append(out, data[idx+1:]...)
	return out
}

func guidedGooberFiles(name string, selected map[string]bool, opts GuidedOptions) ([]configSeedFile, error) {
	sourceDir := guidedExampleRoot + "/goobers/" + name
	data, err := configexamples.Files.ReadFile(sourceDir + "/goober.yaml")
	if err != nil {
		return nil, err
	}
	var goober apiv1.Goober
	if err := yaml.Unmarshal(data, &goober); err != nil {
		return nil, fmt.Errorf("decode canonical goober %s: %w", name, err)
	}
	goober.Spec.Gaggle = opts.GaggleName
	// The canonical acme-web template mixes harnesses goober-by-goober
	// (implementer ships claude-code, the rest copilot — #2548); guided init
	// offers one harness choice for the whole generated fleet (#2777), so
	// every selected goober is normalized to it here regardless of what the
	// template itself declares.
	goober.Spec.Harness = apiv1.Harness(opts.Harness)
	goober.Spec.Capabilities = prependCapability(goober.Spec.Capabilities, string(capability.AgentModel))
	workflows := goober.Spec.Workflows[:0]
	for _, workflow := range goober.Spec.Workflows {
		if selected[workflow] {
			workflows = append(workflows, workflow)
		}
	}
	goober.Spec.Workflows = workflows
	data, err = yaml.Marshal(goober)
	if err != nil {
		return nil, fmt.Errorf("encode guided goober %s: %w", name, err)
	}
	instructions, err := configexamples.Files.ReadFile(sourceDir + "/instructions.md")
	if err != nil {
		return nil, err
	}
	instructions = []byte(strings.ReplaceAll(string(instructions), guidedExampleDisplayName, opts.DisplayName))
	targetDir := filepath.Join("gaggles", opts.GaggleName, "goobers", name)
	return []configSeedFile{
		{
			path: filepath.ToSlash(filepath.Join(targetDir, "goober.yaml")),
			data: data,
		},
		{
			path: filepath.ToSlash(filepath.Join(targetDir, "instructions.md")),
			data: instructions,
		},
	}, nil
}

func prependCapability(capabilities []string, name string) []string {
	for _, existing := range capabilities {
		if existing == name {
			return capabilities
		}
	}
	return append([]string{name}, capabilities...)
}

func guidedManifest(opts GuidedOptions) []byte {
	return []byte(fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: %s
spec:
  instance:
    name: %s
    environment: dev
  connections:
    - name: %s
      type: repo
      provider: github
      secretRef:
        name: repo-token
    - name: %s
      type: backlog
      provider: github
      secretRef:
        name: backlog-token
  gaggles:
    - %s
`, yamlScalar(opts.GaggleName+"-instance"), yamlScalar(opts.GaggleName),
		guidedRepositoryConnectionName, guidedBacklogConnectionName, yamlScalar(opts.GaggleName)))
}

func guidedGaggle(opts GuidedOptions) []byte {
	ciCommand := ""
	requiredCapabilities := ""
	if len(opts.CICommand) > 0 {
		// #2071: this line and the `local-ci` stage in this gaggle's
		// implementation.yaml (MGV-1/#1009) must be edited together — the
		// stage's declared command is only what runs when this override is
		// unset; the comment there points back here.
		ciCommand = "  # Overrides the `local-ci` stage's declared command in this gaggle's\n" +
			"  # implementation.yaml (MGV-1/#1009); edit both together.\n" +
			"  ciCommand: " + yamlStringList(opts.CICommand) + "\n"
	}
	if len(opts.RequiredCapabilities) > 0 {
		requiredCapabilities = "  requiredCapabilities: " + yamlStringList(opts.RequiredCapabilities) + "\n"
	}
	return []byte(fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: %s
spec:
  displayName: %s
  project:
    provider: github
    owner: %s
    name: %s
    branch: %s
    connectionRef: %s
  backlog:
    provider: github
    project: %s
    labels:
      - goobers
    connectionRef: %s
%s%s  isolation:
    namespace: %s
`, yamlScalar(opts.GaggleName), yamlScalar(opts.DisplayName), yamlScalar(opts.RepoOwner),
		yamlScalar(opts.RepoName), yamlScalar(opts.RepoBranch), guidedRepositoryConnectionName,
		yamlScalar(opts.RepoOwner+"/"+opts.RepoName), guidedBacklogConnectionName, ciCommand, requiredCapabilities,
		yamlScalar("gaggle-"+opts.GaggleName)))
}

func yamlScalar(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func yamlStringList(values []string) string {
	data, _ := json.Marshal(values)
	return string(data)
}
