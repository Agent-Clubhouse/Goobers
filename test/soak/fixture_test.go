package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Installed fixtures use the current executable. Let the test executable serve
// the same native subcommand for both fixture tests and the real-CLI smoke.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "fixture" {
		main()
	}
	os.Exit(m.Run())
}

func TestNativeFixtureResultsAndAmbientGitIsolation(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "not-a-repository"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "invalid configuration")
	t.Setenv("GIT_TEMPLATE_DIR", filepath.Join(t.TempDir(), "missing-template"))
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("invalid configuration"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	for _, mode := range []string{"success", "failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			root := t.TempDir()
			cmd := exec.CommandContext(ctx, binary, "fixture", mode)
			cmd.Dir = root
			start := time.Now()
			output, err := cmd.CombinedOutput()
			if mode == "success" && err != nil {
				t.Fatalf("fixture: %v: %s", err, output)
			}
			if mode == "failure" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					t.Fatalf("expected fixture exit 1, got %v: %s", err, output)
				}
			}
			if time.Since(start) < 3*time.Second {
				t.Fatal("fixture did not hold its overlap window")
			}
			want := "{\"fixture\":\"complete\",\"integrity\":\"unapproved\"}\n"
			if mode == "failure" {
				want = "{\"errorCode\":\"soak_fixture_failure\",\"errorMessage\":\"soak_fixture_failure\",\"errorRetryable\":false,\"integrity\":\"unapproved\"}\n"
			}
			data, err := os.ReadFile(filepath.Join(root, "result.json"))
			if err != nil || string(data) != want {
				t.Fatalf("result = %s, error = %v", data, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != "result.json" {
				t.Fatalf("fixture did not clean its temporary repository: %v, %v", entries, err)
			}
		})
	}
}

func TestNativeFixtureCancellationCleansWithoutExpectedResult(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer
	if code := runFixture(ctx, []string{"failure"}, &stderr); code != 2 {
		t.Fatalf("cancelled fixture exit = %d: %s", code, &stderr)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled fixture left files or claimed expected failure: %v, %v", entries, err)
	}
}
