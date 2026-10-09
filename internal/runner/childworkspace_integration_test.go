//go:build integration

package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type childWorkspaceAgent struct {
	invoke func(apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error)
}

func (g childWorkspaceAgent) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if done := invoke.RegisterWorkspaceWriter(ctx); done != nil {
		defer done(nil) // this fixture only writes synchronously in the host
	}
	return g.invoke(env)
}
func (childWorkspaceAgent) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	panic("unexpected reviewer")
}

type childWorkspaceFixture struct {
	config      Config
	input       StartInput
	parent      string
	child       *worktree.Worktree
	parentHead  string
	parentIndex []byte
}

func prepareChildWorkspaceFixture(t *testing.T, pause bool) childWorkspaceFixture {
	t.Helper()
	ctx := t.Context()
	parent := t.TempDir()
	runGit(t, parent, "init", "--initial-branch=main")
	runGit(t, parent, "config", "user.email", "child-test@example.invalid")
	runGit(t, parent, "config", "user.name", "Child Test")
	childWorkspaceWrite(t, parent, "main.txt", []byte("base\n"))
	runGit(t, parent, "add", ".")
	runGit(t, parent, "commit", "-m", "base")
	root := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.WorkingCopy(ctx, parent); err != nil {
		t.Fatal(err)
	}
	head := gitOutput(t, parent, "rev-parse", "HEAD")
	index, err := os.ReadFile(filepath.Join(parent, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	childWorkspaceWrite(t, parent, "main.txt", []byte("parent dirty\n"))
	childWorkspaceWrite(t, parent, "parent.bin", []byte{0, 255, 1})
	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}).CanonicalKey()
	at := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	snapshot, err := recovery.CaptureChildSnapshot(ctx, parent, key, "parent-run", at, at.Add(time.Hour), recovery.SnapshotPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	snapshot, err = recovery.WriteChildSnapshotBundle(ctx, parent, snapshot, &archive, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(root, "child.bundle")
	if err := os.WriteFile(archivePath, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.WithRecoveryMirror(ctx, parent, func(mirror string) error {
		return recovery.ImportChildSnapshot(ctx, mirror, archivePath, snapshot, 1<<20)
	}); err != nil {
		t.Fatal(err)
	}
	child, err := manager.CreateChildFromSnapshot(ctx, worktree.ChildOptions{RepoURL: parent, RunID: "child-workspace", OwnerRunID: "child-run", Gaggle: "web", SnapshotSHA: snapshot.Record.SnapshotSHA})
	if err != nil {
		t.Fatal(err)
	}
	input := childWorkspaceStart(childWorkspaceMachine(t, apiv1.WorkspaceRepo, pause))
	input.ChildWorkspace = &ChildWorkspaceAdmission{WorkspaceID: "child-workspace", ForkSHA: snapshot.Record.SnapshotSHA, RepositoryDigest: worktree.RepositoryDigest(parent)}
	config := Config{Worktrees: manager, RunsDir: filepath.Join(root, "runs"), ScratchDir: filepath.Join(root, "scratch"), ConfigGeneration: journal.Digest([]byte("config")), RepoCloneURL: func(apiv1.RepoRef) (string, error) { return parent, nil }}
	return childWorkspaceFixture{config: config, input: input, parent: parent, child: child, parentHead: head, parentIndex: index}
}

func childWorkspaceWrite(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func childWorkspaceRead(t *testing.T, dir, name string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s content=%q error=%v, want %q", name, got, err, want)
	}
}

func (f childWorkspaceFixture) assertParentUnchanged(t *testing.T) {
	t.Helper()
	childWorkspaceRead(t, f.parent, "main.txt", []byte("parent dirty\n"))
	childWorkspaceRead(t, f.parent, "parent.bin", []byte{0, 255, 1})
	childWorkspaceRead(t, f.parent, filepath.Join(".git", "index"), f.parentIndex)
	if got := gitOutput(t, f.parent, "rev-parse", "HEAD"); got != f.parentHead {
		t.Fatal("child execution changed parent HEAD")
	}
}

func (f childWorkspaceFixture) runner(t *testing.T, invokeFn func(apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error)) *Runner {
	t.Helper()
	cfg := f.config
	cfg.NewAgentic = func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
		return childWorkspaceAgent{invoke: invokeFn}, nil
	}
	runner, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestIntegrationChildWorkspaceRunnerAdoptsAndRetainsRetryEdits(t *testing.T) {
	testdep.Require(t, "git")
	f := prepareChildWorkspaceFixture(t, false)
	calls := 0
	r := f.runner(t, func(env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
		calls++
		if env.Workspace != f.child.Path {
			t.Fatalf("workspace=%q, want adopted %q", env.Workspace, f.child.Path)
		}
		childWorkspaceRead(t, env.Workspace, "parent.bin", []byte{0, 255, 1})
		if calls == 1 {
			childWorkspaceRead(t, env.Workspace, "main.txt", []byte("parent dirty\n"))
			childWorkspaceWrite(t, env.Workspace, "main.txt", []byte("first attempt edit\n"))
			childWorkspaceWrite(t, env.Workspace, "retry.bin", []byte{1, 0, 2})
			return apiv1.ResultEnvelope{}, errors.New("retry after child edit")
		}
		childWorkspaceRead(t, env.Workspace, "main.txt", []byte("first attempt edit\n"))
		childWorkspaceRead(t, env.Workspace, "retry.bin", []byte{1, 0, 2})
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	})
	result, err := r.Start(t.Context(), f.input)
	if err != nil || result.Phase != journal.PhaseCompleted || calls != 2 {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
	}
	childWorkspaceRead(t, f.child.Path, "main.txt", []byte("first attempt edit\n"))
	f.assertParentUnchanged(t)
	rd, err := journal.OpenRead(filepath.Join(f.config.RunsDir, f.input.RunID))
	if err != nil {
		t.Fatal(err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyChildWorkspaceQuiescence(rd, id, events); err != nil {
		t.Fatal(err)
	}
	admission, err := PinnedChildWorkspaceAdmission(rd, id)
	if err != nil || admission == nil || *admission != *f.input.ChildWorkspace {
		t.Fatalf("pinned admission=%+v err=%v", admission, err)
	}
}

func TestIntegrationChildWorkspaceResumeRequiresExactExistingCustody(t *testing.T) {
	testdep.Require(t, "git")
	for _, scenario := range []string{"retained", "missing", "foreign-branch", "altered-pin"} {
		t.Run(scenario, func(t *testing.T) {
			f := prepareChildWorkspaceFixture(t, true)
			calls := 0
			invokeFn := func(env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
				calls++
				if env.Workspace != f.child.Path {
					t.Fatal("resume replaced child workspace")
				}
				childWorkspaceRead(t, env.Workspace, "main.txt", []byte("retained progress\n"))
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}
			result, err := f.runner(t, invokeFn).Start(t.Context(), f.input)
			if err != nil || result.FinalState != "approval" || calls != 0 {
				t.Fatalf("pause=%+v %v calls=%d", result, err, calls)
			}
			childWorkspaceWrite(t, f.child.Path, "main.txt", []byte("retained progress\n"))
			switch scenario {
			case "missing":
				if err := os.Rename(f.child.Path, f.child.Path+"-removed"); err != nil {
					t.Fatal(err)
				}
			case "foreign-branch":
				runGit(t, f.child.Path, "checkout", "-b", "foreign-branch")
			case "altered-pin":
				rd, err := journal.OpenRead(filepath.Join(f.config.RunsDir, f.input.RunID))
				if err != nil {
					t.Fatal(err)
				}
				id, err := rd.Identity()
				if err != nil {
					t.Fatal(err)
				}
				for _, input := range id.Inputs {
					if input.Name == ChildWorkspaceInputName {
						childWorkspaceWrite(t, filepath.Join(f.config.RunsDir, f.input.RunID), input.Ref.Path, []byte("{}"))
					}
				}
			}
			in := humanResumeInput(f.input.RunID, f.input.Machine, "approval", latestHumanPauseSeq(t, f.config.RunsDir, f.input.RunID, "approval"), "pass")
			in.GooberDigest = f.input.GooberDigest
			result, err = f.runner(t, invokeFn).Resume(t.Context(), in)
			if scenario == "retained" {
				if err != nil || result.Phase != journal.PhaseCompleted || calls != 1 {
					t.Fatalf("resume=%+v %v calls=%d", result, err, calls)
				}
			} else if err == nil || calls != 0 {
				t.Fatalf("invalid custody ran: %+v %v calls=%d", result, err, calls)
			}
			if scenario == "missing" {
				if _, err := os.Stat(f.child.Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("resume recreated missing child")
				}
			}
			f.assertParentUnchanged(t)
		})
	}
}

func TestIntegrationChildWorkspaceHumanRerunPreservesProgress(t *testing.T) {
	testdep.Require(t, "git")
	f := prepareChildWorkspaceFixture(t, false)
	calls := 0
	agent := func(env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
		calls++
		if env.Workspace != f.child.Path {
			t.Fatal("human rerun replaced child workspace")
		}
		if env.InstructionAddendum == "" {
			childWorkspaceWrite(t, env.Workspace, "main.txt", []byte("needs guidance\n"))
			return apiv1.ResultEnvelope{Status: apiv1.ResultBlocked, Error: &apiv1.ErrorInfo{Code: "needs_guidance", Message: "select the implementation"}}, nil
		}
		childWorkspaceRead(t, env.Workspace, "main.txt", []byte("needs guidance\n"))
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
	result, err := f.runner(t, agent).Start(t.Context(), f.input)
	if err != nil || result.Phase != journal.PhaseEscalated {
		t.Fatalf("start=%+v %v", result, err)
	}
	result, err = f.runner(t, agent).RerunStage(t.Context(), RerunStageInput{RunID: f.input.RunID, Machine: f.input.Machine, RepoRef: f.input.RepoRef, GooberDigest: f.input.GooberDigest, Stage: "work", Actor: "operator", InstructionAddendum: "Use the saved implementation", ExpectedTerminalSeq: terminalRunSequence(t, f.config.RunsDir, f.input.RunID)})
	if err != nil || result.Phase != journal.PhaseCompleted || calls != 2 {
		t.Fatalf("rerun=%+v %v calls=%d", result, err, calls)
	}
	f.assertParentUnchanged(t)
}
