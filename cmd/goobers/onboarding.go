package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/agentkit"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/samples"
)

const (
	onboardingActionVersion       = 2
	onboardingIssueSeedVersion    = 1 // Keep stable so envelope changes do not duplicate starter issues.
	seedConfigSourceAction        = "seed-config-source"
	stubAgentInstructionsAction   = "stub-agent-instructions"
	stubSampleAction              = "stub-sample"
	stubSampleRoot                = "getting-started-task-api"
	defaultWorkTrackingTokenEnv   = "GOOBERS_GITHUB_ISSUES_TOKEN"
	stubSampleProviderTimeout     = 30 * time.Second
	stubSampleNextInstanceCommand = "goobers init --template=quickstart ./tutorial-instance"
)

const onboardingHelp = "Usage: goobers onboarding <command> [flags]\n\n" +
	"Run non-interactive, machine-readable onboarding actions. Actions never\n" +
	"prompt, write secrets, create a remote, or touch a repository that was not\n" +
	"explicitly named.\n\n" +
	"Commands:\n" +
	"  stub-agent-instructions  install agent assets into a config source\n" +
	"  stub-sample              materialize the getting-started sample tree\n\n" +
	"Run `goobers onboarding <command> -h` for action flags.\n"

const stubSampleHelp = "Usage: goobers onboarding stub-sample --destination <path> [--work-tracking <owner/repo>] [--token-env <NAME>] [--force] [--json]\n\n" +
	"Materialize the bundled getting-started sample into a destination tree. The\n" +
	"action writes only beneath the requested path, refuses conflicting user-owned\n" +
	"files unless `--force` is set, and optionally seeds starter work items when\n" +
	"both a repository target and a token are supplied.\n\n" +
	"Flags:\n" +
	"  --destination <path>     required sample destination\n" +
	"  --work-tracking <owner/repo>  optional GitHub repo to seed with starter issues\n" +
	"  --token-env <NAME>       issue token environment variable (default: GOOBERS_GITHUB_ISSUES_TOKEN)\n" +
	"  --force                  replace conflicting regular files\n" +
	"  --json                   emit the versioned action envelope\n\n" +
	"Exit codes: 0 = created or refreshed the sample, 1 = write or seeding error, 2 = usage error.\n"

const stubAgentInstructionsHelp = "Usage: goobers onboarding stub-agent-instructions --source-tree <path> [--harness <name>] [--json]\n\n" +
	"Install the release-matched Goobers agent toolkit and the selected harness\n" +
	"instruction reference into a checked-in config source repository. This action\n" +
	"delegates to `agent-kit install`: product-owned toolkit files are installed\n" +
	"beneath `.goobers/agent-toolkit/`, existing user instructions are preserved,\n" +
	"and collisions or drift fail without overwriting files.\n\n" +
	"Flags:\n" +
	"  --source-tree <path>  required config source repository root\n" +
	"  --harness <name>      required: copilot, claude, or generic\n" +
	"  --json                emit the versioned config-source action envelope\n\n" +
	"Exit codes: 0 = installed or already current, 1 = unsafe target, collision,\n" +
	"drift, or write error, 2 = usage error.\n"

type onboardingActionResult struct {
	Action      string   `json:"action"`
	Version     int      `json:"version"`
	Created     []string `json:"created"`
	Updated     []string `json:"updated,omitempty"`
	Skipped     []string `json:"skipped"`
	Path        string   `json:"path"`
	NextCommand string   `json:"nextCommand"`
	Prompts     []string `json:"prompts,omitempty"`
	Commands    []string `json:"commands,omitempty"`
}

type agentToolkitInstallActionResult struct {
	Action  onboardingActionResult
	Install agentkit.InstallResult
}

func executeSeedConfigSourceAction(
	root string,
	harness string,
	guided *instance.GuidedOptions,
	goos string,
) (onboardingActionResult, error) {
	var (
		seeded *instance.ConfigSourceSeedResult
		err    error
	)
	if guided == nil {
		seeded, err = instance.SeedQuickstartConfigSourceWithOptions(root, instance.QuickstartOptions{Harness: harness})
	} else {
		if err := instance.CheckGuidedSourceTarget(root); err != nil {
			return onboardingActionResult{}, err
		}
		seeded, err = instance.SeedGuidedConfigSource(root, *guided)
	}
	if err != nil {
		return onboardingActionResult{}, err
	}
	absolute := absolutePath(seeded.Root)
	return onboardingActionResult{
		Action:      seedConfigSourceAction,
		Version:     onboardingActionVersion,
		Created:     seeded.Created,
		Skipped:     seeded.Skipped,
		Path:        absolute,
		NextCommand: "goobers validate --source-tree --json " + quoteShellArg(absolute, goos),
	}, nil
}

type onboardingSeedCatalog struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Sample        onboardingSeedSample      `json:"sample"`
	Labels        []providers.WorkItemLabel `json:"labels"`
	Issues        []onboardingSeedIssue     `json:"issues"`
}

type onboardingSeedSample struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type onboardingSeedIssue struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Labels   []string `json:"labels"`
	Assignee string   `json:"assignee,omitempty"`
}

type onboardingSampleFile struct {
	path string
	data []byte
}

type onboardingIssueSeeder interface {
	EnsureWorkItemLabels(context.Context, providers.RepositoryRef, []providers.WorkItemLabel) (providers.EnsureWorkItemLabelsResult, error)
	ListWorkItems(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	CreateWorkItem(context.Context, providers.CreateWorkItemRequest) (providers.WorkItem, error)
}

var newOnboardingIssueSeeder = func(token string) onboardingIssueSeeder {
	return providers.NewGitHubProvider(token)
}

var beforeOnboardingSamplePublish = func() {}

func runOnboarding(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && isHelpArg(args[0]) {
		pf(stdout, "%s", onboardingHelp)
		return 0
	}
	if len(args) > 0 {
		pf(stderr, "error: unknown onboarding command %q\n", args[0])
	}
	pf(stderr, "%s", onboardingHelp)
	return 2
}

func runOnboardingStubAgentInstructions(args []string, stdout, stderr io.Writer) int {
	flags := newCLIFlagSet("onboarding stub-agent-instructions", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sourceTree := flags.String("source-tree", "", "config source repository root")
	harness := flags.String("harness", "", "harness adapter: copilot, claude, or generic")
	jsonOutput := flags.Bool("json", false, "emit the versioned config-source action envelope")
	flags.Usage = helpUsage(stderr, "onboarding stub-agent-instructions")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*sourceTree) == "" || strings.TrimSpace(*harness) == "" {
		flags.Usage()
		return 2
	}
	if !supportedAgentKitHarness(*harness) {
		pf(stderr, "error: unsupported harness %q (want copilot, claude, or generic)\n", *harness)
		return 2
	}
	executed, err := executeAgentToolkitInstallAction(*sourceTree, *harness, runtime.GOOS)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	if *jsonOutput {
		if err := encodeSchemaJSON(stdout, schemas.OnboardingAction, executed.Action); err != nil {
			pf(stderr, "error: encode onboarding action result: %v\n", err)
			return 1
		}
		return 0
	}
	printOnboardingActionResult(stdout, executed.Action)
	writeAgentKitNextSteps(stdout, executed.Action.Path, "")
	return 0
}

func executeAgentToolkitInstallAction(
	sourceTree, harness, goos string,
) (agentToolkitInstallActionResult, error) {
	if !supportedAgentKitHarness(harness) {
		return agentToolkitInstallActionResult{}, fmt.Errorf(
			"unsupported harness %q (want copilot, claude, or generic)",
			harness,
		)
	}
	absolute, err := filepath.Abs(sourceTree)
	if err != nil {
		return agentToolkitInstallActionResult{}, fmt.Errorf("resolve config source path: %w", err)
	}
	if _, err := instance.LoadGuidedSourceConfig(absolute); err != nil {
		return agentToolkitInstallActionResult{}, fmt.Errorf(
			"agent toolkit destination %s is not a valid Goobers config source: %w",
			absolute,
			err,
		)
	}
	if _, report, err := instance.LoadConfigDir(absolute); err != nil {
		return agentToolkitInstallActionResult{}, fmt.Errorf(
			"agent toolkit destination %s is not a valid Goobers config source: %w (report: %+v)",
			absolute,
			err,
			report,
		)
	}

	bundle, err := currentAgentToolkitBundle()
	if err != nil {
		return agentToolkitInstallActionResult{}, fmt.Errorf("build bundled agent toolkit: %w", err)
	}
	repository, err := agentkit.OpenRepository(absolute)
	if err != nil {
		return agentToolkitInstallActionResult{}, err
	}
	installed, err := repository.Install(bundle, harness)
	if err != nil {
		return agentToolkitInstallActionResult{}, err
	}
	report, err := repository.Check(bundle)
	if err != nil {
		return agentToolkitInstallActionResult{}, fmt.Errorf("verify installed agent toolkit: %w", err)
	}
	if report.State != "current" {
		return agentToolkitInstallActionResult{}, fmt.Errorf(
			"verify installed agent toolkit: state is %s",
			report.State,
		)
	}

	result := onboardingActionResult{
		Action:      stubAgentInstructionsAction,
		Version:     onboardingActionVersion,
		Created:     []string{},
		Skipped:     []string{},
		Path:        absolute,
		NextCommand: "goobers agent-kit check " + quoteShellArg(absolute, goos),
		Prompts:     agentKitStarterPrompts(""),
		Commands:    agentKitMaintenanceCommands(absolute, goos),
	}
	if installed.Installed {
		result.Created = append(result.Created, agentkit.InstalledRoot)
	} else {
		result.Skipped = append(result.Skipped, agentkit.InstalledRoot)
	}
	if installed.InstructionCreated || installed.InstructionUpdated {
		result.Created = append(result.Created, installed.InstructionPath)
	} else {
		result.Skipped = append(result.Skipped, installed.InstructionPath)
	}
	return agentToolkitInstallActionResult{Action: result, Install: installed}, nil
}

func loadOnboardingSample() ([]onboardingSampleFile, onboardingSeedCatalog, error) {
	root, err := fs.Sub(samples.Files, stubSampleRoot)
	if err != nil {
		return nil, onboardingSeedCatalog{}, fmt.Errorf("open embedded sample: %w", err)
	}
	var files []onboardingSampleFile
	if err := fs.WalkDir(root, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("embedded sample contains symbolic link %s", path)
		}
		data, err := fs.ReadFile(root, path)
		if err != nil {
			return err
		}
		files = append(files, onboardingSampleFile{path: filepath.ToSlash(path), data: data})
		return nil
	}); err != nil {
		return nil, onboardingSeedCatalog{}, fmt.Errorf("read embedded sample: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	seedData, err := fs.ReadFile(root, "seed-issues.json")
	if err != nil {
		return nil, onboardingSeedCatalog{}, fmt.Errorf("read embedded seed issues: %w", err)
	}
	var catalog onboardingSeedCatalog
	if err := json.Unmarshal(seedData, &catalog); err != nil {
		return nil, onboardingSeedCatalog{}, fmt.Errorf("decode embedded seed issues: %w", err)
	}
	if catalog.SchemaVersion != 1 || catalog.Sample.ID == "" || catalog.Sample.Version == "" {
		return nil, onboardingSeedCatalog{}, fmt.Errorf("embedded seed issues have an invalid version contract")
	}
	for _, issue := range catalog.Issues {
		if issue.ID == "" || issue.Title == "" {
			return nil, onboardingSeedCatalog{}, fmt.Errorf("embedded seed issue id and title are required")
		}
	}
	return files, catalog, nil
}

func materializeOnboardingSample(
	destination string,
	files []onboardingSampleFile,
	force bool,
) (onboardingActionResult, error) {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return onboardingActionResult{}, fmt.Errorf("resolve destination: %w", err)
	}
	result := onboardingActionResult{
		Action:      stubSampleAction,
		Version:     onboardingActionVersion,
		Created:     []string{},
		Skipped:     []string{},
		Path:        absolute,
		NextCommand: stubSampleNextInstanceCommand,
	}

	destinationRoot, err := openOnboardingDestination(absolute, false)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return onboardingActionResult{}, err
	}
	defer func() {
		if destinationRoot != nil {
			_ = destinationRoot.Close()
		}
	}()

	write := make([]bool, len(files))
	replace := make([]bool, len(files))
	previousMode := make([]fs.FileMode, len(files))
	for i, file := range files {
		relative := filepath.FromSlash(file.path)
		if file.path == "." || file.path == "" || !filepath.IsLocal(relative) {
			return onboardingActionResult{}, fmt.Errorf("embedded sample path %q is unsafe", file.path)
		}
		plan, err := planOnboardingSampleFile(destinationRoot, absolute, relative, file.data, force)
		if err != nil {
			return onboardingActionResult{}, err
		}
		write[i] = plan.write
		replace[i] = plan.replace
		previousMode[i] = plan.previousMode
		if plan.write {
			result.Created = append(result.Created, file.path)
		} else {
			result.Skipped = append(result.Skipped, file.path)
		}
	}

	hasWrites := false
	for _, needed := range write {
		hasWrites = hasWrites || needed
	}
	if !hasWrites {
		return result, nil
	}

	beforeOnboardingSamplePublish()
	if destinationRoot == nil {
		destinationRoot, err = openOnboardingDestination(absolute, true)
		if err != nil {
			return onboardingActionResult{}, err
		}
	} else if err := validateOnboardingDestinationBinding(absolute, destinationRoot); err != nil {
		return onboardingActionResult{}, err
	}

	for i, file := range files {
		if !write[i] {
			continue
		}
		relative := filepath.FromSlash(file.path)
		parent, err := openOnboardingSampleParent(destinationRoot, filepath.Dir(relative), true)
		if err != nil {
			return onboardingActionResult{}, err
		}
		target := filepath.Join(absolute, relative)
		err = writeOnboardingSampleFile(parent, filepath.Base(relative), file.data, replace[i], previousMode[i])
		_ = parent.Close()
		if err != nil {
			return onboardingActionResult{}, fmt.Errorf("write %s: %w", target, err)
		}
	}
	return result, nil
}

type onboardingSampleFilePlan struct {
	write        bool
	replace      bool
	previousMode fs.FileMode
}

func planOnboardingSampleFile(
	root *os.Root,
	destination, relative string,
	data []byte,
	force bool,
) (onboardingSampleFilePlan, error) {
	if root == nil {
		return onboardingSampleFilePlan{write: true}, nil
	}
	parent, err := openOnboardingSampleParent(root, filepath.Dir(relative), false)
	if errors.Is(err, fs.ErrNotExist) {
		return onboardingSampleFilePlan{write: true}, nil
	}
	if err != nil {
		return onboardingSampleFilePlan{}, err
	}
	defer func() { _ = parent.Close() }()

	name := filepath.Base(relative)
	target := filepath.Join(destination, relative)
	info, err := parent.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return onboardingSampleFilePlan{write: true}, nil
	case err != nil:
		return onboardingSampleFilePlan{}, fmt.Errorf("inspect %s: %w", target, err)
	case info.Mode()&fs.ModeSymlink != 0:
		return onboardingSampleFilePlan{}, fmt.Errorf("refusing symbolic link %s", target)
	case !info.Mode().IsRegular():
		return onboardingSampleFilePlan{}, fmt.Errorf("refusing non-regular file %s", target)
	}

	current, err := readOnboardingSampleFile(parent, name, info)
	if err != nil {
		return onboardingSampleFilePlan{}, fmt.Errorf("read %s: %w", target, err)
	}
	if bytes.Equal(current, data) {
		return onboardingSampleFilePlan{}, nil
	}
	if !force {
		return onboardingSampleFilePlan{}, fmt.Errorf("refusing to replace user-owned file %s without --force", target)
	}
	return onboardingSampleFilePlan{write: true, replace: true, previousMode: info.Mode().Perm()}, nil
}

func readOnboardingSampleFile(root *os.Root, name string, expected fs.FileInfo) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	current, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if current.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symbolic link %s", name)
	}
	if !current.Mode().IsRegular() || !opened.Mode().IsRegular() ||
		!os.SameFile(expected, current) || !os.SameFile(expected, opened) {
		return nil, fmt.Errorf("file changed while opening %s", name)
	}
	return io.ReadAll(file)
}

func writeOnboardingSampleFile(root *os.Root, target string, data []byte, replace bool, previousMode fs.FileMode) error {
	staged, stagedName, err := createOnboardingStagedFile(root, target)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(stagedName) }()
	if err := staged.Chmod(0o644); err != nil {
		_ = staged.Close()
		return fmt.Errorf("set staged file mode: %w", err)
	}
	if _, err := staged.Write(data); err != nil {
		_ = staged.Close()
		return fmt.Errorf("write staged file: %w", err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("close staged file: %w", err)
	}

	restoreMode := false
	if replace && runtime.GOOS == "windows" && previousMode.Perm()&0o200 == 0 {
		// Windows cannot replace a destination carrying the read-only attribute.
		info, err := root.Lstat(target)
		if err != nil {
			return fmt.Errorf("inspect existing file: %w", err)
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular file %s", target)
		}
		if err := root.Chmod(target, previousMode.Perm()|0o200); err != nil {
			return fmt.Errorf("make existing file replaceable: %w", err)
		}
		restoreMode = true
	}
	var publishErr error
	if replace {
		publishErr = root.Rename(stagedName, target)
	} else {
		publishErr = root.Link(stagedName, target)
	}
	if publishErr != nil {
		action := "publish staged file without replacing destination"
		if replace {
			action = "replace with staged file"
		}
		replaceErr := fmt.Errorf("%s: %w", action, publishErr)
		if restoreMode {
			if restoreErr := root.Chmod(target, previousMode.Perm()); restoreErr != nil {
				return errors.Join(replaceErr, fmt.Errorf("restore existing file mode: %w", restoreErr))
			}
		}
		return replaceErr
	}
	return nil
}

func createOnboardingStagedFile(root *os.Root, target string) (*os.File, string, error) {
	var random [8]byte
	for range 100 {
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", fmt.Errorf("create staged file name: %w", err)
		}
		name := "." + target + "-" + hex.EncodeToString(random[:])
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("create staged file: %w", err)
		}
		return file, name, nil
	}
	return nil, "", fmt.Errorf("create staged file: exhausted unique names")
}

func openOnboardingDestination(destination string, create bool) (*os.Root, error) {
	volumeRoot := filepath.VolumeName(destination) + string(filepath.Separator)
	relative, err := filepath.Rel(volumeRoot, destination)
	if err != nil {
		return nil, fmt.Errorf("resolve destination %s: %w", destination, err)
	}
	if relative != "." && !filepath.IsLocal(relative) {
		return nil, fmt.Errorf("destination %s is outside volume root %s", destination, volumeRoot)
	}
	current, err := os.OpenRoot(volumeRoot)
	if err != nil {
		return nil, fmt.Errorf("open destination volume %s: %w", volumeRoot, err)
	}
	if relative == "." {
		return current, nil
	}

	parts := strings.Split(relative, string(filepath.Separator))
	display := volumeRoot
	for i, part := range parts {
		display = filepath.Join(display, part)
		description := "destination ancestor"
		if i == len(parts)-1 {
			description = "destination"
		}
		next, err := openStableOnboardingDirectory(current, part, display, description, create)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

func openOnboardingSampleParent(root *os.Root, relative string, create bool) (*os.Root, error) {
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, fmt.Errorf("open destination: %w", err)
	}
	if relative == "." {
		return current, nil
	}
	display := root.Name()
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		display = filepath.Join(display, part)
		next, err := openStableOnboardingDirectory(current, part, display, "sample parent", create)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

func openStableOnboardingDirectory(
	parent *os.Root,
	name, display, description string,
	create bool,
) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) && create {
		if err := parent.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("create %s %s: %w", description, display, err)
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect %s %s: %w", description, display, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symbolic-link %s %s", description, display)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s %s is not a directory", description, display)
	}

	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open %s %s: %w", description, display, err)
	}
	opened, err := child.Stat(".")
	if err != nil {
		_ = child.Close()
		return nil, fmt.Errorf("inspect opened %s %s: %w", description, display, err)
	}
	current, err := parent.Lstat(name)
	if err != nil {
		_ = child.Close()
		return nil, fmt.Errorf("reinspect %s %s: %w", description, display, err)
	}
	if current.Mode()&fs.ModeSymlink != 0 {
		_ = child.Close()
		return nil, fmt.Errorf("refusing symbolic-link %s %s", description, display)
	}
	if !current.IsDir() || !os.SameFile(info, current) || !os.SameFile(info, opened) {
		_ = child.Close()
		return nil, fmt.Errorf("%s %s changed while opening", description, display)
	}
	return child, nil
}

func validateOnboardingDestinationBinding(destination string, expected *os.Root) error {
	current, err := openOnboardingDestination(destination, false)
	if err != nil {
		return err
	}
	defer func() { _ = current.Close() }()
	expectedInfo, err := expected.Stat(".")
	if err != nil {
		return fmt.Errorf("inspect opened destination %s: %w", destination, err)
	}
	currentInfo, err := current.Stat(".")
	if err != nil {
		return fmt.Errorf("inspect current destination %s: %w", destination, err)
	}
	if !os.SameFile(expectedInfo, currentInfo) {
		return fmt.Errorf("destination %s changed after preflight", destination)
	}
	return nil
}

func appendPendingSeedIssues(result *onboardingActionResult, catalog onboardingSeedCatalog, reason string) {
	for _, issue := range catalog.Issues {
		result.Skipped = append(result.Skipped, fmt.Sprintf("issue:%s (pending: %s)", issue.ID, reason))
	}
}

func seedOnboardingIssues(
	ctx context.Context,
	seeder onboardingIssueSeeder,
	repository providers.RepositoryRef,
	catalog onboardingSeedCatalog,
	result *onboardingActionResult,
) error {
	return seedOnboardingIssuesAs(ctx, seeder, repository, catalog, stubSampleAction, result)
}

// seedOnboardingIssuesAs is seedOnboardingIssues with the action segment of
// the dedupe run-id made explicit, so `goobers connect --seed` reuses the
// same idempotent machinery under its own marker namespace.
func seedOnboardingIssuesAs(
	ctx context.Context,
	seeder onboardingIssueSeeder,
	repository providers.RepositoryRef,
	catalog onboardingSeedCatalog,
	action string,
	result *onboardingActionResult,
) error {
	labels, err := seeder.EnsureWorkItemLabels(ctx, repository, catalog.Labels)
	if err != nil {
		return err
	}
	for _, name := range labels.Created {
		result.Created = append(result.Created, "label:"+name)
	}
	for _, name := range labels.Skipped {
		result.Skipped = append(result.Skipped, "label:"+name)
	}

	existing, err := seeder.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: repository,
		State:      "all",
	})
	if err != nil {
		return err
	}
	for _, issue := range catalog.Issues {
		runID := onboardingSeedRunID(catalog, issue, action)
		marker := "goobers run-id: " + runID
		found := false
		for _, item := range existing {
			if strings.Contains(item.Body, marker) {
				found = true
				break
			}
		}
		if found {
			result.Skipped = append(result.Skipped, "issue:"+issue.ID)
			continue
		}
		if _, err := seeder.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
			Repository: repository,
			Title:      issue.Title,
			Body:       issue.Body,
			Labels:     append([]string(nil), issue.Labels...),
			Assignee:   issue.Assignee,
			RunID:      runID,
		}); err != nil {
			return fmt.Errorf("create issue %q: %w", issue.ID, err)
		}
		result.Created = append(result.Created, "issue:"+issue.ID)
	}
	return nil
}

func onboardingSeedRunID(catalog onboardingSeedCatalog, issue onboardingSeedIssue, action string) string {
	return fmt.Sprintf(
		"onboarding/%s/v%d/%s@%s/%s",
		action,
		onboardingIssueSeedVersion,
		catalog.Sample.ID,
		catalog.Sample.Version,
		issue.ID,
	)
}

func printOnboardingActionResult(stdout io.Writer, result onboardingActionResult) {
	pf(stdout, "sample target: %s\n", result.Path)
	for _, item := range result.Created {
		pf(stdout, "  created  %s\n", item)
	}

	for _, item := range result.Skipped {
		if strings.Contains(item, "(pending:") {
			pf(stdout, "  pending  %s\n", item)
		} else {
			pf(stdout, "  skipped  %s\n", item)
		}
	}
	pf(stdout, "next: %s\n", result.NextCommand)
}

func runOnboardingStubSample(args []string, stdout, stderr io.Writer) int {
	flags := newCLIFlagSet("onboarding stub-sample", flag.ContinueOnError)
	flags.SetOutput(stderr)
	destination := flags.String("destination", "", "sample destination")
	workTracking := flags.String("work-tracking", "", "GitHub owner/repo to seed")
	tokenEnv := flags.String("token-env", defaultWorkTrackingTokenEnv, "issue token environment variable")
	force := flags.Bool("force", false, "replace conflicting regular files")
	jsonOutput := flags.Bool("json", false, "emit the versioned action envelope")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*destination) == "" {
		return 2
	}
	if !instance.ValidGuidedTokenEnvName(*tokenEnv) {
		pf(stderr, "error: --token-env must name a valid environment variable\n")
		return 2
	}

	var repository *providers.RepositoryRef
	if strings.TrimSpace(*workTracking) != "" {
		owner, name, err := parseGitHubRepo(*workTracking)
		if err != nil {
			pf(stderr, "error: --work-tracking: %v\n", err)
			return 2
		}
		repository = &providers.RepositoryRef{
			Provider: providers.ProviderGitHub,
			Owner:    owner,
			Name:     name,
		}
	}

	files, catalog, err := loadOnboardingSample()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	result, err := materializeOnboardingSample(*destination, files, *force)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	switch {
	case repository == nil:
		appendPendingSeedIssues(&result, catalog, "work-tracking target not supplied")
	case os.Getenv(*tokenEnv) == "":
		appendPendingSeedIssues(&result, catalog, "credentials unavailable")
	default:
		ctx, cancel := context.WithTimeout(context.Background(), stubSampleProviderTimeout)
		defer cancel()
		if err := seedOnboardingIssues(ctx, newOnboardingIssueSeeder(os.Getenv(*tokenEnv)), *repository, catalog, &result); err != nil {
			pf(stderr, "error: seed %s/%s: %v\n", repository.Owner, repository.Name, err)
			return 1
		}
	}

	if *jsonOutput {
		if err := encodeSchemaJSON(stdout, schemas.OnboardingAction, result); err != nil {
			pf(stderr, "error: encode action result: %v\n", err)
			return 1
		}
		return 0
	}
	printOnboardingActionResult(stdout, result)
	return 0
}
