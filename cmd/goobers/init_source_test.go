package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	goobersassets "github.com/goobers/goobers"
	"github.com/goobers/goobers/internal/agentkit"
	"github.com/goobers/goobers/internal/instance"
)

type fakeGuidedGitHubOperations struct {
	clone       func(string, string, string) error
	cloneCalls  []string
	createCalls []string
}

func seedGuidedSourceForTest(root string, opts instance.GuidedOptions) error {
	_, err := instance.SeedGuidedConfigSource(root, opts)
	return err
}

func (f *fakeGuidedGitHubOperations) Clone(_ context.Context, owner, name, destination string) error {
	f.cloneCalls = append(f.cloneCalls, owner+"/"+name+" -> "+destination)
	if f.clone != nil {
		return f.clone(owner, name, destination)
	}
	return nil
}

func (f *fakeGuidedGitHubOperations) Create(_ context.Context, owner, name, visibility, _ string) error {
	f.createCalls = append(f.createCalls, owner+"/"+name+" "+visibility)
	return nil
}

func TestGuidedInitUsesExistingLocalSourceNonDestructively(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "config-source")
	if err := seedGuidedSourceForTest(sourceRoot, guidedSourceTestOptions()); err != nil {
		t.Fatalf("SeedGuidedConfigSource: %v", err)
	}
	sentinel := filepath.Join(sourceRoot, "README.md")
	if err := os.WriteFile(sentinel, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	instanceRoot := filepath.Join(t.TempDir(), "instance")
	input := strings.NewReader(strings.Join([]string{
		guidedSourceExistingLocal,
		sourceRoot,
		"",
		"yes",
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer

	res, result, code, err := runGuidedInit(
		instanceRoot,
		input,
		&stdout,
		&stderr,
		&fakeGuidedGitHubOperations{},
	)
	if err != nil || code != 0 {
		t.Fatalf("runGuidedInit = result %+v code %d err %v, stdout=%q stderr=%q", result, code, err, stdout.String(), stderr.String())
	}
	if res == nil || result.ConfigRepo != sourceRoot ||
		result.TargetRepo != "https://github.com/app-org/widget-service" {
		t.Fatalf("guided result = res %+v mapping %+v", res, result)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "keep me\n" {
		t.Fatalf("existing source changed: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(instanceRoot, "config", "README.md")); !os.IsNotExist(err) {
		t.Fatalf("non-definition source file materialized into runtime config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, agentkit.InstalledRoot)); !os.IsNotExist(err) {
		t.Fatalf("skipped toolkit installation changed config source: %v", err)
	}
	if !strings.Contains(stdout.String(), "Skipping agent toolkit installation; no toolkit files will be written.") {
		t.Fatalf("guided output did not report side-effect-free skip:\n%s", stdout.String())
	}
}

func TestGuidedInitInstallsAgentToolkitForEachHarness(t *testing.T) {
	tests := []struct {
		harness     string
		instruction string
	}{
		{harness: "copilot", instruction: ".github/copilot-instructions.md"},
		{harness: "claude", instruction: "CLAUDE.md"},
		{harness: "generic", instruction: "AGENTS.md"},
	}
	for _, test := range tests {
		t.Run(test.harness, func(t *testing.T) {
			base := onboardingTestTempDir(t)
			sourceRoot := filepath.Join(base, "config-source")
			instanceRoot := filepath.Join(base, "instance")
			input := strings.NewReader(strings.Join([]string{
				guidedSourceNewLocal,
				sourceRoot,
				"app-org/widget-service",
				"",
				"work-nomination",
				"", // accept the default harness (copilot)
				"",
				"",
				"",
				"no",
				"yes",
				test.harness,
				"",
				"yes",
			}, "\n") + "\n")
			var stdout, stderr bytes.Buffer

			_, _, code, err := runGuidedInit(
				instanceRoot,
				input,
				&stdout,
				&stderr,
				&fakeGuidedGitHubOperations{},
			)
			if err != nil || code != 0 {
				t.Fatalf("runGuidedInit: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
			}
			for _, path := range []string{
				filepath.Join(sourceRoot, ".git"),
				filepath.Join(sourceRoot, filepath.FromSlash(agentkit.InstalledManifestPath)),
				filepath.Join(sourceRoot, filepath.FromSlash(test.instruction)),
			} {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("installed path %s: %v", path, err)
				}
			}
			instruction, err := os.ReadFile(filepath.Join(sourceRoot, filepath.FromSlash(test.instruction)))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(instruction), ".goobers/agent-toolkit/adapters/") {
				t.Fatalf("instruction lacks adapter reference: %q", instruction)
			}
			for _, want := range []string{
				"Agent toolkit installation preview:",
				"destination:       " + sourceRoot,
				"harness:           " + test.harness,
				"Starter prompts:",
				"Goobers Getting Started skill",
				"Goobers run operator skill",
				"Goobers instance at " + instanceRoot,
				"Goobers workflow upgrade skill",
				"goobers agent-kit check",
				"goobers agent-kit update --write",
			} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("guided output lacks %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}

func TestGuidedInitHandlesNonGitExistingLocalSourceAfterHarnessSelection(t *testing.T) {
	tests := []struct {
		name       string
		initialize bool
	}{
		{name: "skip"},
		{name: "initialize", initialize: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := onboardingTestTempDir(t)
			sourceRoot := filepath.Join(base, "config-source")
			if err := seedGuidedSourceForTest(sourceRoot, guidedSourceTestOptions()); err != nil {
				t.Fatalf("SeedGuidedConfigSource: %v", err)
			}
			sentinel := filepath.Join(sourceRoot, "README.md")
			if err := os.WriteFile(sentinel, []byte("keep me\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			answers := []string{
				guidedSourceExistingLocal,
				sourceRoot,
				"",
				"yes",
				"generic",
				"",
			}
			if test.initialize {
				answers = append(answers, "yes", "yes")
			} else {
				answers = append(answers, "no")
			}
			var stdout, stderr bytes.Buffer

			_, _, code, err := runGuidedInit(
				filepath.Join(base, "instance"),
				strings.NewReader(strings.Join(answers, "\n")+"\n"),
				&stdout,
				&stderr,
				&fakeGuidedGitHubOperations{},
			)
			if err != nil || code != 0 {
				t.Fatalf("runGuidedInit: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "keep me\n" {
				t.Fatalf("existing instructions changed: data=%q err=%v", data, err)
			}
			if !strings.Contains(stdout.String(), "Initialize the selected config source with Git for agent toolkit installation?") {
				t.Fatalf("guided output did not offer explicit Git initialization:\n%s", stdout.String())
			}
			for _, path := range []string{
				filepath.Join(sourceRoot, ".git"),
				filepath.Join(sourceRoot, filepath.FromSlash(agentkit.InstalledManifestPath)),
			} {
				_, statErr := os.Stat(path)
				if test.initialize && statErr != nil {
					t.Errorf("initialized toolkit path %s: %v", path, statErr)
				}
				if !test.initialize && !os.IsNotExist(statErr) {
					t.Errorf("skipped initialization changed %s: %v", path, statErr)
				}
			}
			if test.initialize {
				if !strings.Contains(stdout.String(), "repository setup:  initialize the selected config source with Git") {
					t.Errorf("installation preview omitted Git initialization:\n%s", stdout.String())
				}
			} else if !strings.Contains(
				stdout.String(),
				"Agent toolkit installation skipped; the selected config source remains non-Git and no toolkit files were written.",
			) {
				t.Errorf("guided output did not report the non-Git skip:\n%s", stdout.String())
			}
		})
	}
}

func TestGuidedAgentToolkitPreviewReportsInstalledStatesWithoutWriting(t *testing.T) {
	tests := []struct {
		name       string
		bundle     func(t *testing.T) agentkit.Bundle
		modify     bool
		wantState  string
		wantDetail string
	}{
		{
			name: "current",
			bundle: func(t *testing.T) agentkit.Bundle {
				t.Helper()
				bundle, err := currentAgentToolkitBundle()
				if err != nil {
					t.Fatal(err)
				}
				return bundle
			},
			wantState: "current",
		},
		{
			name: "outdated",
			bundle: func(t *testing.T) agentkit.Bundle {
				t.Helper()
				bundle, err := agentkit.Build(goobersassets.AgentToolkitAssets, "v0.1.0", "old123")
				if err != nil {
					t.Fatal(err)
				}
				return bundle
			},
			wantState: "outdated",
		},
		{
			name: "modified",
			bundle: func(t *testing.T) agentkit.Bundle {
				t.Helper()
				bundle, err := currentAgentToolkitBundle()
				if err != nil {
					t.Fatal(err)
				}
				return bundle
			},
			modify:     true,
			wantState:  "modified",
			wantDetail: agentkit.InstalledRoot + "/README.md",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := cliAgentKitRepository(t)
			if _, err := instance.SeedQuickstartConfigSource(root); err != nil {
				t.Fatalf("seed config source: %v", err)
			}
			repository, err := agentkit.OpenRepository(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repository.Install(test.bundle(t), "generic"); err != nil {
				t.Fatal(err)
			}
			readme := filepath.Join(root, filepath.FromSlash(agentkit.InstalledRoot+"/README.md"))
			if test.modify {
				if err := os.WriteFile(readme, []byte("local edit\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(readme)
			if err != nil {
				t.Fatal(err)
			}
			confirmation := "no"
			if test.wantState != "current" {
				confirmation = "yes"
			}
			input := bufio.NewReader(strings.NewReader("generic\n\n" + confirmation + "\n"))
			var stdout bytes.Buffer

			selection, err := promptGuidedAgentToolkit(
				guidedPrompter{reader: input, out: &stdout},
				guidedSourceSelection{
					Mode: guidedSourceExistingLocal,
					Root: root,
				},
				root,
			)
			if err != nil {
				t.Fatalf("promptGuidedAgentToolkit: %v", err)
			}
			if selection.Harness != "" {
				t.Fatalf("declined selection = %+v", selection)
			}
			if !strings.Contains(stdout.String(), "current state:     "+test.wantState) {
				t.Errorf("preview lacks state %q:\n%s", test.wantState, stdout.String())
			}
			if test.wantDetail != "" && !strings.Contains(stdout.String(), test.wantDetail) {
				t.Errorf("preview lacks detail %q:\n%s", test.wantDetail, stdout.String())
			}
			if test.wantState != "current" &&
				!strings.Contains(stdout.String(), "installation skipped because the existing toolkit is "+test.wantState) {
				t.Errorf("preview did not safely skip %s toolkit:\n%s", test.wantState, stdout.String())
			}
			after, err := os.ReadFile(readme)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("declined preview changed owned file from %q to %q", before, after)
			}
		})
	}
}

func TestGuidedInitClonesExistingGitHubSourceDistinctFromTarget(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "fleet-config")
	remote := &fakeGuidedGitHubOperations{
		clone: func(_, _ string, destination string) error {
			return seedGuidedSourceForTest(destination, guidedSourceTestOptions())
		},
	}
	input := strings.NewReader(strings.Join([]string{
		guidedSourceExistingGitHub,
		"config-org/fleet-config",
		checkout,
		"",
		"yes",
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer

	_, result, code, err := runGuidedInit(
		filepath.Join(t.TempDir(), "instance"),
		input,
		&stdout,
		&stderr,
		remote,
	)
	if err != nil || code != 0 {
		t.Fatalf("runGuidedInit = result %+v code %d err %v, stdout=%q stderr=%q", result, code, err, stdout.String(), stderr.String())
	}
	if len(remote.cloneCalls) != 2 ||
		remote.cloneCalls[1] != "config-org/fleet-config -> "+checkout {
		t.Fatalf("clone calls = %v", remote.cloneCalls)
	}
	stagingRoot := strings.TrimPrefix(remote.cloneCalls[0], "config-org/fleet-config -> ")
	if stagingRoot == remote.cloneCalls[0] || stagingRoot == checkout {
		t.Fatalf("preflight clone destination = %q", stagingRoot)
	}
	if _, err := os.Stat(stagingRoot); !os.IsNotExist(err) {
		t.Fatalf("temporary checkout was not removed: %v", err)
	}
	if result.ConfigRepo != "https://github.com/config-org/fleet-config" ||
		result.TargetRepo != "https://github.com/app-org/widget-service" {
		t.Fatalf("config and target repositories were not kept distinct: %+v", result)
	}

	reuseInput := strings.NewReader(strings.Join([]string{
		guidedSourceExistingGitHub,
		"config-org/fleet-config",
		checkout,
		"",
		"yes",
	}, "\n") + "\n")
	if _, _, code, err := runGuidedInit(
		filepath.Join(t.TempDir(), "second-instance"),
		reuseInput,
		&stdout,
		&stderr,
		remote,
	); err != nil || code != 0 {
		t.Fatalf("reuse checkout: code %d err %v, stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	if len(remote.cloneCalls) != 2 {
		t.Fatalf("existing checkout was cloned over: %v", remote.cloneCalls)
	}
}

func TestGuidedInitDeclinedGitHubExistingLeavesDestinationUntouched(t *testing.T) {
	for _, test := range []struct {
		name        string
		createEmpty bool
	}{
		{name: "absent"},
		{name: "empty", createEmpty: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			checkout := filepath.Join(base, "fleet-config")
			if test.createEmpty {
				if err := os.Mkdir(checkout, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			remote := &fakeGuidedGitHubOperations{
				clone: func(_, _ string, destination string) error {
					return seedGuidedSourceForTest(destination, guidedSourceTestOptions())
				},
			}
			input := strings.NewReader(strings.Join([]string{
				guidedSourceExistingGitHub,
				"config-org/fleet-config",
				checkout,
				"",
				"no",
			}, "\n") + "\n")
			var stdout, stderr bytes.Buffer
			instanceRoot := filepath.Join(base, "instance")

			res, _, code, err := runGuidedInit(
				instanceRoot,
				input,
				&stdout,
				&stderr,
				remote,
			)
			if err == nil || code != 2 || res != nil || !strings.Contains(err.Error(), "cancelled before writing") {
				t.Fatalf("runGuidedInit = res %+v code %d err %v, stdout=%q stderr=%q", res, code, err, stdout.String(), stderr.String())
			}
			if len(remote.cloneCalls) != 1 ||
				remote.cloneCalls[0] == "config-org/fleet-config -> "+checkout {
				t.Fatalf("clone calls = %v", remote.cloneCalls)
			}
			stagingRoot := strings.TrimPrefix(remote.cloneCalls[0], "config-org/fleet-config -> ")
			if _, statErr := os.Stat(stagingRoot); !os.IsNotExist(statErr) {
				t.Fatalf("temporary checkout was not removed: %v", statErr)
			}
			if test.createEmpty {
				entries, readErr := os.ReadDir(checkout)
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("declined mapping changed empty destination: entries=%v err=%v", entries, readErr)
				}
			} else if _, statErr := os.Stat(checkout); !os.IsNotExist(statErr) {
				t.Fatalf("declined mapping created destination: %v", statErr)
			}
			if _, statErr := os.Stat(instanceRoot); !os.IsNotExist(statErr) {
				t.Fatalf("declined mapping created instance: %v", statErr)
			}
		})
	}
}

func TestGuidedInitDeclinedRemoteCreateKeepsLocalSource(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "local-config")
	remote := &fakeGuidedGitHubOperations{}
	input := strings.NewReader(strings.Join([]string{
		guidedSourceNewLocal,
		sourceRoot,
		"app-org/widget-service",
		"",
		"work-nomination",
		"", // accept the default harness (copilot)
		"",
		"",
		"",
		"yes",
		"config-org/fleet-config",
		"public",
		"",
		"no",
		"yes",
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer

	_, result, code, err := runGuidedInit(
		filepath.Join(t.TempDir(), "instance"),
		input,
		&stdout,
		&stderr,
		remote,
	)
	if err != nil || code != 0 {
		t.Fatalf("runGuidedInit = result %+v code %d err %v, stdout=%q stderr=%q", result, code, err, stdout.String(), stderr.String())
	}
	if len(remote.createCalls) != 0 {
		t.Fatalf("declined repository creation calls = %v", remote.createCalls)
	}
	for _, want := range []string{
		"owner:      config-org",
		"name:       fleet-config",
		"visibility: public",
		"Repository creation declined",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
	if result.ConfigRepo != sourceRoot || result.RemoteCreated {
		t.Fatalf("declined remote result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, instance.GuidedSourceInstanceFile)); err != nil {
		t.Fatalf("local source was not created: %v", err)
	}
}

func TestGuidedInitDeclinedMappingWritesNothing(t *testing.T) {
	base := t.TempDir()
	sourceRoot := filepath.Join(base, "config-source")
	instanceRoot := filepath.Join(base, "instance")
	input := strings.NewReader(strings.Join([]string{
		guidedSourceNewLocal,
		sourceRoot,
		"app-org/widget-service",
		"",
		"work-nomination",
		"",
		"",
		"",
		"",
		"no",
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer

	res, _, code, err := runGuidedInit(
		instanceRoot,
		input,
		&stdout,
		&stderr,
		&fakeGuidedGitHubOperations{},
	)
	if err == nil || code != 2 || res != nil || !strings.Contains(err.Error(), "cancelled before writing") {
		t.Fatalf("runGuidedInit = res %+v code %d err %v, stdout=%q stderr=%q", res, code, err, stdout.String(), stderr.String())
	}
	for _, path := range []string{sourceRoot, instanceRoot} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("declined mapping wrote %s: %v", path, statErr)
		}
	}
	for _, want := range []string{
		"Onboarding mapping to create:",
		"config-source: " + sourceRoot,
		"instance-root: " + instanceRoot,
		"target-repo:   https://github.com/app-org/widget-service",
		"backlog:       https://github.com/app-org/widget-service/issues",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
}

func TestGuidedInitRejectsInvalidExistingSourceBeforeInstanceWrite(t *testing.T) {
	sourceRoot := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(sourceRoot, instance.GuidedSourceInstanceFile),
		[]byte("not: valid: yaml\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	instanceRoot := filepath.Join(t.TempDir(), "instance")
	input := strings.NewReader(strings.Join([]string{
		guidedSourceExistingLocal,
		sourceRoot,
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer

	res, _, code, err := runGuidedInit(
		instanceRoot,
		input,
		&stdout,
		&stderr,
		&fakeGuidedGitHubOperations{},
	)
	if err == nil || code != 1 || res != nil {
		t.Fatalf("runGuidedInit = res %+v code %d err %v, stdout=%q stderr=%q", res, code, err, stdout.String(), stderr.String())
	}
	if _, statErr := os.Stat(instanceRoot); !os.IsNotExist(statErr) {
		t.Fatalf("invalid source wrote instance root: %v", statErr)
	}
}

func guidedSourceTestOptions() instance.GuidedOptions {
	return instance.GuidedOptions{
		GaggleName:           "widget-service",
		DisplayName:          "Widget Service",
		RepoOwner:            "app-org",
		RepoName:             "widget-service",
		RepoBranch:           "main",
		RepoTokenEnv:         "GOOBERS_GITHUB_REPO_TOKEN",
		WorkTrackingTokenEnv: "GOOBERS_GITHUB_ISSUES_TOKEN",
		Workflows:            []string{instance.GuidedWorkflowWorkNomination},
	}
}
