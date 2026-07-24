package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
)

func TestGuidedInitProducesValidatedRunnableInstance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "widget-instance")
	input := strings.NewReader(strings.Join([]string{
		"https://github.com/acme/Widget.Service.git",
		"",
		"",
		"",
		"",
		"",
		"",
		"",
		"",
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer

	code := runInitWithInput([]string{"--guided", root}, input, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("guided init code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("guided init stderr = %q", stderr.String())
	}
	for _, want := range []string{
		"OK: instance.yaml valid; config/ valid (1 gaggle(s), 4 goober(s), 3 workflow(s))",
		"docs/guides/github-token-scopes.md",
		"Work tracking: GitHub Issues in acme/Widget.Service",
		"Repository read PAT permissions: Contents: Read-only.",
		"Work-tracking PAT permissions: Issues: Read and write.",
		"Pull-request PAT permissions: Pull requests: Read and write; Contents: Read and write.",
		"Implementation CI polling also requires: Checks: Read-only; Commit statuses: Read-only.",
		"Repository push PAT permissions: Contents: Read and write.",
		"Copilot model auth: press Enter to use the current user's stored Copilot CLI sign-in.",
		"For a headless service/CI account",
		"docs/concepts/README.md",
		"Author workflows:",
		"docs/guides/dsl-authoring-skill.md",
		"Make custom agent stages:",
		"docs/requirements/goober.md",
		"View journal telemetry:",
		"`goobers trace` / `goobers telemetry`",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("guided init stdout lacks %q:\n%s", want, stdout.String())
		}
	}

	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Owner != "acme" ||
		cfg.Repos[0].Name != "Widget.Service" ||
		cfg.Repos[0].Token.Env != "GOOBERS_GITHUB_REPO_TOKEN" {
		t.Fatalf("unexpected guided instance config: %+v", cfg)
	}
	wantCredentials := map[string]string{
		string(capability.GitHubIssuesWrite): "GOOBERS_GITHUB_ISSUES_TOKEN",
		string(capability.GitHubPRWrite):     "GOOBERS_GITHUB_PR_TOKEN",
		string(capability.RepoPush):          "GOOBERS_GITHUB_PUSH_TOKEN",
	}
	if len(cfg.Credentials) != len(wantCredentials) {
		t.Fatalf("guided credentials = %+v, want %v", cfg.Credentials, wantCredentials)
	}
	for _, credential := range cfg.Credentials {
		if want := wantCredentials[credential.Capability]; credential.Token.Env != want {
			t.Errorf("credential %q token env = %q, want %q", credential.Capability, credential.Token.Env, want)
		}
	}
	for _, name := range instance.GuidedWorkflowNames() {
		path := filepath.Join(root, "config", "gaggles", "widget-service", "workflows", name+".yaml")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("selected workflow %q not scaffolded: %v", name, err)
		}
	}
}

func TestPromptGuidedOptionsOnlyRequestsSelectedCredentialClasses(t *testing.T) {
	input := strings.NewReader(strings.Join([]string{
		"acme/widget",
		"",
		"work-nomination",
		"",
		"",
		"",
	}, "\n") + "\n")
	var stdout bytes.Buffer

	opts, err := promptGuidedOptions(input, &stdout)
	if err != nil {
		t.Fatalf("promptGuidedOptions: %v", err)
	}
	if opts.RepoTokenEnv != "GOOBERS_GITHUB_REPO_TOKEN" ||
		opts.WorkTrackingTokenEnv != "GOOBERS_GITHUB_ISSUES_TOKEN" ||
		opts.CopilotTokenEnv != "" {
		t.Fatalf("unexpected common token refs: %+v", opts)
	}
	if opts.PullRequestTokenEnv != "" || opts.RepoPushTokenEnv != "" {
		t.Fatalf("work nomination received unused token refs: %+v", opts)
	}
	for _, unwanted := range []string{"Pull-request PAT", "Repository push PAT", "Checks: Read-only"} {
		if strings.Contains(stdout.String(), unwanted) {
			t.Errorf("work-nomination prompt unexpectedly contains %q:\n%s", unwanted, stdout.String())
		}
	}
}

func TestPromptGuidedOptionsUsesReadOnlyPullRequestScopeForCuration(t *testing.T) {
	input := strings.NewReader(strings.Join([]string{
		"acme/widget",
		"",
		"backlog-curation",
		"",
		"",
		"",
		"",
	}, "\n") + "\n")
	var stdout bytes.Buffer

	opts, err := promptGuidedOptions(input, &stdout)
	if err != nil {
		t.Fatalf("promptGuidedOptions: %v", err)
	}
	if opts.PullRequestTokenEnv != "GOOBERS_GITHUB_PR_TOKEN" || opts.RepoPushTokenEnv != "" {
		t.Fatalf("unexpected curation token refs: %+v", opts)
	}
	if !strings.Contains(stdout.String(), "Pull-request PAT permissions: Pull requests: Read-only.") {
		t.Errorf("curation prompt lacks read-only PR guidance:\n%s", stdout.String())
	}
	for _, unwanted := range []string{"Implementation CI polling", "Repository push PAT"} {
		if strings.Contains(stdout.String(), unwanted) {
			t.Errorf("curation prompt unexpectedly contains %q:\n%s", unwanted, stdout.String())
		}
	}
}

func TestInitModesAreMutuallyExclusive(t *testing.T) {
	for _, args := range [][]string{
		{"--guided", "--demo"},
		{"--guided", "--template=quickstart"},
		{"--demo", "--template=quickstart"},
	} {
		var stdout, stderr bytes.Buffer
		code := runInitWithInput(args, strings.NewReader(""), &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "--demo, --guided, and --template cannot be combined") {
			t.Errorf("args = %v, code = %d, stdout = %q, stderr = %q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestInitRejectsUnknownTemplate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runInitWithInput([]string{"--template=production"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), `unknown init template "production"`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestGuidedInitRejectsExistingInstanceBeforePrompt(t *testing.T) {
	root := t.TempDir()
	if _, err := instance.Init(root); err != nil {
		t.Fatalf("plain Init: %v", err)
	}
	layout := instance.NewLayout(root)
	configBefore, err := os.ReadFile(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(layout.ConfigDir(), "manifest.yaml")
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	input := strings.NewReader("acme/replacement\n")
	var stdout, stderr bytes.Buffer
	code := runInitWithInput([]string{"--guided", root}, input, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("guided rerun code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if input.Len() != len("acme/replacement\n") {
		t.Fatalf("guided rerun consumed prompt input before rejecting existing config")
	}
	if !strings.Contains(stderr.String(), "guided setup requires an unconfigured target") ||
		!strings.Contains(stderr.String(), instance.ConfigFileName) {
		t.Fatalf("guided rerun stderr = %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "Guided first-run setup") ||
		strings.Contains(stdout.String(), "Ready to run") {
		t.Fatalf("guided rerun reported setup progress or success: %q", stdout.String())
	}
	configAfter, err := os.ReadFile(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	manifestAfter, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configAfter, configBefore) || !bytes.Equal(manifestAfter, manifestBefore) {
		t.Fatal("guided rerun modified existing configuration")
	}
}

func TestParseWorkflowSelection(t *testing.T) {
	got, err := parseWorkflowSelection("3, implementation")
	if err != nil {
		t.Fatalf("parseWorkflowSelection: %v", err)
	}
	want := []string{instance.GuidedWorkflowImplementation, instance.GuidedWorkflowWorkNomination}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("selection = %v, want %v", got, want)
	}
	for _, input := range []string{"", "4", "1,implementation"} {
		if _, err := parseWorkflowSelection(input); err == nil {
			t.Errorf("parseWorkflowSelection(%q) succeeded, want error", input)
		}
	}
}

func TestGuidedInputValidation(t *testing.T) {
	for _, input := range []string{"main", "release/v1", "feature/widget.v2"} {
		if !validBranch(input) {
			t.Errorf("validBranch(%q) = false", input)
		}
	}
	for _, input := range []string{"", "@", "-main", "feature//x", ".hidden", "feature/.hidden", "main.lock", "feature/x.lock"} {
		if validBranch(input) {
			t.Errorf("validBranch(%q) = true", input)
		}
	}
	longName := guidedGaggleName(strings.Repeat("widget-", 20))
	if len(longName) > 50 || strings.HasSuffix(longName, "-") {
		t.Errorf("guidedGaggleName produced invalid bounded name %q", longName)
	}
	for _, test := range []struct {
		input string
		want  []string
	}{
		{input: "npm run ci", want: []string{"npm", "run", "ci"}},
		{input: `["go", "test", "./..."]`, want: []string{"go", "test", "./..."}},
	} {
		got, err := parseCommand(test.input)
		if err != nil {
			t.Errorf("parseCommand(%q): %v", test.input, err)
		} else if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
			t.Errorf("parseCommand(%q) = %v, want %v", test.input, got, test.want)
		}
	}
	for _, input := range []string{"", "[]", `["make", ""]`, `["make"`} {
		if _, err := parseCommand(input); err == nil {
			t.Errorf("parseCommand(%q) succeeded, want error", input)
		}
	}
}
