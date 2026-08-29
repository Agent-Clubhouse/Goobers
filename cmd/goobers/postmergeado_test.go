package main

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/goobers/goobers/providers"
)

// fakeADOWorkItemCloser records every backlog call the ADO post-merge close
// makes. It implements adoWorkItemCloser (the base-Provider subset the close
// routes through), so the test never touches a concrete *GitHubProvider and can
// assert exactly which work-item id was mutated and against which repo.
type fakeADOWorkItemCloser struct {
	item     providers.WorkItem
	comments []providers.Comment

	statusReqs  []providers.UpdateWorkItemStatusRequest
	commentReqs []providers.UpdateWorkItemRequest
	getIDs      []string
	getRepos    []providers.RepositoryRef
}

func (f *fakeADOWorkItemCloser) GetWorkItem(_ context.Context, repo providers.RepositoryRef, id string) (providers.WorkItem, error) {
	f.getIDs = append(f.getIDs, id)
	f.getRepos = append(f.getRepos, repo)
	return f.item, nil
}

func (f *fakeADOWorkItemCloser) ListComments(_ context.Context, _ providers.RepositoryRef, _ string) ([]providers.Comment, error) {
	return f.comments, nil
}

func (f *fakeADOWorkItemCloser) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.commentReqs = append(f.commentReqs, req)
	return providers.WorkItem{}, nil
}

func (f *fakeADOWorkItemCloser) UpdateWorkItemStatus(_ context.Context, req providers.UpdateWorkItemStatusRequest) (providers.WorkItem, error) {
	f.statusReqs = append(f.statusReqs, req)
	return providers.WorkItem{}, nil
}

// backlogRef is a distinguishing ADO backlog RepositoryRef: its Project differs
// from any code-repo project so the test proves the close targets the backlog
// project passed in, never a routed code repo.
var backlogRef = providers.RepositoryRef{
	Provider: providers.ProviderADO,
	Owner:    "example-org",
	Project:  "Example Backlog",
	Name:     "example-repo",
}

// TestPerformPostMergeADOClosesReferencedWorkItem is the load-bearing assertion:
// on ADO, post-merge resolves the work-item id from the merged PR body's
// "Fixes #N" (N is the work-item id, not a PR number), marks it done against the
// backlog repo, and leaves the dedupe comment.
func TestPerformPostMergeADOClosesReferencedWorkItem(t *testing.T) {
	closer := &fakeADOWorkItemCloser{
		item: providers.WorkItem{ID: "1456", State: "open"},
	}
	poll := providers.PullRequestPollResult{
		Number: 359,
		Body:   "Implements the widget.\n\nFixes #1456",
	}
	var stdout, stderr bytes.Buffer

	errs := performPostMergeADO(context.Background(), closer, backlogRef, poll, "359", &stdout, &stderr)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}

	if len(closer.statusReqs) != 1 {
		t.Fatalf("UpdateWorkItemStatus calls = %d, want 1 (%+v)", len(closer.statusReqs), closer.statusReqs)
	}
	got := closer.statusReqs[0]
	if got.ID != "1456" {
		t.Errorf("closed work item id = %q, want 1456 (the body's Fixes #N, not the PR number)", got.ID)
	}
	if got.Status != providers.WorkItemStatusDone {
		t.Errorf("status = %q, want %q", got.Status, providers.WorkItemStatusDone)
	}
	if got.Repository.Project != backlogRef.Project {
		t.Errorf("status write targeted project %q, want backlog project %q", got.Repository.Project, backlogRef.Project)
	}
	// The id read must also be the work-item id (1456), never the PR number 359.
	for _, id := range closer.getIDs {
		if id == "359" {
			t.Fatalf("GetWorkItem was called with the PR number 359 — PR-as-work-item wrong-object hazard")
		}
	}
	if len(closer.commentReqs) != 1 {
		t.Fatalf("dedupe comment writes = %d, want 1", len(closer.commentReqs))
	}
	if want := "Merged in pull request #359."; closer.commentReqs[0].Comment != want {
		t.Errorf("comment = %q, want %q", closer.commentReqs[0].Comment, want)
	}
	if closer.commentReqs[0].Repository.Project != backlogRef.Project {
		t.Errorf("comment targeted project %q, want backlog project %q", closer.commentReqs[0].Repository.Project, backlogRef.Project)
	}
	if want := "closed 1 work item"; !bytes.Contains(stdout.Bytes(), []byte(want)) {
		t.Errorf("stdout = %q, want a mention of %q", stdout.String(), want)
	}
}

// TestPerformPostMergeADOIdempotentWhenAlreadyDone proves the close is a no-op
// on a re-run: an already-completed work item (State closed + goobers/status:done
// label) gets no status write, and an already-present dedupe comment is not
// re-posted.
func TestPerformPostMergeADOIdempotentWhenAlreadyDone(t *testing.T) {
	closer := &fakeADOWorkItemCloser{
		item: providers.WorkItem{
			ID:     "1456",
			State:  "closed",
			Labels: []string{"goobers/status:" + string(providers.WorkItemStatusDone)},
		},
		comments: []providers.Comment{{Body: "Merged in pull request #359."}},
	}
	poll := providers.PullRequestPollResult{Number: 359, Body: "Fixes #1456"}
	var stdout, stderr bytes.Buffer

	errs := performPostMergeADO(context.Background(), closer, backlogRef, poll, "359", &stdout, &stderr)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if len(closer.statusReqs) != 0 {
		t.Errorf("UpdateWorkItemStatus calls = %d, want 0 (already done)", len(closer.statusReqs))
	}
	if len(closer.commentReqs) != 0 {
		t.Errorf("dedupe comment writes = %d, want 0 (comment already present)", len(closer.commentReqs))
	}
}

// TestPerformPostMergeADONoReferenceIsNotAnError proves a merged PR whose body
// closes no work item is a clean exit, not an error — matching the GitHub
// close-out contract (not every merged PR closes a backlog item).
func TestPerformPostMergeADONoReferenceIsNotAnError(t *testing.T) {
	closer := &fakeADOWorkItemCloser{item: providers.WorkItem{ID: "1"}}
	poll := providers.PullRequestPollResult{Number: 359, Body: "A manual fix, no backlog work item."}
	var stdout, stderr bytes.Buffer

	errs := performPostMergeADO(context.Background(), closer, backlogRef, poll, "359", &stdout, &stderr)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if len(closer.getIDs)+len(closer.statusReqs)+len(closer.commentReqs) != 0 {
		t.Errorf("no work item referenced but backlog was touched: gets=%v statuses=%d comments=%d",
			closer.getIDs, len(closer.statusReqs), len(closer.commentReqs))
	}
	if want := "closed 0 work item"; !bytes.Contains(stdout.Bytes(), []byte(want)) {
		t.Errorf("stdout = %q, want a mention of %q", stdout.String(), want)
	}
}

// TestCloseReferencedWorkItemsADOMultipleAndError proves each distinct closing
// ref is marked done, and a per-item failure is collected (not fatal to the
// others) — mirroring closeReferencedIssues' best-effort posture.
func TestCloseReferencedWorkItemsADOSurfacesPerItemError(t *testing.T) {
	closer := &erroringCloser{failID: "1456"}
	closed, errs := closeReferencedWorkItemsADO(context.Background(), closer, backlogRef, "Fixes #1456 and closes #1457", "359")
	if len(closed) != 1 || closed[0] != "1457" {
		t.Fatalf("closed = %v, want [1457] (1456 failed)", closed)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly one (for #1456)", errs)
	}
}

type erroringCloser struct {
	failID string
}

func (e *erroringCloser) GetWorkItem(_ context.Context, _ providers.RepositoryRef, id string) (providers.WorkItem, error) {
	if id == e.failID {
		return providers.WorkItem{}, fmt.Errorf("boom")
	}
	return providers.WorkItem{ID: id, State: "open"}, nil
}
func (e *erroringCloser) ListComments(_ context.Context, _ providers.RepositoryRef, _ string) ([]providers.Comment, error) {
	return nil, nil
}
func (e *erroringCloser) UpdateWorkItem(_ context.Context, _ providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	return providers.WorkItem{}, nil
}
func (e *erroringCloser) UpdateWorkItemStatus(_ context.Context, _ providers.UpdateWorkItemStatusRequest) (providers.WorkItem, error) {
	return providers.WorkItem{}, nil
}
