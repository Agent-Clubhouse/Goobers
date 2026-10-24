package terminalwriter

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

// Backend is one repository's provider surface, already authenticated for a
// single capability. Implementations do not scrub; the Writer does.
type Backend interface {
	Kind() providers.ProviderKind
	GetWorkItem(ctx context.Context, repo providers.RepositoryRef, id string) (providers.WorkItem, error)
	ListComments(ctx context.Context, repo providers.RepositoryRef, id string) ([]providers.Comment, error)
	UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error)
	UpdateComment(ctx context.Context, repo providers.RepositoryRef, commentID, body string) error
	PullLabels(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]string, error)
	LabelPull(ctx context.Context, repo providers.RepositoryRef, pullID string, add, remove []string) error
	// DeleteBranch keeps a branch an open pull request uses, returning
	// *providers.BranchInUseError.
	DeleteBranch(ctx context.Context, req providers.DeleteBranchRequest) (providers.DeleteBranchResult, error)
	ReleaseWorkItemClaim(ctx context.Context, req providers.ClaimWorkItemRequest) (providers.WorkItem, error)
}

// Backends builds the Backend for repo, a repository bound's run may write to,
// with credentials for one capability alone.
type Backends interface {
	For(ctx context.Context, bound Binding, repo providers.RepositoryRef, capability string) (Backend, error)
}

// ErrUnsupported is a permanent refusal: the bound repository's provider has
// no idempotent form of the operation.
var ErrUnsupported = errors.New("operation is not supported idempotently on this provider")

// Writer is the cloud providerwriter.Writer.
type Writer struct {
	runs     Runs
	backends Backends
	scrubber journal.Scrubber
}

var _ providerwriter.Writer = (*Writer)(nil)

// New returns a Writer binding runs through runs, writing through backends and
// scrubbing every provider error it returns with scrubber.
func New(runs Runs, backends Backends, scrubber journal.Scrubber) (*Writer, error) {
	if runs == nil || backends == nil || scrubber == nil {
		return nil, errors.New("terminal writer requires runs, backends and a scrubber")
	}
	return &Writer{runs: runs, backends: backends, scrubber: scrubber}, nil
}

// target is a bound run's repository and how it relates to the run.
type target struct {
	bound     Binding
	repo      providers.RepositoryRef
	workspace bool
	workItems bool
}

// resolve binds ref.RunID and checks ref.Repo is one of its repositories.
func (w *Writer) resolve(ctx context.Context, runID string, repo providers.RepositoryRef) (target, error) {
	bound, err := w.runs.Bind(ctx, runID)
	if err != nil {
		return target{}, err
	}
	t := target{bound: bound, repo: repo, workspace: sameRepository(repo, bound.Workspace), workItems: sameRepository(repo, bound.WorkItems)}
	if !t.workspace && !t.workItems {
		return target{}, fmt.Errorf("run %s may not write %s: %w", runID, repo.CanonicalKey(), providerwriter.ErrRepositoryNotPinned)
	}
	return t, nil
}

func (w *Writer) workItemTarget(ctx context.Context, ref providerwriter.ItemRef) (target, error) {
	t, err := w.resolve(ctx, ref.RunID, ref.Repo)
	if err == nil && !t.workItems {
		err = fmt.Errorf("run %s work items do not live in %s: %w", ref.RunID, ref.Repo.CanonicalKey(), providerwriter.ErrRepositoryNotPinned)
	}
	return t, err
}

// WorkItem reads ref's work item.
func (w *Writer) WorkItem(ctx context.Context, ref providerwriter.ItemRef) (providers.WorkItem, error) {
	t, err := w.workItemTarget(ctx, ref)
	if err != nil {
		return providers.WorkItem{}, w.fail("get work item", err)
	}
	backend, err := w.backends.For(ctx, t.bound, t.repo, string(capability.GitHubIssuesRead))
	if err != nil {
		return providers.WorkItem{}, w.fail("get work item", err)
	}
	item, err := backend.GetWorkItem(ctx, t.repo, ref.ItemID)
	return item, w.fail("get work item", err)
}

// Comments lists ref's comments.
func (w *Writer) Comments(ctx context.Context, ref providerwriter.ItemRef) ([]providers.Comment, error) {
	t, err := w.workItemTarget(ctx, ref)
	if err != nil {
		return nil, w.fail("list comments", err)
	}
	backend, err := w.backends.For(ctx, t.bound, t.repo, string(capability.GitHubIssuesRead))
	if err != nil {
		return nil, w.fail("list comments", err)
	}
	comments, err := backend.ListComments(ctx, t.repo, ref.ItemID)
	return comments, w.fail("list comments", err)
}

// Update applies req to ref. A labels-only Update of the run's workspace
// repository labels the pull request ref.ItemID; any other Update is a work
// item update.
func (w *Writer) Update(ctx context.Context, ref providerwriter.ItemRef, req providers.UpdateWorkItemRequest) error {
	req.Repository, req.ID = ref.Repo, ref.ItemID
	if req.Comment != "" && req.IdempotencyKey == "" {
		return w.fail("update work item", fmt.Errorf("a comment needs an idempotency key: %w", providerwriter.ErrPermanent))
	}
	t, err := w.resolve(ctx, ref.RunID, ref.Repo)
	if err != nil {
		return w.fail("update work item", err)
	}
	if t.workspace && labelsOnly(req) {
		return w.fail("label pull request", w.labelPull(ctx, t, ref.ItemID, req.AddLabels, req.RemoveLabels))
	}
	if !t.workItems {
		return w.fail("update work item", fmt.Errorf("run %s work items do not live in %s: %w", ref.RunID, ref.Repo.CanonicalKey(), providerwriter.ErrRepositoryNotPinned))
	}
	backend, err := w.backends.For(ctx, t.bound, t.repo, string(capability.GitHubIssuesWrite))
	if err != nil {
		return w.fail("update work item", err)
	}
	if req.Comment != "" && backend.Kind() == providers.ProviderADO {
		// Azure Boards ignores IdempotencyKey: a retried post would duplicate.
		return w.fail("update work item", fmt.Errorf("comment on %s: %w", backend.Kind(), ErrUnsupported))
	}
	_, err = backend.UpdateWorkItem(ctx, req)
	return w.fail("update work item", err)
}

// labelPull applies the label changes the pull request still needs, so a
// retried label after an ambiguous outcome writes nothing.
func (w *Writer) labelPull(ctx context.Context, t target, pullID string, add, remove []string) error {
	backend, err := w.backends.For(ctx, t.bound, t.bound.Workspace, prWrite(t.bound.Workspace))
	if err != nil {
		return err
	}
	current, err := backend.PullLabels(ctx, t.bound.Workspace, pullID)
	if err != nil {
		return err
	}
	var missing, present []string
	for _, name := range add {
		if !slices.Contains(current, name) && !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
	}
	for _, name := range remove {
		if slices.Contains(current, name) && !slices.Contains(present, name) {
			present = append(present, name)
		}
	}
	if len(missing) == 0 && len(present) == 0 {
		return nil
	}
	return backend.LabelPull(ctx, t.bound.Workspace, pullID, missing, present)
}

// UpdateComment replaces a comment's body.
func (w *Writer) UpdateComment(ctx context.Context, ref providerwriter.ItemRef, commentID, body string) error {
	t, err := w.workItemTarget(ctx, ref)
	if err != nil {
		return w.fail("update comment", err)
	}
	backend, err := w.backends.For(ctx, t.bound, t.repo, string(capability.GitHubIssuesWrite))
	if err != nil {
		return w.fail("update comment", err)
	}
	if backend.Kind() == providers.ProviderADO {
		return w.fail("update comment", fmt.Errorf("work item comment on %s: %w", backend.Kind(), ErrUnsupported))
	}
	return w.fail("update comment", backend.UpdateComment(ctx, t.repo, commentID, body))
}

// PullLabels lists the labels on the run's workspace pull request pullID.
func (w *Writer) PullLabels(ctx context.Context, runID, pullID string) ([]string, error) {
	bound, err := w.runs.Bind(ctx, runID)
	if err != nil {
		return nil, w.fail("list pull labels", err)
	}
	backend, err := w.backends.For(ctx, bound, bound.Workspace, string(capability.GitHubPRRead))
	if err != nil {
		return nil, w.fail("list pull labels", err)
	}
	labels, err := backend.PullLabels(ctx, bound.Workspace, pullID)
	return labels, w.fail("list pull labels", err)
}

// DeleteBranch deletes branch from the run's workspace repository. Neither
// outcome below is a failure; both reach the caller unscrubbed, for it to
// record: a branch an open pull request uses is kept and reported as
// *providers.BranchInUseError, and an absent branch is reported as
// providers.ErrBranchNotFound (journaled as "branch-not-found").
func (w *Writer) DeleteBranch(ctx context.Context, runID, branch string) error {
	bound, err := w.runs.Bind(ctx, runID)
	if err != nil {
		return w.fail("delete branch", err)
	}
	backend, err := w.backends.For(ctx, bound, bound.Workspace, string(capability.GitHubBranchDelete))
	if err != nil {
		return w.fail("delete branch", err)
	}
	result, err := backend.DeleteBranch(ctx, providers.DeleteBranchRequest{Repository: bound.Workspace, Name: branch})
	if err == nil && !result.Deleted {
		return fmt.Errorf("delete branch %q: %w", branch, providers.ErrBranchNotFound)
	}
	return w.fail("delete branch", err)
}

// ReleaseClaimMarker releases ref.RunID's own claim on ref.ItemID. A claim a
// successor holds is never closed: the refusal is permanent.
func (w *Writer) ReleaseClaimMarker(ctx context.Context, ref providerwriter.ItemRef, reason claimrelease.Reason) error {
	t, err := w.workItemTarget(ctx, ref)
	if err != nil {
		return w.fail("release claim marker", err)
	}
	backend, err := w.backends.For(ctx, t.bound, t.repo, string(capability.GitHubIssuesWrite))
	if err != nil {
		return w.fail("release claim marker", err)
	}
	_, err = backend.ReleaseWorkItemClaim(ctx, providers.ClaimWorkItemRequest{
		Repository:         t.repo,
		ID:                 ref.ItemID,
		RunID:              ref.RunID,
		ExpectedClaimRunID: ref.RunID,
		Reason:             reason,
	})
	return w.fail("release claim marker", err)
}

func labelsOnly(req providers.UpdateWorkItemRequest) bool {
	return (len(req.AddLabels) > 0 || len(req.RemoveLabels) > 0) &&
		req.Title == nil && req.Body == nil && req.Assignee == nil && req.Milestone == nil &&
		req.State == "" && req.Comment == "" && req.ExpectedRevision == ""
}

// prWrite is the capability a pull request label is written with: ADO grants
// it as provider:pr:write.
func prWrite(repo providers.RepositoryRef) string {
	if repo.Provider == providers.ProviderADO {
		return string(capability.ProviderPRWrite)
	}
	return string(capability.GitHubPRWrite)
}

// writeError is a scrubbed provider-writer failure. Unwrap keeps the
// classification a caller retries on (providerwriter.Permanent) and a rate
// limit's retry-after, never the provider's own error text.
type writeError struct {
	message string
	kinds   []error
}

func (e *writeError) Error() string   { return e.message }
func (e *writeError) Unwrap() []error { return e.kinds }

// fail scrubs err for op. A kept branch passes through untouched: it is a
// decision carrying no provider text, and the caller must see its type.
func (w *Writer) fail(op string, err error) error {
	if err == nil {
		return nil
	}
	var inUse *providers.BranchInUseError
	if errors.As(err, &inUse) {
		return err
	}
	var kinds []error
	switch {
	case errors.Is(err, providerwriter.ErrRepositoryNotPinned):
		kinds = append(kinds, providerwriter.ErrRepositoryNotPinned)
	case errors.Is(err, providerwriter.ErrPermanent), errors.Is(err, ErrUnsupported), errors.Is(err, providers.ErrClaimHeldByAnotherRun):
		kinds = append(kinds, providerwriter.ErrPermanent)
	}
	if limit, ok := providers.AsRateLimitError(err); ok {
		safe := *limit
		safe.Endpoint = w.scrub(safe.Endpoint)
		safe.RetryAfterRaw = w.scrub(safe.RetryAfterRaw)
		safe.RemainingRaw = w.scrub(safe.RemainingRaw)
		safe.ResetRaw = w.scrub(safe.ResetRaw)
		kinds = append(kinds, &safe)
	}
	return &writeError{message: op + ": " + w.scrub(err.Error()), kinds: kinds}
}

func (w *Writer) scrub(s string) string { return string(w.scrubber.Scrub([]byte(s))) }
