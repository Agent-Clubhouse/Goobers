//go:build integration && (darwin || linux)

package childpod

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationLocalExecutorReturnsRealProcessTreeAndFreshCheckout(t *testing.T) {
	testdep.Require(t, "git", "sh")
	r := requestFixture()
	host := t.TempDir()
	podTestGit(t, host, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(host, "source.txt"), []byte("fork\n"), 0600); err != nil {
		t.Fatal(err)
	}
	podTestGit(t, host, "add", ".")
	podTestGit(t, host, "commit", "-m", "source")
	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "example", Name: "repo"}).CanonicalKey()
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
	executor := LocalExecutor{Root: t.TempDir(), Blobs: blobs, Recorder: &recordFake{}, BeforeStart: func(context.Context, Request, Contract, string) error { return nil }, Returned: func(context.Context, Request, journal.Ref) error { return nil }}
	var previous string
	for attempt := 1; attempt <= 2; attempt++ {
		r.Attempt.Number = attempt
		r.Attempt.PodAttempt++
		ctx, err := credentials.WithChildCeiling(t.Context(), r.Ceiling)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(ctx)
		ctx, proof := invoke.WithWorkspaceQuiescence(ctx)
		_, err = executor.Execute(ctx, r, func(ctx context.Context, path string) (dispatcher.SurrenderedResult, error) {
			if path == host || path == previous {
				t.Fatal("physical attempt reused managed or previous checkout")
			}
			previous = path
			result, runErr := (harness.ExecProcessRunner{}).Run(ctx, harness.ProcessRequest{Command: []string{"/bin/sh", "-c", "printf 'returned\\n' >> source.txt"}, Dir: path, Env: credentials.TrustedLocalBaseEnvironment(ctx, os.Environ()), Timeout: 5 * time.Second})
			if runErr != nil || result.ExitCode != 0 {
				t.Fatalf("local process: %v %+v", runErr, result)
			}
			cancel()
			return dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, context.Canceled
		})
		cancel()
		if !errors.Is(err, context.Canceled) || proof.Verify() != nil {
			t.Fatalf("return/custody: %v %v", err, proof.Verify())
		}
		expected := "fork\nreturned\n"
		if attempt == 2 {
			expected += "returned\n"
		}
		data, err := os.ReadFile(filepath.Join(host, "source.txt"))
		if err != nil || string(data) != expected {
			t.Fatalf("verified return=%q err=%v", data, err)
		}
		if _, err := recovery.CaptureChildResult(t.Context(), host, r.Identity.RunID, fork, r.StartedAt, r.StartedAt.Add(24*time.Hour)); err != nil {
			t.Fatalf("return lost original fork lineage: %v", err)
		}
	}
}
