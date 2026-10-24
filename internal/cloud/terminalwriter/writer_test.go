package terminalwriter

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

var (
	workspaceRepo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "service"}
	backlogRepo   = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "backlog"}
	otherRepo     = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "other"}
)

type fakeRuns map[string]Binding

func (r fakeRuns) Bind(_ context.Context, runID string) (Binding, error) {
	bound, ok := r[runID]
	if !ok {
		return Binding{}, providerwriter.ErrRepositoryNotPinned
	}
	return bound, nil
}

func binding(runID string) Binding {
	return Binding{RunID: runID, Gaggle: "g", Generation: "gen", Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "service"}, Workspace: workspaceRepo, WorkItems: backlogRepo}
}

type call struct {
	op         string
	repo       providers.RepositoryRef
	capability string
}

type fakeBackend struct {
	kind     providers.ProviderKind
	calls    *[]call
	cap      string
	labels   map[string][]string
	err      error
	released *providers.ClaimWorkItemRequest
}

func (b *fakeBackend) record(op string, repo providers.RepositoryRef) {
	*b.calls = append(*b.calls, call{op: op, repo: repo, capability: b.cap})
}
func (b *fakeBackend) Kind() providers.ProviderKind { return b.kind }
func (b *fakeBackend) GetWorkItem(_ context.Context, repo providers.RepositoryRef, id string) (providers.WorkItem, error) {
	b.record("get", repo)
	return providers.WorkItem{ID: id}, b.err
}
func (b *fakeBackend) ListComments(_ context.Context, repo providers.RepositoryRef, _ string) ([]providers.Comment, error) {
	b.record("comments", repo)
	return nil, b.err
}
func (b *fakeBackend) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	b.record("update", req.Repository)
	return providers.WorkItem{}, b.err
}
func (b *fakeBackend) UpdateComment(_ context.Context, repo providers.RepositoryRef, _, _ string) error {
	b.record("update-comment", repo)
	return b.err
}
func (b *fakeBackend) PullLabels(_ context.Context, repo providers.RepositoryRef, pullID string) ([]string, error) {
	b.record("pull-labels", repo)
	return slices.Clone(b.labels[pullID]), b.err
}
func (b *fakeBackend) LabelPull(_ context.Context, repo providers.RepositoryRef, pullID string, add, remove []string) error {
	b.record("label-pull:"+strings.Join(add, ",")+"-"+strings.Join(remove, ","), repo)
	if b.err != nil {
		return b.err
	}
	labels := slices.DeleteFunc(b.labels[pullID], func(name string) bool { return slices.Contains(remove, name) })
	b.labels[pullID] = append(labels, add...)
	return nil
}
func (b *fakeBackend) DeleteBranch(_ context.Context, req providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
	b.record("delete-branch:"+req.Name, req.Repository)
	return providers.DeleteBranchResult{}, b.err
}
func (b *fakeBackend) ReleaseWorkItemClaim(_ context.Context, req providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
	b.record("release", req.Repository)
	b.released = &req
	return providers.WorkItem{}, b.err
}

type fakeBackends struct{ backend *fakeBackend }

func (f fakeBackends) For(_ context.Context, _ Binding, _ providers.RepositoryRef, capability string) (Backend, error) {
	f.backend.cap = capability
	return f.backend, nil
}

func newTestWriter(t *testing.T, backend *fakeBackend, runs fakeRuns) (*Writer, *journal.RegistryScrubber) {
	t.Helper()
	if backend.calls == nil {
		backend.calls = &[]call{}
	}
	if backend.kind == "" {
		backend.kind = providers.ProviderGitHub
	}
	if backend.labels == nil {
		backend.labels = map[string][]string{}
	}
	registry, scrubber := journal.DefaultScrubber()
	w, err := New(runs, fakeBackends{backend}, scrubber)
	if err != nil {
		t.Fatal(err)
	}
	return w, registry
}

func TestDeleteBranchKeepsABranchAnOpenPullRequestUses(t *testing.T) {
	inUse := &providers.BranchInUseError{Name: "goobers/run-1", PullID: 12}
	backend := &fakeBackend{err: inUse}
	w, _ := newTestWriter(t, backend, fakeRuns{"run-1": binding("run-1")})
	err := w.DeleteBranch(t.Context(), "run-1", "goobers/run-1")
	var kept *providers.BranchInUseError
	if !errors.As(err, &kept) || kept != inUse {
		t.Fatalf("a kept branch must reach the caller as the provider's *BranchInUseError, got %v", err)
	}
	if providerwriter.Permanent(err) {
		t.Fatal("a kept branch is a decision, not a permanent failure")
	}
	if got := *backend.calls; len(got) != 1 || got[0].op != "delete-branch:goobers/run-1" || got[0].repo != workspaceRepo || got[0].capability != string(capability.GitHubBranchDelete) {
		t.Fatalf("delete targeted %+v, want the run's workspace with branch-delete only", got)
	}
}

func TestPullRequestLabelIsIdempotentForTheRun(t *testing.T) {
	backend := &fakeBackend{}
	w, _ := newTestWriter(t, backend, fakeRuns{"run-1": binding("run-1")})
	ref := providerwriter.ItemRef{RunID: "run-1", Repo: workspaceRepo, ItemID: "12"}
	label := providers.UpdateWorkItemRequest{AddLabels: []string{"goobers:run-aborted"}}
	for range 2 {
		if err := w.Update(t.Context(), ref, label); err != nil {
			t.Fatal(err)
		}
	}
	var writes []string
	for _, c := range *backend.calls {
		if strings.HasPrefix(c.op, "label-pull") {
			writes = append(writes, c.op)
			if c.capability != string(capability.GitHubPRWrite) || c.repo != workspaceRepo {
				t.Fatalf("label written as %+v", c)
			}
		}
	}
	if !slices.Equal(writes, []string{"label-pull:goobers:run-aborted-"}) {
		t.Fatalf("a retried label must not write again: %v", writes)
	}
	// The run, not the caller, decides the repository: another run's
	// workspace is refused before any provider call.
	before := len(*backend.calls)
	err := w.Update(t.Context(), providerwriter.ItemRef{RunID: "run-1", Repo: otherRepo, ItemID: "12"}, label)
	if !errors.Is(err, providerwriter.ErrRepositoryNotPinned) || !providerwriter.Permanent(err) || len(*backend.calls) != before {
		t.Fatalf("unpinned label: err=%v calls=%d", err, len(*backend.calls)-before)
	}
}

func TestUpdateRefusesAnUnkeyedCommentBeforeAnyProviderCall(t *testing.T) {
	backend := &fakeBackend{}
	w, _ := newTestWriter(t, backend, fakeRuns{"run-1": binding("run-1")})
	err := w.Update(t.Context(), providerwriter.ItemRef{RunID: "run-1", Repo: backlogRepo, ItemID: "7"}, providers.UpdateWorkItemRequest{Comment: "failed"})
	if !providerwriter.Permanent(err) || len(*backend.calls) != 0 {
		t.Fatalf("unkeyed comment: err=%v calls=%v", err, *backend.calls)
	}
}

func TestADOHasNoIdempotentCommentWrites(t *testing.T) {
	backend := &fakeBackend{kind: providers.ProviderADO}
	w, _ := newTestWriter(t, backend, fakeRuns{"run-1": binding("run-1")})
	ref := providerwriter.ItemRef{RunID: "run-1", Repo: backlogRepo, ItemID: "7"}
	for name, err := range map[string]error{
		"keyed comment":  w.Update(t.Context(), ref, providers.UpdateWorkItemRequest{Comment: "failed", IdempotencyKey: "run-1:failed"}),
		"comment update": w.UpdateComment(t.Context(), ref, "c1", "body"),
	} {
		if !errors.Is(err, providerwriter.ErrPermanent) {
			t.Errorf("%s on ADO: %v, want a permanent refusal", name, err)
		}
	}
	for _, c := range *backend.calls {
		if c.op == "update" || c.op == "update-comment" {
			t.Fatalf("ADO comment reached the provider: %+v", c)
		}
	}
}

func TestReleaseClaimMarkerIsScopedToTheRunsOwnClaim(t *testing.T) {
	backend := &fakeBackend{}
	w, _ := newTestWriter(t, backend, fakeRuns{"run-1": binding("run-1")})
	ref := providerwriter.ItemRef{RunID: "run-1", Repo: backlogRepo, ItemID: "7"}
	if err := w.ReleaseClaimMarker(t.Context(), ref, claimrelease.Reason{}); err != nil {
		t.Fatal(err)
	}
	if req := backend.released; req == nil || req.RunID != "run-1" || req.ExpectedClaimRunID != "run-1" || req.LedgerAuthorized || req.Repository != backlogRepo || req.ID != "7" {
		t.Fatalf("release request = %+v", backend.released)
	}
	backend.err = &providers.ClaimHeldError{Holder: "run-2", Expected: "run-1"}
	if err := w.ReleaseClaimMarker(t.Context(), ref, claimrelease.Reason{}); !providerwriter.Permanent(err) {
		t.Fatalf("a successor's claim must be a permanent refusal: %v", err)
	}
	// Work items live in the backlog, not the workspace.
	if err := w.ReleaseClaimMarker(t.Context(), providerwriter.ItemRef{RunID: "run-1", Repo: workspaceRepo, ItemID: "7"}, claimrelease.Reason{}); !errors.Is(err, providerwriter.ErrRepositoryNotPinned) {
		t.Fatalf("workspace claim release: %v", err)
	}
}

func TestProviderErrorsAreScrubbedAndStayRetryable(t *testing.T) {
	backend := &fakeBackend{err: errors.New("401 for token ghs_secretvalue")}
	w, registry := newTestWriter(t, backend, fakeRuns{"run-1": binding("run-1")})
	registry.Register([]byte("ghs_secretvalue"))
	_, err := w.WorkItem(t.Context(), providerwriter.ItemRef{RunID: "run-1", Repo: backlogRepo, ItemID: "7"})
	if err == nil || strings.Contains(err.Error(), "ghs_secretvalue") {
		t.Fatalf("provider error not scrubbed: %v", err)
	}
	if providerwriter.Permanent(err) {
		t.Fatalf("a provider failure must stay retryable: %v", err)
	}
	if got := (*backend.calls)[0].capability; got != string(capability.GitHubIssuesRead) {
		t.Fatalf("read used %s", got)
	}
}
