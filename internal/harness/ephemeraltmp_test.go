package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ephemeraltmp_test.go covers the agentic half of the self-runner
// `tmp:ephemeral` binding. It exists because a runner property that is only
// half enforced is worse than one that is absent: the solver tells the
// operator runner `self` enforces the effect, and an agentic stage running
// `go build` through its agent charges the same unbounded cache the
// deterministic stages did.

func lastEnvValue(env []string, name string) (string, bool) {
	value, ok := "", false
	for _, entry := range env {
		k, v, cut := strings.Cut(entry, "=")
		if cut && k == name {
			value, ok = v, true
		}
	}
	return value, ok
}

func runCopilotWithEphemeralTmp(t *testing.T, root string) (*fakeProcessRunner, string) {
	t.Helper()
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:          []string{"copilot"},
		Runner:           runner,
		EphemeralTmp:     true,
		EphemeralTmpRoot: root,
	}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return runner, workspace
}

// TestCopilotAdapterBindsAndReclaimsEphemeralTmp: the agent's subprocess gets
// an attempt-private temp and an attempt-private build cache, and both are
// gone when the attempt returns.
func TestCopilotAdapterBindsAndReclaimsEphemeralTmp(t *testing.T) {
	root := t.TempDir()
	daemonCache := filepath.Join(root, "gocache")
	t.Setenv("TMPDIR", root)
	t.Setenv("GOCACHE", daemonCache)

	runner, _ := runCopilotWithEphemeralTmp(t, root)

	tmpdir, ok := lastEnvValue(runner.lastReq.Env, "TMPDIR")
	if !ok {
		t.Fatal("the harness subprocess environment carries no TMPDIR")
	}
	if tmpdir == root {
		t.Fatalf("TMPDIR = %q, want an attempt-private directory beneath %q", tmpdir, root)
	}
	if filepath.Dir(tmpdir) != root {
		t.Fatalf("TMPDIR = %q, want it carved out of the daemon temp root %q", tmpdir, root)
	}
	if gocache, _ := lastEnvValue(runner.lastReq.Env, "GOCACHE"); gocache != filepath.Join(tmpdir, "gocache") {
		t.Fatalf("GOCACHE = %q, want the temp-nested cache re-rooted to %q", gocache, filepath.Join(tmpdir, "gocache"))
	}
	if _, err := os.Stat(tmpdir); !os.IsNotExist(err) {
		t.Fatalf("the attempt's temp %q survived the stage (stat err %v)", tmpdir, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "goobers-ephemeral-tmp-") {
			t.Fatalf("the attempt left %q behind in the temp root", entry.Name())
		}
	}
}

// TestCopilotAdapterWithoutEphemeralTmpIsUnchanged is the zero-declaration
// invariance guard for the agentic path.
func TestCopilotAdapterWithoutEphemeralTmpIsUnchanged(t *testing.T) {
	root := t.TempDir()
	daemonCache := filepath.Join(root, "gocache")
	t.Setenv("TMPDIR", root)
	t.Setenv("GOCACHE", daemonCache)

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if tmpdir, _ := lastEnvValue(runner.lastReq.Env, "TMPDIR"); tmpdir != root {
		t.Fatalf("TMPDIR = %q, want the daemon's own temp root %q unchanged", tmpdir, root)
	}
	if gocache, _ := lastEnvValue(runner.lastReq.Env, "GOCACHE"); gocache != daemonCache {
		t.Fatalf("GOCACHE = %q, want %q unchanged", gocache, daemonCache)
	}
}

// TestSandboxConfinementStillOwnsTMPDIRUnderEphemeralTmp pins the ordering
// that keeps the two isolations from fighting: an enforced sandbox routes the
// CLI's temp into the workspace so the policy needs no writable root beyond
// the worktree, and that area is already attempt-private. Pointing TMPDIR at a
// directory outside the sandbox's writable roots would break every confined
// agentic stage. The cache re-rooting still applies, which is the half the
// confinement never covered.
func TestSandboxConfinementStillOwnsTMPDIRUnderEphemeralTmp(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("GOCACHE", filepath.Join(root, "gocache"))

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:          []string{"copilot"},
		Runner:           runner,
		EphemeralTmp:     true,
		EphemeralTmpRoot: root,
	}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Sandbox:        &stubSandbox{},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	tmpdir, ok := lastEnvValue(runner.lastReq.Env, "TMPDIR")
	if !ok {
		t.Fatal("the confined subprocess carries no TMPDIR")
	}
	if !strings.HasPrefix(tmpdir, workspace+string(filepath.Separator)) {
		t.Fatalf("TMPDIR = %q, want the in-workspace confinement path under %q to keep winning", tmpdir, workspace)
	}
	gocache, _ := lastEnvValue(runner.lastReq.Env, "GOCACHE")
	if gocache == filepath.Join(root, "gocache") {
		t.Fatalf("GOCACHE = %q, want the temp-nested cache re-rooted even under confinement", gocache)
	}
}

// TestClaudeAdapterBindsAndReclaimsEphemeralTmp: the same property, on the
// other harness, so the self entry's declaration is true whichever one a
// goober names.
func TestClaudeAdapterBindsAndReclaimsEphemeralTmp(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("GOCACHE", filepath.Join(root, "gocache"))

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &ClaudeAdapter{
		Command:          []string{"claude"},
		Runner:           runner,
		EphemeralTmp:     true,
		EphemeralTmpRoot: root,
	}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	gocache, ok := lastEnvValue(runner.lastReq.Env, "GOCACHE")
	if !ok {
		t.Fatal("the harness subprocess environment carries no GOCACHE")
	}
	if gocache == filepath.Join(root, "gocache") {
		t.Fatalf("GOCACHE = %q, want it re-rooted out of the shared temp root", gocache)
	}
	scope := filepath.Dir(gocache)
	if filepath.Dir(scope) != root || !strings.HasPrefix(filepath.Base(scope), "goobers-ephemeral-tmp-") {
		t.Fatalf("GOCACHE = %q is not inside an attempt-private directory under %q", gocache, root)
	}
	if _, err := os.Stat(scope); !os.IsNotExist(err) {
		t.Fatalf("the attempt's temp %q survived the stage (stat err %v)", scope, err)
	}
}

// TestAdapterEphemeralTmpFailsClosed: an unbindable restriction refuses the
// stage rather than running the agent against ambient temp.
func TestAdapterEphemeralTmpFailsClosed(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	unusable := filepath.Join(blocker, "under-a-file")
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	}

	copilot := &CopilotAdapter{
		Command:          []string{"copilot"},
		Runner:           &fakeProcessRunner{result: ProcessResult{ExitCode: 0}},
		EphemeralTmp:     true,
		EphemeralTmpRoot: unusable,
	}
	if _, err := copilot.Run(context.Background(), req); err == nil {
		t.Fatal("copilot-cli ran with an unbindable tmp:ephemeral; it must fail closed")
	} else if !strings.Contains(err.Error(), "tmp:ephemeral") {
		t.Fatalf("error %q does not name the restriction that could not be bound", err)
	}

	claude := &ClaudeAdapter{
		Command:          []string{"claude"},
		Runner:           &fakeProcessRunner{result: ProcessResult{ExitCode: 0}},
		EphemeralTmp:     true,
		EphemeralTmpRoot: unusable,
	}
	if _, err := claude.Run(context.Background(), req); err == nil {
		t.Fatal("claude-code ran with an unbindable tmp:ephemeral; it must fail closed")
	} else if !strings.Contains(err.Error(), "tmp:ephemeral") {
		t.Fatalf("error %q does not name the restriction that could not be bound", err)
	}
}
