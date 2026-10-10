//go:build integration

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
)

type interruptedJoinRecorder struct {
	parallelworkspace.Recorder
	phase string
	after bool
	drop  bool
}

var errJoinInterrupted = errors.New("test: interrupted join journal boundary")

func (r interruptedJoinRecorder) Append(event journal.Event) error {
	if event.Runner["kind"] == spec.JoinKind && event.Runner["phase"] == r.phase {
		if r.drop {
			return nil
		}
		if r.after {
			if err := r.Recorder.Append(event); err != nil {
				return err
			}
		}
		return errJoinInterrupted
	}
	return r.Recorder.Append(event)
}

func TestIntegrationParallelForkJoinRefusesChangedWorkAndMissingJournal(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"conflict", "changed-file", "dropped-intent", "branch-recorder"} {
		t.Run(mode, func(t *testing.T) {
			f := prepareForkJoin(t, mode == "conflict")
			switch mode {
			case "conflict":
				if err := f.service.Join(t.Context(), f.run, f.request); !errors.Is(err, recovery.ErrIncompatibleSnapshot) {
					t.Fatal("conflicting branch results accepted", err)
				}
			case "changed-file":
				rec := interruptedJoinRecorder{Recorder: f.run, phase: "ready", after: true}
				if err := f.service.Join(t.Context(), rec, f.request); !errors.Is(err, errJoinInterrupted) {
					t.Fatal(err)
				}
				childWorkspaceWrite(t, f.root.Path, "a.txt", []byte("intervening human edit\n"))
				if err := f.service.RecoverJoins(t.Context(), f.run); !errors.Is(err, recovery.ErrWorkspaceChanged) {
					t.Fatal("recovery overwrote a third state", err)
				}
				childWorkspaceRead(t, f.root.Path, "a.txt", []byte("intervening human edit\n"))
				assertPendingJoinCleanup(t, f, true)
				if err := os.Remove(filepath.Join(f.root.Path, "a.txt")); err != nil {
					t.Fatal(err)
				}
				if err := f.service.RecoverJoins(t.Context(), f.run); err != nil {
					t.Fatal("recovery after explicit restoration of expected state", err)
				}
				return
			default:
				refs := gitOutput(t, f.root.Path, "show-ref")
				var recorder parallelworkspace.Recorder = interruptedJoinRecorder{Recorder: f.run, phase: "prepared", drop: true}
				if mode == "branch-recorder" {
					var err error
					recorder, err = OwnedBranchRecorder(f.run, 1)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := f.service.Join(t.Context(), recorder, f.request); err == nil {
					t.Fatal("join proceeded without durable root intent")
				}
				if gitOutput(t, f.root.Path, "show-ref") != refs {
					t.Fatal("join pinned before its root intent was acknowledged")
				}
			}
			reader, err := journal.OpenReadOnly(f.run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			seed, err := parallelworkspace.ReadSource(reader, f.request.Seed, f.request.Parallel, f.request.Sequence)
			if err != nil {
				t.Fatal(err)
			}
			if err := recovery.CheckChildSnapshotCurrent(t.Context(), f.root.Path, seed); err != nil {
				t.Fatal("refused join modified root", err)
			}
		})
	}
}

type forkJoinFixture struct {
	r        *Runner
	run      *journal.Run
	service  parallelworkspace.Service
	request  spec.JoinRequest
	root     *worktree.Worktree
	in       StartInput
	parallel apiv1.Parallel
	runtime  parallelRuntime
	outcomes []*parallelBranchResult
}

func prepareForkJoin(t *testing.T, conflict bool) forkJoinFixture {
	t.Helper()
	return prepareForkJoinStatus(t, conflict, journal.BranchSucceeded)
}

func prepareForkJoinStatus(t *testing.T, conflict bool, second journal.BranchStatus) forkJoinFixture {
	t.Helper()
	r, run, frame, _, _ := prepareChildWaitRuntime(t)
	service := parallelworkspace.Service{Worktrees: r.cfg.Worktrees, CloneURL: r.cfg.RepoCloneURL, Policy: func(path string) (recovery.SnapshotPolicy, error) {
		childWorkspaceWrite(t, path, "staging.txt", []byte("staged\n"))
		runGit(t, path, "add", "staging.txt")
		childWorkspaceWrite(t, path, "staging.txt", []byte("working\n"))
		return recovery.SnapshotPolicy{}, nil
	}}
	r.cfg.PrepareParentForkSource = func(ctx context.Context, rec OwnedJournalRecorder, request spec.Request, previous *spec.Source) (spec.Source, error) {
		return service.Prepare(ctx, rec, request, previous)
	}
	r.cfg.PrepareParentForkResult = func(ctx context.Context, rec OwnedJournalRecorder, request spec.ResultRequest, previous *spec.Source) (spec.Source, error) {
		return service.Result(ctx, rec, request, previous)
	}
	r.cfg.JoinParentFork = func(ctx context.Context, rec OwnedJournalRecorder, request spec.JoinRequest) error {
		return service.Join(ctx, rec, request)
	}
	parallel := apiv1.Parallel{Name: "fan", Branches: []apiv1.Branch{{Name: "a", Start: frame.t.Name}, {Name: "b", Start: frame.t.Name}}}
	par := newParallelExec(parallel)
	if err := run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: parallel.Name, Completeness: par.completeness()}); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	forks, err := r.prepareParallelForks(t.Context(), run, frame.in, parallel, "", events)
	if err != nil {
		t.Fatal(err)
	}
	url, err := r.cfg.RepoCloneURL(frame.in.RepoRef)
	if err != nil {
		t.Fatal(err)
	}
	runtime := parallelRuntime{forks: forks}
	outcomes := []*parallelBranchResult{{index: 0, status: journal.BranchSucceeded}, {index: 1, status: second, failed: second == journal.BranchFailed}}
	for index, owner := range forks {
		workspace, err := r.cfg.Worktrees.AdoptHeldStage(t.Context(), url, owner)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			childWorkspaceWrite(t, workspace.Path, "a.txt", []byte("a\n"))
			childWorkspaceWrite(t, workspace.Path, "shared.txt", []byte("same\n"))
		} else {
			childWorkspaceWrite(t, workspace.Path, "b.bin", []byte{0, 255})
			value := "same\n"
			if conflict {
				value = "different\n"
			}
			childWorkspaceWrite(t, workspace.Path, "shared.txt", []byte(value))
		}
		if err := r.settleParallelRuntimeBranch(t.Context(), run, frame.in, par, runtime, *outcomes[index]); err != nil {
			t.Fatal(err)
		}
	}
	request, err := parallelForkJoinRequest(reader, frame.in, parallel, outcomes)
	if err != nil {
		t.Fatal(err)
	}
	root, err := r.cfg.Worktrees.AdoptHeldStage(t.Context(), url, request.Root)
	if err != nil {
		t.Fatal(err)
	}
	return forkJoinFixture{r: r, run: run, service: service, request: request, root: root, in: frame.in, parallel: parallel, runtime: runtime, outcomes: outcomes}
}

func TestIntegrationParallelForkJoinDurableInterruption(t *testing.T) {
	testdep.Require(t, "git")
	for _, checkpoint := range []string{"prepared", "ready", "partial", "applied"} {
		t.Run(checkpoint, func(t *testing.T) {
			f := prepareForkJoin(t, false)
			head := gitOutput(t, f.root.Path, "rev-parse", "HEAD")
			recorder := interruptedJoinRecorder{Recorder: f.run, phase: checkpoint}
			if checkpoint == "partial" {
				recorder.phase, recorder.after = "ready", true
			}
			if err := f.service.Join(t.Context(), recorder, f.request); !errors.Is(err, errJoinInterrupted) {
				t.Fatal("did not reach durable interruption", err)
			}
			if checkpoint == "partial" {
				childWorkspaceWrite(t, f.root.Path, "a.txt", []byte("a\n"))
			}
			assertPendingJoinCleanup(t, f, checkpoint != "prepared")
			if err := f.run.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, _, err := journal.Recover(f.run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			f.run = reopened
			// Root runner resume uses the same settled result receipts, even when
			// files were partially applied before the crash.
			if err := f.r.joinParallelForks(t.Context(), reopened, f.in, f.parallel, f.runtime, f.outcomes, true); err != nil {
				t.Fatal("resume exact merge plan", err)
			}
			assertForkJoinApplied(t, f, head)
			childWorkspaceWrite(t, f.root.Path, "a.txt", []byte("later join-agent edit\n"))
			if err := f.service.Join(t.Context(), reopened, f.request); err != nil {
				t.Fatal("acknowledged join was revalidated against later work", err)
			}
			childWorkspaceRead(t, f.root.Path, "a.txt", []byte("later join-agent edit\n"))
		})
	}
}

func assertPendingJoinCleanup(t *testing.T, f forkJoinFixture, pending bool) {
	t.Helper()
	reader, err := journal.OpenReadOnly(f.run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.root.HeldCleanupTarget(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ParentCleanupWorkspace(reader, target)
	if pending && !errors.Is(err, ErrParentReturnPending) {
		t.Fatal("unfinished application did not fence cleanup", err)
	}
}

func assertForkJoinApplied(t *testing.T, f forkJoinFixture, head string) {
	t.Helper()
	childWorkspaceRead(t, f.root.Path, "a.txt", []byte("a\n"))
	childWorkspaceRead(t, f.root.Path, "b.bin", []byte{0, 255})
	childWorkspaceRead(t, f.root.Path, "shared.txt", []byte("same\n"))
	childWorkspaceRead(t, f.root.Path, "staging.txt", []byte("working\n"))
	if gitOutput(t, f.root.Path, "rev-parse", "HEAD") != head || gitOutput(t, f.root.Path, "show", ":staging.txt") != "staged" {
		t.Fatal("merge changed HEAD or unrelated staging")
	}
	reader, err := journal.OpenReadOnly(f.run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := spec.PendingJoins(reader)
	if err != nil || len(pending) != 0 {
		t.Fatal("application did not acknowledge durable completion", err)
	}
}

func TestIntegrationParallelForkJoinRetainsFailedBranchWithoutApplyingIt(t *testing.T) {
	testdep.Require(t, "git")
	f := prepareForkJoinStatus(t, true, journal.BranchFailed)
	if err := f.r.joinParallelForks(t.Context(), f.run, f.in, f.parallel, f.runtime, f.outcomes, true); err != nil {
		t.Fatal(err)
	}
	childWorkspaceRead(t, f.root.Path, "a.txt", []byte("a\n"))
	childWorkspaceRead(t, f.root.Path, "shared.txt", []byte("same\n"))
	if _, err := os.Stat(filepath.Join(f.root.Path, "b.bin")); !os.IsNotExist(err) {
		t.Fatal("failed branch code was applied", err)
	}
	reader, err := journal.OpenReadOnly(f.run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	failed := f.request.Results[1]
	request := spec.ResultRequest{Request: f.request.Request, Plan: f.request.Plan, Seed: f.request.Seed, Branch: failed.Branch, Status: failed.Status, Custody: failed.Custody}
	if _, err := parallelworkspace.ReadResult(reader, request, failed.Source); err != nil {
		t.Fatal("failed branch output was not retained", err)
	}
}
