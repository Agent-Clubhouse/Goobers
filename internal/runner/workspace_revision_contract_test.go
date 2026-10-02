package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacerevision"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func TestWorkspaceRevisionTypedResultAdmissionMatrix(t *testing.T) {
	for _, status := range []apiv1.ResultStatus{apiv1.ResultSuccess, apiv1.ResultFailure, apiv1.ResultBlocked, apiv1.ResultNoWork} {
		for _, producer := range []apiv1.TaskType{apiv1.TaskDeterministic, apiv1.TaskAgentic} {
			for _, malformed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/malformed=%t", producer, status, malformed), func(t *testing.T) {
					jr, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "matrix", Workflow: "matrix"}, nil)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = jr.Close() })
					lookups := 0
					r := &Runner{cfg: Config{ResolveRepositoryIdentity: func(ctx context.Context, repo apiv1.RepoRef) (providers.RepositoryMetadata, error) {
						lookups++
						return lookupRunnerRepositoryFixture(ctx, repo)
					}}}
					revision := runnerWorkspaceRevision("acme", "web", strings.Repeat("a", 40))
					if malformed {
						revision.CommitSHA = "short"
					}
					result := apiv1.ResultEnvelope{Status: status, WorkspaceRevision: revision}
					in := StartInput{RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}}
					err = r.prepareTaskResult(context.Background(), jr, apiv1.Task{Name: "produce", Type: producer}, &result, nil, &in, taskFrame{}, 1, "")
					want := ""
					if malformed {
						want = workspacerevision.CodeInvalid
					} else if producer == apiv1.TaskAgentic {
						want = workspacerevision.CodeUnauthorized
					}
					var coded *workspacerevision.Error
					if (err == nil) != (want == "") || (err != nil && (!errors.As(err, &coded) || coded.Code != want)) {
						t.Fatalf("admission error = %v, want %q", err, want)
					}
					accepted := want == "" && status == apiv1.ResultSuccess
					if (in.workspaceRevision != nil) != accepted || (lookups != 0) != accepted {
						t.Fatalf("accepted=%t authority=%+v lookups=%d", accepted, in.workspaceRevision, lookups)
					}
					if result.Status != status || (want == "" && !accepted && result.WorkspaceRevision != nil) {
						t.Fatalf("wrong normalized result: %+v", result)
					}
				})
			}
		}
	}
}

func scratchRevisionRunner(t *testing.T, det invoke.Deterministic) (*Runner, string) {
	t.Helper()
	manager, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runsDir := t.TempDir()
	r, err := New(Config{
		Worktrees: manager, RunsDir: runsDir, ScratchDir: t.TempDir(),
		NewDeterministic:          func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		ResolveRepositoryIdentity: lookupRunnerRepositoryFixture,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, runsDir
}

func TestRunnerWorkspaceRevisionDispatchRefusalStopsRetry(t *testing.T) {
	det := &sequencedDeterministic{failures: []error{
		&workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "invalid control"},
	}}
	r, runsDir := scratchRevisionRunner(t, det)
	def := workspaceRevisionResumeMachine(t).Def
	def.Spec.Tasks[0].Retry = &apiv1.RetryPolicy{MaxAttempts: 3}
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Start(context.Background(), StartInput{RunID: "refusal", Machine: machine, Gaggle: "acme-web"})
	var coded *workspacerevision.Error
	if !errors.As(err, &coded) || coded.Code != workspacerevision.CodeInvalid || det.calls != 1 {
		t.Fatalf("permanent error retried or recoded: calls=%d err=%v", det.calls, err)
	}
	refusals := 0
	for _, event := range readRunnerEvents(t, runsDir, "refusal") {
		if event.Type == journal.EventStageFinished {
			t.Fatal("permanent refusal was persisted as an accepted result")
		}
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == workspacerevision.CodeInvalid {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("revision refusal events=%d", refusals)
	}
}

func TestRunnerWorkspaceRevisionRecoveryReauthorizesRetainedIdentity(t *testing.T) {
	revision := runnerWorkspaceRevision("acme", "web", strings.Repeat("a", 40))
	det := &workspaceRevisionResumeDeterministic{revision: revision, received: make(chan apiv1.InvocationEnvelope, 1)}
	r, _ := scratchRevisionRunner(t, det)
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	events := []journal.Event{{Type: journal.EventStageFinished, Stage: "produce", Status: "success", WorkspaceRevision: revision}}
	in := StartInput{RepoRef: repo, Machine: workspaceRevisionResumeMachine(t)}
	if _, err := r.restoreWorkspaceRevision(context.Background(), in, events); err != nil {
		t.Fatal(err)
	}
	r.cfg.ResolveRepositoryIdentity = func(ctx context.Context, route apiv1.RepoRef) (providers.RepositoryMetadata, error) {
		metadata, err := lookupRunnerRepositoryFixture(ctx, route)
		metadata.Repository.Name = "changed"
		return metadata, err
	}
	if restored, err := r.restoreWorkspaceRevision(context.Background(), in, events); err == nil || restored.workspaceRevision != nil {
		t.Fatalf("recovery reused stale authorization: %+v %v", restored.workspaceRevision, err)
	}
}
