package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

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

			code, stdout, stderr := runArgs(
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

	code, _, stderr := runArgs(t, "onboarding", "stub-sample", "--destination", destination)
	if code != 1 || !strings.Contains(stderr, "without --force") {
		t.Fatalf("without force: code=%d stderr=%q", code, stderr)
	}
	if got, err := os.ReadFile(conflict); err != nil || string(got) != "user-owned\n" {
		t.Fatalf("conflict changed before refusal: data=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("preflight wrote files before refusing conflict: %v", err)
	}

	code, _, stderr = runArgs(t, "onboarding", "stub-sample", "--destination", destination, "--force")
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

func TestOnboardingStubSampleRefusesSymlinkedDestinationAncestor(t *testing.T) {
	parent := onboardingTestTempDir(t)
	outside := onboardingTestTempDir(t)
	link := filepath.Join(parent, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	destination := filepath.Join(link, "sample")

	code, _, stderr := runArgs(t, "onboarding", "stub-sample", "--destination", destination)
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

	code, _, stderr := runArgs(t, "onboarding", "stub-sample", "--destination", destination, "--force")
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

	code, stdout, stderr := runArgs(
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
		code, stdout, stderr := runArgs(
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
