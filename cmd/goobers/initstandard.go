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
	provider := strings.ToLower(strings.TrimSpace(input.Provider))
	if provider != "" && provider != "github" && provider != "ado" {
		return nil, fmt.Errorf("--provider must be github or ado")
	}
	identity := guidedRepositoryIdentity{provider: provider, owner: "your-org", name: "your-repo"}
	gaggleName := "example"
	if provider == "ado" {
		identity.project = "your-project"
	}
	if strings.TrimSpace(input.Repo) != "" {
		parsed, err := parseGuidedRepositoryIdentity(input.Repo)
		if err != nil {
			return nil, fmt.Errorf("--repo: %w", err)
		}
		if provider != "" && provider != parsed.provider {
			return nil, fmt.Errorf("--provider=%s conflicts with --repo provider %s", provider, parsed.provider)
		}
		identity = parsed
		provider = parsed.provider
		gaggleName = guidedGaggleName(parsed.name)
	}
	if provider == "" {
		provider = "github"
		identity.provider = provider
	}
	workflows := splitLabelList(input.Workflows)
	if !input.WorkflowsSet {
		workflows = []string{instance.GuidedWorkflowImplementation, instance.GuidedWorkflowBacklogCuration}
	}
	opts := &instance.GuidedOptions{
		GaggleName:   gaggleName,
		DisplayName:  guidedRepositoryDisplayName(provider, identity.owner, identity.project, identity.name),
		RepoProvider: provider, RepoOwner: identity.owner, RepoProject: identity.project, RepoName: identity.name,
		RepoBranch: input.Branch, GitHubCLIUser: input.GitHubCLIUser, RepoAuthKind: input.RepoAuthKind,
		Harness: input.Harness, RepoTokenEnv: "GOOBERS_GITHUB_TOKEN",
		WorkTrackingTokenEnv: "GOOBERS_GITHUB_ISSUES_TOKEN",
		PullRequestTokenEnv:  "GOOBERS_GITHUB_PR_TOKEN", RepoPushTokenEnv: "GOOBERS_GITHUB_PUSH_TOKEN",
		Workflows: workflows, IssueScope: input.IssueScope, AssignedTo: input.AssignedTo,
		PullRequestCI: input.PullRequestCI, CICommand: argv, RequiredCapabilities: splitLabelList(input.Capabilities),
	}
	if provider == "ado" {
		if !input.RepoAuthKindSet {
			opts.RepoAuthKind = instance.ADOAuthPAT
		}
		opts.RepoTokenEnv = "GOOBERS_ADO_TOKEN"
		opts.WorkTrackingTokenEnv, opts.PullRequestTokenEnv, opts.RepoPushTokenEnv = "", "", ""
	}
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
