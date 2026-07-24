package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/sandbox"
)

// stubSandbox is a deterministic sandbox.Sandbox double: it records the
// policy it was asked to apply and prepends a recognizable wrapper prefix,
// mimicking the argv-rewrite shape of the real Seatbelt/bubblewrap wrappers
// without requiring either to be installed.
type stubSandbox struct {
	policies []sandbox.Policy
	wrapErr  error
}

func (s *stubSandbox) Wrap(command *exec.Cmd, policy sandbox.Policy) error {
	if s.wrapErr != nil {
		return s.wrapErr
	}
	s.policies = append(s.policies, sandbox.Policy{
		Workspace:     policy.Workspace,
		WritableRoots: append([]string(nil), policy.WritableRoots...),
	})
	command.Path = "sandbox-stub"
	command.Args = append([]string{"sandbox-stub", "--confine"}, command.Args...)
	return nil
}

func (s *stubSandbox) Mechanism() string { return "stub" }

// capturingProcessRunner records every ProcessRequest, so tests can assert on
// both the initial turn and the contract-recovery turn.
type capturingProcessRunner struct {
	reqs []ProcessRequest
	act  func(call int, req ProcessRequest) error
}

func (c *capturingProcessRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	call := len(c.reqs)
	c.reqs = append(c.reqs, req)
	if c.act != nil {
		if err := c.act(call, req); err != nil {
			return ProcessResult{ExitCode: 1}, err
		}
	}
	return ProcessResult{ExitCode: 0}, nil
}

func TestCopilotAdapterConfinesSubprocessUnderEnforcedSandbox(t *testing.T) {
	workspace := t.TempDir()
	sb := &stubSandbox{}
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Sandbox:        sb,
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	command := runner.lastReq.Command
	if len(command) < 3 || command[0] != "sandbox-stub" || command[1] != "--confine" {
		t.Fatalf("exec'd argv is not sandbox-wrapped: %v", command)
	}
	if command[2] != "copilot" {
		t.Fatalf("wrapped argv target = %q, want the original copilot invocation: %v", command[2], command)
	}
	logDir := filepath.Join(workspace, ".goobers", "sandbox", "logs")
	logFlag := slices.Index(command, "--log-dir")
	if logFlag < 0 || logFlag+1 >= len(command) || command[logFlag+1] != logDir {
		t.Fatalf("wrapped argv missing --log-dir %s: %v", logDir, command)
	}

	if len(sb.policies) != 1 {
		t.Fatalf("Wrap called %d times, want 1", len(sb.policies))
	}
	if sb.policies[0].Workspace != workspace {
		t.Fatalf("policy workspace = %q, want %q", sb.policies[0].Workspace, workspace)
	}
	if len(sb.policies[0].WritableRoots) != 0 {
		t.Fatalf("policy writable roots = %v, want none for a git-less workspace", sb.policies[0].WritableRoots)
	}

	copilotHome := filepath.Join(workspace, ".goobers", "sandbox", "copilot-home")
	tempDir := filepath.Join(workspace, ".goobers", "sandbox", "tmp")
	wantEnv := map[string]string{"COPILOT_HOME": copilotHome, "TMPDIR": tempDir}
	for name, want := range wantEnv {
		got, found := "", false
		for _, entry := range runner.lastReq.Env {
			if value, ok := strings.CutPrefix(entry, name+"="); ok {
				got, found = value, true
			}
		}
		if !found || got != want {
			t.Fatalf("subprocess env %s = %q (found=%v), want %q", name, got, found, want)
		}
	}
	for _, dir := range []string{copilotHome, tempDir, logDir} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("sandbox runtime directory %s missing: %v", dir, err)
		}
	}
}

// TestCopilotAdapterWithoutSandboxKeepsLaunchUnchanged pins the opt-in
// default: a nil RunRequest.Sandbox must produce the exact pre-sandbox argv —
// no wrapper prefix, no --log-dir, no COPILOT_HOME/TMPDIR overrides pointed
// into the workspace.
func TestCopilotAdapterWithoutSandboxKeepsLaunchUnchanged(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	command := runner.lastReq.Command
	if command[0] != "copilot" {
		t.Fatalf("argv[0] = %q, want the unwrapped copilot invocation: %v", command[0], command)
	}
	if slices.Contains(command, "--log-dir") {
		t.Fatalf("unsandboxed argv unexpectedly carries --log-dir: %v", command)
	}
	for _, entry := range runner.lastReq.Env {
		if strings.HasPrefix(entry, "COPILOT_HOME=") {
			t.Fatalf("unsandboxed env unexpectedly carries a COPILOT_HOME override: %v", entry)
		}
		if value, ok := strings.CutPrefix(entry, "TMPDIR="); ok && strings.HasPrefix(value, workspace) {
			t.Fatalf("unsandboxed env TMPDIR redirected into the workspace: %v", entry)
		}
	}
	if dir := filepath.Join(workspace, ".goobers", "sandbox"); dirExists(dir) {
		t.Fatalf("unsandboxed run created sandbox runtime state at %s", dir)
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// TestCopilotAdapterRecoveryTurnStaysConfined proves the contract-recovery
// turn (a clean exit with no completion file) reuses the WRAPPED argv with
// only the prompt swapped — the wrapper prefix must shift the prompt index,
// not corrupt the recovery invocation or escape the sandbox.
func TestCopilotAdapterRecoveryTurnStaysConfined(t *testing.T) {
	workspace := t.TempDir()
	sb := &stubSandbox{}
	runner := &capturingProcessRunner{
		act: func(call int, req ProcessRequest) error {
			if call == 0 {
				return nil // first turn: clean exit, no completion file
			}
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Sandbox:        sb,
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.reqs) != 2 {
		t.Fatalf("harness calls = %d, want initial + recovery", len(runner.reqs))
	}
	first, second := runner.reqs[0].Command, runner.reqs[1].Command
	if second[0] != "sandbox-stub" {
		t.Fatalf("recovery argv is not sandbox-wrapped: %v", second)
	}
	if len(first) != len(second) {
		t.Fatalf("recovery argv length %d != initial %d", len(second), len(first))
	}
	diffs := 0
	promptIdx := -1
	for i := range first {
		if first[i] != second[i] {
			diffs++
			promptIdx = i
		}
	}
	if diffs != 1 {
		t.Fatalf("recovery argv differs from initial at %d positions, want exactly the prompt: %v vs %v", diffs, first, second)
	}
	// The prompt follows the wrapper prefix (2 args), the base command (1)
	// and the prompt flag (1).
	if wantIdx := 4; promptIdx != wantIdx {
		t.Fatalf("recovery prompt swapped at index %d, want %d (wrapper-shifted)", promptIdx, wantIdx)
	}
	if !strings.Contains(second[promptIdx], "completion") && !strings.Contains(second[promptIdx], "result") {
		t.Fatalf("recovery prompt does not look like the completion-recovery prompt: %q", second[promptIdx])
	}
}

func TestCopilotAdapterSandboxWrapFailureFailsRun(t *testing.T) {
	workspace := t.TempDir()
	sb := &stubSandbox{wrapErr: os.ErrPermission}
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Sandbox:        sb,
	}
	_, err := adapter.Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("Run error = %v, want a sandbox wrap failure", err)
	}
	if len(runner.lastReq.Command) != 0 {
		t.Fatalf("subprocess ran despite a sandbox wrap failure: %v", runner.lastReq.Command)
	}
}

// TestGitWritableRootsNarrowsLinkedWorktreeCommonDir pins the S3/#166
// containment shape for the mirror layout internal/worktree provisions: the
// grant is the run's own gitdir plus the mirror's objects/refs/logs
// subdirectories — never the mirror common dir itself, whose hooks/ and
// config the daemon's next unconfined `git worktree add` would execute if a
// confined agent could write them.
func TestGitWritableRootsNarrowsLinkedWorktreeCommonDir(t *testing.T) {
	mirror := t.TempDir()
	workspace := t.TempDir()
	gitdir := filepath.Join(mirror, "worktrees", "run-1")
	// A real bare mirror always has objects/ and refs/; logs/ only appears
	// after the first reflog write, so it is deliberately absent here.
	for _, dir := range []string{gitdir, filepath.Join(mirror, "objects"), filepath.Join(mirror, "refs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	roots, err := gitWritableRoots(workspace)
	if err != nil {
		t.Fatalf("gitWritableRoots: %v", err)
	}
	want := []string{
		gitdir,
		filepath.Join(mirror, "objects"),
		filepath.Join(mirror, "refs"),
		filepath.Join(mirror, "logs"),
	}
	if !slices.Equal(roots, want) {
		t.Fatalf("roots = %v, want %v", roots, want)
	}
	// The missing reflog dir must have been created: the sandbox seam
	// requires writable roots to exist, and a stage's first commit writes it.
	if info, err := os.Stat(filepath.Join(mirror, "logs")); err != nil || !info.IsDir() {
		t.Fatalf("mirror logs/ not materialized for the sandbox grant: %v", err)
	}
	// And no grant may cover the daemon-executed surface.
	for _, root := range roots {
		for _, denied := range []string{mirror, filepath.Join(mirror, "hooks"), filepath.Join(mirror, "worktrees", "run-2")} {
			if rel, err := filepath.Rel(root, denied); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("root %s covers %s — a confined agent could plant daemon-executed state", root, denied)
			}
		}
	}
}

func TestGitWritableRootsNarrowsCommonDirElsewhere(t *testing.T) {
	workspace := t.TempDir()
	gitdir := t.TempDir()
	common := t.TempDir()
	if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte(common+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots, err := gitWritableRoots(workspace)
	if err != nil {
		t.Fatalf("gitWritableRoots: %v", err)
	}
	cleaned := filepath.Clean(common)
	want := []string{
		gitdir,
		filepath.Join(cleaned, "objects"),
		filepath.Join(cleaned, "refs"),
		filepath.Join(cleaned, "logs"),
	}
	if !slices.Equal(roots, want) {
		t.Fatalf("roots = %v, want %v", roots, want)
	}
}

// TestGitWritableRootsDanglingCommonDirFailsClosed: a commondir file naming a
// nonexistent directory must be an error, not a silent mkdir at an
// attacker-chosen path and not an unconfined fallback.
func TestGitWritableRootsDanglingCommonDirFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	gitdir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")
	if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte(missing+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if roots, err := gitWritableRoots(workspace); err == nil {
		t.Fatalf("gitWritableRoots = %v, want an error for a dangling commondir", roots)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("dangling common dir was materialized: %v", err)
	}
}

func TestGitWritableRootsNoGitState(t *testing.T) {
	roots, err := gitWritableRoots(t.TempDir())
	if err != nil {
		t.Fatalf("gitWritableRoots: %v", err)
	}
	if len(roots) != 0 {
		t.Fatalf("roots = %v, want none for a workspace without git state", roots)
	}
}

func TestGitWritableRootsSelfContainedGitDir(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	roots, err := gitWritableRoots(workspace)
	if err != nil {
		t.Fatalf("gitWritableRoots: %v", err)
	}
	if len(roots) != 0 {
		t.Fatalf("roots = %v, want none for a self-contained .git directory", roots)
	}
}
