package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/providers"
)

// fakeDependencyCheckProvider is the narrow backlogIssueProvider slice
// filterDeclaredDependencyEligibilityDebug depends on, stubbed via the same
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
// ADO's state until ADO-N32 — must have an item with a nonzero
// BlockedByCount excluded with a warning, never silently passed through as
// "not blocked". blockerCalls staying 0 proves providers.Dispatcher
// refused before the provider's own HasOpenWorkItemBlocker (which this
// fake implements) was ever entered — declaration, not interface
// satisfaction, is the authority.
func TestFilterDeclaredDependencyEligibilityFailsClosedWhenUndeclared(t *testing.T) {
	fake := &fakeDependencyCheckProvider{caps: providers.NewCapabilitySet(), blocked: false}
	repo := providers.RepositoryRef{Owner: "acme", Name: "widgets"}
	eligible := []providers.WorkItem{{ID: "42", BlockedByCount: 1}}

	filtered, warnings := filterDeclaredDependencyEligibilityDebug(context.Background(), fake, repo, eligible, nil)

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

// newADOPredecessorProvider serves successor 42, linked to predecessor 41
// in predecessorState, over the per-item GET, workitemsbatch and the Issue
// type's state categories (Basic process: Doing is InProgress, Done is
// Completed).
func newADOPredecessorProvider(t *testing.T, predecessorState string) (*providers.ADOProvider, *atomic.Int32) {
	t.Helper()
	var successorReads atomic.Int32
	item := func(id int, state string, relations []map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"id": id,
			"fields": map[string]interface{}{
				"System.WorkItemType": "Issue",
				"System.Title":        "work " + strconv.Itoa(id),
				"System.State":        state,
			},
			"relations": relations,
		}
	}
	successor := item(42, "To Do", []map[string]interface{}{{
		"rel":        "System.LinkTypes.Dependency-Reverse",
		"url":        "https://dev.azure.com/org/project/_apis/wit/workItems/41",
		"attributes": map[string]interface{}{"name": "Predecessor"},
	}})
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, _ *http.Request) {
		successorReads.Add(1)
		writeADOJSON(t, w, successor)
	})
	mux.HandleFunc("/org/project/_apis/wit/workitemsbatch", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []int `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.IDs) != 1 || body.IDs[0] != 41 {
			t.Errorf("workitemsbatch ids = %v (err %v), want [41]", body.IDs, err)
		}
		writeADOJSON(t, w, map[string]interface{}{"value": []interface{}{item(41, predecessorState, nil)}})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitemtypes/", func(w http.ResponseWriter, _ *http.Request) {
		writeADOJSON(t, w, map[string]interface{}{"value": []map[string]string{
			{"name": "To Do", "category": "Proposed"},
			{"name": "Doing", "category": "InProgress"},
			{"name": "Done", "category": "Completed"},
		}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) {
		p.BaseURL = server.URL
	}), &successorReads
}

// TestFilterDeclaredDependencyEligibilityUsesADOPredecessorState pins
// ADO-N32 end to end through backlog-query's filter: an ADO item whose only
// predecessor is done is eligible, and one whose predecessor is still open
// is excluded, naming the open predecessor. Before ADO declared
// backlog.blockers, both were excluded with an "unsupported" warning.
func TestFilterDeclaredDependencyEligibilityUsesADOPredecessorState(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project", Name: "repo"}
	for _, tc := range []struct {
		predecessorState string
		wantEligible     bool
	}{
		{predecessorState: "Done", wantEligible: true},
		{predecessorState: "Doing", wantEligible: false},
	} {
		t.Run(tc.predecessorState, func(t *testing.T) {
			provider, successorReads := newADOPredecessorProvider(t, tc.predecessorState)
			item, err := provider.GetWorkItem(context.Background(), repo, "42")
			if err != nil {
				t.Fatalf("GetWorkItem: %v", err)
			}
			successorReads.Store(0)
			if item.BlockedByCount != 1 {
				t.Fatalf("BlockedByCount = %d, want 1 for an ADO predecessor relation", item.BlockedByCount)
			}
			var reasons []string
			filtered, warnings := filterDeclaredDependencyEligibilityDebug(
				context.Background(), provider, repo, []providers.WorkItem{item},
				func(_ providers.WorkItem, reason string) { reasons = append(reasons, reason) },
			)
			if len(warnings) != 0 {
				t.Fatalf("warnings = %+v, want none (ADO declares backlog.blockers)", warnings)
			}
			if eligible := len(filtered) == 1; eligible != tc.wantEligible {
				t.Fatalf("eligible = %v, want %v (filtered %+v)", eligible, tc.wantEligible, filtered)
			}
			if !tc.wantEligible && (len(reasons) != 1 || !strings.Contains(reasons[0], "open blocker(s): 41")) {
				t.Fatalf("exclusion reasons = %v, want one naming open blocker 41", reasons)
			}
			// #5960: the verdict and the exclusion reason come from one
			// read of the item's dependencies, not a yes/no check followed
			// by a second full read to name the blockers.
			if got := successorReads.Load(); got != 1 {
				t.Fatalf("successor reads = %d, want exactly 1 per dependency check", got)
			}
		})
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

	filtered, warnings := filterDeclaredDependencyEligibilityDebug(context.Background(), fake, repo, eligible, nil)

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

// fakeDependencyListProvider is fakeDependencyCheckProvider for a provider
// that also lists its blockers, as GitHub and ADO do.
type fakeDependencyListProvider struct {
	fakeDependencyCheckProvider
	blockers  map[string][]providers.WorkItem
	listCalls int
}

func (f *fakeDependencyListProvider) ListWorkItemBlockers(_ context.Context, _ providers.RepositoryRef, id string) ([]providers.WorkItem, error) {
	f.listCalls++
	return f.blockers[id], f.blockerErr
}

// TestFilterDeclaredDependencyEligibilityReadsBlockersOnce pins #5960: a
// provider that lists its blockers is read once per item, and both the
// blocked verdict and the exclusion reason come from that one list — an
// item whose only blockers are closed is eligible, one with an open (or
// stateless) blocker is excluded naming exactly those.
func TestFilterDeclaredDependencyEligibilityReadsBlockersOnce(t *testing.T) {
	fake := &fakeDependencyListProvider{
		fakeDependencyCheckProvider: fakeDependencyCheckProvider{caps: providers.NewCapabilitySet(providers.CapBacklogBlockers), blocked: true},
		blockers: map[string][]providers.WorkItem{
			"blocked":  {{ID: "8", State: "closed"}, {ID: "7", State: "open"}, {ID: "9"}},
			"released": {{ID: "8", State: "closed"}},
		},
	}
	repo := providers.RepositoryRef{Owner: "acme", Name: "widgets"}
	eligible := []providers.WorkItem{{ID: "blocked", BlockedByCount: 3}, {ID: "released", BlockedByCount: 1}}
	reasons := map[string]string{}

	filtered, warnings := filterDeclaredDependencyEligibilityDebug(context.Background(), fake, repo, eligible,
		func(item providers.WorkItem, reason string) { reasons[item.ID] = reason })

	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	if len(filtered) != 1 || filtered[0].ID != "released" {
		t.Fatalf("filtered = %+v, want only the item whose blockers all closed", filtered)
	}
	if want := "native issue dependencies include open blocker(s): 7,9"; reasons["blocked"] != want {
		t.Fatalf("exclusion reason = %q, want %q", reasons["blocked"], want)
	}
	if fake.listCalls != 2 || fake.blockerCalls != 0 {
		t.Fatalf("list calls = %d, has-open calls = %d; want one list per item and no separate yes/no read", fake.listCalls, fake.blockerCalls)
	}
}

// TestFilterDeclaredDependencyEligibilityListerStillNeedsDeclaration: a
// provider that can list blockers but does not declare backlog.blockers
// still fails closed through the Dispatcher, never reaching either read.
func TestFilterDeclaredDependencyEligibilityListerStillNeedsDeclaration(t *testing.T) {
	fake := &fakeDependencyListProvider{
		fakeDependencyCheckProvider: fakeDependencyCheckProvider{caps: providers.NewCapabilitySet()},
		blockers:                    map[string][]providers.WorkItem{"42": {{ID: "8", State: "closed"}}},
	}
	filtered, warnings := filterDeclaredDependencyEligibilityDebug(context.Background(), fake,
		providers.RepositoryRef{Owner: "acme", Name: "widgets"}, []providers.WorkItem{{ID: "42", BlockedByCount: 1}}, nil)
	if len(filtered) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], string(providers.CapBacklogBlockers)) {
		t.Fatalf("filtered = %+v, warnings = %+v; want the item excluded with an unsupported-capability warning", filtered, warnings)
	}
	if fake.listCalls != 0 || fake.blockerCalls != 0 {
		t.Fatalf("list calls = %d, has-open calls = %d; want neither reached", fake.listCalls, fake.blockerCalls)
	}
}

// TestFilterDeclaredDependencyEligibilityListErrorFailsClosed: a list error
// excludes the item with a warning, as a failed yes/no check did.
func TestFilterDeclaredDependencyEligibilityListErrorFailsClosed(t *testing.T) {
	fake := &fakeDependencyListProvider{
		fakeDependencyCheckProvider: fakeDependencyCheckProvider{
			caps:       providers.NewCapabilitySet(providers.CapBacklogBlockers),
			blockerErr: errors.New("predecessor batch unavailable"),
		},
	}
	var reasons []string
	filtered, warnings := filterDeclaredDependencyEligibilityDebug(context.Background(), fake,
		providers.RepositoryRef{Owner: "acme", Name: "widgets"}, []providers.WorkItem{{ID: "42", BlockedByCount: 1}},
		func(_ providers.WorkItem, reason string) { reasons = append(reasons, reason) })
	if len(filtered) != 0 || len(warnings) != 1 || len(reasons) != 1 || !strings.Contains(reasons[0], "check unavailable") {
		t.Fatalf("filtered = %+v, warnings = %+v, reasons = %v; want the item excluded as unavailable", filtered, warnings, reasons)
	}
}
