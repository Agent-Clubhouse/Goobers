package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func TestOnboardingActionsComposeToCleanInstance(t *testing.T) {
	base := onboardingTestTempDir(t)
	sampleRoot := filepath.Join(base, "sample")
	sourceRoot := filepath.Join(base, "config-source")
	instanceRoot := filepath.Join(base, "instance")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	runAgentKitTestGit(t, sourceRoot, "init", "--quiet")
	invocations := [][]string{
		{
			"onboarding", "stub-sample",
			"--destination", sampleRoot,
			"--json",
		},
		{
			"init",
			"--template=quickstart",
			"--source-tree", sourceRoot,
			"--json",
		},
		{
			"onboarding", "stub-agent-instructions",
			"--source-tree", sourceRoot,
			"--harness", "generic",
			"--json",
		},
	}

	results := make([]onboardingActionResult, 0, len(invocations))
	for _, args := range invocations {
		code, stdout, stderr := runOnboardingFixtureArgs(t, args...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
		var result onboardingActionResult
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatalf("%v: decode result: %v\n%s", args, err, stdout)
		}
		if result.Version != onboardingActionVersion {
			t.Fatalf("%v: version=%d, want %d", args, result.Version, onboardingActionVersion)
		}
		switch result.Action {
		case stubSampleAction:
			result.Path = "<sample>"
		case seedConfigSourceAction, stubAgentInstructionsAction:
			result.Path = "<source-tree>"
			result.NextCommand = strings.ReplaceAll(
				result.NextCommand,
				absolutePath(sourceRoot),
				"<source-tree>",
			)
			for i := range result.Commands {
				result.Commands[i] = strings.ReplaceAll(
					result.Commands[i],
					absolutePath(sourceRoot),
					"<source-tree>",
				)
			}
		default:
			t.Fatalf("%v: unexpected action %q", args, result.Action)
		}
		results = append(results, result)
	}

	assertCleanValidation := func(args ...string) {
		t.Helper()
		code, stdout, stderr := runArgs(t, args...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
		var result diagnosticsEnvelope
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatalf("%v: decode validation: %v\n%s", args, err, stdout)
		}
		if !result.OK || result.Counts.Errors != 0 {
			t.Fatalf("%v: validation had errors: %s", args, stdout)
		}
		for _, finding := range result.Findings {
			if finding.Code != placeholderFindingCode || finding.Severity != "warning" {
				t.Fatalf("%v: validation had a non-placeholder finding: %s", args, stdout)
			}
		}
	}

	// implement/run-tests/review already ship as gaggle-scoped packages inside
	// the quickstart-v1 template (SKILL002 fix); declaring shared-level
	// stand-ins for those too would collide with the scoped ones (SKILL001)
	// instead of being a harmless no-op. Only the stub-agent-instructions
	// additions still need a stand-in.
	tutorSkills := []string{"config-authoring", "nomination", "triage", "tutor-diagnosis"}
	createDeclaredSkillPackages(t, filepath.Dir(sourceRoot), tutorSkills...)
	assertCleanValidation("validate", "--source-tree", "--json", sourceRoot)

	sourceConfig, err := instance.LoadGuidedSourceConfig(sourceRoot)
	if err != nil {
		t.Fatalf("load composed config source: %v", err)
	}
	if _, err := instance.InitGuidedFromSource(instanceRoot, sourceRoot, sourceConfig); err != nil {
		t.Fatalf("materialize composed instance: %v", err)
	}
	createDeclaredSkillPackages(t, instanceRoot, tutorSkills...)
	assertCleanValidation("validate", "--json", instanceRoot)

	var golden bytes.Buffer
	encoder := json.NewEncoder(&golden)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(results); err != nil {
		t.Fatal(err)
	}
	assertGoldenFile(
		t,
		filepath.Join("testdata", "onboarding", "composed.golden.json"),
		golden.String(),
	)
}

func TestOnboardingStubSampleDispatchesFromCLI(t *testing.T) {
	destination := filepath.Join(onboardingTestTempDir(t), "sample")
	code, stdout, stderr := runArgs(t, "onboarding", "stub-sample", "--destination", destination, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("onboarding stub-sample: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var result onboardingActionResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode result: %v\n%s", err, stdout)
	}
	if result.Action != stubSampleAction {
		t.Fatalf("result.Action = %q, want %q", result.Action, stubSampleAction)
	}
	if result.Version != onboardingActionVersion {
		t.Fatalf("result.Version = %d, want %d", result.Version, onboardingActionVersion)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("sample directory not created: %v", err)
	}
}

func TestOnboardingStubAgentInstructionsDestinationGoldens(t *testing.T) {
	for _, fixture := range []string{"empty", "partial", "populated"} {
		t.Run(fixture, func(t *testing.T) {
			root := cliAgentKitRepository(t)
			if _, err := instance.SeedQuickstartConfigSource(root); err != nil {
				t.Fatalf("seed config source: %v", err)
			}
			switch fixture {
			case "partial":
				code, _, stderr := runArgs(t, "agent-kit", "install", root)
				if code != 0 {
					t.Fatalf("seed toolkit: code=%d stderr=%q", code, stderr)
				}
				instruction := filepath.Join(root, ".github", "copilot-instructions.md")
				if err := os.MkdirAll(filepath.Dir(instruction), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(instruction, []byte("# User-owned instructions\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "populated":
				code, _, stderr := runArgs(t, "agent-kit", "install", "--harness", "copilot", root)
				if code != 0 {
					t.Fatalf("seed populated fixture: code=%d stderr=%q", code, stderr)
				}
			}

			code, stdout, stderr := runOnboardingFixtureArgs(
				t,
				"onboarding", "stub-agent-instructions",
				"--source-tree", root,
				"--harness", "copilot",
				"--json",
			)
			if code != 0 || stderr != "" {
				t.Fatalf("stub agent instructions: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			var result onboardingActionResult
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("decode result: %v\n%s", err, stdout)
			}
			if len(result.Prompts) != 3 ||
				!strings.Contains(result.Prompts[0], "Getting Started") ||
				!strings.Contains(result.Prompts[1], "run operator") ||
				!strings.Contains(result.Prompts[1], "<instance-path>") ||
				!strings.Contains(result.Prompts[2], "workflow upgrade") {
				t.Fatalf("starter prompts = %v", result.Prompts)
			}
			if len(result.Commands) != 3 ||
				!strings.Contains(result.Commands[0], "agent-kit check") ||
				!strings.Contains(result.Commands[1], "agent-kit update") ||
				!strings.Contains(result.Commands[2], "agent-kit update --write") {
				t.Fatalf("maintenance commands = %v", result.Commands)
			}
			result.Path = "<source-tree>"
			result.NextCommand = "goobers agent-kit check '<source-tree>'"
			for i := range result.Commands {
				result.Commands[i] = strings.ReplaceAll(result.Commands[i], root, "<source-tree>")
			}
			normalized, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			assertGoldenFile(
				t,
				filepath.Join("testdata", "onboarding", "stub-agent-instructions."+fixture+".golden.json"),
				string(normalized)+"\n",
			)

			instruction, err := os.ReadFile(filepath.Join(root, ".github", "copilot-instructions.md"))
			if err != nil {
				t.Fatal(err)
			}
			if fixture == "partial" && !strings.HasPrefix(string(instruction), "# User-owned instructions\n") {
				t.Fatalf("user instructions were not preserved: %q", instruction)
			}
			if !strings.Contains(string(instruction), ".goobers/agent-toolkit/adapters/copilot.md") {
				t.Fatalf("managed reference missing from instructions: %q", instruction)
			}
		})
	}
}

func TestOnboardingStubAgentInstructionsRefusesToolkitCollision(t *testing.T) {
	root := cliAgentKitRepository(t)
	if _, err := instance.SeedQuickstartConfigSource(root); err != nil {
		t.Fatalf("seed config source: %v", err)
	}
	toolkit := filepath.Join(root, ".goobers", "agent-toolkit")
	if err := os.MkdirAll(toolkit, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(toolkit, "user-owned.txt")
	if err := os.WriteFile(sentinel, []byte("preserve me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runOnboardingFixtureArgs(
		t,
		"onboarding", "stub-agent-instructions",
		"--source-tree", root,
		"--harness", "generic",
		"--json",
	)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "exists without an installed manifest") {
		t.Fatalf("collision: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "preserve me\n" {
		t.Fatalf("user-owned toolkit collision changed: data=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("instruction file written after collision: %v", err)
	}
}

func TestOnboardingStubAgentInstructionsRejectsNonConfigRepository(t *testing.T) {
	root := cliAgentKitRepository(t)
	sentinel := filepath.Join(root, "README.md")
	if err := os.WriteFile(sentinel, []byte("application repository\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(
		t,
		"onboarding", "stub-agent-instructions",
		"--source-tree", root,
		"--harness", "generic",
		"--json",
	)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "not a valid Goobers config source") {
		t.Fatalf("non-config target: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, ".goobers")); !os.IsNotExist(err) {
		t.Fatalf("non-config target received toolkit files: %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "application repository\n" {
		t.Fatalf("non-config target changed: data=%q err=%v", got, err)
	}
}

func TestOnboardingStubAgentInstructionsFlagValidation(t *testing.T) {
	root := cliAgentKitRepository(t)
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missing source", args: []string{"onboarding", "stub-agent-instructions"}, want: "Usage:"},
		{
			name: "missing harness",
			args: []string{
				"onboarding", "stub-agent-instructions",
				"--source-tree", root,
			},
			want: "Usage:",
		},
		{
			name: "unsupported harness",
			args: []string{
				"onboarding", "stub-agent-instructions",
				"--source-tree", root,
				"--harness", "other",
			},
			want: "unsupported harness",
		},
		{
			name: "positional argument",
			args: []string{
				"onboarding", "stub-agent-instructions",
				"--source-tree", root,
				"extra",
			},
			want: "Usage:",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, test.args...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, test.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q, want %q", code, stdout, stderr, test.want)
			}
		})
	}
}

func TestOnboardingStubSampleDestinationGoldens(t *testing.T) {
	files, _, err := loadOnboardingSample()
	if err != nil {
		t.Fatal(err)
	}
	source := make(map[string][]byte, len(files))
	for _, file := range files {
		source[file.path] = file.data
	}

	for _, fixture := range []string{"empty", "partial", "populated"} {
		t.Run(fixture, func(t *testing.T) {
			destination := filepath.Join(onboardingTestTempDir(t), "sample")
			switch fixture {
			case "partial":
				if err := os.MkdirAll(destination, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(destination, "README.md"), source["README.md"], 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(destination, "user-note.txt"), []byte("preserve me\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "populated":
				if _, err := materializeOnboardingSample(destination, files, false); err != nil {
					t.Fatal(err)
				}
			}

			code, stdout, stderr := runOnboardingFixtureArgs(
				t,
				"onboarding", "stub-sample",
				"--destination", destination,
				"--json",
			)
			if code != 0 {
				t.Fatalf("stub-sample: code=%d stderr=%q", code, stderr)
			}
			var result onboardingActionResult
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("decode result: %v\n%s", err, stdout)
			}
			result.Path = "<destination>"
			normalized, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			assertGoldenFile(
				t,
				filepath.Join("testdata", "onboarding", "stub-sample."+fixture+".golden.json"),
				string(normalized)+"\n",
			)

			if fixture == "partial" {
				note, err := os.ReadFile(filepath.Join(destination, "user-note.txt"))
				if err != nil || string(note) != "preserve me\n" {
					t.Fatalf("user-owned extra file changed: data=%q err=%v", note, err)
				}
			}
			for path, want := range source {
				got, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(path)))
				if err != nil {
					t.Fatalf("read materialized %s: %v", path, err)
				}
				if string(got) != string(want) {
					t.Errorf("materialized %s differs from embedded source", path)
				}
			}
		})
	}
}

func TestOnboardingStubSampleRefusesClobberBeforeWriting(t *testing.T) {
	files, _, err := loadOnboardingSample()
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(onboardingTestTempDir(t), "sample")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	conflict := filepath.Join(destination, "package.json")
	if err := os.WriteFile(conflict, []byte("user-owned\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runOnboardingFixtureArgs(t, "onboarding", "stub-sample", "--destination", destination)
	if code != 1 || !strings.Contains(stderr, "without --force") {
		t.Fatalf("without force: code=%d stderr=%q", code, stderr)
	}
	if got, err := os.ReadFile(conflict); err != nil || string(got) != "user-owned\n" {
		t.Fatalf("conflict changed before refusal: data=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("preflight wrote files before refusing conflict: %v", err)
	}

	code, _, stderr = runOnboardingFixtureArgs(t, "onboarding", "stub-sample", "--destination", destination, "--force")
	if code != 0 {
		t.Fatalf("with force: code=%d stderr=%q", code, stderr)
	}
	var packageJSON []byte
	for _, file := range files {
		if file.path == "package.json" {
			packageJSON = file.data
			break
		}
	}
	if got, err := os.ReadFile(conflict); err != nil || string(got) != string(packageJSON) {
		t.Fatalf("forced file differs from embedded source: err=%v", err)
	}
}

func TestWriteOnboardingSampleFileDoesNotReplaceFileCreatedAfterPreflight(t *testing.T) {
	dir := onboardingTestTempDir(t)
	target := filepath.Join(dir, "package.json")
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("target unexpectedly exists before simulated preflight: %v", err)
	}
	if err := os.WriteFile(target, []byte("user-owned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	err = writeOnboardingSampleFile(root, "package.json", []byte("sample\n"), false, 0)
	if err == nil || !strings.Contains(err.Error(), "without replacing destination") {
		t.Fatalf("writeOnboardingSampleFile error = %v, want no-replace publication failure", err)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "user-owned\n" {
		t.Fatalf("file created after preflight was replaced: data=%q err=%v", got, readErr)
	}
}

func TestOnboardingStubSampleRefusesParentReplacedBySymlinkAfterPreflight(t *testing.T) {
	base := onboardingTestTempDir(t)
	parent := filepath.Join(base, "parent")
	destination := filepath.Join(parent, "sample")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	movedParent := filepath.Join(base, "moved-parent")
	outside := onboardingTestTempDir(t)

	previous := beforeOnboardingSamplePublish
	beforeOnboardingSamplePublish = func() {
		if err := os.Rename(parent, movedParent); err != nil {
			t.Skipf("cannot replace open parent on this platform: %v", err)
		}
		if err := os.Symlink(outside, parent); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
	}
	t.Cleanup(func() { beforeOnboardingSamplePublish = previous })

	files := []onboardingSampleFile{{path: "package.json", data: []byte("sample\n")}}
	_, err := materializeOnboardingSample(destination, files, false)
	if err == nil || !strings.Contains(err.Error(), "symbolic-link destination ancestor") {
		t.Fatalf("materializeOnboardingSample error = %v, want substituted-parent refusal", err)
	}
	if entries, readErr := os.ReadDir(outside); readErr != nil || len(entries) != 0 {
		t.Fatalf("sample was redirected through substituted parent: entries=%v err=%v", entries, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(movedParent, "sample", "package.json")); !os.IsNotExist(statErr) {
		t.Fatalf("sample was published after destination binding changed: %v", statErr)
	}
}

func TestOnboardingStubSampleRefusesSymlinkedDestinationAncestor(t *testing.T) {
	parent := onboardingTestTempDir(t)
	outside := onboardingTestTempDir(t)
	link := filepath.Join(parent, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	destination := filepath.Join(link, "sample")

	code, _, stderr := runOnboardingFixtureArgs(t, "onboarding", "stub-sample", "--destination", destination)
	if code != 1 || !strings.Contains(stderr, "symbolic-link destination ancestor") {
		t.Fatalf("symlinked ancestor: code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(outside, "sample")); !os.IsNotExist(err) {
		t.Fatalf("sample was written through symlinked ancestor: %v", err)
	}
}

func TestOnboardingStubSampleForceReplacesReadOnlyConflict(t *testing.T) {
	files, _, err := loadOnboardingSample()
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(onboardingTestTempDir(t), "sample")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	conflict := filepath.Join(destination, "package.json")
	if err := os.WriteFile(conflict, []byte("user-owned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(conflict, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(conflict, 0o600) })

	code, _, stderr := runOnboardingFixtureArgs(t, "onboarding", "stub-sample", "--destination", destination, "--force")
	if code != 0 {
		t.Fatalf("with force: code=%d stderr=%q", code, stderr)
	}
	for _, file := range files {
		got, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(file.path)))
		if err != nil {
			t.Fatalf("read materialized %s: %v", file.path, err)
		}
		if string(got) != string(file.data) {
			t.Errorf("materialized %s differs from embedded source", file.path)
		}
	}
}

func TestOnboardingStubSampleReportsPendingWithoutCredentials(t *testing.T) {
	const tokenEnv = "GOOBERS_ONBOARDING_TEST_MISSING_TOKEN"
	original, hadOriginal := os.LookupEnv(tokenEnv)
	if err := os.Unsetenv(tokenEnv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadOriginal {
			_ = os.Setenv(tokenEnv, original)
		} else {
			_ = os.Unsetenv(tokenEnv)
		}
	})

	called := false
	previous := newOnboardingIssueSeeder
	newOnboardingIssueSeeder = func(string) onboardingIssueSeeder {
		called = true
		return nil
	}
	t.Cleanup(func() { newOnboardingIssueSeeder = previous })

	code, stdout, stderr := runOnboardingFixtureArgs(
		t,
		"onboarding", "stub-sample",
		"--destination", filepath.Join(onboardingTestTempDir(t), "sample"),
		"--work-tracking", "acme/tutorial",
		"--token-env", tokenEnv,
		"--json",
	)
	if code != 0 {
		t.Fatalf("stub-sample: code=%d stderr=%q", code, stderr)
	}
	if called {
		t.Fatal("provider was constructed without credentials")
	}
	var result onboardingActionResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	var pending int
	for _, item := range result.Skipped {
		if strings.Contains(item, "(pending: credentials unavailable)") {
			pending++
		}
	}
	if pending != 3 {
		t.Fatalf("pending issue count = %d, want 3; skipped=%v", pending, result.Skipped)
	}
}

func TestOnboardingStubSampleSeedsLabelsAndIssuesIdempotently(t *testing.T) {
	const tokenEnv = "GOOBERS_ONBOARDING_TEST_TOKEN"
	t.Setenv(tokenEnv, "test-token")
	seeder := &fakeOnboardingIssueSeeder{labels: map[string]bool{}}
	previous := newOnboardingIssueSeeder
	newOnboardingIssueSeeder = func(token string) onboardingIssueSeeder {
		if token != "test-token" {
			t.Fatalf("provider token = %q", token)
		}
		return seeder
	}
	t.Cleanup(func() { newOnboardingIssueSeeder = previous })

	destination := filepath.Join(onboardingTestTempDir(t), "sample")
	run := func() onboardingActionResult {
		t.Helper()
		code, stdout, stderr := runOnboardingFixtureArgs(
			t,
			"onboarding", "stub-sample",
			"--destination", destination,
			"--work-tracking", "acme/tutorial",
			"--token-env", tokenEnv,
			"--json",
		)
		if code != 0 {
			t.Fatalf("stub-sample: code=%d stderr=%q", code, stderr)
		}
		var result onboardingActionResult
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	first := run()
	for _, want := range []string{
		"label:goobers:approved",
		"label:goobers:ready",
		"issue:reject-empty-task-titles",
		"issue:make-completion-idempotent",
		"issue:filter-tasks-by-status",
	} {
		if !slices.Contains(first.Created, want) {
			t.Errorf("first created lacks %q: %v", want, first.Created)
		}
	}
	if len(seeder.createRequests) != 3 {
		t.Fatalf("created issues = %d, want 3", len(seeder.createRequests))
	}
	for _, request := range seeder.createRequests {
		if request.Repository.Owner != "acme" || request.Repository.Name != "tutorial" || request.RunID == "" {
			t.Errorf("create request = %+v", request)
		}
	}

	second := run()
	if len(second.Created) != 0 {
		t.Fatalf("second created = %v, want none", second.Created)
	}
	for _, want := range []string{
		"label:goobers:approved",
		"label:goobers:ready",
		"issue:reject-empty-task-titles",
		"issue:make-completion-idempotent",
		"issue:filter-tasks-by-status",
	} {
		if !slices.Contains(second.Skipped, want) {
			t.Errorf("second skipped lacks %q: %v", want, second.Skipped)
		}
	}
	if len(seeder.createRequests) != 3 {
		t.Fatalf("rerun created additional issues: %d total", len(seeder.createRequests))
	}
}

func onboardingTestTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func runOnboardingFixtureArgs(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	if len(args) >= 2 && args[0] == "onboarding" && args[1] == "stub-sample" {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		code := runOnboardingStubSample(args[2:], &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	return runArgs(t, args...)
}

type fakeOnboardingIssueSeeder struct {
	labels         map[string]bool
	items          []providers.WorkItem
	createRequests []providers.CreateWorkItemRequest
}

func (f *fakeOnboardingIssueSeeder) EnsureWorkItemLabels(
	_ context.Context,
	_ providers.RepositoryRef,
	labels []providers.WorkItemLabel,
) (providers.EnsureWorkItemLabelsResult, error) {
	result := providers.EnsureWorkItemLabelsResult{Created: []string{}, Skipped: []string{}}
	for _, label := range labels {
		if f.labels[label.Name] {
			result.Skipped = append(result.Skipped, label.Name)
		} else {
			f.labels[label.Name] = true
			result.Created = append(result.Created, label.Name)
		}
	}
	return result, nil
}

func (f *fakeOnboardingIssueSeeder) ListWorkItems(
	context.Context,
	providers.ListWorkItemsRequest,
) ([]providers.WorkItem, error) {
	return append([]providers.WorkItem(nil), f.items...), nil
}

func (f *fakeOnboardingIssueSeeder) CreateWorkItem(
	_ context.Context,
	request providers.CreateWorkItemRequest,
) (providers.WorkItem, error) {
	f.createRequests = append(f.createRequests, request)
	item := providers.WorkItem{
		Provider: providers.ProviderGitHub,
		ID:       request.RunID,
		Title:    request.Title,
		Body:     request.Body + "\n\n---\ngoobers run-id: " + request.RunID,
		Labels:   append([]string(nil), request.Labels...),
	}
	f.items = append(f.items, item)
	return item, nil
}
