package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
)

func assertNoStandardMissingSkillWarnings(t *testing.T, report *validate.Report) {
	t.Helper()
	for _, warning := range report.Warnings() {
		if warning.Code == validate.WarningMissingSkillPackage {
			t.Fatalf("standard scaffold emitted a missing-skill-package warning: %+v", warning)
		}
	}
}

func TestInitStandardNonInteractive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "standard")
	code, stdout, stderr := runArgs(t, "init", "--template=standard", "--harness=claude-code", "--ci-command=[\"npm\",\"run\",\"ci\"]", "--required-capabilities=node@24", root)
	if code != 0 {
		t.Fatalf("init code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	set, report, err := instance.LoadConfigDir(filepath.Join(root, "config"))
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	assertNoStandardMissingSkillWarnings(t, report)
	if len(set.Workflows) != 2 || len(set.Goobers) != 3 {
		t.Fatalf("got %d workflows and %d goobers, want canonical pair and its three personas", len(set.Workflows), len(set.Goobers))
	}
	for _, wf := range set.Workflows {
		if wf.Name != "implementation" && wf.Name != "backlog-curation" {
			t.Errorf("unexpected workflow %q", wf.Name)
		}
		for _, task := range wf.Spec.Tasks {
			if task.Name == "local-ci" && !reflect.DeepEqual(task.Run.Command, []string{"npm", "run", "ci"}) {
				t.Errorf("local CI command=%v", task.Run.Command)
			}
		}
	}
	for _, goober := range set.Goobers {
		if goober.Spec.Harness != "claude-code" {
			t.Errorf("%s harness=%s", goober.Name, goober.Spec.Harness)
		}
	}
	if !reflect.DeepEqual(set.Gaggles[0].Spec.CICommand, []string{"npm", "run", "ci"}) {
		t.Fatalf("gaggle CI command=%v", set.Gaggles[0].Spec.CICommand)
	}
}

func TestInitStandardADO(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ado")
	code, stdout, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("ADO init code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	set, report, err := instance.LoadConfigDir(filepath.Join(root, "config"))
	if err != nil || report.HasErrors() {
		t.Fatalf("load: %v report=%+v", err, report)
	}
	assertNoStandardMissingSkillWarnings(t, report)
	if len(set.Gaggles) != 1 || set.Gaggles[0].Spec.Project.Provider != "ado" || set.Gaggles[0].Spec.Project.Project != "your-project" || set.Gaggles[0].Spec.Backlog.Provider != "ado" || set.Gaggles[0].Spec.Backlog.Project != "your-project" {
		t.Fatalf("ADO identity not preserved: %+v", set.Gaggles)
	}
	data, err := os.ReadFile(filepath.Join(root, "instance.yaml"))
	if err != nil || !strings.Contains(string(data), "GOOBERS_ADO_TOKEN") || strings.Contains(string(data), "GOOBERS_GITHUB") {
		t.Fatalf("ADO credentials incorrect: %s %v", data, err)
	}
}

func TestInitStandardMatchesGuidedOutput(t *testing.T) {
	tests := []struct {
		name string
		opts instance.GuidedOptions
		args []string
	}{
		{
			name: "GitHub CLI auth and full workflow selection",
			opts: instance.GuidedOptions{
				GaggleName: "widgets", DisplayName: "acme/widgets", RepoProvider: "github",
				RepoOwner: "acme", RepoName: "widgets", RepoBranch: "release/next", GitHubCLIUser: "octocat",
				Harness: "claude-code", ClaudeTokenEnv: "CLAUDE_TOKEN",
				Workflows:  []string{instance.GuidedWorkflowImplementation, instance.GuidedWorkflowBacklogCuration, instance.GuidedWorkflowWorkNomination},
				IssueScope: "assigned", AssignedTo: "octocat", CICommand: []string{"npm", "run", "ci"},
				RequiredCapabilities: []string{"node@24", "os=linux"},
			},
			args: []string{
				"--provider=github", "--repo=acme/widgets", "--branch=release/next", "--issue-scope=assigned", "--assigned-to=octocat",
				"--workflows=implementation,backlog-curation,work-nomination", "--harness=claude-code",
				`--ci-command=["npm","run","ci"]`, "--required-capabilities=node@24,os=linux",
				"--github-cli-user=octocat", "--model-token-env=CLAUDE_TOKEN",
				"--repo-auth-kind=", "--repo-token-env=", "--work-tracking-token-env=", "--pr-token-env=", "--push-token-env=",
			},
		},
		{
			name: "Azure CLI auth and pull request CI",
			opts: instance.GuidedOptions{
				GaggleName: "widgets", DisplayName: "acme/platform/widgets", RepoProvider: "ado",
				RepoOwner: "acme", RepoProject: "platform", RepoName: "widgets", RepoBranch: "main",
				RepoAuthKind: instance.ADOAuthAzureCLI, Harness: "copilot",
				Workflows: []string{instance.GuidedWorkflowImplementation}, IssueScope: "all", PullRequestCI: true,
			},
			args: []string{
				"--provider=ado", "--repo=acme/platform/widgets", "--branch=main", "--issue-scope=all",
				"--workflows=implementation", "--harness=copilot", "--pr-ci", "--repo-auth-kind=",
				"--repo-token-env=", "--work-tracking-token-env=", "--pr-token-env=", "--push-token-env=",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directRoot := filepath.Join(t.TempDir(), "guided")
			if _, err := instance.InitGuided(directRoot, test.opts); err != nil {
				t.Fatalf("direct guided init: %v", err)
			}
			cliRoot := filepath.Join(t.TempDir(), "cli")
			args := append([]string{"init", "--template=standard"}, test.args...)
			args = append(args, cliRoot)
			if code, stdout, stderr := runArgs(t, args...); code != 0 {
				t.Fatalf("CLI init code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			if got, want := generatedInitTree(t, cliRoot), generatedInitTree(t, directRoot); !reflect.DeepEqual(got, want) {
				t.Fatalf("CLI and guided output differ\nCLI: %#v\nguided: %#v", got, want)
			}
		})
	}
}

func generatedInitTree(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, entry := range []string{instance.ConfigFileName, instance.ConfigDirName} {
		path := filepath.Join(root, entry)
		if err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			got[filepath.ToSlash(relative)] = string(data)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return got
}

func TestInitStandardRejectsUnsafeOrIncompleteOptions(t *testing.T) {
	for _, extra := range [][]string{
		{}, {"--ci-command=[]", "--required-capabilities=node@24"},
		{"--ci-command=[\"npm\",\"\"]", "--required-capabilities=node@24"},
		{"--ci-command=[\"npm\"]"},
		{"--ci-command=[\"npm\"]", "--required-capabilities=invalid capability"},
		{"--ci-command=[\"npm\"] trailing", "--required-capabilities=node@24"},
	} {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "absent")
			args := append([]string{"init", "--template=standard"}, extra...)
			code, _, stderr := runArgs(t, append(args, root)...)
			if code == 0 {
				t.Fatalf("invalid options accepted: %v", extra)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("invalid init wrote destination: %v; %s", err, stderr)
			}
		})
	}
}

func TestInitStandardWorkflowValidation(t *testing.T) {
	valid := [][]string{
		{"--workflows=work-nomination"},
		{"--workflows=implementation", "--pr-ci"},
	}
	for _, extra := range valid {
		t.Run("valid "+strings.Join(extra, " "), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "instance")
			args := append([]string{"init", "--template=standard", "--repo=acme/widgets"}, extra...)
			if code, _, stderr := runArgs(t, append(args, root)...); code != 0 {
				t.Fatalf("valid standard options refused: %s", stderr)
			}
		})
	}
	invalid := [][]string{
		{"--workflows="},
		{"--workflows=implementation", "--pr-ci", `--ci-command=["npm"]`},
		{"--workflows=work-nomination", `--ci-command=["npm"]`, "--required-capabilities=node@24"},
		{"--workflows=work-nomination", "--issue-scope=assigned"},
		{"--provider=ado", "--repo=acme/widgets"},
	}
	for _, extra := range invalid {
		t.Run("invalid "+strings.Join(extra, " "), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "instance")
			args := append([]string{"init", "--template=standard"}, extra...)
			if code, _, _ := runArgs(t, append(args, root)...); code != 2 {
				t.Fatalf("invalid standard options accepted: %v", extra)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("invalid init wrote destination: %v", err)
			}
		})
	}
}

func TestInitStandardPreservesExistingTarget(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "instance.yaml")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, _ := runArgs(t, "init", "--template=standard", "--ci-command=[\"npm\"]", "--required-capabilities=node@24", root)
	if code == 0 {
		t.Fatal("populated destination accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep me" {
		t.Fatalf("existing file changed: %q %v", data, err)
	}
}

func TestInitStandardModeBoundaries(t *testing.T) {
	for _, flags := range [][]string{
		{"--provider=ado"},
		{"--repo="},
		{"--branch="},
		{"--pr-ci=false"},
		{"--template=quickstart", "--provider=ado"},
		{"--template=standard", "--ci-command=[\"npm\"]", "--required-capabilities=node@24", "--provider=unknown"},
		{"--ci-command=[\"npm\"]", "--required-capabilities=node@24"},
		{"--template=quickstart", "--ci-command=[\"npm\"]"},
		{"--template=standard", "--ci-command=[\"npm\"]", "--required-capabilities=node@24", "--demo"},
		{"--template=standard", "--ci-command=[\"npm\"]", "--required-capabilities=node@24", "--guided"},
		{"--template=standard", "--ci-command=[\"npm\"]", "--required-capabilities=node@24", "--harness=unknown"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "absent")
			args := append([]string{"init"}, flags...)
			code, _, stderr := runArgs(t, append(args, root)...)
			if code != 2 {
				t.Fatalf("usage code=%d: %s", code, stderr)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("invalid mode created target: %v", err)
			}
		})
	}
}
