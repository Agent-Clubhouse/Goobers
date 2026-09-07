package main

import (
	"encoding/json"
	"fmt"

	"github.com/goobers/goobers/internal/instance"
)

const standardInitTemplate = "standard"

func standardInitOptions(template, harness, ciCommand, capabilities string) (*instance.GuidedOptions, error) {
	if template != standardInitTemplate {
		if ciCommand != "" || capabilities != "" {
			return nil, fmt.Errorf("--ci-command and --required-capabilities require --template=standard")
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
	return &instance.GuidedOptions{
		GaggleName: "example", RepoOwner: "your-org", RepoName: "your-repo",
		Harness: harness, RepoTokenEnv: "GOOBERS_GITHUB_TOKEN",
		WorkTrackingTokenEnv: "GOOBERS_GITHUB_ISSUES_TOKEN",
		PullRequestTokenEnv:  "GOOBERS_GITHUB_PR_TOKEN", RepoPushTokenEnv: "GOOBERS_GITHUB_PUSH_TOKEN",
		Workflows: []string{instance.GuidedWorkflowImplementation, instance.GuidedWorkflowBacklogCuration},
		CICommand: argv, RequiredCapabilities: splitLabelList(capabilities),
	}, nil
}

func seedInitTemplate(root, template, harness string, demo bool, standard *instance.GuidedOptions) (*instance.InitResult, error) {
	switch {
	case standard != nil:
		return instance.InitGuided(root, *standard)
	case template == instance.QuickstartTemplate:
		return instance.InitQuickstartWithOptions(root, instance.QuickstartOptions{Harness: harness})
	case demo:
		return instance.InitDemo(root)
	default:
		return instance.Init(root)
	}
}
