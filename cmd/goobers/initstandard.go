package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/goobers/goobers/internal/instance"
)

const standardInitTemplate = "standard"

type standardInitInput struct {
	Template, Harness, CICommand, Capabilities, Provider string
	Repo, Branch, IssueScope, AssignedTo, Workflows      string
	RepoAuthKind, RepoTokenEnv, WorkTrackingTokenEnv     string
	PullRequestTokenEnv, RepoPushTokenEnv                string
	ModelTokenEnv, GitHubCLIUser                         string
	PullRequestCI                                        bool
	AnyStandardOption, WorkflowsSet, RepoAuthKindSet     bool
	RepoTokenEnvSet, WorkTrackingTokenEnvSet             bool
	PullRequestTokenEnvSet, RepoPushTokenEnvSet          bool
}

func hasStandardInitFlag(visited map[string]bool) bool {
	for _, name := range []string{
		"ci-command", "required-capabilities", "provider", "repo", "branch", "issue-scope", "assigned-to",
		"pr-ci", "workflows", "repo-auth-kind", "repo-token-env", "work-tracking-token-env", "pr-token-env",
		"push-token-env", "model-token-env", "github-cli-user",
	} {
		if visited[name] {
			return true
		}
	}
	return false
}

func standardInitOptions(input standardInitInput) (*instance.GuidedOptions, error) {
	template := input.Template
	if template != standardInitTemplate {
		if input.AnyStandardOption {
			return nil, fmt.Errorf("standard onboarding options require --template=standard")
		}
		return nil, nil
	}
	var argv []string
	if input.CICommand != "" {
		if err := json.Unmarshal([]byte(input.CICommand), &argv); err != nil || len(argv) == 0 {
			return nil, fmt.Errorf("--ci-command must be a non-empty JSON argv array")
		}
	}
	provider, repo, err := standardInitRepository(input)
	if err != nil {
		return nil, err
	}
	repoAuthKind := input.RepoAuthKind
	if provider == "ado" {
		// An unset or empty kind defaults to azure-cli (normalizeGuidedOptions);
		// only PAT auth names a token variable.
		if repoAuthKind, err = standardADORepoAuthKind(input); err != nil {
			return nil, err
		}
	}
	// The placeholder identity and the provider's credential defaults are
	// shared with the provider compile-matrix gate (test/providermatrix), so
	// the scaffold it validates is the one this command writes.
	defaults := instance.StandardInitDefaults(provider, repoAuthKind)
	opts := &defaults
	if repo != nil {
		opts.GaggleName = guidedGaggleName(repo.name)
		opts.RepoOwner, opts.RepoProject, opts.RepoName = repo.owner, repo.project, repo.name
	}
	opts.DisplayName = guidedRepositoryDisplayName(provider, opts.RepoOwner, opts.RepoProject, opts.RepoName)
	opts.RepoBranch, opts.GitHubCLIUser, opts.Harness = input.Branch, input.GitHubCLIUser, input.Harness
	opts.Workflows = splitLabelList(input.Workflows)
	if !input.WorkflowsSet {
		opts.Workflows = standardDefaultWorkflows(provider)
	}
	opts.IssueScope, opts.AssignedTo = input.IssueScope, input.AssignedTo
	opts.PullRequestCI, opts.CICommand, opts.RequiredCapabilities = input.PullRequestCI, argv, splitLabelList(input.Capabilities)
	if input.RepoTokenEnvSet {
		opts.RepoTokenEnv = input.RepoTokenEnv
	}
	if input.WorkTrackingTokenEnvSet {
		opts.WorkTrackingTokenEnv = input.WorkTrackingTokenEnv
	}
	if input.PullRequestTokenEnvSet {
		opts.PullRequestTokenEnv = input.PullRequestTokenEnv
	}
	if input.RepoPushTokenEnvSet {
		opts.RepoPushTokenEnv = input.RepoPushTokenEnv
	}
	if input.Harness == "claude-code" {
		opts.ClaudeTokenEnv = input.ModelTokenEnv
	} else {
		opts.CopilotTokenEnv = input.ModelTokenEnv
	}
	return opts, nil
}

// standardInitRepository resolves the provider `init --template=standard`
// scaffolds for and, when --repo names one, the repository identity. With no
// --repo the identity is nil (the caller keeps the placeholder) and the
// provider is --provider, defaulting to github.
func standardInitRepository(input standardInitInput) (string, *guidedRepositoryIdentity, error) {
	provider := strings.ToLower(strings.TrimSpace(input.Provider))
	if provider != "" && provider != "github" && provider != "ado" {
		return "", nil, fmt.Errorf("--provider must be github or ado")
	}
	if strings.TrimSpace(input.Repo) == "" {
		if provider == "" {
			provider = "github"
		}
		return provider, nil, nil
	}
	parsed, err := parseGuidedRepositoryIdentity(input.Repo)
	if err != nil {
		return "", nil, standardRepoParseError(provider, err)
	}
	if provider != "" && provider != parsed.provider {
		return "", nil, fmt.Errorf("--provider=%s conflicts with --repo provider %s", provider, parsed.provider)
	}
	return parsed.provider, &parsed, nil
}

// standardRepoParseError words a --repo parse failure. An explicit
// --provider=ado must not surface the GitHub parser's "host must be
// github.com" for a URL that is not Azure DevOps cloud (for example Azure
// DevOps Server, which is unsupported).
func standardRepoParseError(provider string, err error) error {
	if provider == "ado" {
		return fmt.Errorf("--repo: not a supported Azure DevOps repository (expected https://dev.azure.com/<organization>/<project>/_git/<repository>; Azure DevOps Server is not supported)")
	}
	return fmt.Errorf("--repo: %w", err)
}

// standardADORepoAuthKind resolves the Azure DevOps repository auth kind.
// Only PAT auth reads a token variable, so naming one with --repo-token-env
// and no kind selects pat (as scripted ADO onboarding did before azure-cli
// became the default); naming one with any other kind is a usage error
// instead of a silently ignored flag. An empty --repo-token-env names nothing.
func standardADORepoAuthKind(input standardInitInput) (string, error) {
	kind := strings.TrimSpace(input.RepoAuthKind)
	if !input.RepoTokenEnvSet || strings.TrimSpace(input.RepoTokenEnv) == "" {
		return kind, nil
	}
	switch kind {
	case "":
		return instance.ADOAuthPAT, nil
	case instance.ADOAuthPAT:
		return kind, nil
	default:
		return "", fmt.Errorf("--repo-token-env is read only by --repo-auth-kind=%s on Azure DevOps; drop --repo-token-env or use --repo-auth-kind=%s instead of %q",
			instance.ADOAuthPAT, instance.ADOAuthPAT, kind)
	}
}

// standardDefaultWorkflows is the module set `init --template=standard` seeds
// when --workflows is not given. Azure DevOps also gets merge-review so the
// instance lands the pull requests it opens (docs/design/ado-parity-dsl-2-0.md
// §7.1); the GitHub default is unchanged.
func standardDefaultWorkflows(provider string) []string {
	workflows := []string{instance.GuidedWorkflowImplementation, instance.GuidedWorkflowBacklogCuration}
	if provider == "ado" {
		workflows = append(workflows, instance.GuidedWorkflowMergeReview)
	}
	return workflows
}

func seedInitTemplate(root, template, harness string, demo bool, standard *instance.GuidedOptions, diagnostic io.Writer) (*instance.InitResult, error) {
	observe := func(root, id string) error {
		_, err := fmt.Fprintf(diagnostic, "Instance root: %q; instance ID: %q\n", canonicalStatusRoot(root), id)
		return err
	}
	switch {
	case standard != nil:
		return instance.InitGuided(root, *standard, observe)
	case template == instance.QuickstartTemplate:
		return instance.InitQuickstartWithOptions(root, instance.QuickstartOptions{Harness: harness}, observe)
	case demo:
		return instance.InitDemo(root, observe)
	default:
		return instance.Init(root, observe)
	}
}
