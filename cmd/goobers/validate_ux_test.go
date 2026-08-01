package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

func TestValidateForeignLayoutDiagnosticsAndExitCodes(t *testing.T) {
	type mutation func(t *testing.T, root string)
	tests := []struct {
		name   string
		mutate mutation
		code   int
		want   string
	}{
		{name: "valid", code: 0, want: "OK: instance.yaml valid; config/ valid"},
		{
			name: "unbound workflow",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
				replaceInFile(t, path, "  gaggle: example", "  gaggle: ghost")
			},
			code: 1,
			want: `gaggles/example/workflows/default-implement.yaml Workflow/default-implement: spec.gaggle names "ghost", but no Gaggle/ghost definition was found`,
		},
		{
			name: "manifest gaggle mismatch",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, "config", "manifest.yaml")
				replaceInFile(t, path, "    - example", "    - ghost")
			},
			code: 1,
			want: `manifest.yaml Manifest/example-instance: spec.gaggles references "ghost", but no Gaggle/ghost definition was found`,
		},
		{
			name: "capability typo",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "goober.yaml")
				appendToFile(t, path, "  capabilities:\n    - github:prs:write\n")
			},
			code: 1,
			want: `Goober/coder: spec.capabilities contains unknown capability "github:prs:write"; did you mean "github:pr:write"?`,
		},
		{
			name: "missing instructions",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "instructions.md")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			code: 1,
			want: `Goober/coder: spec.instructions file "instructions.md" was not found`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "foreign")
			if code, _, stderr := runArgs(t, "init", root); code != 0 {
				t.Fatalf("init: code=%d stderr=%q", code, stderr)
			}
			if tc.mutate != nil {
				tc.mutate(t, root)
			}
			code, stdout, stderr := runArgs(t, "validate", root)
			if code != tc.code {
				t.Fatalf("validate code=%d, want %d; stdout=%q stderr=%q", code, tc.code, stdout, stderr)
			}
			if !strings.Contains(stdout, tc.want) {
				t.Fatalf("validate stdout missing %q:\n%s", tc.want, stdout)
			}
		})
	}
}

// TestValidateGitHubAnnotations is #687's config-repo PR gate: each finding
// becomes a GitHub Actions ::error/::warning workflow command anchored to its
// file, written to stderr so it composes cleanly with --json (stdout stays a
// single parseable JSON document either way).
func TestValidateGitHubAnnotations(t *testing.T) {
	root := filepath.Join(t.TempDir(), "foreign")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}
	path := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, path, "  gaggle: example", "  gaggle: ghost")

	t.Run("plain", func(t *testing.T) {
		code, _, stderr := runArgs(t, "validate", "--github-annotations", root)
		if code != 1 {
			t.Fatalf("validate --github-annotations code=%d, want 1; stderr=%q", code, stderr)
		}
		want := `::error file=config/gaggles/example/workflows/default-implement.yaml,title=REF007::spec.gaggle names "ghost", but no Gaggle/ghost definition was found`
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing annotation %q:\n%s", want, stderr)
		}
	})

	t.Run("composes with --json", func(t *testing.T) {
		code, stdout, stderr := runArgs(t, "validate", "--json", "--github-annotations", root)
		if code != 1 {
			t.Fatalf("code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
		}
		if !strings.Contains(stderr, "::error file=") {
			t.Fatalf("stderr missing an annotation:\n%s", stderr)
		}
		if strings.Contains(stdout, "::error") || strings.Contains(stdout, "::warning") {
			t.Fatalf("--json stdout must stay pure JSON, no workflow commands: %s", stdout)
		}
		var envelope diagnosticsEnvelope
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("stdout is not valid JSON with annotations enabled: %v\n%s", err, stdout)
		}
	})

	t.Run("a valid config never emits an error annotation", func(t *testing.T) {
		cleanRoot := filepath.Join(t.TempDir(), "clean")
		if code, _, stderr := runArgs(t, "init", cleanRoot); code != 0 {
			t.Fatalf("init: code=%d stderr=%q", code, stderr)
		}
		code, _, stderr := runArgs(t, "validate", "--github-annotations", cleanRoot)
		if code != 0 {
			t.Fatalf("code=%d, want 0; stderr=%q", code, stderr)
		}
		if strings.Contains(stderr, "::error") {
			t.Fatalf("a valid (exit 0) config must never emit an ::error annotation: %s", stderr)
		}
	})
}

func TestValidateCheckRepos(t *testing.T) {
	root := filepath.Join(t.TempDir(), "foreign")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}
	t.Setenv("GOOBERS_GITHUB_TOKEN", "test-token")

	original := targetRepositoryReachable
	t.Cleanup(func() { targetRepositoryReachable = original })
	originalSize := targetRepositorySize
	t.Cleanup(func() { targetRepositorySize = originalSize })

	called := 0
	targetRepositoryReachable = func(_ context.Context, repo instance.RepoRef, token string, _ credentials.StoreResolver) error {
		called++
		if repo.Owner != "your-org" || repo.Name != "your-repo" {
			t.Errorf("repository = %s/%s, want your-org/your-repo", repo.Owner, repo.Name)
		}
		if token != "test-token" {
			t.Errorf("token = %q, want resolved test token", token)
		}
		return nil
	}
	sizeCalled := 0
	targetRepositorySize = func(context.Context, instance.RepoRef, string) (int64, error) {
		sizeCalled++
		return 100, nil
	}
	code, stdout, stderr := runArgs(t, "validate", "--check-repos", root)
	if code != 0 {
		t.Fatalf("validate --check-repos: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if called != 1 || !strings.Contains(stdout, "REPOSITORY repos[0] your-org/your-repo: reachable") {
		t.Fatalf("repository check calls=%d stdout=%q", called, stdout)
	}
	if sizeCalled != 1 {
		t.Fatalf("size check calls=%d, want 1", sizeCalled)
	}
	if strings.Contains(stdout, "checkout-size threshold") {
		t.Fatalf("did not expect oversized-repo warning for a small repo:\n%s", stdout)
	}

	targetRepositoryReachable = func(context.Context, instance.RepoRef, string, credentials.StoreResolver) error {
		return errors.New("repository not found or access denied for test-token")
	}
	code, stdout, stderr = runArgs(t, "validate", "--check-repos", root)
	if code != 1 {
		t.Fatalf("failed repository check code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"REPOSITORY repos[0] your-org/your-repo: unreachable: repository not found or access denied for [REDACTED]",
		"Check the owner/name, token source, repository access, and network connection.",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("failed repository check output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "test-token") {
		t.Fatalf("repository check output leaked the resolved token: %q", stdout)
	}
}

func TestValidateStrictFailsOnWarnings(t *testing.T) {
	root := initDeterministicDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, workflowPath, `        command: ["true"]`, "        command: [\"true\"]\n      expectedOutputs:\n        - artifact")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("advisory validate code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "expectedOutputs is declared but the stage has no inputs.resultFile") {
		t.Fatalf("advisory validate did not render warning:\n%s", stdout)
	}

	code, stdout, stderr = runArgs(t, "validate", "--strict", root)
	if code != 1 {
		t.Fatalf("strict validate code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"expectedOutputs is declared but the stage has no inputs.resultFile",
		"config directory has 1 warning(s); --strict treats warnings as errors",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("strict validate output missing %q:\n%s", want, stdout)
		}
	}
}

func TestValidateModelFallbackWarnsAndUsesAdvisoryExit(t *testing.T) {
	root := initDemo(t)
	gooberPath := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "goober.yaml")
	replaceInFile(t, gooberPath, "  model: auto", "  model: retired-model")
	replaceInFile(t, gooberPath, "  harnessOptions: {}", "  harnessOptions:\n    fallback-to-default: true")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	want := `WARNING MODEL002 Goober/coder: requested model "retired-model" is unavailable; using the harness default`
	if !strings.Contains(stdout, want) {
		t.Fatalf("validate output missing %q:\n%s", want, stdout)
	}

	code, stdout, stderr = runArgs(t, "validate", "--json", root)
	if code != 0 || stderr != "" {
		t.Fatalf("validate --json code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	assertFindingSource(
		t,
		decodeDiagnosticsEnvelope(t, stdout),
		string(validate.WarningModelFallback),
		filepath.ToSlash(filepath.Join("config", "gaggles", "example", "goobers", "coder", "goober.yaml")),
		"/spec/harnessOptions/fallback-to-default",
	)

	code, stdout, stderr = runArgs(t, "validate", "--strict", root)
	if code != 1 {
		t.Fatalf("strict validate code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}

	replaceInFile(t, gooberPath, "    fallback-to-default: true", "    fallback-to-default: false")
	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("invalid model validate code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `unknown model "retired-model"; valid models:`) {
		t.Fatalf("invalid model output omitted valid-model list:\n%s", stdout)
	}
}

func TestValidatePrintsDSLVersionSummary(t *testing.T) {
	root := initDeterministicDemo(t)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "DSLVERSION Workflow/default-implement: 1.4 (supported)") {
		t.Fatalf("validate output missing the DSL version summary line:\n%s", stdout)
	}
}

func TestValidateWarnsOnMissingDSLVersionPin(t *testing.T) {
	root := initDeterministicDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, workflowPath, "dslVersion: \"1.4\"\n", "")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"DVL001",
		`spec has no dslVersion pin; defaulting to "1.4"`,
		"DSLVERSION Workflow/default-implement: 1.4 (defaulted; no dslVersion pin) (supported)",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("validate output missing %q:\n%s", want, stdout)
		}
	}
}

func TestValidateRejectsUnmetProviderCapabilityRequirement(t *testing.T) {
	root := initDeterministicDemo(t)
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, gagglePath, "provider: github\n    owner: your-org", "provider: ado\n    project: your-project\n    owner: your-org")

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, workflowPath, "spec:\n  gaggle: example",
		"spec:\n  gaggle: example\n  requires:\n    capabilities:\n      - pr.review.threads")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("validate: code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"requires provider capability",
		"pr.review.threads",
		`"ado"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("validate output missing %q:\n%s", want, stdout)
		}
	}
}

// siblingSameRepoYAML declares a sibling targeting the exact repo
// initDeterministicDemo's example gaggle uses (github/your-org/your-repo).
const siblingSameRepoYAML = `  requireLabels:
    - area:frontend
  siblings:
    - project:
        provider: github
        owner: your-org
        name: your-repo
      label: Billing team
      requireLabels:
        - area:frontend
`

func TestValidateWarnsOnSiblingLabelOverlap(t *testing.T) {
	root := initDeterministicDemo(t)
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, gagglePath, "  isolation:\n    namespace: gaggle-example\n",
		"  isolation:\n    namespace: gaggle-example\n"+siblingSameRepoYAML)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d, want 0 (warning-only); stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"SIB001",
		`overlaps declared sibling "Billing team"`,
		"area:frontend",
		"your-org/your-repo",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("validate output missing %q:\n%s", want, stdout)
		}
	}
}

func TestValidateNoWarningOnDisjointSiblingLabels(t *testing.T) {
	root := initDeterministicDemo(t)
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, gagglePath, "  isolation:\n    namespace: gaggle-example\n",
		"  isolation:\n    namespace: gaggle-example\n"+
			`  requireLabels:
    - area:frontend
  siblings:
    - project:
        provider: github
        owner: your-org
        name: your-repo
      label: Billing team
      requireLabels:
        - area:billing
`)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "SIB001") {
		t.Fatalf("validate output unexpectedly warned on disjoint sibling labels:\n%s", stdout)
	}
}

func TestValidateNoWarningOnSiblingDifferentRepo(t *testing.T) {
	root := initDeterministicDemo(t)
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, gagglePath, "  isolation:\n    namespace: gaggle-example\n",
		"  isolation:\n    namespace: gaggle-example\n"+
			`  requireLabels:
    - area:frontend
  siblings:
    - project:
        provider: github
        owner: some-other-org
        name: unrelated-repo
      label: Unrelated team
      requireLabels:
        - area:frontend
`)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "SIB001") {
		t.Fatalf("validate output unexpectedly warned on a sibling targeting a different repo:\n%s", stdout)
	}
}

func TestValidateWorkflowOverrideChangesSiblingOverlapScope(t *testing.T) {
	root := initDeterministicDemo(t)
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, gagglePath, "  isolation:\n    namespace: gaggle-example\n",
		"  isolation:\n    namespace: gaggle-example\n"+siblingSameRepoYAML)

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, workflowPath, "  start: local-ci\n  tasks:\n    - name: local-ci\n",
		"  start: claim-work\n  tasks:\n"+
			"    - name: claim-work\n"+
			"      type: deterministic\n"+
			"      goal: claim work\n"+
			"      run:\n"+
			"        command: [\"goobers\", \"backlog-query\"]\n"+
			"      capabilities: [\"github:issues:write\"]\n"+
			"      inputs:\n"+
			"        requireLabels: \"goobers:ready,area:special\"\n"+
			"      next: local-ci\n"+
			"    - name: local-ci\n")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	// The task's own requireLabels ("goobers:ready,area:special") fully
	// replaces the gaggle default ("area:frontend") for this workflow, so it
	// no longer overlaps the sibling's declared "area:frontend" — proving
	// the override, not the gaggle default, drove the comparison.
	if strings.Contains(stdout, "SIB001") {
		t.Fatalf("validate output unexpectedly warned despite the workflow's own requireLabels override:\n%s", stdout)
	}
}

func TestCheckTargetRepositoriesAllowsTokenlessADOAuth(t *testing.T) {
	original := targetRepositoryReachable
	t.Cleanup(func() { targetRepositoryReachable = original })

	targetRepositoryReachable = func(_ context.Context, repo instance.RepoRef, token string, _ credentials.StoreResolver) error {
		if repo.Provider != "ado" || repo.Project != "widgets" {
			t.Fatalf("repository = %#v", repo)
		}
		if token != "" {
			t.Fatalf("token = %q, want no materialized token", token)
		}
		return nil
	}
	var stdout strings.Builder
	ok := checkTargetRepositoriesAtFile([]instance.RepoRef{{
		Provider: "ado",
		Owner:    "acme",
		Project:  "widgets",
		Name:     "web",
		Auth:     &instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI},
	}}, nil, &stdout, "instance.yaml")
	if !ok || !strings.Contains(stdout.String(), "reachable") {
		t.Fatalf("checkTargetRepositories() = %v, output %q", ok, stdout.String())
	}
}

func TestCheckTargetRepositoriesWarnsOnOversizedGitHubRepo(t *testing.T) {
	t.Setenv("GITHUB_TOKEN_TEST_1547", "test-token")
	originalReachable := targetRepositoryReachable
	t.Cleanup(func() { targetRepositoryReachable = originalReachable })
	targetRepositoryReachable = func(context.Context, instance.RepoRef, string, credentials.StoreResolver) error {
		return nil
	}
	originalSize := targetRepositorySize
	t.Cleanup(func() { targetRepositorySize = originalSize })
	targetRepositorySize = func(context.Context, instance.RepoRef, string) (int64, error) {
		return 2 * oversizedRepoThresholdKB, nil
	}

	var stdout strings.Builder
	ok := checkTargetRepositoriesAtFile([]instance.RepoRef{{
		Provider: "github",
		Owner:    "acme",
		Name:     "monorepo",
		Token:    instance.TokenRef{Env: "GITHUB_TOKEN_TEST_1547"},
	}}, nil, &stdout, "instance.yaml")
	if !ok {
		t.Fatalf("checkTargetRepositories() = %v, want true (a size warning is advisory, not a failure)", ok)
	}
	for _, want := range []string{
		"REPOSITORY repos[0] acme/monorepo: WARNING: repository is 2048 MB, larger than the 1024 MB checkout-size threshold",
		"AdditionalRepos",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("checkTargetRepositories() output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestCheckTargetRepositoriesSizeCheckFailureIsNonFatal(t *testing.T) {
	t.Setenv("GITHUB_TOKEN_TEST_1547", "test-token")
	originalReachable := targetRepositoryReachable
	t.Cleanup(func() { targetRepositoryReachable = originalReachable })
	targetRepositoryReachable = func(context.Context, instance.RepoRef, string, credentials.StoreResolver) error {
		return nil
	}
	originalSize := targetRepositorySize
	t.Cleanup(func() { targetRepositorySize = originalSize })
	targetRepositorySize = func(context.Context, instance.RepoRef, string) (int64, error) {
		return 0, errors.New("rate limited")
	}

	var stdout strings.Builder
	ok := checkTargetRepositoriesAtFile([]instance.RepoRef{{
		Provider: "github",
		Owner:    "acme",
		Name:     "monorepo",
		Token:    instance.TokenRef{Env: "GITHUB_TOKEN_TEST_1547"},
	}}, nil, &stdout, "instance.yaml")
	if !ok {
		t.Fatalf("checkTargetRepositories() = %v, want true (size-check failure must not fail --check-repos)", ok)
	}
	if !strings.Contains(stdout.String(), "REPOSITORY repos[0] acme/monorepo: could not determine repository size: rate limited") {
		t.Fatalf("checkTargetRepositories() output missing size-check-failure diagnostic:\n%s", stdout.String())
	}
}

func replaceInFile(t *testing.T, path, old, replacement string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(raw), old, replacement, 1)
	if updated == string(raw) {
		t.Fatalf("%s does not contain %q", path, old)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendToFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
