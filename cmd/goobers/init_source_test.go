package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

type fakeGuidedGitHubOperations struct {
	clone       func(string, string, string) error
	cloneCalls  []string
	createCalls []string
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
	if err := instance.InitGuidedSource(sourceRoot, guidedSourceTestOptions()); err != nil {
		t.Fatalf("InitGuidedSource: %v", err)
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
}

func TestGuidedInitClonesExistingGitHubSourceDistinctFromTarget(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "fleet-config")
	remote := &fakeGuidedGitHubOperations{
		clone: func(_, _ string, destination string) error {
			return instance.InitGuidedSource(destination, guidedSourceTestOptions())
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
					return instance.InitGuidedSource(destination, guidedSourceTestOptions())
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

func TestGuidedInitCreatesConfirmedEmptyGitHubRepository(t *testing.T) {
	t.Setenv("GOOBERS_GITHUB_CONFIG_REPO_TOKEN", "test-token")
	sourceRoot := filepath.Join(t.TempDir(), "local-config")
	remote := &fakeGuidedGitHubOperations{}
	input := strings.NewReader(strings.Join([]string{
		guidedSourceNewLocal,
		sourceRoot,
		"app-org/widget-service",
		"",
		"work-nomination",
		"",
		"",
		"",
		"yes",
		"config-org/fleet-config",
		"private",
		"",
		"yes",
		"yes",
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	instanceRoot := filepath.Join(t.TempDir(), "instance")

	code := runInitWithInputForOSAndGitHub(
		[]string{"--guided", instanceRoot},
		input,
		&stdout,
		&stderr,
		"darwin",
		remote,
	)
	if code != 0 {
		t.Fatalf("guided init code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(remote.createCalls) != 1 || remote.createCalls[0] != "config-org/fleet-config private" {
		t.Fatalf("repository creation calls = %v", remote.createCalls)
	}
	for _, want := range []string{
		"config-repo:  https://github.com/config-org/fleet-config",
		"instance-root: " + instanceRoot,
		"target-repo:   https://github.com/app-org/widget-service",
		"The GitHub config repository is empty; no commit or push was performed",
		"git -C " + strconv.Quote(sourceRoot) + " init",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "test-token") {
		t.Fatal("repository-creation token was printed")
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, ".git")); !os.IsNotExist(err) {
		t.Fatalf("guided setup initialized or populated git unexpectedly: %v", err)
	}
}

func TestGuidedInitRequiresConfiguredTokenBeforeRemoteCreate(t *testing.T) {
	t.Setenv("MISSING_CONFIG_REPO_TOKEN", "")
	sourceRoot := filepath.Join(t.TempDir(), "local-config")
	remote := &fakeGuidedGitHubOperations{}
	input := strings.NewReader(strings.Join([]string{
		guidedSourceNewLocal,
		sourceRoot,
		"app-org/widget-service",
		"",
		"work-nomination",
		"",
		"",
		"",
		"yes",
		"config-org/fleet-config",
		"private",
		"MISSING_CONFIG_REPO_TOKEN",
		"yes",
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer

	code := runInitWithInputForOSAndGitHub(
		[]string{"--guided", filepath.Join(t.TempDir(), "instance")},
		input,
		&stdout,
		&stderr,
		"darwin",
		remote,
	)
	if code != 2 ||
		!strings.Contains(stderr.String(), "MISSING_CONFIG_REPO_TOKEN is not set") {
		t.Fatalf("guided init code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(remote.createCalls) != 0 {
		t.Fatalf("repository creation calls = %v", remote.createCalls)
	}
	if _, err := os.Stat(sourceRoot); !os.IsNotExist(err) {
		t.Fatalf("missing remote credential wrote source tree: %v", err)
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
