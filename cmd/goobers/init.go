package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runnercap"
	"github.com/goobers/goobers/internal/version"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const initHelp = "Usage: goobers init [--demo [--insecure] | --template=quickstart [--source-tree <path> [--json]]] [path]\n\n" +
	"Scaffold an instance root at path (default \".\"): instance.yaml, config/\n" +
	"(seeded with a starter example), gaggles/, scheduler/, and a telemetry.db\n" +
	"placeholder. The daemon creates per-gaggle runs/ and workcopies/ under\n" +
	"gaggles/<gaggle>/ at runtime. Re-running is safe — existing pieces are left\n" +
	"untouched.\n" +
	"--template=quickstart seeds the versioned onboarding workflow; it is\n" +
	"intentionally not production-safe. With --source-tree <path>, it instead\n" +
	"seeds the checked-in source layout (instance.yaml.example, manifest.yaml,\n" +
	"and gaggles/) without runtime state. The source-tree action is non-interactive,\n" +
	"preserves every existing file, and reports each created or skipped path;\n" +
	"--json emits its versioned machine-readable result envelope. --demo seeds a hermetic mock-provider full-loop tour\n" +
	"requiring no repo, provider credentials, model tokens, or network writes. The\n" +
	"demo is supported on Linux and macOS, where network isolation is enforced; it is\n" +
	"fail-closed on Windows (no enforced network:none equivalent exists there) unless\n" +
	"--insecure is also given, which scaffolds the demo anyway and reports the\n" +
	"isolation limitation — an explicit, narrowly-scoped opt-in that does not alter\n" +
	"the general Windows sandbox policy (#651). Use `goobers preflight` to check and\n" +
	"launch the fully isolated WSL 2 route instead. --insecure requires --demo.\n"

func runInit(args []string, stdout, stderr io.Writer) int {
	return runInitWithInput(args, os.Stdin, stdout, stderr)
}

func runInitWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runInitWithInputForOS(args, stdin, stdout, stderr, runtime.GOOS)
}

func runInitWithInputForOS(args []string, stdin io.Reader, stdout, stderr io.Writer, goos string) int {
	return runInitWithInputForOSAndGitHub(args, stdin, stdout, stderr, goos, defaultGuidedGitHubOperations{})
}

func runInitWithInputForOSAndGitHub(
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	goos string,
	github guidedGitHubOperations,
) int {
	for _, arg := range args {
		if arg == "--guided" || strings.HasPrefix(arg, "--guided=") {
			pf(stderr, "error: `goobers init --guided` has been removed.\n\n")
			pf(stderr, "Web wizard:\n  goobers getting-started\n\n")
			pf(stderr, "Agent CLI prompt:\n  \"Use the Goobers Getting Started skill to inspect my repository, derive its default branch, CI command, toolchain, and conventions, and create the smallest validated configuration. Explain each write and ask only when required evidence or behavior cannot be safely derived.\"\n")
			return 2
		}
	}
	fs := newCLIFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	demo := fs.Bool("demo", false, "seed a credential-free runnable demo workflow")
	insecure := fs.Bool("insecure", false, "with --demo on a platform without enforced network isolation (Windows), scaffold anyway without it")
	guided := new(bool)
	template := fs.String("template", "", "seed a named onboarding template (available: quickstart)")
	sourceTree := fs.String("source-tree", "", "seed the selected template as a checked-in config source at path")
	asJSON := fs.Bool("json", false, "emit the config-source action result as JSON")
	fs.Usage = helpUsage(stderr, "init")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sourceTreeSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "source-tree" {
			sourceTreeSet = true
		}
	})
	selectedModes := 0
	for _, selected := range []bool{*demo, *template != ""} {
		if selected {
			selectedModes++
		}
	}
	if selectedModes > 1 {
		pf(stderr, "error: --demo and --template cannot be combined\n")
		return 2
	}
	if *insecure && !*demo {
		pf(stderr, "error: --insecure requires --demo\n")
		return 2
	}
	if *template != "" && *template != instance.QuickstartTemplate {
		pf(stderr, "error: unknown init template %q (available: %s)\n", *template, instance.QuickstartTemplate)
		return 2
	}
	if sourceTreeSet && *sourceTree == "" {
		pf(stderr, "error: --source-tree destination must not be empty\n")
		return 2
	}
	if *sourceTree != "" && *template != instance.QuickstartTemplate {
		pf(stderr, "error: --source-tree requires --template=%s\n", instance.QuickstartTemplate)
		return 2
	}
	if *asJSON && *sourceTree == "" {
		pf(stderr, "error: --json is supported by init only with --source-tree\n")
		return 2
	}
	if *sourceTree != "" && fs.NArg() != 0 {
		pf(stderr, "error: --source-tree supplies the destination; do not also pass [path]\n")
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	demoUnisolated := *demo && goos != "linux" && goos != "darwin"
	if demoUnisolated && !*insecure {
		pf(stderr, "error: --demo is supported only on Linux and macOS because enforced network isolation is unavailable on %s; run `goobers preflight` for the fully isolated WSL 2 route, or pass --insecure to proceed without isolation\n", goos)
		return 2
	}
	if *sourceTree != "" {
		return seedQuickstartConfigSource(*sourceTree, *asJSON, stdout, stderr, goos)
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	if *guided {
		if err := instance.CheckGuidedInitTarget(root); err != nil {
			pf(stderr, "error: %v\n", err)
			printDefaultedTargetNote(stderr, err, fs.NArg())
			return 2
		}
	}

	var res *instance.InitResult
	var guidedResult guidedInitResult
	var err error
	errCode := 2
	if *guided {
		res, guidedResult, errCode, err = runGuidedInit(root, stdin, stdout, stderr, github)
	} else if *template == instance.QuickstartTemplate {
		res, err = instance.InitQuickstart(root)
	} else if *demo {
		res, err = instance.InitDemo(root)
	} else {
		res, err = instance.Init(root)
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		printDefaultedTargetNote(stderr, err, fs.NArg())
		return errCode
	}

	abs, err := filepath.Abs(res.Root)
	if err != nil {
		abs = res.Root
	}
	if len(res.Created) == 0 {
		pf(stdout, "instance already initialized at %s (nothing to do)\n", abs)
		if demoUnisolated {
			pln(stdout, demoInsecureWarning)
		}
		if *guided {
			if code := finishGuidedInit(root, abs, guidedResult, stdout, stderr); code != 0 {
				return code
			}
		} else if code := finishInitValidation(root, stdout, stderr); code != 0 {
			return code
		}
		if err := ensureInitCompleted(root); err != nil {
			pf(stderr, "error: record successful init completion: %v\n", err)
			return 2
		}
		return 0
	}
	pf(stdout, "initialized instance at %s\n", abs)
	for _, c := range res.Created {
		pf(stdout, "  created  %s\n", c)
	}
	for _, s := range res.Skipped {
		pf(stdout, "  skipped  %s (already exists)\n", s)
	}
	pf(stdout, "\nLearn the desired-state model: %s\n", documentationURL("docs/concepts/README.md"))
	demoSeeded := false
	for _, created := range res.Created {
		if created == instance.ConfigDirName {
			demoSeeded = true
			break
		}
	}
	if *demo && demoSeeded {
		if demoUnisolated {
			pln(stdout, demoInsecureWarning)
		}
		pf(stdout, demoTourBanner, abs)
	}
	if *guided {
		if code := finishGuidedInit(root, abs, guidedResult, stdout, stderr); code != 0 {
			return code
		}
	} else if code := finishInitValidation(root, stdout, stderr); code != 0 {
		return code
	}
	if err := ensureInitCompleted(root); err != nil {
		pf(stderr, "error: record successful init completion: %v\n", err)
		return 2
	}
	return 0
}

func finishInitValidation(root string, stdout, stderr io.Writer) int {
	pln(stdout, "\nPost-init validation:")
	if code := runValidate([]string{root}, stdout, stderr); code != 0 {
		pf(stderr, "error: initialized instance did not pass validation\n")
		return code
	}

	layout := instance.NewLayout(root)
	findings, err := findTemplatePlaceholders(root, layout.ConfigFile(), layout.ConfigDir())
	if err != nil {
		pf(stderr, "error: inspect initialized configuration placeholders: %v\n", err)
		return 2
	}
	if len(findings) == 0 {
		pln(stdout, "\nNext: no placeholder edits are required.")
		return 0
	}
	pln(stdout, "\nNext: edit these files before running a live workflow:")
	seen := make(map[string]bool, len(findings))
	for _, finding := range findings {
		if seen[finding.file] {
			continue
		}
		seen[finding.file] = true
		pf(stdout, "  %s\n", finding.file)
	}
	return 0
}

// printDefaultedTargetNote explains, after an init target-conflict refusal,
// that the conflicting target was never chosen explicitly — init with no
// [path] defaults to the current directory, the exact trap of running it from
// inside a source checkout (#2513).
func printDefaultedTargetNote(stderr io.Writer, err error, narg int) {
	var conflict *instance.TargetConflictError
	if narg == 0 && errors.As(err, &conflict) {
		pf(stderr, "note: no [path] argument was given, so the target defaulted to the current directory\n")
	}
}

func ensureInitCompleted(root string) error {
	instanceLog, _, err := journal.OpenInstanceLog(instance.NewLayout(root).SchedulerDir())
	if err != nil {
		return err
	}
	events, err := journal.ReadInstanceLog(instanceLog.Dir())
	if err != nil {
		_ = instanceLog.Close()
		return err
	}
	for _, event := range events {
		if event.Type == journal.EventInitCompleted {
			return instanceLog.Close()
		}
	}
	if err := instanceLog.Append(journal.Event{Type: journal.EventInitCompleted}); err != nil {
		_ = instanceLog.Close()
		return err
	}
	return instanceLog.Close()
}

func seedQuickstartConfigSource(root string, asJSON bool, stdout, stderr io.Writer, goos string) int {
	envelope, err := executeSeedConfigSourceAction(root, nil, goos)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	var validationOutput strings.Builder
	if code := runValidate(
		[]string{"--source-tree", "--json", envelope.Path},
		&validationOutput,
		&validationOutput,
	); code != 0 {
		pf(stderr, "error: seeded config source failed validation\n%s", validationOutput.String())
		return code
	}
	if asJSON {
		if err := encodeSchemaJSON(stdout, schemas.OnboardingAction, envelope); err != nil {
			pf(stderr, "error: encode onboarding action result: %v\n", err)
			return 1
		}
		return 0
	}

	pf(stdout, "seeded quickstart config source at %s\n", envelope.Path)
	for _, created := range envelope.Created {
		pf(stdout, "  created  %s\n", created)
	}
	for _, skipped := range envelope.Skipped {
		pf(stdout, "  skipped  %s (already exists)\n", skipped)
	}
	pf(stdout, "\nNext: %s\n", envelope.NextCommand)
	return 0
}

func quoteShellArg(arg, goos string) string {
	if goos == "windows" {
		// nextCommand targets PowerShell, where single-quoted strings are literal.
		return "'" + strings.ReplaceAll(arg, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}

func finishGuidedInit(root, abs string, result guidedInitResult, stdout, stderr io.Writer) int {
	pln(stdout, "")
	if code := runValidate([]string{root}, stdout, stderr); code != 0 {
		pf(stderr, "error: guided setup did not produce a valid instance\n")
		return code
	}
	pf(stdout, `
Onboarding mapping:
  config-repo:  %s
  config-source: %s
  instance-root: %s
  target-repo:   %s
  backlog:       %s
  mapping:       %s -> %s -> %s

After editing the checked-in source, validate and materialize it before startup:
  goobers validate --source-tree %s
  goobers config materialize %s
  goobers up %s
`,
		result.ConfigRepo,
		result.SourceRoot,
		abs,
		result.TargetRepo,
		result.Backlog,
		result.SourceRoot,
		filepath.Join(abs, instance.GagglesDirName, result.Gaggle),
		result.TargetRepo,
		strconv.Quote(result.SourceRoot),
		strconv.Quote(abs),
		strconv.Quote(abs),
	)
	if result.RemoteCreated {
		pf(stdout, `
The GitHub config repository is empty; no commit or push was performed:
  git -C %s init
  git -C %s remote add origin %s
`, strconv.Quote(result.SourceRoot), strconv.Quote(result.SourceRoot), strconv.Quote(result.ConfigRepo+".git"))
	}
	pf(
		stdout,
		guidedDocsBanner,
		abs,
		documentationURL("docs/guides/dsl-authoring-skill.md"),
		documentationURL("docs/requirements/goober.md"),
		documentationURL("docs/stage-contract.md"),
		documentationURL("docs/cli/README.md"),
	)
	return 0
}

type guidedPrompter struct {
	reader *bufio.Reader
	out    io.Writer
}

func promptGuidedOptions(stdin io.Reader, stdout io.Writer) (instance.GuidedOptions, error) {
	p := guidedPrompter{reader: bufio.NewReader(stdin), out: stdout}
	return promptGuidedOptionsWithPrompter(p)
}

func promptGuidedOptionsWithPrompter(p guidedPrompter) (instance.GuidedOptions, error) {
	stdout := p.out
	repoText, err := p.ask("Main GitHub repository (owner/name or URL)", "", validGitHubRepoInput)
	if err != nil {
		return instance.GuidedOptions{}, err
	}
	repoOwner, repoName, err := parseGitHubRepo(repoText)
	if err != nil {
		return instance.GuidedOptions{}, err
	}
	branch, err := p.ask("Default branch", "main", validBranch)
	if err != nil {
		return instance.GuidedOptions{}, err
	}

	pln(stdout, "")
	pf(stdout, "Work tracking: GitHub Issues in %s/%s (Azure DevOps is not yet supported).\n", repoOwner, repoName)
	pln(stdout, "The local runner currently requires code and work tracking in the same repository.")

	pln(stdout, "")
	pln(stdout, "Canonical workflows:")
	pln(stdout, "  1) implementation    issue -> implementation -> review -> CI -> PR")
	pln(stdout, "  2) backlog-curation  approved issues -> scoped ready work")
	pln(stdout, "  3) work-nomination   telemetry and code signals -> proposed issues")
	workflowText, err := p.ask("Select workflows (comma-separated names or numbers)", "all", validWorkflowSelection)
	if err != nil {
		return instance.GuidedOptions{}, err
	}
	workflows, err := parseWorkflowSelection(workflowText)
	if err != nil {
		return instance.GuidedOptions{}, err
	}
	var ciCommand []string
	var requiredCapabilities []string
	for _, workflow := range workflows {
		if workflow != instance.GuidedWorkflowImplementation {
			continue
		}
		cwd, getwdErr := os.Getwd()
		if getwdErr != nil {
			return instance.GuidedOptions{}, fmt.Errorf("resolve current directory for CI detection: %w", getwdErr)
		}
		stack, detected, detectedCapability := detectCICommandDefault(cwd)
		defaultCI := strings.Join(detected, " ")
		if stack != "" {
			pf(stdout, "Guessed %s from a build manifest in the current directory %s; confirm the target repository's local CI command and toolchain capability below.\n", stack, cwd)
		} else {
			pln(stdout, "No recognized build manifest (Makefile, go.mod, *.csproj/*.sln, package.json, pom.xml, build.gradle(.kts), Cargo.toml, Package.swift, pyproject.toml/setup.py/requirements.txt) found in the current directory; enter the target repository's local CI command and toolchain capability explicitly.")
		}
		ciText, promptErr := p.ask("Local CI command (space-separated argv or JSON array)", defaultCI, validCommand)
		if promptErr != nil {
			return instance.GuidedOptions{}, promptErr
		}
		ciCommand, err = parseCommand(ciText)
		if err != nil {
			return instance.GuidedOptions{}, err
		}
		capabilityText, promptErr := p.ask("Required toolchain capability", detectedCapability, runnercap.ValidToken)
		if promptErr != nil {
			return instance.GuidedOptions{}, promptErr
		}
		requiredCapabilities = []string{capabilityText}
		break
	}

	pln(stdout, "")
	pln(stdout, "Agent harness: every generated agentic goober uses the same one.")
	pln(stdout, "  1) copilot      GitHub Copilot CLI")
	pln(stdout, "  2) claude-code  Anthropic Claude Code CLI")
	harnessText, err := p.ask("Select harness (name or number)", "copilot", validHarnessSelection)
	if err != nil {
		return instance.GuidedOptions{}, err
	}
	harness, err := parseHarnessSelection(harnessText)
	if err != nil {
		return instance.GuidedOptions{}, err
	}

	pln(stdout, "")
	pln(stdout, "Create separate fine-grained, least-privilege PATs; never paste their values here.")
	pln(stdout, "  Create: https://github.com/settings/personal-access-tokens/new")
	pf(stdout, "  Scopes: %s\n", documentationURL("docs/guides/github-token-scopes.md"))
	pf(stdout, "  Repository access: select only %s/%s for repository-scoped PATs.\n", repoOwner, repoName)
	pln(stdout, "Repository read PAT permissions: Contents: Read-only.")
	repoTokenEnv, err := p.ask("Repository read PAT environment variable", "GOOBERS_GITHUB_REPO_TOKEN", instance.ValidGuidedTokenEnvName)
	if err != nil {
		return instance.GuidedOptions{}, err
	}
	pln(stdout, "Work-tracking PAT permissions: Issues: Read and write.")
	workTrackingTokenEnv, err := p.ask("Work-tracking PAT environment variable", "GOOBERS_GITHUB_ISSUES_TOKEN", instance.ValidGuidedTokenEnvName)
	if err != nil {
		return instance.GuidedOptions{}, err
	}

	pullRequestTokenEnv := ""
	needsPullRequests := slices.Contains(workflows, instance.GuidedWorkflowImplementation) ||
		slices.Contains(workflows, instance.GuidedWorkflowBacklogCuration)
	if needsPullRequests {
		if slices.Contains(workflows, instance.GuidedWorkflowImplementation) {
			pln(stdout, "Pull-request PAT permissions: Pull requests: Read and write; Contents: Read and write.")
			pln(stdout, "Implementation CI polling also requires: Checks: Read-only; Commit statuses: Read-only.")
		} else {
			pln(stdout, "Pull-request PAT permissions: Pull requests: Read-only.")
		}
		pullRequestTokenEnv, err = p.ask("Pull-request PAT environment variable", "GOOBERS_GITHUB_PR_TOKEN", instance.ValidGuidedTokenEnvName)
		if err != nil {
			return instance.GuidedOptions{}, err
		}
	}

	repoPushTokenEnv := ""
	if slices.Contains(workflows, instance.GuidedWorkflowImplementation) {
		pln(stdout, "Repository push PAT permissions: Contents: Read and write.")
		repoPushTokenEnv, err = p.ask("Repository push PAT environment variable", "GOOBERS_GITHUB_PUSH_TOKEN", instance.ValidGuidedTokenEnvName)
		if err != nil {
			return instance.GuidedOptions{}, err
		}
	}

	var copilotTokenEnv, claudeTokenEnv string
	switch apiv1.Harness(harness) {
	case apiv1.HarnessClaudeCode:
		pln(stdout, "Claude Code model auth: press Enter to use the current user's stored `claude auth login` sign-in.")
		pln(stdout, "For a headless service/CI account, enter an environment variable holding an Anthropic API key or OAuth token.")
		claudeTokenEnv, err = p.ask("Optional Claude Code token environment variable", "", func(value string) bool {
			return value == "" || instance.ValidGuidedTokenEnvName(value)
		})
		if err != nil {
			return instance.GuidedOptions{}, err
		}
	default:
		pln(stdout, "Copilot model auth: press Enter to use the current user's stored Copilot CLI sign-in.")
		pln(stdout, "For a headless service/CI account, enter an environment variable holding a Copilot Requests: Read-only PAT.")
		copilotTokenEnv, err = p.ask("Optional Copilot Requests PAT environment variable", "", func(value string) bool {
			return value == "" || instance.ValidGuidedTokenEnvName(value)
		})
		if err != nil {
			return instance.GuidedOptions{}, err
		}
	}

	return instance.GuidedOptions{
		GaggleName:           guidedGaggleName(repoName),
		DisplayName:          repoOwner + "/" + repoName,
		RepoOwner:            repoOwner,
		RepoName:             repoName,
		RepoBranch:           branch,
		RepoTokenEnv:         repoTokenEnv,
		WorkTrackingTokenEnv: workTrackingTokenEnv,
		PullRequestTokenEnv:  pullRequestTokenEnv,
		RepoPushTokenEnv:     repoPushTokenEnv,
		Harness:              harness,
		CopilotTokenEnv:      copilotTokenEnv,
		ClaudeTokenEnv:       claudeTokenEnv,
		Workflows:            workflows,
		CICommand:            ciCommand,
		RequiredCapabilities: requiredCapabilities,
	}, nil
}

func (p guidedPrompter) ask(label, defaultValue string, valid func(string) bool) (string, error) {
	for {
		if defaultValue == "" {
			pf(p.out, "%s: ", label)
		} else {
			pf(p.out, "%s [%s]: ", label, defaultValue)
		}
		line, err := p.reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read %s: %w", label, err)
		}
		value := strings.TrimSpace(line)
		if value == "" {
			value = defaultValue
		}
		if valid(value) {
			return value, nil
		}
		pf(p.out, "  Invalid value; try again.\n")
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read %s: input ended after an invalid value", label)
		}
	}
}

func validGitHubRepoInput(value string) bool {
	_, _, err := parseGitHubRepo(value)
	return err == nil
}

func parseGitHubRepo(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "git@github.com:") {
		value = strings.TrimPrefix(value, "git@github.com:")
	} else if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		if !strings.EqualFold(parsed.Host, "github.com") {
			return "", "", fmt.Errorf("repository URL host must be github.com")
		}
		value = strings.TrimPrefix(parsed.Path, "/")
	}
	value = strings.TrimSuffix(value, ".git")
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !githubRepoPart.MatchString(parts[0]) || !githubRepoPart.MatchString(parts[1]) {
		return "", "", fmt.Errorf("GitHub repository must be owner/name or a github.com URL")
	}
	return parts[0], parts[1], nil
}

var githubRepoPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func validBranch(value string) bool {
	if value == "" || value == "@" || strings.HasPrefix(value, "-") ||
		strings.HasSuffix(value, ".") ||
		strings.Contains(value, "..") || strings.Contains(value, "@{") ||
		strings.ContainsAny(value, " ~^:?*[\\") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validHarnessSelection(value string) bool {
	_, err := parseHarnessSelection(value)
	return err == nil
}

func parseHarnessSelection(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "copilot":
		return string(apiv1.HarnessCopilot), nil
	case "2", "claude-code", "claude":
		return string(apiv1.HarnessClaudeCode), nil
	default:
		return "", fmt.Errorf("invalid harness selection %q", value)
	}
}

func validWorkflowSelection(value string) bool {
	_, err := parseWorkflowSelection(value)
	return err == nil
}

func parseWorkflowSelection(value string) ([]string, error) {
	available := instance.GuidedWorkflowNames()
	if strings.EqualFold(strings.TrimSpace(value), "all") {
		return available, nil
	}
	byToken := make(map[string]string, len(available)*2)
	for i, name := range available {
		byToken[name] = name
		byToken[strconv.Itoa(i+1)] = name
	}
	selected := make(map[string]bool)
	for _, token := range strings.Split(value, ",") {
		token = strings.ToLower(strings.TrimSpace(token))
		name, ok := byToken[token]
		if !ok || selected[name] {
			return nil, fmt.Errorf("invalid workflow selection %q", value)
		}
		selected[name] = true
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("select at least one workflow")
	}
	result := make([]string, 0, len(selected))
	for _, name := range available {
		if selected[name] {
			result = append(result, name)
		}
	}
	return result, nil
}

func validCommand(value string) bool {
	_, err := parseCommand(value)
	return err == nil
}

func parseCommand(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	var command []string
	if strings.HasPrefix(value, "[") {
		if err := json.Unmarshal([]byte(value), &command); err != nil {
			return nil, fmt.Errorf("local CI command JSON: %w", err)
		}
	} else {
		command = strings.Fields(value)
	}
	if len(command) == 0 {
		return nil, fmt.Errorf("local CI command must name a program")
	}
	for _, arg := range command {
		if strings.TrimSpace(arg) == "" {
			return nil, fmt.Errorf("local CI command arguments must not be empty")
		}
	}
	return command, nil
}

func guidedGaggleName(repo string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(repo) {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			b.WriteRune(r)
			lastHyphen = false
		} else if !lastHyphen && b.Len() > 0 {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "repository"
	}
	if len(name) > 50 {
		name = strings.TrimRight(name[:50], "-")
	}
	return name
}

const guidedDocsBanner = `
Ready to run from %s:
  goobers up
  goobers run <workflow>

Developer docs:
  Author workflows:         %s
  Make custom agent stages: %s and %s
  View journal telemetry:   %s (` + "`goobers trace` / `goobers telemetry`" + `)
`

// releaseVersionPattern matches a real tagged release — stable
// (vMAJOR.MINOR.PATCH) or pre-release (vMAJOR.MINOR.PATCH-beta.2 and
// similar SemVer 2.0.0 suffixes) — as opposed to a "dev" or bare-commit
// build, which has no tag to link docs against.
var releaseVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)

func documentationURL(path string) string {
	ref := "main"
	if releaseVersionPattern.MatchString(version.Version) {
		ref = url.PathEscape(version.Version)
	}
	return fmt.Sprintf("https://github.com/Agent-Clubhouse/Goobers/blob/%s/%s", ref, path)
}

const demoTourBanner = `
Demo full loop (run these from %s):
  goobers run demo    # watch curate -> implement -> review -> merge preview
  goobers trace <id>  # inspect the journal and merge-preview artifact
`

// demoInsecureWarning is printed whenever --demo --insecure scaffolds a demo
// on a platform with no enforced network:none equivalent (issue #1545).
// Scaffolding is unconditional once --insecure opts in, but actually running
// the demo still requires the same trusted-local-execution env var every
// other network:none stage needs on Windows (internal/executor/
// network_windows.go) — this issue narrowly lifts the CLI-level refusal to
// even scaffold, it does not alter that general Windows sandbox policy.
const demoInsecureWarning = "\nwarning: demo scaffolded WITHOUT enforced network isolation — this platform\n" +
	"has no network:none equivalent. Before `goobers run demo`, set\n" +
	"GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE=1 for trusted-local execution only.\n" +
	"For full isolation instead, run `goobers preflight` and launch the command through WSL 2.\n"
