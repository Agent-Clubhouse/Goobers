package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/goobers/goobers/internal/instance"
)

const standardInitTemplate = "standard"

func standardInitOptions(template, harness, ciCommand, capabilities, provider string) (*instance.GuidedOptions, error) {
	if template != standardInitTemplate {
		if ciCommand != "" || capabilities != "" || provider != "" {
			return nil, fmt.Errorf("--ci-command, --required-capabilities and --provider require --template=standard")
		}
		return nil, nil
	}
	var argv []string
	if err := json.Unmarshal([]byte(ciCommand), &argv); err != nil || len(argv) == 0 {
		return nil, fmt.Errorf("--template=standard requires --ci-command as a non-empty JSON argv array")
	}
	if len(splitLabelList(capabilities)) == 0 {
		return nil, fmt.Errorf("--template=standard requires --required-capabilities (comma-separated toolchain capabilities)")
	}
	if provider != "" && provider != "github" && provider != "ado" {
		return nil, fmt.Errorf("--provider must be github or ado")
	}
	opts := &instance.GuidedOptions{
		GaggleName: "example", RepoOwner: "your-org", RepoName: "your-repo",
		Harness: harness, RepoTokenEnv: "GOOBERS_GITHUB_TOKEN",
		WorkTrackingTokenEnv: "GOOBERS_GITHUB_ISSUES_TOKEN",
		PullRequestTokenEnv:  "GOOBERS_GITHUB_PR_TOKEN", RepoPushTokenEnv: "GOOBERS_GITHUB_PUSH_TOKEN",
		Workflows: []string{instance.GuidedWorkflowImplementation, instance.GuidedWorkflowBacklogCuration},
		CICommand: argv, RequiredCapabilities: splitLabelList(capabilities),
	}
	if provider == "ado" {
		opts.RepoProvider, opts.RepoProject = "ado", "your-project"
		opts.RepoAuthKind, opts.RepoTokenEnv = instance.ADOAuthPAT, "GOOBERS_ADO_TOKEN"
		opts.WorkTrackingTokenEnv, opts.PullRequestTokenEnv, opts.RepoPushTokenEnv = "", "", ""
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
