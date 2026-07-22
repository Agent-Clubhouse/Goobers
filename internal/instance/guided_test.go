package instance

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
)

func TestInitGuidedSelectedCanonicalWorkflows(t *testing.T) {
	root := filepath.Join(t.TempDir(), "guided")
	opts := GuidedOptions{
		GaggleName:           "widget-service",
		DisplayName:          "acme/widget-service",
		RepoOwner:            "acme",
		RepoName:             "widget-service",
		RepoBranch:           "release/v1",
		RepoTokenEnv:         "WIDGET_REPO_TOKEN",
		WorkTrackingTokenEnv: "WIDGET_ISSUES_TOKEN",
		PullRequestTokenEnv:  "WIDGET_PR_TOKEN",
		RepoPushTokenEnv:     "WIDGET_PUSH_TOKEN",
		CopilotTokenEnv:      "WIDGET_COPILOT_TOKEN",
		Workflows:            []string{GuidedWorkflowImplementation, GuidedWorkflowWorkNomination},
		CICommand:            []string{"npm", "run", "ci"},
	}

	res, err := InitGuided(root, opts)
	if err != nil {
		t.Fatalf("InitGuided: %v", err)
	}
	if len(res.Created) != 5 || len(res.Skipped) != 0 {
		t.Fatalf("unexpected init result: %+v", res)
	}

	layout := NewLayout(root)
	cfg, err := LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Owner != "acme" ||
		cfg.Repos[0].Name != "widget-service" || cfg.Repos[0].Token.Env != "WIDGET_REPO_TOKEN" {
		t.Fatalf("unexpected guided repository config: %+v", cfg.Repos)
	}
	wantCredentials := map[string]string{
		string(capability.GitHubIssuesWrite): "WIDGET_ISSUES_TOKEN",
		string(capability.GitHubPRWrite):     "WIDGET_PR_TOKEN",
		string(capability.RepoPush):          "WIDGET_PUSH_TOKEN",
		string(capability.AgentModel):        "WIDGET_COPILOT_TOKEN",
	}
	if len(cfg.Credentials) != len(wantCredentials) {
		t.Fatalf("unexpected guided credential config: %+v", cfg.Credentials)
	}
	for _, credential := range cfg.Credentials {
		if want := wantCredentials[credential.Capability]; credential.Token.Env != want {
			t.Errorf("credential %q token env = %q, want %q", credential.Capability, credential.Token.Env, want)
		}
	}

	set, report, err := LoadConfigDir(layout.ConfigDir())
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if len(set.Gaggles) != 1 || len(set.Workflows) != 2 || len(set.Goobers) != 3 {
		t.Fatalf("unexpected guided config shape: gaggles=%d workflows=%d goobers=%d",
			len(set.Gaggles), len(set.Workflows), len(set.Goobers))
	}
	gaggle := set.Gaggles[0]
	if gaggle.Name != "widget-service" || gaggle.Spec.Project.Owner != "acme" ||
		gaggle.Spec.Project.Name != "widget-service" || gaggle.Spec.Project.Branch != "release/v1" ||
		gaggle.Spec.Backlog.Project != "acme/widget-service" ||
		!slices.Equal(gaggle.Spec.CICommand, []string{"npm", "run", "ci"}) {
		t.Fatalf("unexpected guided gaggle: %+v", gaggle)
	}

	for _, goober := range set.Goobers {
		if !slices.Contains(goober.Spec.Capabilities, string(capability.AgentModel)) {
			t.Errorf("goober %q lacks agent:model: %v", goober.Name, goober.Spec.Capabilities)
		}
		for _, workflow := range goober.Spec.Workflows {
			if workflow != GuidedWorkflowImplementation && workflow != GuidedWorkflowWorkNomination {
				t.Errorf("goober %q retained unselected workflow %q", goober.Name, workflow)
			}
		}
	}
	for _, workflow := range set.Workflows {
		if workflow.Spec.Gaggle != "widget-service" {
			t.Errorf("workflow %q gaggle = %q", workflow.Name, workflow.Spec.Gaggle)
		}
		for _, task := range workflow.Spec.Tasks {
			if task.Goober != "" && !slices.Contains(task.Capabilities, string(capability.AgentModel)) {
				t.Errorf("workflow %q agentic task %q lacks agent:model: %v",
					workflow.Name, task.Name, task.Capabilities)
			}
		}
	}

	if _, err := os.Stat(filepath.Join(layout.ConfigDir(), "gaggles", "widget-service", "workflows", GuidedWorkflowBacklogCuration+".yaml")); !os.IsNotExist(err) {
		t.Fatalf("unselected workflow exists, stat error = %v", err)
	}
	instructions, err := os.ReadFile(filepath.Join(layout.ConfigDir(), "gaggles", "widget-service", "goobers", "implementer", "instructions.md"))
	if err != nil {
		t.Fatalf("read implementer instructions: %v", err)
	}
	if strings.Contains(string(instructions), "Acme Web") || !strings.Contains(string(instructions), "acme/widget-service") {
		t.Fatalf("instructions were not specialized for the repository")
	}
}

func TestInitGuidedRejectsInvalidOptionsBeforeWriting(t *testing.T) {
	root := filepath.Join(t.TempDir(), "guided")
	_, err := InitGuided(root, GuidedOptions{
		GaggleName:           "widget",
		RepoOwner:            "acme",
		RepoName:             "widget",
		RepoTokenEnv:         "TOKEN",
		WorkTrackingTokenEnv: "ISSUES_TOKEN",
		CopilotTokenEnv:      "MODEL_TOKEN",
		Workflows:            []string{"not-canonical"},
	})
	if err == nil || !strings.Contains(err.Error(), `unknown guided workflow "not-canonical"`) {
		t.Fatalf("InitGuided error = %v", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("invalid guided setup wrote root, stat error = %v", statErr)
	}
}

func TestInitGuidedRejectsExistingConfigurationBeforeWriting(t *testing.T) {
	opts := GuidedOptions{
		GaggleName:           "widget",
		RepoOwner:            "acme",
		RepoName:             "widget",
		RepoTokenEnv:         "REPO_TOKEN",
		WorkTrackingTokenEnv: "ISSUES_TOKEN",
		PullRequestTokenEnv:  "PR_TOKEN",
		CopilotTokenEnv:      "MODEL_TOKEN",
		Workflows:            []string{GuidedWorkflowBacklogCuration},
	}
	for _, test := range []struct {
		name    string
		blocker string
		seed    func(t *testing.T, layout Layout) string
	}{
		{
			name:    "instance file",
			blocker: ConfigFileName,
			seed: func(t *testing.T, layout Layout) string {
				t.Helper()
				if err := os.WriteFile(layout.ConfigFile(), []byte("sentinel"), 0o644); err != nil {
					t.Fatal(err)
				}
				return layout.ConfigFile()
			},
		},
		{
			name:    "populated config directory",
			blocker: ConfigDirName,
			seed: func(t *testing.T, layout Layout) string {
				t.Helper()
				if err := os.MkdirAll(layout.ConfigDir(), 0o755); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(layout.ConfigDir(), "custom.yaml")
				if err := os.WriteFile(path, []byte("sentinel"), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			layout := NewLayout(root)
			sentinel := test.seed(t, layout)

			_, err := InitGuided(root, opts)
			if err == nil || !strings.Contains(err.Error(), "guided setup requires an unconfigured target") ||
				!strings.Contains(err.Error(), test.blocker) {
				t.Fatalf("InitGuided error = %v", err)
			}
			data, readErr := os.ReadFile(sentinel)
			if readErr != nil || string(data) != "sentinel" {
				t.Fatalf("existing configuration changed: data=%q err=%v", data, readErr)
			}
			if test.blocker == ConfigDirName {
				if _, statErr := os.Stat(layout.ConfigFile()); !os.IsNotExist(statErr) {
					t.Fatalf("rejected guided setup wrote %s, stat error = %v", ConfigFileName, statErr)
				}
			} else if _, statErr := os.Stat(layout.ConfigDir()); !os.IsNotExist(statErr) {
				t.Fatalf("rejected guided setup wrote %s, stat error = %v", ConfigDirName, statErr)
			}
		})
	}
}

func TestInitGuidedIndividualWorkflowSelections(t *testing.T) {
	for _, workflow := range GuidedWorkflowNames() {
		t.Run(workflow, func(t *testing.T) {
			opts := GuidedOptions{
				GaggleName:           "widget",
				RepoOwner:            "acme",
				RepoName:             "widget",
				RepoTokenEnv:         "REPO_TOKEN",
				WorkTrackingTokenEnv: "ISSUES_TOKEN",
				CopilotTokenEnv:      "MODEL_TOKEN",
				Workflows:            []string{workflow},
			}
			if workflow == GuidedWorkflowImplementation {
				opts.CICommand = []string{"go", "test", "./..."}
				opts.PullRequestTokenEnv = "PR_TOKEN"
				opts.RepoPushTokenEnv = "PUSH_TOKEN"
			} else if workflow == GuidedWorkflowBacklogCuration {
				opts.PullRequestTokenEnv = "PR_TOKEN"
			}
			root := filepath.Join(t.TempDir(), "guided")
			if _, err := InitGuided(root, opts); err != nil {
				t.Fatalf("InitGuided: %v", err)
			}
			set, report, err := LoadConfigDir(NewLayout(root).ConfigDir())
			if err != nil {
				t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
			}
			if len(set.Workflows) != 1 || set.Workflows[0].Name != workflow {
				t.Fatalf("guided workflows = %+v, want only %q", set.Workflows, workflow)
			}
			cfg, err := LoadConfig(NewLayout(root).ConfigFile())
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			wantCredentials := map[string]string{
				string(capability.GitHubIssuesWrite): "ISSUES_TOKEN",
				string(capability.AgentModel):        "MODEL_TOKEN",
			}
			if workflow == GuidedWorkflowImplementation || workflow == GuidedWorkflowBacklogCuration {
				wantCredentials[string(capability.GitHubPRWrite)] = "PR_TOKEN"
			}
			if workflow == GuidedWorkflowImplementation {
				wantCredentials[string(capability.RepoPush)] = "PUSH_TOKEN"
			}
			if len(cfg.Credentials) != len(wantCredentials) {
				t.Fatalf("guided credentials = %+v, want %v", cfg.Credentials, wantCredentials)
			}
			for _, credential := range cfg.Credentials {
				if want := wantCredentials[credential.Capability]; credential.Token.Env != want {
					t.Errorf("credential %q token env = %q, want %q", credential.Capability, credential.Token.Env, want)
				}
			}
		})
	}
}

func TestValidGuidedTokenEnvNameRejectsTokenValues(t *testing.T) {
	for _, value := range []string{"GOOBERS_GITHUB_TOKEN", "MODEL_TOKEN"} {
		if !ValidGuidedTokenEnvName(value) {
			t.Errorf("ValidGuidedTokenEnvName(%q) = false", value)
		}
	}
	for _, value := range []string{"", "NOT-AN-ENV", "github_pat_11AASecret", "ghp_123456789"} {
		if ValidGuidedTokenEnvName(value) {
			t.Errorf("ValidGuidedTokenEnvName(%q) = true", value)
		}
	}
}

func TestInitGuidedTokenValidationDoesNotEchoSecret(t *testing.T) {
	const secret = "github_pat_11AASecret"
	_, err := InitGuided(filepath.Join(t.TempDir(), "guided"), GuidedOptions{
		GaggleName:           "widget",
		RepoOwner:            "acme",
		RepoName:             "widget",
		RepoTokenEnv:         secret,
		WorkTrackingTokenEnv: "ISSUES_TOKEN",
		PullRequestTokenEnv:  "PR_TOKEN",
		CopilotTokenEnv:      "MODEL_TOKEN",
		Workflows:            []string{GuidedWorkflowBacklogCuration},
	})
	if err == nil {
		t.Fatal("InitGuided succeeded with a token value as token.env")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("validation error exposed token value: %v", err)
	}
}
