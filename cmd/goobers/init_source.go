package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/agentkit"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

const (
	guidedSourceNewLocal       = "new-local"
	guidedSourceExistingLocal  = "existing-local"
	guidedSourceExistingGitHub = "github-existing"
)

type guidedGitHubOperations interface {
	Clone(context.Context, string, string, string) error
	Create(context.Context, string, string, string, string) error
}

type defaultGuidedGitHubOperations struct{}

func (defaultGuidedGitHubOperations) Clone(ctx context.Context, owner, name, destination string) error {
	_, err := providers.NewGitHubProvider("").CloneRepository(ctx, providers.CloneRequest{
		Repository: providers.RepositoryRef{
			Provider: providers.ProviderGitHub,
			Owner:    owner,
			Name:     name,
		},
		Destination: destination,
	})
	return err
}

func (defaultGuidedGitHubOperations) Create(ctx context.Context, owner, name, visibility, token string) error {
	_, err := providers.NewGitHubProvider(token).CreateRepository(ctx, providers.CreateRepositoryRequest{
		Owner:      owner,
		Name:       name,
		Visibility: visibility,
	})
	return err
}

type guidedSourceSelection struct {
	Mode       string
	Root       string
	ConfigRepo string
	Owner      string
	Name       string
}

type guidedRemoteCreate struct {
	Owner      string
	Name       string
	Visibility string
	Token      string
}

type guidedSourcePreparation struct {
	validationRoot         string
	cloneAfterConfirmation bool
	cleanup                func()
}

type guidedInitResult struct {
	SourceRoot    string
	ConfigRepo    string
	TargetRepo    string
	Backlog       string
	Gaggle        string
	RemoteCreated bool
}

type guidedAgentToolkitSelection struct {
	Harness              string
	Destination          string
	InitializeRepository bool
}

func runGuidedInit(
	root string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	github guidedGitHubOperations,
) (*instance.InitResult, guidedInitResult, int, error) {
	p := guidedPrompter{reader: bufio.NewReader(stdin), out: stdout}
	pln(stdout, "Guided first-run setup")
	pln(stdout, "")

	source, err := promptGuidedSource(p, root)
	if err != nil {
		return nil, guidedInitResult{}, 2, err
	}
	if err := instance.CheckGuidedSourceInstancePaths(root, source.Root); err != nil {
		return nil, guidedInitResult{}, 2, err
	}

	var (
		cfg          *instance.Config
		result       guidedInitResult
		remoteCreate *guidedRemoteCreate
		agentToolkit guidedAgentToolkitSelection
	)
	if source.Mode == guidedSourceNewLocal {
		opts, promptErr := promptGuidedOptionsWithPrompter(p)
		if promptErr != nil {
			return nil, guidedInitResult{}, 2, promptErr
		}
		remoteCreate, err = promptGuidedRemoteCreate(p, opts)
		if err != nil {
			return nil, guidedInitResult{}, 2, err
		}
		if remoteCreate != nil {
			source.ConfigRepo = "https://github.com/" + remoteCreate.Owner + "/" + remoteCreate.Name
		}
		result = guidedResultForOptions(source, opts, remoteCreate != nil)
		if err := confirmGuidedMapping(p, root, result); err != nil {
			return nil, guidedInitResult{}, 2, err
		}
		agentToolkit, err = promptGuidedAgentToolkit(p, source, "")
		if err != nil {
			return nil, guidedInitResult{}, 2, err
		}
		if _, err := executeSeedConfigSourceAction(source.Root, &opts, runtime.GOOS); err != nil {
			return nil, guidedInitResult{}, 2, err
		}
		if code := validateGuidedSource(source.Root, stdout, stderr); code != 0 {
			return nil, guidedInitResult{}, code, fmt.Errorf("new config source failed validation")
		}
		cfg, err = instance.LoadGuidedSourceConfig(source.Root)
		if err != nil {
			return nil, guidedInitResult{}, 2, fmt.Errorf("load generated config source: %w", err)
		}
	} else {
		preparation, prepareErr := prepareGuidedSource(source, stdout, stderr, github)
		if prepareErr != nil {
			return nil, guidedInitResult{}, 2, prepareErr
		}
		if preparation.cleanup != nil {
			defer preparation.cleanup()
		}
		if code := validateGuidedSource(preparation.validationRoot, stdout, stderr); code != 0 {
			return nil, guidedInitResult{}, code, fmt.Errorf("existing config source failed validation")
		}
		cfg, err = instance.LoadGuidedSourceConfig(preparation.validationRoot)
		if err != nil {
			return nil, guidedInitResult{}, 2, fmt.Errorf("load config source: %w", err)
		}
		result, err = promptExistingSourceTarget(p, source, preparation.validationRoot, cfg)
		if err != nil {
			return nil, guidedInitResult{}, 2, err
		}
		if err := confirmGuidedMapping(p, root, result); err != nil {
			return nil, guidedInitResult{}, 2, err
		}
		agentToolkit, err = promptGuidedAgentToolkit(p, source, preparation.validationRoot)
		if err != nil {
			return nil, guidedInitResult{}, 2, err
		}
		if preparation.cloneAfterConfirmation {
			if err := cloneGuidedSource(source, source.Root, github); err != nil {
				return nil, guidedInitResult{}, 2, err
			}
			if code := validateGuidedSource(source.Root, stdout, stderr); code != 0 {
				return nil, guidedInitResult{}, code, fmt.Errorf("existing config source failed validation")
			}
			cfg, err = instance.LoadGuidedSourceConfig(source.Root)
			if err != nil {
				return nil, guidedInitResult{}, 2, fmt.Errorf("load config source: %w", err)
			}
			finalResult, err := guidedResultForExistingSource(
				source,
				source.Root,
				cfg,
				result.TargetRepo,
			)
			if err != nil {
				return nil, guidedInitResult{}, 2, err
			}
			if finalResult != result {
				return nil, guidedInitResult{}, 2, fmt.Errorf("GitHub config source changed after mapping confirmation")
			}
		}
	}

	if err := installGuidedAgentToolkit(root, source, agentToolkit, stdout); err != nil {
		return nil, guidedInitResult{}, 1, err
	}
	if remoteCreate != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := github.Create(
			ctx,
			remoteCreate.Owner,
			remoteCreate.Name,
			remoteCreate.Visibility,
			remoteCreate.Token,
		); err != nil {
			return nil, guidedInitResult{}, 2, fmt.Errorf("create GitHub config repository: %w", err)
		}
	}
	res, err := instance.InitGuidedFromSource(root, source.Root, cfg)
	if err != nil {
		return nil, guidedInitResult{}, 2, err
	}
	if err := offerGuidedPortalExtension(p); err != nil {
		pf(stdout, "Portal extension setup skipped: %v\nInstall or update it later with `goobers portal-extension install` or `goobers portal-extension update`.\n", err)
	}
	return res, result, 0, nil
}

func promptGuidedAgentToolkit(
	p guidedPrompter,
	source guidedSourceSelection,
	inspectionRoot string,
) (guidedAgentToolkitSelection, error) {
	pln(p.out, "")
	pln(p.out, "Install Goobers assistance for your coding agent?")
	pln(p.out, "Supported harnesses: copilot, claude, generic, or skip.")
	detected := detectGuidedAgentToolkitHarnesses(inspectionRoot)
	if len(detected) == 0 {
		pln(p.out, "Detected harnesses: none; no harness will be selected automatically.")
	} else {
		pf(p.out, "Detected harnesses: %s; choose one explicitly or skip.\n", strings.Join(detected, ", "))
	}
	harness, err := p.ask("Agent harness", "skip", validGuidedAgentToolkitHarness)
	if err != nil {
		return guidedAgentToolkitSelection{}, err
	}
	harness = strings.ToLower(strings.TrimSpace(harness))
	if harness == "skip" {
		pln(p.out, "Skipping agent toolkit installation; no toolkit files will be written.")
		return guidedAgentToolkitSelection{}, nil
	}

	destination := absolutePath(source.Root)
	selectedDestination, err := p.ask("Confirm agent toolkit destination (must match selected config source)", destination, func(value string) bool {
		return sameAbsolutePath(value, destination)
	})
	if err != nil {
		return guidedAgentToolkitSelection{}, err
	}
	selectedDestination = absolutePath(selectedDestination)

	bundle, err := currentAgentToolkitBundle()
	if err != nil {
		return guidedAgentToolkitSelection{}, fmt.Errorf("build bundled agent toolkit: %w", err)
	}
	adapter, ok := bundle.Adapter(harness)
	if !ok {
		return guidedAgentToolkitSelection{}, fmt.Errorf("unsupported agent toolkit harness %q", harness)
	}
	initializeRepository := source.Mode == guidedSourceNewLocal
	state, modified, missing, err := inspectGuidedAgentToolkitState(inspectionRoot, bundle)
	if source.Mode == guidedSourceExistingLocal && errors.Is(err, agentkit.ErrRepositoryMarkerMissing) {
		initialize, promptErr := p.ask(
			"Initialize the selected config source with Git for agent toolkit installation? (yes/no)",
			"no",
			validYesNo,
		)
		if promptErr != nil {
			return guidedAgentToolkitSelection{}, promptErr
		}
		if !isYes(initialize) {
			pln(p.out, "Agent toolkit installation skipped; the selected config source remains non-Git and no toolkit files were written.")
			return guidedAgentToolkitSelection{}, nil
		}
		initializeRepository = true
		state, modified, missing, err = "not-installed", nil, nil, nil
	}
	if err != nil {
		return guidedAgentToolkitSelection{}, err
	}
	pf(p.out, `
Agent toolkit installation preview:
  destination:       %s
  harness:           %s
  bundle:            %s from Goobers %s (commit %s)
  current state:     %s
  product-owned:     %s (%d files including manifest)
  user-owned:        %s (preserved; managed reference only)
`,
		selectedDestination,
		harness,
		bundle.Manifest.BundleVersion,
		bundle.Manifest.Producer.Version,
		bundle.Manifest.Producer.Commit,
		state,
		agentkit.InstalledRoot,
		len(bundle.Manifest.Assets)+1,
		adapter.InstructionTarget,
	)
	if len(modified) > 0 {
		pf(p.out, "  modified owned:    %s\n", strings.Join(modified, ", "))
	}
	if len(missing) > 0 {
		pf(p.out, "  missing owned:     %s\n", strings.Join(missing, ", "))
	}
	if initializeRepository {
		pln(p.out, "  repository setup:  initialize the selected config source with Git")
	}
	if state != "not-installed" && state != "current" {
		commands := agentKitMaintenanceCommands(selectedDestination, runtime.GOOS)
		pf(p.out, "Agent toolkit installation skipped because the existing toolkit is %s; no files were written.\n", state)
		pf(p.out, "  Check:  %s\n", commands[0])
		pf(p.out, "  Review: %s\n", commands[1])
		if state == "modified" {
			pf(
				p.out,
				"  Apply after reviewing local changes: goobers agent-kit update --write --replace-modified %s\n",
				quoteShellArg(selectedDestination, runtime.GOOS),
			)
		} else {
			pf(p.out, "  Apply:  %s\n", commands[2])
		}
		return guidedAgentToolkitSelection{}, nil
	}
	confirmed, err := p.ask("Install this agent toolkit? (yes/no)", "no", validYesNo)
	if err != nil {
		return guidedAgentToolkitSelection{}, err
	}
	if !isYes(confirmed) {
		pln(p.out, "Agent toolkit installation declined; no toolkit files will be written.")
		return guidedAgentToolkitSelection{}, nil
	}
	return guidedAgentToolkitSelection{
		Harness:              harness,
		Destination:          selectedDestination,
		InitializeRepository: initializeRepository,
	}, nil
}

func detectGuidedAgentToolkitHarnesses(root string) []string {
	type harnessSignal struct {
		name        string
		instruction string
		executable  string
	}
	signals := []harnessSignal{
		{name: "copilot", instruction: ".github/copilot-instructions.md", executable: "copilot"},
		{name: "claude", instruction: "CLAUDE.md", executable: "claude"},
		{name: "generic", instruction: "AGENTS.md"},
	}
	detected := make([]string, 0, len(signals))
	for _, signal := range signals {
		found := false
		if root != "" {
			info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(signal.instruction)))
			found = err == nil && info.Mode().IsRegular()
		}
		if !found && signal.executable != "" {
			_, err := exec.LookPath(signal.executable)
			found = err == nil
		}
		if found {
			detected = append(detected, signal.name)
		}
	}
	return detected
}

func inspectGuidedAgentToolkitState(
	root string,
	bundle agentkit.Bundle,
) (string, []string, []string, error) {
	if root == "" {
		return "not-installed", nil, nil, nil
	}
	repository, err := agentkit.OpenRepository(root)
	if err != nil {
		return "", nil, nil, fmt.Errorf(
			"selected config source %s cannot be inspected for agent toolkit installation: %w",
			root,
			err,
		)
	}
	report, err := repository.Check(bundle)
	if err != nil {
		return "", nil, nil, fmt.Errorf("inspect agent toolkit state: %w", err)
	}
	state := report.State
	switch state {
	case "upgrade-available":
		state = "outdated"
	case "missing-manifest":
		state = "not-installed"
	}
	return state, report.Modified, report.Missing, nil
}

func installGuidedAgentToolkit(
	instanceRoot string,
	source guidedSourceSelection,
	selection guidedAgentToolkitSelection,
	stdout io.Writer,
) error {
	if selection.Harness == "" {
		return nil
	}
	if !sameAbsolutePath(selection.Destination, source.Root) {
		return fmt.Errorf("agent toolkit destination must be the selected config source %s", absolutePath(source.Root))
	}
	if selection.InitializeRepository {
		command := exec.Command("git", "-C", selection.Destination, "init", "--quiet")
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("initialize config source repository: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	executed, err := executeAgentToolkitInstallAction(selection.Destination, selection.Harness, runtime.GOOS)
	if err != nil {
		return err
	}
	if executed.Install.Installed {
		pf(stdout, "\nInstalled Goobers agent toolkit in %s for %s.\n", executed.Action.Path, selection.Harness)
	} else {
		pf(stdout, "\nGoobers agent toolkit in %s is already current for %s.\n", executed.Action.Path, selection.Harness)
	}
	writeAgentKitNextSteps(stdout, executed.Action.Path, instanceRoot)
	return nil
}

func validGuidedAgentToolkitHarness(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "skip" || supportedAgentKitHarness(value)
}

func sameAbsolutePath(left, right string) bool {
	return filepath.Clean(absolutePath(left)) == filepath.Clean(absolutePath(right))
}

func promptGuidedSource(p guidedPrompter, instanceRoot string) (guidedSourceSelection, error) {
	pln(p.out, "Configuration source (desired state; separate from runtime state):")
	pln(p.out, "  1) new-local        create a new local source tree")
	pln(p.out, "  2) existing-local   use an existing local source tree")
	pln(p.out, "  3) github-existing  clone or reuse an existing GitHub config repository")
	mode, err := p.ask("Config source type", guidedSourceNewLocal, validGuidedSourceMode)
	if err != nil {
		return guidedSourceSelection{}, err
	}
	mode = normalizeGuidedSourceMode(mode)

	switch mode {
	case guidedSourceNewLocal:
		defaultPath, err := defaultGuidedSourcePath(instanceRoot)
		if err != nil {
			return guidedSourceSelection{}, err
		}
		sourceRoot, err := p.ask(
			"New config source path",
			defaultPath,
			validNonEmptyPath,
		)
		if err != nil {
			return guidedSourceSelection{}, err
		}
		if err := instance.CheckGuidedSourceTarget(sourceRoot); err != nil {
			return guidedSourceSelection{}, err
		}
		return guidedSourceSelection{
			Mode:       mode,
			Root:       sourceRoot,
			ConfigRepo: sourceRoot,
		}, nil
	case guidedSourceExistingLocal:
		sourceRoot, err := p.ask("Existing config source path", "", validNonEmptyPath)
		if err != nil {
			return guidedSourceSelection{}, err
		}
		return guidedSourceSelection{
			Mode:       mode,
			Root:       sourceRoot,
			ConfigRepo: sourceRoot,
		}, nil
	case guidedSourceExistingGitHub:
		repository, err := p.ask(
			"Existing GitHub config repository (owner/name or URL)",
			"",
			validGitHubRepoInput,
		)
		if err != nil {
			return guidedSourceSelection{}, err
		}
		owner, name, err := parseGitHubRepo(repository)
		if err != nil {
			return guidedSourceSelection{}, err
		}
		defaultPath, err := defaultGuidedCheckoutPath(instanceRoot, name)
		if err != nil {
			return guidedSourceSelection{}, err
		}
		sourceRoot, err := p.ask(
			"Local checkout path",
			defaultPath,
			validNonEmptyPath,
		)
		if err != nil {
			return guidedSourceSelection{}, err
		}
		return guidedSourceSelection{
			Mode:       mode,
			Root:       sourceRoot,
			ConfigRepo: "https://github.com/" + owner + "/" + name,
			Owner:      owner,
			Name:       name,
		}, nil
	default:
		return guidedSourceSelection{}, fmt.Errorf("unsupported config source type %q", mode)
	}
}

func prepareGuidedSource(
	source guidedSourceSelection,
	stdout, stderr io.Writer,
	github guidedGitHubOperations,
) (guidedSourcePreparation, error) {
	if source.Mode != guidedSourceExistingGitHub {
		return guidedSourcePreparation{validationRoot: source.Root}, nil
	}
	entries, err := os.ReadDir(source.Root)
	switch {
	case err == nil && len(entries) > 0:
		pf(stdout, "Using existing config repository checkout at %s without modifying it.\n", source.Root)
		return guidedSourcePreparation{validationRoot: source.Root}, nil
	case err != nil && !os.IsNotExist(err):
		return guidedSourcePreparation{}, fmt.Errorf("inspect config source checkout %s: %w", source.Root, err)
	}

	stagingRoot, err := os.MkdirTemp("", "goobers-config-source-*")
	if err != nil {
		return guidedSourcePreparation{}, fmt.Errorf("create temporary config source checkout: %w", err)
	}
	cleanup := func() {
		if err := os.RemoveAll(stagingRoot); err != nil {
			pf(stderr, "warning: remove temporary config source checkout %s: %v\n", stagingRoot, err)
		}
	}
	if err := cloneGuidedSource(source, stagingRoot, github); err != nil {
		cleanup()
		return guidedSourcePreparation{}, err
	}
	pf(stdout, "Staged %s/%s in a temporary checkout before writing %s.\n", source.Owner, source.Name, source.Root)
	return guidedSourcePreparation{
		validationRoot:         stagingRoot,
		cloneAfterConfirmation: true,
		cleanup:                cleanup,
	}, nil
}

func cloneGuidedSource(
	source guidedSourceSelection,
	destination string,
	github guidedGitHubOperations,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := github.Clone(ctx, source.Owner, source.Name, destination); err != nil {
		return fmt.Errorf("clone GitHub config repository %s/%s: %w", source.Owner, source.Name, err)
	}
	return nil
}

func promptGuidedRemoteCreate(p guidedPrompter, opts instance.GuidedOptions) (*guidedRemoteCreate, error) {
	create, err := p.ask("Create an empty GitHub repository for this config source? (yes/no)", "no", validYesNo)
	if err != nil {
		return nil, err
	}
	if !isYes(create) {
		pln(p.out, "Keeping the config source local-only; no remote repository will be created.")
		return nil, nil
	}

	repository, err := p.ask(
		"New GitHub config repository (owner/name)",
		opts.RepoOwner+"/"+opts.RepoName+"-goobers-config",
		validGitHubRepoInput,
	)
	if err != nil {
		return nil, err
	}
	owner, name, err := parseGitHubRepo(repository)
	if err != nil {
		return nil, err
	}
	visibility, err := p.ask("Repository visibility (private/public/internal)", "private", validRepositoryVisibility)
	if err != nil {
		return nil, err
	}
	tokenEnv, err := p.ask(
		"GitHub repository-creation token environment variable",
		"GOOBERS_GITHUB_CONFIG_REPO_TOKEN",
		instance.ValidGuidedTokenEnvName,
	)
	if err != nil {
		return nil, err
	}

	pf(p.out, `
GitHub config repository to create:
  owner:      %s
  name:       %s
  visibility: %s
`, owner, name, visibility)
	confirmed, err := p.ask("Create this repository? (yes/no)", "no", validYesNo)
	if err != nil {
		return nil, err
	}
	if !isYes(confirmed) {
		pln(p.out, "Repository creation declined; continuing with a local-only config source.")
		return nil, nil
	}
	token := os.Getenv(tokenEnv)
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("GitHub repository-creation token environment variable %s is not set", tokenEnv)
	}
	return &guidedRemoteCreate{
		Owner:      owner,
		Name:       name,
		Visibility: visibility,
		Token:      token,
	}, nil
}

func promptExistingSourceTarget(
	p guidedPrompter,
	source guidedSourceSelection,
	validationRoot string,
	cfg *instance.Config,
) (guidedInitResult, error) {
	if len(cfg.Repos) == 0 || cfg.Repos[0].Provider != "github" {
		return guidedInitResult{}, fmt.Errorf("guided setup requires the config source template to declare a GitHub target repository")
	}
	target := cfg.Repos[0].Owner + "/" + cfg.Repos[0].Name
	selected, err := p.ask("Target GitHub application repository", target, validGitHubRepoInput)
	if err != nil {
		return guidedInitResult{}, err
	}
	return guidedResultForExistingSource(source, validationRoot, cfg, selected)
}

func guidedResultForExistingSource(
	source guidedSourceSelection,
	validationRoot string,
	cfg *instance.Config,
	selectedTarget string,
) (guidedInitResult, error) {
	if len(cfg.Repos) == 0 || cfg.Repos[0].Provider != "github" {
		return guidedInitResult{}, fmt.Errorf("guided setup requires the config source template to declare a GitHub target repository")
	}
	target := cfg.Repos[0].Owner + "/" + cfg.Repos[0].Name
	owner, name, err := parseGitHubRepo(selectedTarget)
	if err != nil {
		return guidedInitResult{}, err
	}
	if !strings.EqualFold(owner+"/"+name, target) {
		return guidedInitResult{}, fmt.Errorf(
			"existing config source targets %s; refusing to rewrite it for %s",
			target,
			owner+"/"+name,
		)
	}

	set, report, err := instance.LoadConfigDir(validationRoot)
	if err != nil {
		return guidedInitResult{}, fmt.Errorf("load existing config source: %w (report: %+v)", err, report)
	}
	for _, gaggle := range set.Gaggles {
		if !strings.EqualFold(gaggle.Spec.Project.Owner, owner) ||
			!strings.EqualFold(gaggle.Spec.Project.Name, name) {
			continue
		}
		backlogProject := gaggle.Spec.Backlog.Project
		if backlogProject == "" {
			backlogProject = target
		}
		return guidedInitResult{
			SourceRoot: absolutePath(source.Root),
			ConfigRepo: guidedConfigRepoDisplay(source),
			TargetRepo: "https://github.com/" + target,
			Backlog:    "https://github.com/" + backlogProject + "/issues",
			Gaggle:     gaggle.Name,
		}, nil
	}
	return guidedInitResult{}, fmt.Errorf("config source has no gaggle targeting %s", target)
}

func guidedResultForOptions(
	source guidedSourceSelection,
	opts instance.GuidedOptions,
	remoteCreated bool,
) guidedInitResult {
	target := opts.RepoOwner + "/" + opts.RepoName
	return guidedInitResult{
		SourceRoot:    absolutePath(source.Root),
		ConfigRepo:    guidedConfigRepoDisplay(source),
		TargetRepo:    "https://github.com/" + target,
		Backlog:       "https://github.com/" + target + "/issues",
		Gaggle:        opts.GaggleName,
		RemoteCreated: remoteCreated,
	}
}

func guidedConfigRepoDisplay(source guidedSourceSelection) string {
	if source.ConfigRepo == source.Root {
		return absolutePath(source.Root)
	}
	return source.ConfigRepo
}

func validateGuidedSource(root string, stdout, stderr io.Writer) int {
	pln(stdout, "")
	return runValidate([]string{"--source-tree", root}, stdout, stderr)
}

func confirmGuidedMapping(p guidedPrompter, instanceRoot string, result guidedInitResult) error {
	instanceAbs := absolutePath(instanceRoot)
	pf(p.out, `
Onboarding mapping to create:
  config-repo:  %s
  config-source: %s
  instance-root: %s
  instance/gaggle: %s
  target-repo:   %s
  backlog:       %s
`,
		result.ConfigRepo,
		result.SourceRoot,
		instanceAbs,
		filepath.Join(instanceAbs, instance.GagglesDirName, result.Gaggle),
		result.TargetRepo,
		result.Backlog,
	)
	confirmed, err := p.ask("Create this onboarding mapping? (yes/no)", "no", validYesNo)
	if err != nil {
		return err
	}
	if !isYes(confirmed) {
		return fmt.Errorf("guided setup cancelled before writing config source or instance")
	}
	return nil
}

func defaultGuidedSourcePath(instanceRoot string) (string, error) {
	clean, err := filepath.Abs(instanceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve instance root for config source default: %w", err)
	}
	base := filepath.Base(clean)
	if base == string(filepath.Separator) {
		base = "goobers"
	}
	return filepath.Join(filepath.Dir(clean), base+"-config"), nil
}

func defaultGuidedCheckoutPath(instanceRoot, repositoryName string) (string, error) {
	clean, err := filepath.Abs(instanceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve instance root for config checkout default: %w", err)
	}
	return filepath.Join(filepath.Dir(clean), repositoryName+"-config"), nil
}

func validGuidedSourceMode(value string) bool {
	switch normalizeGuidedSourceMode(value) {
	case guidedSourceNewLocal, guidedSourceExistingLocal, guidedSourceExistingGitHub:
		return true
	default:
		return false
	}
}

func normalizeGuidedSourceMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1":
		return guidedSourceNewLocal
	case "2":
		return guidedSourceExistingLocal
	case "3":
		return guidedSourceExistingGitHub
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func validNonEmptyPath(value string) bool {
	return strings.TrimSpace(value) != ""
}

func validYesNo(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "y", "yes", "n", "no":
		return true
	default:
		return false
	}
}

func isYes(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "y" || value == "yes"
}

func validRepositoryVisibility(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "private", "public", "internal":
		return true
	default:
		return false
	}
}

func absolutePath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}
