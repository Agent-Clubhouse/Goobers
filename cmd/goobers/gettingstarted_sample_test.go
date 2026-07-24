package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
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
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Status == string(apiv1.ResultSuccess) {
			stages = append(stages, event.Stage)
		}
	}
	if got, want := strings.Join(stages, ","), "query-backlog,implement,review,push-branch,open-pr"; got != want {
		t.Fatalf("successful stages = %q, want %q", got, want)
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
}

func initGettingStartedQuickstart(t *testing.T) (root, remote, disposableRoot string, seed sampleSeedIssue, server *fakeGitHubServer) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "quickstart-instance")
	if code, stdout, stderr := runArgs(t, "init", "--template=quickstart", root); code != 0 {
		t.Fatalf("init quickstart template: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if code, stdout, stderr := runArgs(t, "validate", root); code != 0 {
		t.Fatalf("validate quickstart template: code=%d stdout=%q stderr=%q", code, stdout, stderr)
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
		return &harness.FakeAdapter{
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
					return harness.WriteCompletion(request.Workspace, request.CompletionPath, apiv1.ResultEnvelope{
						Status:  apiv1.ResultSuccess,
						Summary: "implemented the first seeded tutorial issue",
					})
				case "reviewer":
					return harness.WriteCompletion(request.Workspace, request.CompletionPath, apiv1.ResultEnvelope{
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
		command := exec.Command("git", args...)
		command.Dir = worktree
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w: %s", args, err, output)
		}
	}
	return nil
}

func sampleGitOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
