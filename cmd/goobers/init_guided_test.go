package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/version"
)

type guidedInitCallbackWriter struct {
	bytes.Buffer
	onWrite func(string)
}

func (w *guidedInitCallbackWriter) Write(p []byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite(string(p))
	}
	return w.Buffer.Write(p)
}

type guidedPromptTranscriptWriter struct {
	bytes.Buffer
	transcript strings.Builder
}

func (w *guidedPromptTranscriptWriter) Write(p []byte) (int, error) {
	if bytes.HasSuffix(p, []byte(": ")) {
		w.transcript.Write(p)
		w.transcript.WriteByte('\n')
	}
	return w.Buffer.Write(p)
}

func TestGuidedInitReleasePromptTranscript(t *testing.T) {
	goldenPath, err := filepath.Abs(filepath.Join("testdata", "guided-init-release.prompts.golden"))
	if err != nil {
		t.Fatal(err)
	}
	answers, err := os.ReadFile(filepath.Join("testdata", "guided-init-release.answers"))
	if err != nil {
		t.Fatal(err)
	}
	input := bytes.NewReader(answers)
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "Makefile"), []byte("ci:\n\t@true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)

	var stdout guidedPromptTranscriptWriter
	var stderr bytes.Buffer
	code := runInitWithInput(
		[]string{"--guided", filepath.Join("smoke", "quickstart-instance")},
		input,
		&stdout,
		&stderr,
	)
	got := strings.ReplaceAll(stdout.transcript.String(), workspace, "<workspace>")
	got = strings.ReplaceAll(got, string(filepath.Separator), "/")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		diff, diffErr := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A:        difflib.SplitLines(string(want)),
			B:        difflib.SplitLines(got),
			FromFile: goldenPath,
			ToFile:   "actual guided-init prompts",
			Context:  3,
		})
		if diffErr != nil {
			t.Fatalf("diff guided-init prompt transcript: %v", diffErr)
		}
		t.Fatalf("guided-init prompt transcript changed:\n%s", diff)
	}
	if code != 0 {
		t.Fatalf("guided init code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if input.Len() != 0 {
		t.Fatalf("guided init left %d scripted answer bytes unread", input.Len())
	}
}

func TestGuidedInitProducesValidatedRunnableInstance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "widget-instance")
	sourceRoot := root + "-config"
	input := strings.NewReader(strings.Join([]string{
		"",
		sourceRoot,
		"https://github.com/acme/Widget.Service.git",
		"",
		"",
		"make ci", // #2071: no build manifest in this test's cwd, so no default is offered
		"make",
		"", // accept the default harness (copilot)
		"",
		"",
		"",
		"",
		"",
		"",
		"yes",
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
		"Configuration source (desired state; separate from runtime state)",
		"Keeping the config source local-only",
		"config-source: " + sourceRoot,
		"target-repo:   https://github.com/acme/Widget.Service",
		"backlog:       https://github.com/acme/Widget.Service/issues",
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
		"goobers config materialize " + strconv.Quote(root),
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
	if !slices.Equal(cfg.Runner.Capabilities, []string{"make"}) {
		t.Fatalf("guided runner capabilities = %v, want [make]", cfg.Runner.Capabilities)
	}
	sourceAbs, err := filepath.Abs(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkflowSource == nil || cfg.WorkflowSource.Kind != instance.WorkflowSourceKindLocalDir ||
		cfg.WorkflowSource.Path != sourceAbs {
		t.Fatalf("guided workflow source = %+v, want local source %s", cfg.WorkflowSource, sourceAbs)
	}
	wantCredentials := map[string]string{
		string(capability.GitHubIssuesWrite): "GOOBERS_GITHUB_ISSUES_TOKEN",
		string(capability.ProviderPRWrite):   "GOOBERS_GITHUB_PR_TOKEN",
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
	t.Setenv("GOOBERS_GITHUB_REPO_TOKEN", "repo-read-token")
	t.Setenv("GOOBERS_GITHUB_ISSUES_TOKEN", "issues-write-token")
	t.Setenv("GOOBERS_GITHUB_PR_TOKEN", "pr-write-token")
	t.Setenv("GOOBERS_GITHUB_PUSH_TOKEN", "push-token")
	resolver, grants, err := buildCredentials(cfg, nil, "acme", "Widget.Service", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	if got := resolveGrants(t, resolver, grants)[string(capability.ProviderPRWrite)]; got != "pr-write-token" {
		t.Fatalf("guided provider PR credential = %q, want pull-request token", got)
	}
	for _, name := range instance.GuidedWorkflowNames() {
		path := filepath.Join(root, "config", "gaggles", "widget-service", "workflows", name+".yaml")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("selected workflow %q not scaffolded: %v", name, err)
		}
	}
	for _, path := range []string{
		filepath.Join(sourceRoot, instance.GuidedSourceInstanceFile),
		filepath.Join(sourceRoot, "manifest.yaml"),
		filepath.Join(sourceRoot, "gaggles", "widget-service", "gaggle.yaml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("config source path %s not scaffolded: %v", path, err)
		}
	}
	for _, runtimePath := range []string{
		instance.ConfigFileName,
		instance.TelemetryDBName,
		instance.SchedulerDirName,
	} {
		if _, err := os.Stat(filepath.Join(sourceRoot, runtimePath)); !os.IsNotExist(err) {
			t.Errorf("runtime path %s leaked into config source: %v", runtimePath, err)
		}
	}
}

func TestGuidedInitDefaultPathCreatesSiblingConfigSource(t *testing.T) {
	base := t.TempDir()
	instanceRoot := filepath.Join(base, "goobers")
	if err := os.Mkdir(instanceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(instanceRoot)
	input := strings.NewReader(strings.Join([]string{
		"",
		"",
		"acme/widget",
		"",
		"work-nomination",
		"", // accept the default harness (copilot)
		"",
		"",
		"",
		"",
		"yes",
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer

	code := runInitWithInput([]string{"--guided"}, input, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("guided init code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	sourceRoot := filepath.Join(base, "goobers-config")
	if _, err := os.Stat(filepath.Join(sourceRoot, instance.GuidedSourceInstanceFile)); err != nil {
		t.Fatalf("default config source was not created as an instance sibling: %v", err)
	}
	if _, err := os.Stat(filepath.Join(instanceRoot, instance.ConfigFileName)); err != nil {
		t.Fatalf("default instance was not created: %v", err)
	}
	if !strings.Contains(stdout.String(), "config-source: "+sourceRoot) {
		t.Fatalf("guided output does not show sibling source %q:\n%s", sourceRoot, stdout.String())
	}
}

func TestDocumentationURLPinsStableReleaseBuilds(t *testing.T) {
	originalVersion := version.Version
	t.Cleanup(func() { version.Version = originalVersion })

	for _, test := range []struct {
		version string
		ref     string
	}{
		{version: "dev", ref: "main"},
		{version: "db438b0", ref: "main"},
		{version: "v1.2.3", ref: "v1.2.3"},
	} {
		version.Version = test.version
		want := "https://github.com/Agent-Clubhouse/Goobers/blob/" + test.ref + "/docs/concepts/README.md"
		if got := documentationURL("docs/concepts/README.md"); got != want {
			t.Errorf("documentationURL with version %q = %q, want %q", test.version, got, want)
		}
	}
}

// TestPromptGuidedOptionsSelectsClaudeCodeHarness pins #2777: claude-code
// must be choosable in the guided prompt flow (not just discoverable via
// --harness), and choosing it must route the optional model-auth token
// through ClaudeTokenEnv instead of CopilotTokenEnv.
func TestPromptGuidedOptionsSelectsClaudeCodeHarness(t *testing.T) {
	input := strings.NewReader(strings.Join([]string{
		"acme/widget",
		"",
		"work-nomination",
		"claude-code",
		"",
		"",
		"WIDGET_CLAUDE_TOKEN",
	}, "\n") + "\n")
	var stdout bytes.Buffer

	opts, err := promptGuidedOptions(input, &stdout)
	if err != nil {
		t.Fatalf("promptGuidedOptions: %v", err)
	}
	if opts.Harness != "claude-code" {
		t.Fatalf("opts.Harness = %q, want claude-code", opts.Harness)
	}
	if opts.ClaudeTokenEnv != "WIDGET_CLAUDE_TOKEN" || opts.CopilotTokenEnv != "" {
		t.Fatalf("unexpected model-auth token refs: claude=%q copilot=%q", opts.ClaudeTokenEnv, opts.CopilotTokenEnv)
	}
	if !strings.Contains(stdout.String(), "Claude Code model auth") {
		t.Errorf("stdout lacks the Claude Code model-auth prompt:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "Copilot model auth") {
		t.Errorf("stdout unexpectedly shows the Copilot model-auth prompt:\n%s", stdout.String())
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

// TestPromptGuidedOptionsDetectsCICommandDefault is #2071: the ciCommand
// prompt's defaults are seeded from the invoking directory's build manifest
// instead of unconditionally offering the Go-specific `make ci`. Accepting
// them must produce the stack-appropriate command and capability, and the
// detection message must identify the current directory as a guess.
func TestPromptGuidedOptionsDetectsCICommandDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	input := strings.NewReader(strings.Join([]string{
		"acme/widget",
		"",
		"implementation",
		"", // accept the detected default
		"", // accept the detected capability
		"",
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
	if !slices.Equal(opts.CICommand, []string{"npm", "run", "ci"}) {
		t.Fatalf("opts.CICommand = %v, want [npm run ci]", opts.CICommand)
	}
	if !slices.Equal(opts.RequiredCapabilities, []string{"node@20"}) {
		t.Fatalf("opts.RequiredCapabilities = %v, want [node@20]", opts.RequiredCapabilities)
	}
	if !strings.Contains(stdout.String(), "Guessed Node.js") ||
		!strings.Contains(stdout.String(), "current directory") {
		t.Errorf("stdout lacks the detection message:\n%s", stdout.String())
	}
}

// TestPromptGuidedOptionsForcesExplicitCICommandWhenUndetected is #2071's
// other half: a directory with no recognized build manifest must not
// silently fall back to `make ci` — it must force an explicit answer, and
// exhausting the input without one (matching a fully non-interactive,
// defaults-only driver) is a hard error rather than a silent `make ci`.
func TestPromptGuidedOptionsForcesExplicitCICommandWhenUndetected(t *testing.T) {
	dir := t.TempDir() // no recognized manifest
	t.Chdir(dir)

	input := strings.NewReader(strings.Join([]string{
		"acme/widget",
		"",
		"implementation",
		"", // no default to accept -> invalid, loop
	}, "\n") + "\n")
	var stdout bytes.Buffer

	_, err := promptGuidedOptions(input, &stdout)
	if err == nil {
		t.Fatal("promptGuidedOptions succeeded with no ciCommand answered and nothing detected to default to")
	}
	if !strings.Contains(stdout.String(), "No recognized build manifest") {
		t.Errorf("stdout lacks the no-detection message:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "make ci") {
		t.Errorf("stdout unexpectedly offered make ci with no Makefile present:\n%s", stdout.String())
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
	if err := ensureInitCompleted(root); err != nil {
		t.Fatalf("record init completion: %v", err)
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
	wantStderr := "error: guided setup requires an unconfigured target: " + instance.ConfigFileName +
		" already exists in " + root +
		"; choose an empty path, e.g. `goobers init --guided ./my-instance`\n"
	if stderr.String() != wantStderr {
		t.Fatalf("guided rerun stderr = %q, want %q", stderr.String(), wantStderr)
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

func TestGuidedInitRerunAfterInterruptedMaterializationGivesRecovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "interrupted-instance")
	sourceRoot := root + "-config"
	opts := instance.GuidedOptions{
		GaggleName:           "widget",
		RepoOwner:            "acme",
		RepoName:             "widget",
		RepoTokenEnv:         "REPO_TOKEN",
		WorkTrackingTokenEnv: "ISSUES_TOKEN",
		CopilotTokenEnv:      "MODEL_TOKEN",
		Workflows:            []string{instance.GuidedWorkflowWorkNomination},
	}
	if _, err := instance.SeedGuidedConfigSource(sourceRoot, opts); err != nil {
		t.Fatalf("seed guided source: %v", err)
	}
	var mutationErr error
	mutated := false
	firstStdout := &guidedInitCallbackWriter{onWrite: func(output string) {
		if mutated || !strings.Contains(output, "initialized instance at") {
			return
		}
		mutated = true
		mutationErr = os.WriteFile(
			filepath.Join(root, instance.ConfigDirName, "manifest.yaml"),
			[]byte("not: valid: yaml\n"),
			0o644,
		)
	}}
	firstInput := strings.NewReader(strings.Join([]string{
		guidedSourceExistingLocal,
		sourceRoot,
		"",
		"yes",
	}, "\n") + "\n")
	var firstStderr bytes.Buffer
	code := runInitWithInput([]string{"--guided", root}, firstInput, firstStdout, &firstStderr)
	if mutationErr != nil {
		t.Fatalf("invalidate materialized config: %v", mutationErr)
	}
	if !mutated {
		t.Fatalf("guided init did not reach materialization: stdout = %q, stderr = %q", firstStdout.String(), firstStderr.String())
	}
	if code == 0 || !strings.Contains(firstStderr.String(), "guided setup did not produce a valid instance") {
		t.Fatalf("guided init code = %d, stdout = %q, stderr = %q", code, firstStdout.String(), firstStderr.String())
	}

	input := strings.NewReader("acme/replacement\n")
	var stdout, stderr bytes.Buffer
	code = runInitWithInput([]string{"--guided", root}, input, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("guided rerun code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if input.Len() != len("acme/replacement\n") {
		t.Fatal("guided rerun prompted before reporting interrupted setup recovery")
	}
	for _, want := range []string{
		"no init.completed marker",
		"delete " + strconv.Quote(root),
		"goobers init --guided " + strconv.Quote(root),
		"goobers validate " + strconv.Quote(root),
		"goobers config materialize " + strconv.Quote(root),
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("guided rerun stderr = %q, missing %q", stderr.String(), want)
		}
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
