//go:build integration

package childpod

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func podTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, data)
	}
	return strings.TrimSpace(string(data))
}

func TestIntegrationExecutorImportsVerifiedTreeAndPreservesFork(t *testing.T) {
	testdep.Require(t, "git")
	testExecutorTreeReturn(t, false)
}

func TestIntegrationExecutorCancellationImportsSurrenderedTree(t *testing.T) {
	testdep.Require(t, "git")
	testExecutorTreeReturn(t, true)
}

func testExecutorTreeReturn(t *testing.T, canceled bool) {
	t.Helper()
	r := requestFixture()
	host := t.TempDir()
	podTestGit(t, host, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(host, "source.txt"), []byte("fork\n"), 0600); err != nil {
		t.Fatal(err)
	}
	podTestGit(t, host, "add", ".")
	podTestGit(t, host, "commit", "-m", "source")
	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "repo"}).CanonicalKey()
	fork, err := recovery.CaptureChildSnapshot(t.Context(), host, key, r.Identity.RunID, r.StartedAt, r.StartedAt.Add(24*time.Hour), recovery.SnapshotPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	podTestGit(t, host, "reset", "--hard", fork.Record.SnapshotSHA)
	r.Workspace = &WorkspaceInput{Path: host, Fork: fork}
	r.Attempt.Workspace = "repo"
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordFake{}
	executor := Executor{Blobs: blobs, Surrenders: plane, Recorder: recorder}
	callCtx, cancelCall := context.WithCancel(t.Context())
	defer cancelCall()
	executor.Dispatcher = dispatchFunc(func(ctx context.Context, a dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
		data, err := blobs.Get(ctx, a.ChildExecutionDigest)
		if err != nil {
			t.Fatal(err)
		}
		contract, err := DecodeContract(data, a.ChildExecutionDigest)
		if err != nil {
			t.Fatal(err)
		}
		pod := t.TempDir()
		if err = Materialize(ctx, pod, *contract.Workspace); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(pod, "source.txt"), []byte("agent edits\n"), 0600); err != nil {
			t.Fatal(err)
		}
		carrier, _, err := CaptureCarrier(ctx, pod, key, r.Identity.RunID, r.StartedAt, contract.Workspace.Snapshot.Policy)
		if err != nil {
			t.Fatal(err)
		}
		data, _ = json.Marshal(Output{Version: 1, ContractDigest: a.ChildExecutionDigest, Workspace: &carrier})
		digest := journal.Digest(data)
		if err = blobs.Put(ctx, digest, data); err != nil {
			t.Fatal(err)
		}
		data, _ = json.Marshal(dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, ChildWorkspaceDigest: digest})
		if err = plane.Put(ctx, a.RunID, a.Stage, a.PodAttempt, data); err != nil {
			t.Fatal(err)
		}
		var dispatchErr error
		if canceled {
			cancelCall()
			dispatchErr = context.Canceled
		}
		return dispatcher.Report{ChildCreateAttempted: true, ChildPodUID: "exact", WorkspaceWritersStopped: true, SurrenderConfirmed: true}, dispatchErr
	})
	ctx, err := credentials.WithChildCeiling(callCtx, r.Ceiling)
	if err != nil {
		t.Fatal(err)
	}
	ctx, proof := invoke.WithWorkspaceQuiescence(ctx)
	out, report, err := executor.Execute(ctx, r)
	if canceled {
		if !errors.Is(err, context.Canceled) || out.Result.Status != apiv1.ResultSuccess || !report.SurrenderConfirmed {
			t.Fatal(out, report, err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if err = proof.Verify(); err != nil {
		t.Fatal(err)
	}
	if got := podTestGit(t, host, "rev-parse", "HEAD"); got != fork.Record.SnapshotSHA {
		t.Fatal("transport replaced real ancestry")
	}
	if data, err := os.ReadFile(filepath.Join(host, "source.txt")); err != nil || string(data) != "agent edits\n" {
		t.Fatal("result tree missing", err)
	}
	if len(recorder.data) != 4 {
		t.Fatal("input/output/plan/pod proof custody missing", len(recorder.data))
	}
}
