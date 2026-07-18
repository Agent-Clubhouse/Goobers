package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const requireSandboxTestEnv = "GOOBERS_REQUIRE_SANDBOX_TEST"

func TestNativeSandboxConfinement(t *testing.T) {
	s := requiredNativeSandbox(t)
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.txt")
	inside := filepath.Join(workspace, "inside.txt")

	command := exec.Command(
		"sh", "-c",
		`printf 'inside' > "$1"; if printf 'escape' > "$2"; then exit 91; fi`,
		"sandbox-confinement", inside, outside,
	)
	command.Dir = workspace
	if err := s.Wrap(command, Policy{Workspace: workspace}); err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed command: %v\n%s", err, output)
	}

	content, err := os.ReadFile(inside)
	if err != nil {
		t.Fatalf("read in-workspace output: %v", err)
	}
	if string(content) != "inside" {
		t.Fatalf("in-workspace output = %q, want %q", content, "inside")
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("out-of-workspace write was not denied: %v", err)
	}
}

func TestNativeSandboxRejectsOutsideWorkingDirectory(t *testing.T) {
	s := requiredNativeSandbox(t)
	workspace := t.TempDir()
	command := exec.Command("sh", "-c", "true")
	command.Dir = t.TempDir()

	err := s.Wrap(command, Policy{Workspace: workspace})
	if err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("Wrap error = %v, want outside-workspace error", err)
	}
}

func TestNativeSandboxDefaultsWorkingDirectory(t *testing.T) {
	s := requiredNativeSandbox(t)
	workspace := t.TempDir()
	command := exec.Command("sh", "-c", "true")

	if err := s.Wrap(command, Policy{Workspace: workspace}); err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if command.Dir != resolved {
		t.Fatalf("command.Dir = %q, want %q", command.Dir, resolved)
	}
}

func TestNativeSandboxAllowsDeclaredWritableRoot(t *testing.T) {
	s := requiredNativeSandbox(t)
	workspace := t.TempDir()
	runtimeState := t.TempDir()
	stateFile := filepath.Join(runtimeState, "state.txt")
	command := exec.Command("sh", "-c", `printf 'state' > "$1"`, "sandbox-state", stateFile)
	command.Dir = workspace

	if err := s.Wrap(command, Policy{
		Workspace:     workspace,
		WritableRoots: []string{runtimeState},
	}); err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed command: %v\n%s", err, output)
	}
	content, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read runtime state: %v", err)
	}
	if string(content) != "state" {
		t.Fatalf("runtime state = %q, want %q", content, "state")
	}
}

func TestNativeSandboxRejectsInvalidPolicy(t *testing.T) {
	s := requiredNativeSandbox(t)
	workspace := t.TempDir()
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("create file fixture: %v", err)
	}

	tests := []struct {
		name      string
		command   func() *exec.Cmd
		policy    Policy
		wantError string
	}{
		{
			name:      "nil command",
			policy:    Policy{Workspace: workspace},
			wantError: "command is nil",
		},
		{
			name:      "empty command",
			command:   func() *exec.Cmd { return &exec.Cmd{} },
			policy:    Policy{Workspace: workspace},
			wantError: "command is empty",
		},
		{
			name:      "empty workspace",
			command:   trueCommand,
			wantError: "workspace is empty",
		},
		{
			name:      "missing workspace",
			command:   trueCommand,
			policy:    Policy{Workspace: filepath.Join(workspace, "missing")},
			wantError: "resolve workspace",
		},
		{
			name:      "workspace is file",
			command:   trueCommand,
			policy:    Policy{Workspace: file},
			wantError: "not a directory",
		},
		{
			name: "missing command directory",
			command: func() *exec.Cmd {
				command := trueCommand()
				command.Dir = filepath.Join(workspace, "missing")
				return command
			},
			policy:    Policy{Workspace: workspace},
			wantError: "resolve command directory",
		},
		{
			name:      "missing writable root",
			command:   trueCommand,
			policy:    Policy{Workspace: workspace, WritableRoots: []string{filepath.Join(workspace, "missing")}},
			wantError: "resolve writable root",
		},
		{
			name:      "empty writable root",
			command:   trueCommand,
			policy:    Policy{Workspace: workspace, WritableRoots: []string{""}},
			wantError: "writable root is empty",
		},
		{
			name:      "filesystem root writable",
			command:   trueCommand,
			policy:    Policy{Workspace: workspace, WritableRoots: []string{string(filepath.Separator)}},
			wantError: "cannot be a filesystem root",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var command *exec.Cmd
			if test.command != nil {
				command = test.command()
			}
			err := s.Wrap(command, test.policy)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Wrap error = %v, want error containing %q", err, test.wantError)
			}
		})
	}
}

func TestNativeSandboxCopilotLive(t *testing.T) {
	if os.Getenv("GOOBERS_SANDBOX_COPILOT_LIVE") != "1" {
		t.Skip("set GOOBERS_SANDBOX_COPILOT_LIVE=1 to run the signed-in Copilot sandbox probe")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("the recorded live authentication probe targets macOS Seatbelt")
	}
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skip("copilot CLI not found on PATH")
	}

	s := requiredNativeSandbox(t)
	workspace := t.TempDir()
	temp := filepath.Join(workspace, ".tmp")
	if err := os.Mkdir(temp, 0o700); err != nil {
		t.Fatalf("create sandbox temp directory: %v", err)
	}
	outputPath := filepath.Join(workspace, "SANDBOX_AUTH_OK.txt")
	sessionState := filepath.Join(os.Getenv("HOME"), ".copilot", "session-state")
	command := exec.Command(
		"copilot",
		"-p", "Create SANDBOX_AUTH_OK.txt in the current directory containing exactly: authenticated",
		"--allow-all-tools",
		"--log-dir", filepath.Join(workspace, ".logs"),
		"--log-level", "error",
	)
	command.Dir = workspace
	command.Env = liveCopilotEnv(temp)
	if err := s.Wrap(command, Policy{
		Workspace:     workspace,
		WritableRoots: []string{sessionState},
	}); err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed copilot: %v\n%s", err, output)
	}

	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read Copilot output: %v", err)
	}
	if strings.TrimSpace(string(content)) != "authenticated" {
		t.Fatalf("Copilot output = %q, want %q", content, "authenticated")
	}
}

func trueCommand() *exec.Cmd {
	return exec.Command("sh", "-c", "true")
}

func requiredNativeSandbox(t *testing.T) Sandbox {
	t.Helper()
	s, err := New()
	if err == nil {
		return s
	}
	if os.Getenv(requireSandboxTestEnv) != "1" &&
		(errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnsupported)) {
		t.Skip(err)
	}
	t.Fatalf("New: %v", err)
	return nil
}

func liveCopilotEnv(temp string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + temp,
	}
}
