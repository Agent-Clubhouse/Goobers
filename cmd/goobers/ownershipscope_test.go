package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/backlogdefaults"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestIssueOwnershipScopeRejectsWrongOwnerMutations(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}
	base := newOwnershipFakeProvider(repo,
		providers.WorkItem{ID: "1", Assignee: "owner-a"},
		providers.WorkItem{ID: "2", Assignee: "owner-b"},
	)

	ownerA := scopedOwnershipProvider(t, base, "owner-a", "refuse")
	ownerB := scopedOwnershipProvider(t, base, "owner-b", "refuse")

	for _, tc := range []struct {
		name     string
		provider providers.Provider
		id       string
		runID    string
	}{
		{name: "owner-a-provider-refuses-owner-b-issue", provider: ownerA, id: "2", runID: "run-a"},
		{name: "owner-b-provider-refuses-owner-a-issue", provider: ownerB, id: "1", runID: "run-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comment := "status update"
			_, err := tc.provider.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
				Repository: repo,
				ID:         tc.id,
				AddLabels:  []string{providers.LabelClaimed, providers.LabelReady},
				Comment:    comment,
			})
			assertOwnershipRefusal(t, err)

			_, err = tc.provider.UpdateWorkItemStatus(context.Background(), providers.UpdateWorkItemStatusRequest{
				Repository: repo,
				ID:         tc.id,
				Status:     providers.WorkItemStatusInProgress,
				Comment:    "implementing",
			})
			assertOwnershipRefusal(t, err)

			_, err = tc.provider.ClaimWorkItem(context.Background(), providers.ClaimWorkItemRequest{
				Repository: repo,
				ID:         tc.id,
				RunID:      tc.runID,
			})
			assertOwnershipRefusal(t, err)

			releaser := tc.provider.(workItemClaimReleaser)
			_, err = releaser.ReleaseWorkItemClaim(context.Background(), providers.ClaimWorkItemRequest{
				Repository: repo,
				ID:         tc.id,
				RunID:      tc.runID,
			})
			assertOwnershipRefusal(t, err)

			reader := tc.provider.(providerClaimEpochReader)
			_, err = reader.OpenClaimEpochs(context.Background(), repo, tc.id)
			assertOwnershipRefusal(t, err)
		})
	}

	if len(base.updates) != 0 {
		t.Fatalf("updates = %+v, want none against cross-owned issues", base.updates)
	}
	if len(base.statuses) != 0 {
		t.Fatalf("status updates = %+v, want none against cross-owned issues", base.statuses)
	}
	if len(base.claims) != 0 {
		t.Fatalf("claims = %+v, want none against cross-owned issues", base.claims)
	}
	if len(base.releases) != 0 {
		t.Fatalf("claim releases = %+v, want none against cross-owned issues", base.releases)
	}
	if len(base.epochReads) != 0 {
		t.Fatalf("claim epoch reads = %+v, want none against cross-owned issues", base.epochReads)
	}
	for id, item := range base.items {
		if slices.Contains(item.Labels, providers.LabelClaimed) || item.Status != "" || item.Body != "" {
			t.Fatalf("item %s mutated = %+v, want untouched", id, item)
		}
	}
}

func TestIssueOwnershipScopeUnassignedHandlingIsIndependent(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}

	refusing := scopedOwnershipProvider(t, newOwnershipFakeProvider(repo, providers.WorkItem{ID: "7"}), "owner-a", "refuse")
	_, err := refusing.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         "7",
		AddLabels:  []string{providers.LabelReady},
	})
	assertOwnershipRefusal(t, err)

	allowingBase := newOwnershipFakeProvider(repo, providers.WorkItem{ID: "7"})
	allowing := scopedOwnershipProvider(t, allowingBase, "owner-a", "allow")
	if _, err := allowing.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         "7",
		AddLabels:  []string{providers.LabelReady},
	}); err != nil {
		t.Fatalf("UpdateWorkItem with unassigned allow: %v", err)
	}
	if !slices.Contains(allowingBase.items["7"].Labels, providers.LabelReady) {
		t.Fatalf("labels = %v, want %q added", allowingBase.items["7"].Labels, providers.LabelReady)
	}
}

func TestIssueOwnershipScopeEmptyAssigneesOnlyRefusesUnassigned(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}
	base := newOwnershipFakeProvider(repo,
		providers.WorkItem{ID: "7", Assignee: "owner-a"},
		providers.WorkItem{ID: "8"},
	)
	provider := scopedOwnershipProvider(t, base, "", "refuse")

	if _, err := provider.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         "7",
		AddLabels:  []string{providers.LabelReady},
	}); err != nil {
		t.Fatalf("UpdateWorkItem assigned item with unassigned-only refusal: %v", err)
	}
	_, err := provider.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         "8",
		AddLabels:  []string{providers.LabelReady},
	})
	assertOwnershipRefusal(t, err)
}

func TestIssueOwnershipScopeRefusesOutOfScopeReassignmentWithTypedClassification(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}
	provider := scopedOwnershipProvider(t, newOwnershipFakeProvider(repo, providers.WorkItem{ID: "3", Assignee: "owner-a"}), "owner-a", "allow")
	ownerB := "owner-b"

	_, err := provider.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         "3",
		Assignee:   &ownerB,
	})
	assertOwnershipRefusal(t, err)
	code, retryable, extra := classifyProviderError(err)
	if code != errorCodeIssueOwnershipScope || retryable || extra["errorClass"] != "policy_refusal" {
		t.Fatalf("classification = %q, %v, %+v; want ownership policy refusal", code, retryable, extra)
	}
}

func TestIssueOwnershipScopeGuardsCreateAndCommentCapabilities(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}
	base := newOwnershipFakeProvider(repo, providers.WorkItem{ID: "1", Assignee: "owner-b"})
	provider := scopedOwnershipProvider(t, base, "owner-a", "allow")
	if _, ok := provider.(workItemCommentCreator); !ok {
		t.Fatal("wrapped provider lost CreateWorkItemComment capability")
	}
	if _, ok := provider.(workItemLabelEnsurer); !ok {
		t.Fatal("wrapped provider lost EnsureWorkItemLabels capability")
	}

	_, err := provider.CreateWorkItem(context.Background(), providers.CreateWorkItemRequest{
		Repository: repo,
		Title:      "out of scope",
		Assignee:   "owner-b",
	})
	assertOwnershipRefusal(t, err)

	if _, err := provider.CreateWorkItem(context.Background(), providers.CreateWorkItemRequest{
		Repository: repo,
		Title:      "unassigned is allowed",
	}); err != nil {
		t.Fatalf("CreateWorkItem unassigned with allow: %v", err)
	}
	refusingUnassigned := scopedOwnershipProvider(t, base, "owner-a", "refuse")
	_, err = refusingUnassigned.CreateWorkItem(context.Background(), providers.CreateWorkItemRequest{
		Repository: repo,
		Title:      "unassigned is refused",
	})
	assertOwnershipRefusal(t, err)

	commenter := provider.(workItemCommentCreator)
	_, err = commenter.CreateWorkItemComment(context.Background(), repo, "1", "do not touch")
	assertOwnershipRefusal(t, err)
	if len(base.comments) != 0 {
		t.Fatalf("comments = %+v, want no out-of-scope comment writes", base.comments)
	}
}

func TestIssueOwnershipScopeSetsExpectedRevisionAfterScopeCheck(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}
	base := newOwnershipFakeProvider(repo, providers.WorkItem{ID: "1", Assignee: "owner-a", Revision: "1"})
	base.beforeUpdate = func(req providers.UpdateWorkItemRequest) {
		item := base.items[req.ID]
		item.Assignee = "owner-b"
		item.Revision = "2"
		base.items[req.ID] = item
	}
	provider := scopedOwnershipProvider(t, base, "owner-a", "allow")

	_, err := provider.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         "1",
		AddLabels:  []string{providers.LabelReady},
	})
	var conflict *providers.RevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("UpdateWorkItem error = %v, want revision conflict", err)
	}
	if len(base.updates) != 1 || base.updates[0].ExpectedRevision != "1" {
		t.Fatalf("update requests = %+v, want expected revision from checked item", base.updates)
	}
	if slices.Contains(base.items["1"].Labels, providers.LabelReady) {
		t.Fatalf("labels = %v, want guarded write rejected after concurrent reassignment", base.items["1"].Labels)
	}
}

func scopedOwnershipProvider(t *testing.T, provider providers.Provider, assignees, unassigned string) providers.Provider {
	t.Helper()
	t.Setenv(executor.InputEnvVar(backlogdefaults.OwnershipAssigneesInput), assignees)
	t.Setenv(executor.InputEnvVar(backlogdefaults.OwnershipUnassignedInput), unassigned)
	return wrapIssueOwnershipProvider(provider)
}

func assertOwnershipRefusal(t *testing.T, err error) {
	t.Helper()
	var ownershipErr *issueOwnershipScopeError
	if !errors.As(err, &ownershipErr) {
		t.Fatalf("error = %v, want issue ownership refusal", err)
	}
	if ownershipErr.ErrorCode() != errorCodeIssueOwnershipScope || ownershipErr.ErrorClass() != "policy_refusal" {
		t.Fatalf("ownership error = %+v, want typed policy refusal", ownershipErr)
	}
}

type ownershipFakeProvider struct {
	providers.Provider
	repo       providers.RepositoryRef
	items      map[string]providers.WorkItem
	updates    []providers.UpdateWorkItemRequest
	statuses   []providers.UpdateWorkItemStatusRequest
	claims     []providers.ClaimWorkItemRequest
	releases   []providers.ClaimWorkItemRequest
	epochReads []string
	creates    []providers.CreateWorkItemRequest
	comments   []string

	beforeUpdate func(providers.UpdateWorkItemRequest)
}

func newOwnershipFakeProvider(repo providers.RepositoryRef, items ...providers.WorkItem) *ownershipFakeProvider {
	out := &ownershipFakeProvider{repo: repo, items: map[string]providers.WorkItem{}}
	for _, item := range items {
		out.items[item.ID] = item
	}
	return out
}

func (p *ownershipFakeProvider) GetWorkItem(_ context.Context, _ providers.RepositoryRef, id string) (providers.WorkItem, error) {
	item, ok := p.items[id]
	if !ok {
		return providers.WorkItem{}, errors.New("not found")
	}
	return item, nil
}

func (p *ownershipFakeProvider) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	p.updates = append(p.updates, req)
	if p.beforeUpdate != nil {
		p.beforeUpdate(req)
	}
	item := p.items[req.ID]
	if req.ExpectedRevision != "" && item.Revision != req.ExpectedRevision {
		return providers.WorkItem{}, &providers.RevisionConflictError{ItemID: req.ID, Expected: req.ExpectedRevision, Actual: item.Revision}
	}
	if req.Assignee != nil {
		item.Assignee = *req.Assignee
	}
	item.Labels = append(item.Labels, req.AddLabels...)
	if req.Comment != "" {
		item.Body += req.Comment
	}
	p.items[req.ID] = item
	return item, nil
}

func (p *ownershipFakeProvider) CreateWorkItem(_ context.Context, req providers.CreateWorkItemRequest) (providers.WorkItem, error) {
	p.creates = append(p.creates, req)
	id := fmt.Sprintf("%d", len(p.items)+1)
	item := providers.WorkItem{ID: id, Title: req.Title, Body: req.Body, Labels: append([]string(nil), req.Labels...), Assignee: req.Assignee}
	p.items[id] = item
	return item, nil
}

func (p *ownershipFakeProvider) CreateWorkItemComment(_ context.Context, _ providers.RepositoryRef, id, body string) (providers.Comment, error) {
	p.comments = append(p.comments, id+":"+body)
	return providers.Comment{ID: fmt.Sprintf("%d", len(p.comments)), Body: body}, nil
}

func (p *ownershipFakeProvider) EnsureWorkItemLabels(context.Context, providers.RepositoryRef, []providers.WorkItemLabel) (providers.EnsureWorkItemLabelsResult, error) {
	return providers.EnsureWorkItemLabelsResult{}, nil
}

func (p *ownershipFakeProvider) UpdateWorkItemStatus(_ context.Context, req providers.UpdateWorkItemStatusRequest) (providers.WorkItem, error) {
	p.statuses = append(p.statuses, req)
	item := p.items[req.ID]
	item.Status = req.Status
	if req.Comment != "" {
		item.Body += req.Comment
	}
	p.items[req.ID] = item
	return item, nil
}

func (p *ownershipFakeProvider) ClaimWorkItem(_ context.Context, req providers.ClaimWorkItemRequest) (providers.ClaimResult, error) {
	p.claims = append(p.claims, req)
	item := p.items[req.ID]
	item.Labels = append(item.Labels, providers.LabelClaimed)
	p.items[req.ID] = item
	return providers.ClaimResult{Claimed: true, Item: item}, nil
}

func (p *ownershipFakeProvider) ReleaseWorkItemClaim(_ context.Context, req providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
	p.releases = append(p.releases, req)
	item := p.items[req.ID]
	item.Labels = slices.DeleteFunc(item.Labels, func(label string) bool { return label == providers.LabelClaimed })
	p.items[req.ID] = item
	return item, nil
}

func (p *ownershipFakeProvider) OpenClaimEpochs(_ context.Context, _ providers.RepositoryRef, id string) ([]providers.ClaimEpoch, error) {
	p.epochReads = append(p.epochReads, id)
	return []providers.ClaimEpoch{{RunID: "run-" + id, Trusted: true}}, nil
}

func (p *ownershipFakeProvider) ListWorkItemLabelTransitionsForItem(
	context.Context,
	providers.RepositoryRef,
	string,
	string,
) ([]providers.WorkItemLabelTransition, error) {
	return nil, nil
}
