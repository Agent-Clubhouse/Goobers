//go:build integration

package childpod

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationReconcileReplaysPublishedPlanAfterPartialImport(t *testing.T) {
	testdep.Require(t, "git")
	r := requestFixture()
	r.Eligible = retainedFixture().Input.Eligible
	host := t.TempDir()
	podTestGit(t, host, "init", "--initial-branch=main")
	file := filepath.Join(host, "source.txt")
	if err := os.WriteFile(file, []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	podTestGit(t, host, "add", ".")
	podTestGit(t, host, "commit", "-m", "original")
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
	writer, err := journal.Create(t.TempDir(), r.Identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	reader, err := journal.OpenReadOnly(writer.Dir())
	if err != nil {
		t.Fatal(err)
	}
	e := Executor{Blobs: blobs, Surrenders: plane, Recorder: writer, RecoveryReader: reader}
	var retained RetainedAttempt
	e.KeepAttempt = func(_ context.Context, value RetainedAttempt) error {
		retained = value
		_, err := RecordRetainedAttempt(writer, value)
		return err
	}
	calls := 0
	e.Dispatcher = retainedDispatch{dispatchFunc: func(ctx context.Context, a dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
		calls++
		data, err := blobs.Get(ctx, a.ChildExecutionDigest)
		if err != nil {
			t.Fatal(err)
		}
		c, err := DecodeContract(data, a.ChildExecutionDigest)
		if err != nil {
			t.Fatal(err)
		}
		pod := t.TempDir()
		if err = Materialize(ctx, pod, *c.Workspace); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(pod, "source.txt"), []byte("returned\n"), 0600); err != nil {
			t.Fatal(err)
		}
		returned, _, err := CaptureCarrier(ctx, pod, key, r.Identity.RunID, r.StartedAt, c.Workspace.Snapshot.Policy)
		if err != nil {
			t.Fatal(err)
		}
		data, _ = json.Marshal(Output{Version: 1, ContractDigest: a.ChildExecutionDigest, Workspace: &returned})
		digest := journal.Digest(data)
		if err = blobs.Put(ctx, digest, data); err != nil {
			t.Fatal(err)
		}
		data, _ = json.Marshal(dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, ChildWorkspaceDigest: digest})
		if err = plane.Put(ctx, a.RunID, a.Stage, a.PodAttempt, data); err != nil {
			t.Fatal(err)
		}
		return dispatcher.Report{ChildCreateAttempted: true, ChildPodUID: "exact", WorkspaceWritersStopped: true, SurrenderConfirmed: true}, nil
	}}
	ctx, err := credentials.WithChildCeiling(t.Context(), r.Ceiling)
	if err != nil {
		t.Fatal(err)
	}
	_, report, err := e.Execute(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash with the index in its after state and this file before.
	if err = os.WriteFile(file, []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r.Attempt = retained.Input.Attempt
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	out, err := e.Reconcile(canceled, r, retained, report)
	if err != nil || out.Result.Status != apiv1.ResultSuccess || calls != 1 {
		t.Fatal(out, err, calls)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "returned\n" {
		t.Fatal(string(data), err)
	}
	if _, err = e.Reconcile(t.Context(), r, retained, report); err != nil {
		t.Fatal("idempotent replay failed", err)
	}
	if got := podTestGit(t, host, "rev-parse", "HEAD"); got != fork.Record.SnapshotSHA {
		t.Fatal("changed ancestry", got)
	}
	if err = os.WriteFile(file, []byte("operator edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Reconcile(t.Context(), r, retained, report); !errors.Is(err, recovery.ErrWorkspaceChanged) {
		t.Fatal("intervening edit overwritten", err)
	}
	if data, _ := os.ReadFile(file); string(data) != "operator edit\n" {
		t.Fatal("operator edit lost")
	}
}
