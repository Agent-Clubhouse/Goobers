package instance

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
)

func initGuidedForTest(root string, opts GuidedOptions) (*InitResult, error) {
	return InitGuided(root, opts)
}

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
		RequiredCapabilities: []string{"node@20"},
	}

	res, err := initGuidedForTest(root, opts)
	if err != nil {
		t.Fatalf("InitGuided: %v", err)
	}
	if len(res.Created) != 5 || len(res.Skipped) != 0 {
		t.Fatalf("unexpected init result: %+v", res)
	}

	layout := NewLayout(root)
	assertPreviewFeaturesDefaultOff(t, layout.ConfigDir())
	cfg, err := LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Owner != "acme" ||
		cfg.Repos[0].Name != "widget-service" || cfg.Repos[0].Token.Env != "WIDGET_REPO_TOKEN" {
		t.Fatalf("unexpected guided repository config: %+v", cfg.Repos)
	}
	if !slices.Equal(cfg.Runner.Capabilities, []string{"node@20"}) {
		t.Fatalf("guided runner capabilities = %v, want [node@20]", cfg.Runner.Capabilities)
	}
	wantCredentials := map[string]string{
		string(capability.GitHubIssuesWrite): "WIDGET_ISSUES_TOKEN",
		string(capability.ProviderPRWrite):   "WIDGET_PR_TOKEN",
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
	if !slices.Equal(gaggle.Spec.RequiredCapabilities, []string{"node@20"}) {
		t.Fatalf("guided gaggle required capabilities = %v, want [node@20]", gaggle.Spec.RequiredCapabilities)
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
			// #2173: the generated implementation.yaml's local-ci stage must
			// reflect the operator's answered CI command on disk, not the
			// acme-web example's literal `make ci`.
			if task.Name == LocalCIStageName {
				if task.Run == nil || !slices.Equal(task.Run.Command, []string{"npm", "run", "ci"}) {
					t.Errorf("workflow %q local-ci command = %+v, want [npm run ci]", workflow.Name, task.Run)
				}
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

// TestInitGuidedClaudeCodeHarnessAppliesToEveryGoober pins #2777: guided
// init's harness choice is one decision for the whole generated fleet, so it
// must override every selected goober's harness — including implementer,
// whose acme-web template already ships harness: claude-code, and the
// others, whose template ships harness: copilot — uniformly, and route the
// optional model-auth token through the claude-specific field, not Copilot's.
func TestInitGuidedClaudeCodeHarnessAppliesToEveryGoober(t *testing.T) {
	root := filepath.Join(t.TempDir(), "guided")
	opts := GuidedOptions{
		GaggleName:           "widget-service",
		RepoOwner:            "acme",
		RepoName:             "widget-service",
		RepoTokenEnv:         "WIDGET_REPO_TOKEN",
		WorkTrackingTokenEnv: "WIDGET_ISSUES_TOKEN",
		PullRequestTokenEnv:  "WIDGET_PR_TOKEN",
		RepoPushTokenEnv:     "WIDGET_PUSH_TOKEN",
		Harness:              "claude-code",
		ClaudeTokenEnv:       "WIDGET_CLAUDE_TOKEN",
		Workflows:            []string{GuidedWorkflowImplementation, GuidedWorkflowBacklogCuration},
		CICommand:            []string{"npm", "run", "ci"},
		RequiredCapabilities: []string{"node@20"},
	}

	if _, err := initGuidedForTest(root, opts); err != nil {
		t.Fatalf("InitGuided: %v", err)
	}

	layout := NewLayout(root)
	cfg, err := LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	foundModelCredential := false
	for _, credential := range cfg.Credentials {
		if credential.Capability != string(capability.AgentModel) {
			continue
		}
		foundModelCredential = true
		if credential.Token.Env != "WIDGET_CLAUDE_TOKEN" {
			t.Errorf("agent:model credential token env = %q, want WIDGET_CLAUDE_TOKEN", credential.Token.Env)
		}
	}
	if !foundModelCredential {
		t.Fatal("no agent:model credential grant was produced for the claude-code token env")
	}

	set, report, err := LoadConfigDir(layout.ConfigDir())
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if len(set.Goobers) == 0 {
		t.Fatal("no goobers were generated")
	}
	for _, goober := range set.Goobers {
		if goober.Spec.Harness != "claude-code" {
			t.Errorf("goober %q harness = %q, want claude-code", goober.Name, goober.Spec.Harness)
		}
	}
}

func TestInitGuidedRejectsInvalidOptionsBeforeWriting(t *testing.T) {
	root := filepath.Join(t.TempDir(), "guided")
	_, err := initGuidedForTest(root, GuidedOptions{
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

			_, err := initGuidedForTest(root, opts)
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
	for _, workflow := range guidedWorkflowOrder {
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
			switch workflow {
			case GuidedWorkflowImplementation:
				opts.CICommand = []string{"go", "test", "./..."}
				opts.RequiredCapabilities = []string{"go@1.26"}
				opts.PullRequestTokenEnv = "PR_TOKEN"
				opts.RepoPushTokenEnv = "PUSH_TOKEN"
			case GuidedWorkflowBacklogCuration:
				opts.PullRequestTokenEnv = "PR_TOKEN"
			}
			root := filepath.Join(t.TempDir(), "guided")
			if _, err := initGuidedForTest(root, opts); err != nil {
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
				wantCredentials[string(capability.ProviderPRWrite)] = "PR_TOKEN"
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

// TestGuidedGaggleAndWorkflowDocumentCICommandLink is #2071's discoverability
// half: the ciCommand<->local-ci relationship was previously documented only
// in the CRD schema, CONTRIBUTING.md, and hand-written config-examples — none
// of which a user looking at a freshly generated gaggle.yaml/implementation.yaml
// is looking at. Both generated files must now carry an inline comment naming
// the other side of the link, and a workflow with no local-ci stage (no
// implementation workflow selected) must not carry a dangling comment about a
// stage it doesn't have.
func TestGuidedGaggleAndWorkflowDocumentCICommandLink(t *testing.T) {
	t.Run("implementation workflow: both files document the link", func(t *testing.T) {
		opts := GuidedOptions{
			GaggleName:           "widget",
			RepoOwner:            "acme",
			RepoName:             "widget",
			RepoTokenEnv:         "REPO_TOKEN",
			WorkTrackingTokenEnv: "ISSUES_TOKEN",
			PullRequestTokenEnv:  "PR_TOKEN",
			RepoPushTokenEnv:     "PUSH_TOKEN",
			CopilotTokenEnv:      "MODEL_TOKEN",
			Workflows:            []string{GuidedWorkflowImplementation},
			CICommand:            []string{"go", "test", "./..."},
			RequiredCapabilities: []string{"go@1.26"},
		}
		sourceRoot := filepath.Join(t.TempDir(), "widget-config-source")
		if _, err := SeedGuidedConfigSource(sourceRoot, opts); err != nil {
			t.Fatalf("SeedGuidedConfigSource: %v", err)
		}

		gaggleData, err := os.ReadFile(filepath.Join(sourceRoot, "gaggles", "widget", "gaggle.yaml"))
		if err != nil {
			t.Fatalf("read gaggle.yaml: %v", err)
		}
		if !strings.Contains(string(gaggleData), "Overrides the `local-ci` stage's declared command") {
			t.Errorf("gaggle.yaml lacks the ciCommand<->local-ci comment:\n%s", gaggleData)
		}
		if !strings.Contains(string(gaggleData), "MGV-1/#1009") {
			t.Errorf("gaggle.yaml comment lacks the MGV-1/#1009 reference:\n%s", gaggleData)
		}

		workflowData, err := os.ReadFile(filepath.Join(sourceRoot, "gaggles", "widget", "workflows", GuidedWorkflowImplementation+".yaml"))
		if err != nil {
			t.Fatalf("read implementation.yaml: %v", err)
		}
		if !strings.Contains(string(workflowData), `The "local-ci" stage below runs this gaggle's ciCommand`) {
			t.Errorf("implementation.yaml lacks the local-ci<->ciCommand comment:\n%s", workflowData)
		}
		// The comment must precede the tasks list, not follow it, so a reader
		// scanning top-down sees the explanation before the stage itself.
		commentIdx := strings.Index(string(workflowData), "The \"local-ci\" stage below")
		tasksIdx := strings.Index(string(workflowData), "\n  tasks:\n")
		if commentIdx < 0 || tasksIdx < 0 || commentIdx > tasksIdx {
			t.Errorf("comment (idx %d) does not precede tasks: (idx %d):\n%s", commentIdx, tasksIdx, workflowData)
		}
		// The injected comment must not corrupt the YAML: the whole source
		// tree, including this file, must still load cleanly.
		if _, err := LoadGuidedSourceConfig(sourceRoot); err != nil {
			t.Fatalf("config source with the injected comment failed to load: %v", err)
		}
	})

	t.Run("work-nomination only: no local-ci stage, no dangling comment", func(t *testing.T) {
		opts := GuidedOptions{
			GaggleName:           "widget",
			RepoOwner:            "acme",
			RepoName:             "widget",
			RepoTokenEnv:         "REPO_TOKEN",
			WorkTrackingTokenEnv: "ISSUES_TOKEN",
			CopilotTokenEnv:      "MODEL_TOKEN",
			Workflows:            []string{GuidedWorkflowWorkNomination},
		}
		sourceRoot := filepath.Join(t.TempDir(), "widget-config-source")
		if _, err := SeedGuidedConfigSource(sourceRoot, opts); err != nil {
			t.Fatalf("SeedGuidedConfigSource: %v", err)
		}

		gaggleData, err := os.ReadFile(filepath.Join(sourceRoot, "gaggles", "widget", "gaggle.yaml"))
		if err != nil {
			t.Fatalf("read gaggle.yaml: %v", err)
		}
		if strings.Contains(string(gaggleData), "ciCommand") {
			t.Errorf("gaggle.yaml unexpectedly mentions ciCommand with no implementation workflow selected:\n%s", gaggleData)
		}

		workflowData, err := os.ReadFile(filepath.Join(sourceRoot, "gaggles", "widget", "workflows", GuidedWorkflowWorkNomination+".yaml"))
		if err != nil {
			t.Fatalf("read work-nomination.yaml: %v", err)
		}
		if strings.Contains(string(workflowData), "local-ci") {
			t.Errorf("work-nomination.yaml (no local-ci stage) unexpectedly mentions local-ci:\n%s", workflowData)
		}
	})
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
	_, err := initGuidedForTest(filepath.Join(t.TempDir(), "guided"), GuidedOptions{
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

func TestInitGuidedSourceMaterializesSeparateRuntimeState(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "config-source")
	opts := GuidedOptions{
		GaggleName:           "widget",
		RepoOwner:            "app-org",
		RepoName:             "widget",
		RepoTokenEnv:         "REPO_TOKEN",
		WorkTrackingTokenEnv: "ISSUES_TOKEN",
		Workflows:            []string{GuidedWorkflowWorkNomination},
	}
	if _, err := SeedGuidedConfigSource(sourceRoot, opts); err != nil {
		t.Fatalf("SeedGuidedConfigSource: %v", err)
	}
	for _, path := range []string{
		filepath.Join(sourceRoot, GuidedSourceInstanceFile),
		filepath.Join(sourceRoot, "manifest.yaml"),
		filepath.Join(sourceRoot, "gaggles", "widget", "gaggle.yaml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("source path %s: %v", path, err)
		}
	}
	for _, name := range []string{ConfigFileName, ConfigDirName, SchedulerDirName, TelemetryDBName} {
		if _, err := os.Stat(filepath.Join(sourceRoot, name)); !os.IsNotExist(err) {
			t.Errorf("runtime state %s exists in source tree: %v", name, err)
		}
	}

	cfg, err := LoadGuidedSourceConfig(sourceRoot)
	if err != nil {
		t.Fatalf("LoadGuidedSourceConfig: %v", err)
	}
	instanceRoot := filepath.Join(t.TempDir(), "instance")
	if _, err := InitGuidedFromSource(instanceRoot, sourceRoot, cfg); err != nil {
		t.Fatalf("InitGuidedFromSource: %v", err)
	}
	runtimeConfig, err := LoadConfig(NewLayout(instanceRoot).ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig runtime: %v", err)
	}
	sourceAbs, err := filepath.Abs(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeConfig.WorkflowSource == nil ||
		runtimeConfig.WorkflowSource.Kind != WorkflowSourceKindLocalDir ||
		runtimeConfig.WorkflowSource.Path != sourceAbs {
		t.Fatalf("runtime workflow source = %+v", runtimeConfig.WorkflowSource)
	}
	if _, report, err := LoadConfigDir(NewLayout(instanceRoot).ConfigDir()); err != nil {
		t.Fatalf("LoadConfigDir runtime: %v (report: %+v)", err, report)
	}
}

func TestMaterializeWorkflowSourceAppliesLaterSourceEdits(t *testing.T) {
	base := t.TempDir()
	sourceRoot := filepath.Join(base, "config-source")
	opts := GuidedOptions{
		GaggleName:           "widget",
		RepoOwner:            "app-org",
		RepoName:             "widget",
		RepoTokenEnv:         "REPO_TOKEN",
		WorkTrackingTokenEnv: "ISSUES_TOKEN",
		Workflows:            []string{GuidedWorkflowWorkNomination},
	}
	if _, err := SeedGuidedConfigSource(sourceRoot, opts); err != nil {
		t.Fatalf("SeedGuidedConfigSource: %v", err)
	}
	sourceConfig, err := LoadGuidedSourceConfig(sourceRoot)
	if err != nil {
		t.Fatalf("LoadGuidedSourceConfig: %v", err)
	}
	instanceRoot := filepath.Join(base, "instance")
	if _, err := InitGuidedFromSource(instanceRoot, sourceRoot, sourceConfig); err != nil {
		t.Fatalf("InitGuidedFromSource: %v", err)
	}

	sourceConfig.RunConditions.MaxParallelRuns = 7
	if err := WriteConfig(filepath.Join(sourceRoot, GuidedSourceInstanceFile), sourceConfig); err != nil {
		t.Fatalf("WriteConfig source: %v", err)
	}
	instructionsRel := filepath.Join("gaggles", "widget", "goobers", "nominator", "instructions.md")
	if err := os.WriteFile(filepath.Join(sourceRoot, instructionsRel), []byte("source revision\n"), 0o644); err != nil {
		t.Fatalf("write source instructions: %v", err)
	}
	layout := NewLayout(instanceRoot)
	staleRuntimeFile := filepath.Join(layout.ConfigDir(), "stale.yaml")
	if err := os.WriteFile(staleRuntimeFile, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write stale runtime definition: %v", err)
	}
	runtimeState := filepath.Join(layout.SchedulerDir(), "sentinel")
	if err := os.WriteFile(runtimeState, []byte("runtime state\n"), 0o644); err != nil {
		t.Fatalf("write runtime state: %v", err)
	}

	materializedSource, err := MaterializeWorkflowSource(instanceRoot)
	if err != nil {
		t.Fatalf("MaterializeWorkflowSource: %v", err)
	}
	sourceAbs, err := filepath.Abs(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if materializedSource != sourceAbs {
		t.Fatalf("materialized source = %q, want %q", materializedSource, sourceAbs)
	}
	runtimeConfig, err := LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig runtime: %v", err)
	}
	if runtimeConfig.RunConditions.MaxParallelRuns != 7 {
		t.Fatalf("runtime maxParallelRuns = %d, want 7", runtimeConfig.RunConditions.MaxParallelRuns)
	}
	if runtimeConfig.WorkflowSource == nil || runtimeConfig.WorkflowSource.Path != sourceAbs {
		t.Fatalf("runtime workflow source = %+v, want %s", runtimeConfig.WorkflowSource, sourceAbs)
	}
	instructions, err := os.ReadFile(filepath.Join(layout.ConfigDir(), instructionsRel))
	if err != nil || string(instructions) != "source revision\n" {
		t.Fatalf("runtime instructions = %q, err %v", instructions, err)
	}
	if _, err := os.Stat(staleRuntimeFile); !os.IsNotExist(err) {
		t.Fatalf("stale runtime definition survived materialization: %v", err)
	}
	state, err := os.ReadFile(runtimeState)
	if err != nil || string(state) != "runtime state\n" {
		t.Fatalf("runtime state changed: %q, err %v", state, err)
	}
}

func TestMaterializeWorkflowSourceRejectsInvalidSourceWithoutChangingRuntime(t *testing.T) {
	base := t.TempDir()
	sourceRoot := filepath.Join(base, "config-source")
	opts := GuidedOptions{
		GaggleName:           "widget",
		RepoOwner:            "app-org",
		RepoName:             "widget",
		RepoTokenEnv:         "REPO_TOKEN",
		WorkTrackingTokenEnv: "ISSUES_TOKEN",
		Workflows:            []string{GuidedWorkflowWorkNomination},
	}
	if _, err := SeedGuidedConfigSource(sourceRoot, opts); err != nil {
		t.Fatalf("SeedGuidedConfigSource: %v", err)
	}
	sourceConfig, err := LoadGuidedSourceConfig(sourceRoot)
	if err != nil {
		t.Fatalf("LoadGuidedSourceConfig: %v", err)
	}
	instanceRoot := filepath.Join(base, "instance")
	if _, err := InitGuidedFromSource(instanceRoot, sourceRoot, sourceConfig); err != nil {
		t.Fatalf("InitGuidedFromSource: %v", err)
	}
	layout := NewLayout(instanceRoot)
	runtimeManifest, err := os.ReadFile(filepath.Join(layout.ConfigDir(), "manifest.yaml"))
	if err != nil {
		t.Fatalf("read runtime manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "manifest.yaml"), []byte("not: valid: yaml\n"), 0o644); err != nil {
		t.Fatalf("write invalid source manifest: %v", err)
	}

	if _, err := MaterializeWorkflowSource(instanceRoot); err == nil {
		t.Fatal("MaterializeWorkflowSource succeeded with invalid source")
	}
	after, err := os.ReadFile(filepath.Join(layout.ConfigDir(), "manifest.yaml"))
	if err != nil {
		t.Fatalf("read runtime manifest after failure: %v", err)
	}
	if string(after) != string(runtimeManifest) {
		t.Fatal("failed materialization changed the runtime manifest")
	}
}

func TestInitGuidedSourceRefusesToOverwriteExistingTree(t *testing.T) {
	sourceRoot := t.TempDir()
	sentinel := filepath.Join(sourceRoot, "manifest.yaml")
	if err := os.WriteFile(sentinel, []byte("sentinel\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := SeedGuidedConfigSource(sourceRoot, GuidedOptions{
		GaggleName:           "widget",
		RepoOwner:            "app-org",
		RepoName:             "widget",
		RepoTokenEnv:         "REPO_TOKEN",
		WorkTrackingTokenEnv: "ISSUES_TOKEN",
		Workflows:            []string{GuidedWorkflowWorkNomination},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("SeedGuidedConfigSource error = %v", err)
	}
	data, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(data) != "sentinel\n" {
		t.Fatalf("existing source changed: data=%q err=%v", data, readErr)
	}
}

func TestInitGuidedFromSourceRejectsOverlappingRuntimePath(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "config-source")
	opts := GuidedOptions{
		GaggleName:           "widget",
		RepoOwner:            "app-org",
		RepoName:             "widget",
		RepoTokenEnv:         "REPO_TOKEN",
		WorkTrackingTokenEnv: "ISSUES_TOKEN",
		Workflows:            []string{GuidedWorkflowWorkNomination},
	}
	if _, err := SeedGuidedConfigSource(sourceRoot, opts); err != nil {
		t.Fatalf("SeedGuidedConfigSource: %v", err)
	}
	cfg, err := LoadGuidedSourceConfig(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitGuidedFromSource(filepath.Join(sourceRoot, "runtime"), sourceRoot, cfg); err == nil ||
		!strings.Contains(err.Error(), "must be separate paths") {
		t.Fatalf("InitGuidedFromSource overlapping error = %v", err)
	}
}

func TestCheckGuidedSourceInstancePathsRejectsSymlinkedOverlap(t *testing.T) {
	base := t.TempDir()
	sourceRoot := filepath.Join(base, "config-source")
	if err := os.Mkdir(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	sourceLink := filepath.Join(base, "source-link")
	if err := os.Symlink(sourceRoot, sourceLink); err != nil {
		t.Fatal(err)
	}

	err := CheckGuidedSourceInstancePaths(filepath.Join(sourceLink, "runtime"), sourceRoot)
	if err == nil || !strings.Contains(err.Error(), "must be separate paths") {
		t.Fatalf("CheckGuidedSourceInstancePaths symlinked overlap error = %v", err)
	}
}
