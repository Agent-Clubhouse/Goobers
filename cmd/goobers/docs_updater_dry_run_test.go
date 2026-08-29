package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

const (
	docsDryRunID       = "docs-dry-run"
	docsNoWorkRunID    = "docs-no-work"
	docsDryRunTokenEnv = "GOOBERS_DOCS_DRY_RUN_TOKEN"
	docsDryRunFile     = "docs/generated.md"
	docsDryRunMakeEnv  = "GOOBERS_DOCS_DRY_RUN_MAKE"
)

func TestDocsUpdaterNonEmptyDryRunOpensDocsOnlyPR(t *testing.T) {
	dryRun := runDocsUpdaterDryRun(t, docsDryRunID, apiv1.ResultSuccess)
	definition := dryRun.definition
	if dryRun.result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q at %q, want completed; journal: %s", dryRun.result.Phase, dryRun.result.FinalState, docsDryRunJournalSummary(t, dryRun.runDir))
	}

	assertDocsDryRunStages(t, dryRun.runDir, []string{"signal-gather", "update-docs", "validate", "push-branch", "open-pr"})

	dryRun.server.mu.Lock()
	if len(dryRun.server.prs) != 1 {
		count := len(dryRun.server.prs)
		dryRun.server.mu.Unlock()
		t.Fatalf("opened %d PRs, want one", count)
	}
	pr := *dryRun.server.prs[1]
	dryRun.server.mu.Unlock()

	wantHead := providers.BranchName(definition.Name, docsDryRunID)
	if pr.head != wantHead || pr.base != "main" {
		t.Fatalf("PR head/base = %q/%q, want %q/main", pr.head, pr.base, wantHead)
	}
	if !branchExistsOnOrigin(t, dryRun.origin, wantHead) {
		t.Fatalf("PR head %q was not pushed to origin", wantHead)
	}
	changed := strings.Fields(runGitOutputT(
		t,
		dryRun.origin,
		"-c", "safe.bareRepository=all",
		"diff", "--name-only", "main..."+wantHead,
	))
	if want := []string{docsDryRunFile}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("PR changed paths = %v, want docs-only %v", changed, want)
	}
	for _, path := range changed {
		if !pathWithinDocsRoots(path, definition.Spec.DocsRoots) {
			t.Fatalf("PR path %q is outside configured docs roots %v", path, definition.Spec.DocsRoots)
		}
	}
}

func TestDocsUpdaterAgenticNoWorkCompletesWithoutPublishing(t *testing.T) {
	dryRun := runDocsUpdaterDryRun(t, docsNoWorkRunID, apiv1.ResultNoWork)
	if dryRun.result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q at %q, want completed; journal: %s", dryRun.result.Phase, dryRun.result.FinalState, docsDryRunJournalSummary(t, dryRun.runDir))
	}
	assertDocsDryRunStages(t, dryRun.runDir, []string{"signal-gather", "update-docs"})

	dryRun.server.mu.Lock()
	prCount := len(dryRun.server.prs)
	dryRun.server.mu.Unlock()
	if prCount != 0 {
		t.Fatalf("opened %d PRs, want none", prCount)
	}
	head := providers.BranchName(dryRun.definition.Name, docsNoWorkRunID)
	if branchExistsOnOrigin(t, dryRun.origin, head) {
		t.Fatalf("no-work branch %q was pushed to origin", head)
	}
}

type docsUpdaterDryRun struct {
	definition apiv1.Workflow
	origin     string
	server     *fakeGitHubServer
	runDir     string
	result     runner.Result
}

func runDocsUpdaterDryRun(t *testing.T, runID string, agentStatus apiv1.ResultStatus) docsUpdaterDryRun {
	t.Helper()
	definition := loadShippedDocsUpdater(t)
	machine, err := workflow.Compile(workflow.Definition{
		Name:       definition.Name,
		Version:    1,
		DSLVersion: definition.DSLVersion,
		Spec:       definition.Spec,
	})
	if err != nil {
		t.Fatalf("compile shipped docs-updater: %v", err)
	}
	installDocsDryRunMake(t)

	origin := newDocsDryRunOrigin(t)
	server := newFakeGitHubServer(t, "fixture", "repository")
	t.Setenv("GOOBERS_TEST_GITHUB_API_URL", server.server.URL)
	t.Setenv(docsDryRunTokenEnv, "docs-dry-run-token")

	instanceRoot := t.TempDir()
	layout := instance.NewLayout(instanceRoot)
	if err := instance.WriteConfig(layout.ConfigFile(), &instance.Config{
		APIVersion: instance.ConfigAPIVersion,
		Kind:       instance.ConfigKind,
		Repos: []instance.RepoRef{{
			Provider: "github",
			Owner:    "fixture",
			Name:     "repository",
			Token:    instance.TokenRef{Env: docsDryRunTokenEnv},
		}},
	}); err != nil {
		t.Fatalf("write dry-run instance config: %v", err)
	}
	gaggleLayout := layout.ForGaggle(definition.Spec.Gaggle)
	manager, err := worktree.NewManager(gaggleLayout.WorkcopiesDir())
	if err != nil {
		t.Fatalf("create worktree manager: %v", err)
	}
	runsDir := gaggleLayout.RunsDir()
	localRunner := newDocsDryRunRunner(t, instanceRoot, runsDir, origin, manager, agentStatus)

	result, err := localRunner.Start(context.Background(), runner.StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  definition.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{
			Provider: apiv1.ProviderGitHub,
			Owner:    "fixture",
			Name:     "repository",
			Branch:   "main",
		},
	})
	if err != nil {
		t.Fatalf("run shipped docs-updater: %v", err)
	}
	return docsUpdaterDryRun{
		definition: definition,
		origin:     origin,
		server:     server,
		runDir:     filepath.Join(runsDir, runID),
		result:     result,
	}
}

func loadShippedDocsUpdater(t *testing.T) apiv1.Workflow {
	t.Helper()
	set, report, err := instance.LoadConfigDir(filepath.Join("..", "..", "reference-workflows"))
	if err != nil {
		t.Fatalf("load reference-workflows config: %v\n%v", err, report)
	}
	for _, definition := range set.Workflows {
		if definition.Spec.Gaggle == "goobers" && definition.Name == "docs-updater" {
			return definition
		}
	}
	t.Fatal("shipped goobers/docs-updater workflow not found")
	return apiv1.Workflow{}
}

func newDocsDryRunOrigin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	runGitT(t, root, "init", "--bare", "-b", "main", origin)

	seed := filepath.Join(root, "seed")
	runGitT(t, root, "clone", origin, seed)
	runGitT(t, seed, "config", "user.name", "docs fixture")
	runGitT(t, seed, "config", "user.email", "docs-fixture@example.test")
	if err := os.MkdirAll(filepath.Join(seed, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"README.md":          "# Fixture\n",
		"internal/source.go": "package internal\n",
		"Makefile":           "ci:\n\tgit cat-file -e HEAD:" + docsDryRunFile + "\n",
	}
	for path, contents := range files {
		if err := os.WriteFile(filepath.Join(seed, path), []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	runGitT(t, seed, "add", ".")
	runGitT(t, seed, "commit", "-m", "seed fixture")
	runGitT(t, seed, "push", "origin", "main")
	return origin
}

func newDocsDryRunRunner(
	t *testing.T,
	instanceRoot string,
	runsDir string,
	origin string,
	manager *worktree.Manager,
	agentStatus apiv1.ResultStatus,
) *runner.Runner {
	t.Helper()
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{
		Name: "docs-dry-run",
		Env:  docsDryRunTokenEnv,
	}})
	if err != nil {
		t.Fatalf("create credential resolver: %v", err)
	}
	grants := []credentials.Grant{
		{Capability: "repo:push", Ref: "docs-dry-run"},
		{Capability: "provider:pr:write", Ref: "docs-dry-run"},
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	localRunner, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, registrar runner.SecretRegistrar) (invoke.Deterministic, error) {
			injector, err := credentials.NewInjector(resolver, grants, registrar)
			if err != nil {
				return nil, err
			}
			shell, err := executor.NewShellExecutor(injector, rec)
			if err != nil {
				return nil, err
			}
			shell.InstanceRoot = instanceRoot
			shell.SelfBin = executable
			shell.ExtraEnvAllowlist = []string{"GOOBERS_TEST_GITHUB_API_URL", docsDryRunMakeEnv}
			return shell, nil
		},
		NewAgentic: func(gooberName string, rec runner.ArtifactRecorder, registrar runner.SecretRegistrar) (invoke.Goober, error) {
			injector, err := credentials.NewGooberInjector(resolver, gooberName, nil, registrar)
			if err != nil {
				return nil, err
			}
			spanRecorder, ok := rec.(harness.SpanRecorder)
			if !ok {
				return nil, fmt.Errorf("journal recorder %T does not record spans", rec)
			}
			runDir, ok := rec.(interface{ Dir() string })
			if !ok {
				return nil, fmt.Errorf("journal recorder %T does not expose its directory", rec)
			}
			registryScrubber, ok := registrar.(journal.Scrubber)
			if !ok {
				return nil, fmt.Errorf("secret registrar %T is not a journal scrubber", registrar)
			}
			adapter := &harnesstest.FakeAdapter{
				Transcript: []byte("docs updater dry-run fixture\n"),
				Act: func(_ context.Context, request harness.RunRequest) error {
					if request.Mode != harness.ModeInvoke {
						return fmt.Errorf("unexpected harness mode %q", request.Mode)
					}
					summary := "documentation already accurate"
					if agentStatus == apiv1.ResultSuccess {
						path := filepath.Join(request.Workspace, docsDryRunFile)
						if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
							return err
						}
						if err := os.WriteFile(path, []byte("# Generated reference\n"), 0o644); err != nil {
							return err
						}
						runGitT(t, request.Workspace, "add", docsDryRunFile)
						runGitT(
							t,
							request.Workspace,
							"-c", "user.name=docs fixture",
							"-c", "user.email=docs-fixture@example.test",
							"commit", "-m", "docs: add generated reference",
						)
						summary = "updated fixture documentation"
					}
					return harnesstest.WriteCompletion(request.Workspace, request.CompletionPath, apiv1.ResultEnvelope{
						Status:  agentStatus,
						Summary: summary,
					})
				},
			}
			return harness.NewExecutor(
				adapter,
				injector,
				spanRecorder,
				rec,
				harness.NewContextResolver(runDir, runsDir),
				journal.Chain(registryScrubber, journal.NewPatternScrubber()),
				"docs updater dry-run fixture",
			)
		},
		Automated:  gate.NewAutomatedEvaluator(),
		Worktrees:  manager,
		ScratchDir: filepath.Join(instanceRoot, "scratch"),
		RunsDir:    runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return origin, nil
		},
	})
	if err != nil {
		t.Fatalf("create docs dry-run runner: %v", err)
	}
	return localRunner
}

// The hermetic unit-test tier excludes host make. A copy of this test binary
// preserves the shipped "make ci" command while providing its fixture (see
// installMakeExecutableFixture for why it's a copy, not a hard link).
func installDocsDryRunMake(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	name := "make"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	installMakeExecutableFixture(t, dir, name)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(docsDryRunMakeEnv, "1")
}

func runDocsDryRunMake() int {
	if len(os.Args) != 2 || os.Args[1] != "ci" {
		fmt.Fprintf(os.Stderr, "make fixture: args = %q, want [ci]\n", os.Args[1:])
		return 2
	}
	cmd := testgit.Command("cat-file", "-e", "HEAD:"+docsDryRunFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "make fixture: validate generated docs: %v\n", err)
		return 1
	}
	return 0
}

func isDocsDryRunMakeProcess() bool {
	name := filepath.Base(os.Args[0])
	return name == "make" || name == "make.exe"
}

func assertDocsDryRunStages(t *testing.T, runDir string, want []string) {
	t.Helper()
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatalf("open dry-run journal: %v", err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("read dry-run journal: %v", err)
	}
	var started []string
	for _, event := range events {
		if event.Type == journal.EventStageStarted {
			started = append(started, event.Stage)
		}
	}
	if !reflect.DeepEqual(started, want) {
		t.Fatalf("started stages = %v, want %v", started, want)
	}
}

func docsDryRunJournalSummary(t *testing.T, runDir string) string {
	t.Helper()
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return "open journal: " + err.Error()
	}
	events, err := reader.Events()
	if err != nil {
		return "read events: " + err.Error()
	}
	var summary []string
	for _, event := range events {
		switch event.Type {
		case journal.EventStageFinished:
			detail := fmt.Sprintf("%s=%s", event.Stage, event.Status)
			if event.Error != nil {
				detail += ":" + event.Error.Code + ":" + event.Error.Message
			}
			summary = append(summary, detail)
		case journal.EventGateEvaluated:
			summary = append(summary, fmt.Sprintf("%s=%s->%s", event.Gate, event.Verdict, event.Target))
		}
	}
	return strings.Join(summary, ", ")
}

func pathWithinDocsRoots(path string, roots []string) bool {
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/") {
			return true
		}
	}
	return false
}
