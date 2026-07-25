package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

const (
	docsDryRunID       = "docs-dry-run"
	docsDryRunTokenEnv = "GOOBERS_DOCS_DRY_RUN_TOKEN"
	docsDryRunFile     = "docs/generated.md"
)

func TestDocsUpdaterNonEmptyDryRunOpensDocsOnlyPR(t *testing.T) {
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
	localRunner := newDocsDryRunRunner(t, instanceRoot, runsDir, origin, manager)

	result, err := localRunner.Start(context.Background(), runner.StartInput{
		RunID:   docsDryRunID,
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
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}

	assertDocsDryRunStages(t, filepath.Join(runsDir, docsDryRunID))

	server.mu.Lock()
	if len(server.prs) != 1 {
		count := len(server.prs)
		server.mu.Unlock()
		t.Fatalf("opened %d PRs, want one", count)
	}
	pr := *server.prs[1]
	server.mu.Unlock()

	wantHead := providers.BranchName(definition.Name, docsDryRunID)
	if pr.head != wantHead || pr.base != "main" {
		t.Fatalf("PR head/base = %q/%q, want %q/main", pr.head, pr.base, wantHead)
	}
	if !branchExistsOnOrigin(t, origin, wantHead) {
		t.Fatalf("PR head %q was not pushed to origin", wantHead)
	}
	changed := strings.Fields(runGitOutputT(
		t,
		origin,
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

func loadShippedDocsUpdater(t *testing.T) apiv1.Workflow {
	t.Helper()
	set, report, err := instance.LoadConfigDir(filepath.Join("..", "..", "selfhost"))
	if err != nil {
		t.Fatalf("load selfhost config: %v\n%v", err, report)
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
		{Capability: "github:pr:write", Ref: "docs-dry-run"},
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
			shell.ExtraEnvAllowlist = []string{"GOOBERS_TEST_GITHUB_API_URL"}
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
			adapter := &harness.FakeAdapter{
				Transcript: []byte("docs updater dry-run fixture\n"),
				Act: func(_ context.Context, request harness.RunRequest) error {
					if request.Mode != harness.ModeInvoke {
						return fmt.Errorf("unexpected harness mode %q", request.Mode)
					}
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
					return harness.WriteCompletion(request.Workspace, request.CompletionPath, apiv1.ResultEnvelope{
						Status:  apiv1.ResultSuccess,
						Summary: "updated fixture documentation",
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

func assertDocsDryRunStages(t *testing.T, runDir string) {
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
	want := []string{"signal-gather", "update-docs", "validate", "push-branch", "open-pr"}
	if !reflect.DeepEqual(started, want) {
		t.Fatalf("started stages = %v, want %v", started, want)
	}
}

func pathWithinDocsRoots(path string, roots []string) bool {
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/") {
			return true
		}
	}
	return false
}
