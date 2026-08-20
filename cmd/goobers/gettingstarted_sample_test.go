package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/testgit"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

type sampleSeedCatalog struct {
	Sample struct {
		Version string `json:"version"`
	} `json:"sample"`
	Issues []sampleSeedIssue `json:"issues"`
}

type sampleSeedIssue struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

const gettingStartedImplementationWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "1.4"
metadata:
  name: implementation
spec:
  gaggle: example
  displayName: Getting Started Implementation Local CI
  triggers:
    - type: manual
  readiness:
    maxConcurrentRuns: 1
  start: query-backlog
  tasks:
    - name: query-backlog
      type: deterministic
      goal: Claim the first approved tutorial issue.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        requireLabels: "goobers:ready"
        maxItems: "1"
        resultFile: "claimed-item.json"
      capabilities:
        - github:issues:write
        - github:pr:write
      policyActions:
        - claim-backlog-items
      expectedOutputs:
        - claimed-item
      next: implement
    - name: implement
      type: agentic
      goober: implementer
      goal: Implement the claimed tutorial issue and commit the change.
      capabilities:
        - repo:push
        - agent:model
      policyActions:
        - modify-repository
      next: review
    - name: local-ci
      type: deterministic
      goal: Run the project's local CI-equivalent in the worktree.
      run:
        command: ["make", "ci"]
      retry:
        maxAttempts: 1
  gates:
    - name: review
      evaluator: agentic
      agentic:
        goober: reviewer
      branches:
        pass: local-ci
        needs-changes: implement
        fail: "@abort"
`

func TestGettingStartedSampleQuickstartThroughRealRunner(t *testing.T) {
	root, remote, disposableRoot, seed, server := initGettingStartedQuickstart(t)

	code, stdout, stderr := runArgs(t, "run", "quickstart", root)
	if code != 0 {
		t.Fatalf("goobers run quickstart: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "phase=completed") {
		t.Fatalf("quickstart run did not complete: %q", stdout)
	}

	runID := runIDFromRunStdout(t, stdout)
	reader, err := journal.OpenRead(filepath.Join(root, "runs", runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var stages []string
	var firstPROpenAt time.Time
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Status == string(apiv1.ResultSuccess) {
			stages = append(stages, event.Stage)
		}
		operation, _ := event.Runner["operation"].(string)
		if event.Type == journal.EventRefTouched &&
			event.ExternalRef != nil &&
			event.ExternalRef.Kind == "pr" &&
			operation == "open" &&
			(firstPROpenAt.IsZero() || event.Time.Before(firstPROpenAt)) {
			firstPROpenAt = event.Time
		}
	}
	if got, want := strings.Join(stages, ","), "query-backlog,implement,review,local-ci,push-branch,open-pr"; got != want {
		t.Fatalf("successful stages = %q, want %q", got, want)
	}
	assertGettingStartedLocalCI(t, reader, events, apiv1.ResultSuccess)
	instanceEvents, err := journal.ReadInstanceLog(instance.NewLayout(root).SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var initCompletedAt time.Time
	for _, event := range instanceEvents {
		if event.Type == journal.EventInitCompleted &&
			(initCompletedAt.IsZero() || event.Time.Before(initCompletedAt)) {
			initCompletedAt = event.Time
		}
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if identity.Workflow != "quickstart" || initCompletedAt.IsZero() || firstPROpenAt.IsZero() {
		t.Fatalf(
			"manual journal read = workflow %q, init completed %v, first PR open %v",
			identity.Workflow,
			initCompletedAt,
			firstPROpenAt,
		)
	}
	elapsedMilliseconds := firstPROpenAt.Sub(initCompletedAt).Milliseconds()
	if elapsedMilliseconds < 0 {
		t.Fatalf(
			"manual journal read = init completed %v, first PR open %v",
			initCompletedAt,
			firstPROpenAt,
		)
	}

	code, statusStdout, statusStderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("goobers status: code=%d stdout=%q stderr=%q", code, statusStdout, statusStderr)
	}
	elapsed := time.Duration(elapsedMilliseconds) * time.Millisecond
	if want := fmt.Sprintf("First-run success: first PR in %s", elapsed.Truncate(time.Second)); !strings.Contains(statusStdout, want) {
		t.Fatalf(
			"status = %q, want %q from init.completed time %v and first PR-open ref.touched time %v",
			statusStdout,
			want,
			initCompletedAt,
			firstPROpenAt,
		)
	}

	code, statusStdout, statusStderr = runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("goobers status --json: code=%d stdout=%q stderr=%q", code, statusStdout, statusStderr)
	}
	var statusOutput statusJSONOutput
	if err := json.Unmarshal([]byte(statusStdout), &statusOutput); err != nil {
		t.Fatalf("status JSON = %q: %v", statusStdout, err)
	}
	metric := statusOutput.TimeToFirstPR
	if metric == nil ||
		metric.Anchor != telemetry.TimeToFirstPRAnchor ||
		metric.InitCompletedAt == nil || !metric.InitCompletedAt.Equal(initCompletedAt) ||
		metric.FirstPROpenAt == nil || !metric.FirstPROpenAt.Equal(firstPROpenAt) ||
		metric.Milliseconds == nil || *metric.Milliseconds != elapsedMilliseconds {
		t.Fatalf(
			"timeToFirstPR = %#v, want init completed %v, first PR open %v, milliseconds %d",
			metric,
			initCompletedAt,
			firstPROpenAt,
			elapsedMilliseconds,
		)
	}

	server.mu.Lock()
	pr := server.prs[1]
	server.mu.Unlock()
	if pr == nil {
		t.Fatal("GitHub boundary did not receive a pull request")
	}
	if pr.title != seed.Title || pr.base != "main" || !strings.Contains(pr.body, "Fixes #1") {
		t.Fatalf("pull request = %+v, want seeded title, main base, and issue linkage", pr)
	}
	if !strings.HasPrefix(pr.head, "goobers/quickstart/") {
		t.Fatalf("pull request head = %q, want quickstart run branch", pr.head)
	}

	serverSource := sampleGitOutput(t, "", "--git-dir", remote, "show", "refs/heads/"+pr.head+":src/server.ts")
	if !strings.Contains(serverSource, `sendJSON(response, 400, { error: "title is required" })`) {
		t.Fatalf("pushed branch does not resolve seed issue %q", seed.ID)
	}
	serverTests := sampleGitOutput(t, "", "--git-dir", remote, "show", "refs/heads/"+pr.head+":test/server.test.ts")
	for _, want := range []string{"rejects invalid titles", "trims a valid title"} {
		if !strings.Contains(serverTests, want) {
			t.Fatalf("pushed branch tests do not contain %q", want)
		}
	}

	if err := os.RemoveAll(disposableRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(disposableRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("disposable target remains after teardown: %v", err)
	}

	t.Run("broken seed fails local CI", func(t *testing.T) {
		brokenRoot, _, _, _, _ := initGettingStartedSample(t, false, true)
		code, stdout, stderr := runArgs(t, "run", "quickstart", brokenRoot)
		if code != 1 {
			t.Fatalf("goobers run quickstart: code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "phase=failed") {
			t.Fatalf("broken quickstart run did not fail: %q", stdout)
		}
		runID := runIDFromRunStdout(t, stdout)
		reader, err := journal.OpenRead(filepath.Join(brokenRoot, "runs", runID))
		if err != nil {
			t.Fatal(err)
		}
		events, err := reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		assertGettingStartedLocalCI(t, reader, events, apiv1.ResultFailure)
	})
}

func TestGettingStartedSampleImplementationLocalCIThroughRealRunner(t *testing.T) {
	for _, tt := range []struct {
		name       string
		breakLocal bool
		wantCode   int
		wantPhase  journal.RunPhase
		wantStatus apiv1.ResultStatus
	}{
		{name: "pass", wantPhase: journal.PhaseCompleted, wantStatus: apiv1.ResultSuccess},
		{name: "fail", breakLocal: true, wantCode: 1, wantPhase: journal.PhaseFailed, wantStatus: apiv1.ResultFailure},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := initGettingStartedImplementation(t, tt.breakLocal)

			code, stdout, stderr := runArgs(t, "run", "implementation", root)
			if code != tt.wantCode {
				t.Fatalf("goobers run implementation: code=%d, want %d; stdout=%q stderr=%q", code, tt.wantCode, stdout, stderr)
			}
			if !strings.Contains(stdout, "phase="+string(tt.wantPhase)) {
				t.Fatalf("implementation run did not reach %s: %q", tt.wantPhase, stdout)
			}

			runID := runIDFromRunStdout(t, stdout)
			reader, err := journal.OpenRead(filepath.Join(root, "runs", runID))
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			assertGettingStartedLocalCI(t, reader, events, tt.wantStatus)
		})
	}
}

func assertGettingStartedLocalCI(t *testing.T, reader *journal.Reader, events []journal.Event, wantStatus apiv1.ResultStatus) {
	t.Helper()
	var statuses []string
	sawNPMCI := false
	for _, event := range events {
		if event.Type != journal.EventStageFinished || event.Stage != "local-ci" {
			continue
		}
		statuses = append(statuses, event.Status)
		for _, ref := range event.Artifacts {
			data, err := reader.ArtifactBytes(ref)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "@goobers/getting-started-task-api@1.0.0 ci") {
				sawNPMCI = true
			}
		}
	}
	if got, want := strings.Join(statuses, ","), string(wantStatus); got != want {
		t.Fatalf("local-ci stage statuses = %q, want %q", got, want)
	}
	if !sawNPMCI {
		t.Fatal("local-ci artifacts do not show the sample's npm run ci script")
	}
}

func initGettingStartedQuickstart(t *testing.T) (root, remote, disposableRoot string, seed sampleSeedIssue, server *fakeGitHubServer) {
	return initGettingStartedSample(t, false, false)
}

func initGettingStartedImplementation(t *testing.T, breakLocalCI bool) string {
	root, _, _, _, _ := initGettingStartedSample(t, true, breakLocalCI)
	return root
}

func initGettingStartedSample(t *testing.T, implementation, breakLocalCI bool) (root, remote, disposableRoot string, seed sampleSeedIssue, server *fakeGitHubServer) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "quickstart-instance")
	if code, stdout, stderr := runArgs(t, "init", "--template=quickstart", root); code != 0 {
		t.Fatalf("init quickstart template: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if implementation {
		configureGettingStartedImplementation(t, root)
	}
	if code, stdout, stderr := runArgs(t, "validate", root); code != 0 {
		t.Fatalf("validate getting-started fixture: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	catalogData, err := os.ReadFile(filepath.Join("..", "..", "samples", "getting-started-task-api", "seed-issues.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog sampleSeedCatalog
	if err := json.Unmarshal(catalogData, &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Sample.Version != "1.0.0" || len(catalog.Issues) == 0 {
		t.Fatalf("unexpected sample catalog: %+v", catalog)
	}
	seed = catalog.Issues[0]

	cfg, err := instance.LoadConfig(filepath.Join(root, instance.ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Runner.EnvPassthrough = append(cfg.Runner.EnvPassthrough, "GOOBERS_TEST_GITHUB_API_URL")
	if err := instance.WriteConfig(filepath.Join(root, instance.ConfigFileName), cfg); err != nil {
		t.Fatal(err)
	}

	disposableRoot = t.TempDir()
	remote = materializeGettingStartedSample(t, disposableRoot)
	previousCloneURL := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return remote, nil }

	server = newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(1, seed.Title, seed.Labels...)
	server.mu.Lock()
	server.issues[1].body = seed.Body
	server.mu.Unlock()
	t.Setenv("GOOBERS_TEST_GITHUB_API_URL", server.server.URL)
	t.Setenv("GOOBERS_GITHUB_TOKEN", "ghp_getting_started_fixture_token")
	previousProvider := newGitHubProvider
	newGitHubProvider = server.newGitHubProvider

	previousAdapter := newAgenticAdapter
	newAgenticAdapter = func(gooberName string, _ map[string]string) harness.Adapter {
		return &harnesstest.FakeAdapter{
			Transcript: []byte("deterministic " + gooberName + " model boundary\n"),
			Act: func(_ context.Context, request harness.RunRequest) error {
				switch gooberName {
				case "implementer":
					if err := assertClaimedSeedContext(request, seed.Title); err != nil {
						return err
					}
					if err := implementRequiredTaskTitle(request.Workspace); err != nil {
						return err
					}
					if breakLocalCI {
						if err := breakGettingStartedSampleBuild(request.Workspace); err != nil {
							return err
						}
					}
					return harnesstest.WriteCompletion(request.Workspace, request.CompletionPath, apiv1.ResultEnvelope{
						Status:  apiv1.ResultSuccess,
						Summary: "implemented the first seeded tutorial issue",
					})
				case "reviewer":
					if implementation {
						return harnesstest.WriteCompletion(request.Workspace, request.CompletionPath, apiv1.Verdict{
							Decision:  apiv1.VerdictPass,
							Rationale: "seeded issue implementation is focused and complete",
						})
					}
					return harnesstest.WriteCompletion(request.Workspace, request.CompletionPath, apiv1.ResultEnvelope{
						Status:  apiv1.ResultSuccess,
						Summary: "seeded issue implementation is focused and complete",
					})
				default:
					return fmt.Errorf("unexpected goober %q", gooberName)
				}
			},
		}
	}

	t.Cleanup(func() {
		repoCloneURL = previousCloneURL
		newGitHubProvider = previousProvider
		newAgenticAdapter = previousAdapter
	})
	return root, remote, disposableRoot, seed, server
}

func configureGettingStartedImplementation(t *testing.T, root string) {
	t.Helper()
	gaggleDir := filepath.Join(root, "config", "gaggles", "example")
	if err := os.Remove(filepath.Join(gaggleDir, "workflows", "quickstart.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gaggleDir, "workflows", "implementation.yaml"), []byte(gettingStartedImplementationWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, goober := range []string{"implementer", "reviewer"} {
		path := filepath.Join(gaggleDir, "goobers", goober, "goober.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		updated := strings.Replace(string(data), "    - quickstart\n", "    - implementation\n", 1)
		if updated == string(data) {
			t.Fatalf("%s goober does not declare the quickstart workflow", goober)
		}
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func materializeGettingStartedSample(t *testing.T, root string) string {
	t.Helper()
	source := filepath.Join("..", "..", "samples", "getting-started-task-api")
	worktree := filepath.Join(root, "target")
	if err := copyGettingStartedSample(source, worktree); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, worktree, "init", "--initial-branch=main")
	runFixtureGit(t, worktree, "config", "user.email", "quickstart@example.invalid")
	runFixtureGit(t, worktree, "config", "user.name", "Quickstart Fixture")
	runFixtureGit(t, worktree, "add", "-A")
	runFixtureGit(t, worktree, "commit", "-m", "seed versioned tutorial target")
	remote := filepath.Join(root, "target.git")
	runFixtureGit(t, "", "clone", "--bare", worktree, remote)
	return remote
}

func copyGettingStartedSample(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return os.MkdirAll(destination, 0o755)
		}
		first := strings.Split(relative, string(filepath.Separator))[0]
		if first == ".git" || first == "dist" || first == "node_modules" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func assertClaimedSeedContext(request harness.RunRequest, title string) error {
	for _, path := range request.ContextPaths {
		data, err := os.ReadFile(filepath.Join(request.Workspace, path))
		if err != nil {
			return err
		}
		if strings.Contains(string(data), title) {
			return nil
		}
	}
	return fmt.Errorf("claimed seed %q was not supplied to the implementer", title)
}

func implementRequiredTaskTitle(worktree string) error {
	serverPath := filepath.Join(worktree, "src", "server.ts")
	server, err := os.ReadFile(serverPath)
	if err != nil {
		return err
	}
	before := `    const input = await readObject(request);
    const task: Task = {
      id: allocateID(),
      title: typeof input.title === "string" ? input.title : "",
      completed: false
    };`
	after := `    const input = await readObject(request);
    const title = typeof input.title === "string" ? input.title.trim() : "";
    if (title === "") {
      sendJSON(response, 400, { error: "title is required" });
      return;
    }
    const task: Task = {
      id: allocateID(),
      title,
      completed: false
    };`
	if !strings.Contains(string(server), before) {
		return errors.New("sample no longer contains the first seeded issue")
	}
	if err := os.WriteFile(serverPath, []byte(strings.Replace(string(server), before, after, 1)), 0o644); err != nil {
		return err
	}

	testPath := filepath.Join(worktree, "test", "server.test.ts")
	testData, err := os.ReadFile(testPath)
	if err != nil {
		return err
	}
	end := strings.LastIndex(string(testData), "\n});")
	if end < 0 {
		return errors.New("sample server test suite has no closing block")
	}
	regression := "\n\n" +
		"  it(\"rejects invalid titles\", async () => {\n" +
		"    for (const title of [undefined, 42, \"   \"]) {\n" +
		"      const response = await fetch(`${baseURL}/tasks`, {\n" +
		"        method: \"POST\",\n" +
		"        headers: { \"content-type\": \"application/json\" },\n" +
		"        body: JSON.stringify({ title })\n" +
		"      });\n\n" +
		"      assert.equal(response.status, 400);\n" +
		"      assert.deepEqual(await response.json(), { error: \"title is required\" });\n" +
		"    }\n" +
		"  });\n\n" +
		"  it(\"trims a valid title\", async () => {\n" +
		"    const response = await fetch(`${baseURL}/tasks`, {\n" +
		"      method: \"POST\",\n" +
		"      headers: { \"content-type\": \"application/json\" },\n" +
		"      body: JSON.stringify({ title: \"  Watch the workflow  \" })\n" +
		"    });\n\n" +
		"    assert.equal(response.status, 201);\n" +
		"    assert.equal((await response.json()).title, \"Watch the workflow\");\n" +
		"  });"
	updatedTests := string(testData[:end]) + regression + string(testData[end:])
	if err := os.WriteFile(testPath, []byte(updatedTests), 0o644); err != nil {
		return err
	}

	for _, args := range [][]string{
		{"add", "src/server.ts", "test/server.test.ts"},
		{"-c", "user.email=quickstart@example.invalid", "-c", "user.name=Quickstart Agent", "commit", "-m", "fix: reject empty task titles"},
	} {
		command := testgit.Command(args...)
		command.Dir = worktree
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w: %s", args, err, output)
		}
	}
	return nil
}

func breakGettingStartedSampleBuild(worktree string) error {
	path := filepath.Join(worktree, "src", "index.ts")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	data = append(data, []byte("\nconst localCIFailure: string = 42;\n")...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"add", "src/index.ts"},
		{"-c", "user.email=quickstart@example.invalid", "-c", "user.name=Quickstart Agent", "commit", "-m", "test: break the local CI fixture"},
	} {
		command := testgit.Command(args...)
		command.Dir = worktree
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w: %s", args, err, output)
		}
	}
	return nil
}

func sampleGitOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := testgit.Command(args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
