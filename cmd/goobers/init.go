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
)

const initHelp = "Usage: goobers init [--guided | --demo | --template=quickstart] [path]\n\n" +
	"Scaffold an instance root at path (default \".\"): instance.yaml, config/\n" +
	"(seeded with a starter example), runs/, scheduler/, workcopies/, and a\n" +
	"telemetry.db placeholder. Re-running without --guided is safe — existing\n" +
	"pieces are left untouched. --guided is first-run only and refuses a target\n" +
	"with instance.yaml or a populated config/ before prompting. It prompts for\n" +
	"a GitHub repository, work tracking, token references, and canonical workflows,\n" +
	"then validates the result. --template=quickstart seeds the versioned onboarding\n" +
	"workflow; it is intentionally not production-safe. --demo seeds a hermetic mock-provider full-loop tour\n" +
	"requiring no repo, provider credentials, model tokens, or network writes. The\n" +
	"demo is supported on Linux and macOS, where network isolation is enforced.\n"

func runInit(args []string, stdout, stderr io.Writer) int {
	return runInitWithInput(args, os.Stdin, stdout, stderr)
}

func runInitWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runInitWithInputForOS(args, stdin, stdout, stderr, runtime.GOOS)
}

func runInitWithInputForOS(args []string, stdin io.Reader, stdout, stderr io.Writer, goos string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	demo := fs.Bool("demo", false, "seed a credential-free runnable demo workflow")
	guided := fs.Bool("guided", false, "prompt for repository, work tracking, credentials, and workflows")
	template := fs.String("template", "", "seed a named onboarding template (available: quickstart)")
	fs.Usage = helpUsage(stderr, "init")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	selectedModes := 0
	for _, selected := range []bool{*demo, *guided, *template != ""} {
		if selected {
			selectedModes++
		}
	}
	if selectedModes > 1 {
		pf(stderr, "error: --demo, --guided, and --template cannot be combined\n")
		return 2
	}
	if *template != "" && *template != instance.QuickstartTemplate {
		pf(stderr, "error: unknown init template %q (available: %s)\n", *template, instance.QuickstartTemplate)
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if *demo && goos != "linux" && goos != "darwin" {
		pf(stderr, "error: --demo is supported only on Linux and macOS because enforced network isolation is unavailable on %s\n", goos)
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	if *guided {
		if err := instance.CheckGuidedInitTarget(root); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
	}

	var res *instance.InitResult
	var err error
	if *guided {
		var opts instance.GuidedOptions
		opts, err = promptGuidedOptions(stdin, stdout)
		if err == nil {
			res, err = instance.InitGuided(root, opts)
		}
	} else if *template == instance.QuickstartTemplate {
		res, err = instance.InitQuickstart(root)
	} else if *demo {
		res, err = instance.InitDemo(root)
	} else {
		res, err = instance.Init(root)
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	abs, err := filepath.Abs(res.Root)
	if err != nil {
		abs = res.Root
	}
	if len(res.Created) == 0 {
		pf(stdout, "instance already initialized at %s (nothing to do)\n", abs)
		if *guided {
			return finishGuidedInit(root, abs, stdout, stderr)
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
	pf(stdout, "\nLearn the desired-state model: %s\n", conceptsGuideURL)
	demoSeeded := false
	for _, created := range res.Created {
		if created == instance.ConfigDirName {
			demoSeeded = true
			break
		}
	}
	if *demo && demoSeeded {
		pf(stdout, demoTourBanner, abs)
	}
	if *guided {
		return finishGuidedInit(root, abs, stdout, stderr)
	}
	return 0
}

func finishGuidedInit(root, abs string, stdout, stderr io.Writer) int {
	pln(stdout, "")
	if code := runValidate([]string{root}, stdout, stderr); code != 0 {
		pf(stderr, "error: guided setup did not produce a valid instance\n")
		return code
	}
	pf(stdout, guidedDocsBanner, abs)
	return 0
}

type guidedPrompter struct {
	reader *bufio.Reader
	out    io.Writer
}

func promptGuidedOptions(stdin io.Reader, stdout io.Writer) (instance.GuidedOptions, error) {
	p := guidedPrompter{reader: bufio.NewReader(stdin), out: stdout}
	pln(stdout, "Guided first-run setup")
	pln(stdout, "")

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
	pln(stdout, "  1) quickstart         onboarding issue -> implement -> advisory review -> PR (not for production)")
	pln(stdout, "  2) implementation    issue -> implementation -> review -> CI -> PR")
	pln(stdout, "  3) backlog-curation  approved issues -> scoped ready work")
	pln(stdout, "  4) work-nomination   telemetry and code signals -> proposed issues")
	workflowText, err := p.ask("Select workflows (comma-separated names or numbers)", "quickstart", validWorkflowSelection)
	if err != nil {
		return instance.GuidedOptions{}, err
	}
	workflows, err := parseWorkflowSelection(workflowText)
	if err != nil {
		return instance.GuidedOptions{}, err
	}
	var ciCommand []string
	for _, workflow := range workflows {
		if workflow != instance.GuidedWorkflowImplementation {
			continue
		}
		ciText, promptErr := p.ask("Local CI command (space-separated argv or JSON array)", "make ci", validCommand)
		if promptErr != nil {
			return instance.GuidedOptions{}, promptErr
		}
		ciCommand, err = parseCommand(ciText)
		if err != nil {
			return instance.GuidedOptions{}, err
		}
		break
	}

	pln(stdout, "")
	pln(stdout, "Create separate fine-grained, least-privilege PATs; never paste their values here.")
	pln(stdout, "  Create: https://github.com/settings/personal-access-tokens/new")
	pln(stdout, "  Scopes: https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/github-token-scopes.md")
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
	needsPullRequests := slices.Contains(workflows, instance.GuidedWorkflowQuickstart) ||
		slices.Contains(workflows, instance.GuidedWorkflowImplementation) ||
		slices.Contains(workflows, instance.GuidedWorkflowBacklogCuration)
	if needsPullRequests {
		if slices.Contains(workflows, instance.GuidedWorkflowImplementation) {
			pln(stdout, "Pull-request PAT permissions: Pull requests: Read and write; Contents: Read and write.")
			pln(stdout, "Implementation CI polling also requires: Checks: Read-only; Commit statuses: Read-only.")
		} else if slices.Contains(workflows, instance.GuidedWorkflowQuickstart) {
			pln(stdout, "Pull-request PAT permissions: Pull requests: Read and write.")
		} else {
			pln(stdout, "Pull-request PAT permissions: Pull requests: Read-only.")
		}
		pullRequestTokenEnv, err = p.ask("Pull-request PAT environment variable", "GOOBERS_GITHUB_PR_TOKEN", instance.ValidGuidedTokenEnvName)
		if err != nil {
			return instance.GuidedOptions{}, err
		}
	}

	repoPushTokenEnv := ""
	if slices.Contains(workflows, instance.GuidedWorkflowQuickstart) ||
		slices.Contains(workflows, instance.GuidedWorkflowImplementation) {
		pln(stdout, "Repository push PAT permissions: Contents: Read and write.")
		repoPushTokenEnv, err = p.ask("Repository push PAT environment variable", "GOOBERS_GITHUB_PUSH_TOKEN", instance.ValidGuidedTokenEnvName)
		if err != nil {
			return instance.GuidedOptions{}, err
		}
	}

	pln(stdout, "Copilot model auth: press Enter to use the current user's stored Copilot CLI sign-in.")
	pln(stdout, "For a headless service/CI account, enter an environment variable holding a Copilot Requests: Read-only PAT.")
	copilotTokenEnv, err := p.ask("Optional Copilot Requests PAT environment variable", "", func(value string) bool {
		return value == "" || instance.ValidGuidedTokenEnvName(value)
	})
	if err != nil {
		return instance.GuidedOptions{}, err
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
		CopilotTokenEnv:      copilotTokenEnv,
		Workflows:            workflows,
		CICommand:            ciCommand,
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
  Quickstart safety/upgrade: https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/quickstart.md
  Author workflows:          https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/dsl-authoring-skill.md
  Make custom agent stages: https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/requirements/goober.md and docs/stage-contract.md
  View journal telemetry:   https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/cli/README.md (` + "`goobers trace` / `goobers telemetry`" + `)
`

const conceptsGuideURL = "https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/concepts/README.md"

const demoTourBanner = `
Demo full loop (run these from %s):
  goobers run demo    # watch curate -> implement -> review -> merge preview
  goobers trace <id>  # inspect the journal and merge-preview artifact
`
