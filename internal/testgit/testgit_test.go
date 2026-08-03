package testgit

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCommandIsolatesGit(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/host/global")
	t.Setenv("GIT_CONFIG_SYSTEM", "/host/system")

	cmd := Command("status", "--short")
	wantArgs := []string{"git", "-c", "gc.auto=0", "-c", "maintenance.auto=0", "status", "--short"}
	if !slices.Equal(cmd.Args, wantArgs) {
		t.Fatalf("args = %q, want %q", cmd.Args, wantArgs)
	}
	for name, want := range isolatedEnvironment {
		if got := environmentValue(cmd.Env, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestCommandContextIsolatesGit(t *testing.T) {
	cmd := CommandContext(context.Background(), "status")
	if got := environmentValue(cmd.Env, "GIT_CONFIG_NOSYSTEM"); got != "1" {
		t.Fatalf("GIT_CONFIG_NOSYSTEM = %q, want 1", got)
	}
}

func TestEnvironmentPreservesUnrelatedVariables(t *testing.T) {
	t.Setenv("GOOBERS_TESTGIT_PRESERVED", "yes")
	if got := environmentValue(Environment(), "GOOBERS_TESTGIT_PRESERVED"); got != "yes" {
		t.Fatalf("GOOBERS_TESTGIT_PRESERVED = %q, want yes", got)
	}
}

func TestCommandIgnoresHostGitConfig(t *testing.T) {
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[goobers]\n\tambient = leaked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	output, err := Command("config", "--global", "--get", "goobers.ambient").CombinedOutput()
	if err == nil {
		t.Fatalf("host git config leaked into command: %q", output)
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("git config: %v\n%s", err, output)
	}
}

func environmentValue(env []string, name string) string {
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}
