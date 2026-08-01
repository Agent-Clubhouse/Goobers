package main

import (
	"context"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// fakeDependencyCheckProvider is the narrow backlogIssueProvider slice
// filterDeclaredDependencyEligibility depends on, stubbed via the same
// embedded-nil-interface pattern fakeMergePolicyProvider
// (mergepolicycache_test.go) uses. It implements HasOpenWorkItemBlocker
// unconditionally so tests can prove declaration — not interface
// implementation — is what governs whether providers.Dispatcher reaches it
// (CONF-5, #2078, closing #2059).
type fakeDependencyCheckProvider struct {
	providers.Provider
	caps         providers.CapabilitySet
	blocked      bool
	blockerErr   error
	blockerCalls int
}

func (f *fakeDependencyCheckProvider) Kind() providers.ProviderKind { return providers.ProviderADO }

func (f *fakeDependencyCheckProvider) Capabilities() providers.CapabilitySet { return f.caps }

func (f *fakeDependencyCheckProvider) ReleaseWorkItemClaim(context.Context, providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
	return providers.WorkItem{}, nil
}

func (f *fakeDependencyCheckProvider) ListWorkItemLabelTransitionsForItem(context.Context, providers.RepositoryRef, string, string) ([]providers.WorkItemLabelTransition, error) {
	return nil, nil
}

func (f *fakeDependencyCheckProvider) HasOpenWorkItemBlocker(context.Context, providers.RepositoryRef, string) (bool, error) {
	f.blockerCalls++
	return f.blocked, f.blockerErr
}

// TestFilterDeclaredDependencyEligibilityFailsClosedWhenUndeclared is
// CONF-5's (#2078) regression test proving #2059's fail-open class cannot
// recur structurally: a provider that does not declare backlog.blockers —
// ADO's real state as of this fix — must have an item with a nonzero
// BlockedByCount excluded with a warning, never silently passed through as
// "not blocked". blockerCalls staying 0 proves providers.Dispatcher
// refused before the provider's own HasOpenWorkItemBlocker (which this
// fake implements) was ever entered — declaration, not interface
// satisfaction, is the authority.
func TestFilterDeclaredDependencyEligibilityFailsClosedWhenUndeclared(t *testing.T) {
	fake := &fakeDependencyCheckProvider{caps: providers.NewCapabilitySet(), blocked: false}
	repo := providers.RepositoryRef{Owner: "acme", Name: "widgets"}
	eligible := []providers.WorkItem{{ID: "42", BlockedByCount: 1}}

	filtered, warnings := filterDeclaredDependencyEligibility(context.Background(), fake, repo, eligible)

	if len(filtered) != 0 {
		t.Fatalf("filtered = %+v, want empty (item with an undeclared blocker check must fail closed, not pass through)", filtered)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %+v, want exactly one warning", warnings)
	}
	if !strings.Contains(warnings[0], "42") {
		t.Errorf("warning %q does not reference item ID 42", warnings[0])
	}
	if !strings.Contains(warnings[0], string(providers.CapBacklogBlockers)) {
		t.Errorf("warning %q does not reference capability %q", warnings[0], providers.CapBacklogBlockers)
	}
	if fake.blockerCalls != 0 {
		t.Errorf("provider's HasOpenWorkItemBlocker was called %d time(s), want 0 — Dispatcher must refuse before dispatch", fake.blockerCalls)
	}
}

// TestFilterDeclaredDependencyEligibilityDispatchesWhenDeclared is the
// positive counterpart: a provider that does declare backlog.blockers and
// implements the real check (GitHub's and Gitea's path today) still filters
// correctly through providers.Dispatcher.
func TestFilterDeclaredDependencyEligibilityDispatchesWhenDeclared(t *testing.T) {
	fake := &fakeDependencyCheckProvider{caps: providers.NewCapabilitySet(providers.CapBacklogBlockers), blocked: true}
	repo := providers.RepositoryRef{Owner: "acme", Name: "widgets"}
	eligible := []providers.WorkItem{
		{ID: "blocked-item", BlockedByCount: 1},
		{ID: "unblocked-item", BlockedByCount: 0},
	}

	filtered, warnings := filterDeclaredDependencyEligibility(context.Background(), fake, repo, eligible)

	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	if len(filtered) != 1 || filtered[0].ID != "unblocked-item" {
		t.Fatalf("filtered = %+v, want only unblocked-item", filtered)
	}
	if fake.blockerCalls != 1 {
		t.Errorf("provider's HasOpenWorkItemBlocker was called %d time(s), want 1 (only the item with BlockedByCount > 0)", fake.blockerCalls)
	}
}
