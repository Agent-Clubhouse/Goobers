package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestValidateCheckRepos(t *testing.T) {
	root := filepath.Join(t.TempDir(), "foreign")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}
	t.Setenv("GOOBERS_GITHUB_TOKEN", "test-token")

	original := targetRepositoryReachable
	t.Cleanup(func() { targetRepositoryReachable = original })

	called := 0
	targetRepositoryReachable = func(_ context.Context, repo instance.RepoRef, token string) error {
		called++
		if repo.Owner != "your-org" || repo.Name != "your-repo" {
			t.Errorf("repository = %s/%s, want your-org/your-repo", repo.Owner, repo.Name)
		}
		if token != "test-token" {
			t.Errorf("token = %q, want resolved test token", token)
		}
		return nil
	}
	code, stdout, stderr := runArgs(t, "validate", "--check-repos", root)
	if code != 0 {
		t.Fatalf("validate --check-repos: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if called != 1 || !strings.Contains(stdout, "REPOSITORY repos[0] your-org/your-repo: reachable") {
		t.Fatalf("repository check calls=%d stdout=%q", called, stdout)
	}

	targetRepositoryReachable = func(context.Context, instance.RepoRef, string) error {
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
	replaceInFile(t, workflowPath, `        command: ["true"]`, "        command: [\"true\"]\n        image: alpine:3.20")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("advisory validate code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "run.image is not honored by the local runner") {
		t.Fatalf("advisory validate did not render warning:\n%s", stdout)
	}

	code, stdout, stderr = runArgs(t, "validate", "--strict", root)
	if code != 1 {
		t.Fatalf("strict validate code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"run.image is not honored by the local runner",
		"config directory has 2 warning(s); --strict treats warnings as errors",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("strict validate output missing %q:\n%s", want, stdout)
		}
	}
}

func TestCheckTargetRepositoriesAllowsTokenlessADOAuth(t *testing.T) {
	original := targetRepositoryReachable
	t.Cleanup(func() { targetRepositoryReachable = original })

	targetRepositoryReachable = func(_ context.Context, repo instance.RepoRef, token string) error {
		if repo.Provider != "ado" || repo.Project != "widgets" {
			t.Fatalf("repository = %#v", repo)
		}
		if token != "" {
			t.Fatalf("token = %q, want no materialized token", token)
		}
		return nil
	}
	var stdout strings.Builder
	ok := checkTargetRepositories([]instance.RepoRef{{
		Provider: "ado",
		Owner:    "acme",
		Project:  "widgets",
		Name:     "web",
		Auth:     &instance.ADOAuthConfig{Kind: instance.ADOAuthAzureCLI},
	}}, &stdout)
	if !ok || !strings.Contains(stdout.String(), "reachable") {
		t.Fatalf("checkTargetRepositories() = %v, output %q", ok, stdout.String())
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
