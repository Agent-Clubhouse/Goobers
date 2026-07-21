package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestGitRepositoryReachableTimeoutKillsDescendantHoldingOutputPipe(t *testing.T) {
	pidFile := installHangingGit(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		result <- gitRepositoryReachable(ctx, instance.RepoRef{
			Provider: "github",
			Owner:    "example",
			Name:     "repository",
		}, "test-token")
	}()
	waitForFile(t, pidFile)

	start := time.Now()
	cancel()
	err := <-result
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("gitRepositoryReachable error = %v, want canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("gitRepositoryReachable took %s; descendant inherited its output pipe after timeout", elapsed)
	}
}

func TestGitRepositoryReachableTimeoutBoundedWhenEscapedDescendantHoldsOutputPipe(t *testing.T) {
	pidFile := installHangingGit(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		result <- gitRepositoryReachable(ctx, instance.RepoRef{
			Provider: "github",
			Owner:    "example",
			Name:     "repository",
		}, "test-token")
	}()
	waitForFile(t, pidFile)

	start := time.Now()
	cancel()
	err := <-result
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("gitRepositoryReachable error = %v, want canceled", err)
	}
	if elapsed > repositoryKillWaitDelay+2*time.Second {
		t.Fatalf("gitRepositoryReachable took %s; bounded wait did not engage after %s", elapsed, repositoryKillWaitDelay)
	}
}

// waitForFileTimeout bounds how long waitForFile waits for the hanging-git
// fixture's descendant to fork/exec and write its pid file. This is a SETUP
// precondition — the behavior under test is the subsequent cancel-and-kill,
// bounded separately by the `elapsed` assertions — so the ceiling is
// deliberately generous: waitForFile returns the instant the file appears
// (typically well under 100ms), so a large ceiling costs the happy path
// nothing, while a tight one (the previous 2s) intermittently timed out purely
// because spawning an external bash process gets starved under the full
// `-race` suite's CPU contention (#1145). Not a retry band-aid — it fixes an
// assumption (2s is always enough to schedule a subprocess) that is false under
// load.
const waitForFileTimeout = 30 * time.Second

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(waitForFileTimeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("wait for %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not created within %s (fixture descendant never spawned)", path, waitForFileTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func installHangingGit(t *testing.T, escapeProcessGroup bool) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	script := "#!" + bash + "\n"
	if escapeProcessGroup {
		script += "set -m\n"
	}
	script += "sleep 10 &\necho $! > \"$GOOBERS_TEST_CHILD_PID_FILE\"\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_TEST_CHILD_PID_FILE", pidFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	})
	return pidFile
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
